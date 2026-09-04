package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/MoomA-0750/camp/internal/audit"
	"github.com/MoomA-0750/camp/internal/store"
)

// Supervisor は Camp が起こしたセッションを見張る。**campd 側にだけ居る。**
//
// 実際に子を起こすのは実行面（`campd agent`）で、こちらは決めて記録する。
// 分けた理由は proc.go の頭に書いた。
type Supervisor struct {
	db *store.DB

	mu       sync.Mutex
	live     map[string]*liveSession
	agent    *agentConn
	maxConc  int
	stopping bool

	// テストで時間を進めるために差し替える。
	Now func() time.Time
	// テストで待たずに済ませるために差し替える。
	IdleAfter  time.Duration
	TurnAfter  time.Duration
	StartAfter time.Duration
}

type liveSession struct {
	rec   Record
	token string
	// last は最後に何かが起きた時刻。アイドルの判定に使う。
	last time.Time
	// turn はターンが始まった時刻。ゼロならターン中ではない。
	turn time.Time
	// asked は待っている承認。M28 で画面に出す。
	asked map[string]bool
}

// New は supervisor を作る。
func New(db *store.DB) *Supervisor {
	return &Supervisor{
		db:         db,
		live:       map[string]*liveSession{},
		maxConc:    defaultMaxConcurrent,
		Now:        time.Now,
		IdleAfter:  idleTimeout,
		TurnAfter:  turnTimeout,
		StartAfter: startGrace,
	}
}

// SetMaxConcurrent は同時に走らせてよい本数を変える。
func (s *Supervisor) SetMaxConcurrent(n int) {
	if n < 1 {
		n = 1
	}
	if n > maxSessions {
		n = maxSessions
	}
	s.mu.Lock()
	s.maxConc = n
	s.mu.Unlock()
}

// ErrNoAgent は実行面が繋がっていないとき。
var ErrNoAgent = errors.New("実行面が繋がっていない。campd agent を起こす")

// Reconcile は campd の起動時に、DB の行と実際のプロセスを突き合わせる。
//
// **2種類のずれがある。**
//   - 幽霊: DB は走っていると言うが、プロセスは居ない
//   - 孤児: プロセスは生きているが、見張る者が居ない（campd が落ちていた）
//
// 幽霊は閉じる。孤児は閉じない——**まだ動いているものを「終わった」と書かない。**
// orphaned にして、実行面が繋がってきたときに始末を頼む。
func (s *Supervisor) Reconcile() (ghosts, orphans, unknown int, err error) {
	rows, err := listLive(s.db)
	if err != nil {
		return 0, 0, 0, err
	}
	for _, r := range rows {
		alive, known := r.Owner().Alive()
		switch {
		case !known:
			// **「見ていないから居ない」を「居ないから居ない」と読まない。**
			// 判定できなかったものは触らず、数えて外へ出す。
			unknown++
			s.audit(r.ID, "session.reconcile", r.Cwd,
				fmt.Sprintf("pid %d の生死を判定できなかった", r.PID), audit.Error)
		case alive:
			orphans++
			_ = setState(s.db, r.ID, StateOrphaned)
			s.audit(r.ID, "session.orphan", r.Cwd,
				fmt.Sprintf("campd の再起動後も pid %d が生きている", r.PID), audit.OK)
		default:
			ghosts++
			_ = finish(s.db, r.ID, -1, "campd の再起動後に居なかった")
			s.audit(r.ID, "session.ghost", r.Cwd,
				fmt.Sprintf("pid %d はもう居ない", r.PID), audit.OK)
		}
	}
	return ghosts, orphans, unknown, nil
}

// Start は新しいセッションを起こす。**起こすのは実行面だが、決めるのはここ。**
func (s *Supervisor) Start(requestedBy, cwd string) (Record, error) {
	real, err := resolveCwd(cwd)
	if err != nil {
		s.audit("", "session.start", cwd, err.Error(), audit.Denied)
		return Record{}, err
	}

	s.mu.Lock()
	if s.agent == nil {
		s.mu.Unlock()
		s.audit("", "session.start", real, ErrNoAgent.Error(), audit.Denied)
		return Record{}, ErrNoAgent
	}
	if n := len(s.live); n >= s.maxConc {
		s.mu.Unlock()
		err := fmt.Errorf("同時に走らせる上限（%d本）に達している", s.maxConc)
		s.audit("", "session.start", real, err.Error(), audit.Denied)
		return Record{}, err
	}
	agent := s.agent
	s.mu.Unlock()

	id := newID()
	token := newID() + newID()
	t := s.Now().UTC()
	rec := Record{
		ID: id, Cwd: real, State: StateStarting, RequestedBy: requestedBy,
		CreatedAt: t.Format(time.RFC3339), UpdatedAt: t.Format(time.RFC3339),
	}
	// **起こす前に書く。** 起こしてから書くと、その隙に campd が落ちたときに
	// 誰も知らない子が残る。
	if err := insert(s.db, rec); err != nil {
		return Record{}, err
	}

	s.mu.Lock()
	s.live[id] = &liveSession{rec: rec, token: token, last: s.Now(), asked: map[string]bool{}}
	s.mu.Unlock()

	s.audit(id, "session.start", real, "実行面へ起動を依頼した", audit.OK)

	if err := agent.send(Msg{T: MsgStart, Session: id, Token: token, Cwd: real}); err != nil {
		s.fail(id, "実行面へ届かなかった: "+err.Error())
		return Record{}, err
	}
	return rec, nil
}

// Input は走っているセッションへ1行渡す。
func (s *Supervisor) Input(id, text string) error {
	if len(text) > maxText {
		return fmt.Errorf("入力が長すぎる（%d バイト、上限 %d）", len(text), maxText)
	}
	s.mu.Lock()
	ls, agent := s.live[id], s.agent
	if ls == nil {
		s.mu.Unlock()
		return fmt.Errorf("そのセッションは走っていない: %s", id)
	}
	if agent == nil {
		s.mu.Unlock()
		return ErrNoAgent
	}
	if ls.rec.State != StateIdle {
		st := ls.rec.State
		s.mu.Unlock()
		return fmt.Errorf("いまは入力を受けられない（%s）", st)
	}
	ls.rec.State = StateRunning
	ls.turn = s.Now()
	ls.last = s.Now()
	token := ls.token
	s.mu.Unlock()

	_ = setState(s.db, id, StateRunning)
	s.audit(id, "session.input", "", fmt.Sprintf("%d バイト", len(text)), audit.OK)
	return agent.send(Msg{T: MsgInput, Session: id, Token: token, Text: text})
}

// Stop は止める。mode は StopInterrupt か StopTerminate。
func (s *Supervisor) Stop(id, mode string) error {
	if mode != StopInterrupt && mode != StopTerminate {
		return fmt.Errorf("知らない止め方: %s", mode)
	}
	s.mu.Lock()
	ls, agent := s.live[id], s.agent
	if ls == nil {
		s.mu.Unlock()
		return fmt.Errorf("そのセッションは走っていない: %s", id)
	}
	if agent == nil {
		s.mu.Unlock()
		return ErrNoAgent
	}
	ls.rec.State = StateStopping
	ls.last = s.Now()
	token := ls.token
	s.mu.Unlock()

	_ = setState(s.db, id, StateStopping)
	s.audit(id, "session.stop", mode, "", audit.OK)
	return agent.send(Msg{T: MsgStop, Session: id, Token: token, Mode: mode})
}

// Approve は待っている承認に答える。M28 で画面から呼ぶ。
func (s *Supervisor) Approve(id, reqID, behavior, message string) error {
	if behavior != "allow" && behavior != "deny" {
		return fmt.Errorf("知らない答え: %s", behavior)
	}
	s.mu.Lock()
	ls, agent := s.live[id], s.agent
	if ls == nil {
		s.mu.Unlock()
		return fmt.Errorf("そのセッションは走っていない: %s", id)
	}
	if agent == nil {
		s.mu.Unlock()
		return ErrNoAgent
	}
	// **二重に答えさせない。** 同じ承認へ2回答えると、2回目は宙に浮く。
	if !ls.asked[reqID] {
		s.mu.Unlock()
		return fmt.Errorf("その承認は待っていない: %s", reqID)
	}
	delete(ls.asked, reqID)
	ls.last = s.Now()
	token := ls.token
	s.mu.Unlock()

	outcome := audit.OK
	if behavior == "deny" {
		outcome = audit.Denied
	}
	s.audit(id, "tool.approve", reqID, message, outcome)
	return agent.send(Msg{
		T: MsgApprove, Session: id, Token: token,
		ReqID: reqID, Behavior: behavior, Text: message,
	})
}

// Pending は待っている承認の一覧。
func (s *Supervisor) Pending(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ls := s.live[id]
	if ls == nil {
		return nil
	}
	out := make([]string, 0, len(ls.asked))
	for k := range ls.asked {
		out = append(out, k)
	}
	return out
}

// Live は今見張っているセッションの写し。
func (s *Supervisor) Live() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.live))
	for _, ls := range s.live {
		out = append(out, ls.rec)
	}
	return out
}

// AgentConnected は実行面が繋がっているか。
func (s *Supervisor) AgentConnected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agent != nil
}

// fail は起こせなかった・届かなかったセッションを閉じる。
func (s *Supervisor) fail(id, reason string) {
	s.mu.Lock()
	delete(s.live, id)
	s.mu.Unlock()
	_ = finish(s.db, id, -1, reason)
	s.audit(id, "session.exit", "", reason, audit.Error)
}

// Tick は時間切れを回収する。呼ぶ側が周期を決める（テストでは直接呼ぶ）。
func (s *Supervisor) Tick() {
	type action struct {
		id, mode, why string
	}
	var todo []action
	var dead []action

	s.mu.Lock()
	now := s.Now()
	for id, ls := range s.live {
		switch ls.rec.State {
		case StateStarting:
			if now.Sub(ls.last) > s.StartAfter {
				dead = append(dead, action{id: id, why: "起動が確認できないまま時間切れ"})
			}
		case StateRunning:
			if !ls.turn.IsZero() && now.Sub(ls.turn) > s.TurnAfter {
				todo = append(todo, action{id, StopInterrupt, "ターンが長すぎる"})
			}
		case StateIdle:
			if now.Sub(ls.last) > s.IdleAfter {
				todo = append(todo, action{id, StopTerminate, "何も来ないまま時間が経った"})
			}
		}
	}
	s.mu.Unlock()

	for _, a := range dead {
		s.fail(a.id, a.why)
	}
	for _, a := range todo {
		s.audit(a.id, "session.timeout", a.mode, a.why, audit.Timeout)
		_ = s.Stop(a.id, a.mode)
	}
}

// Run は Tick を回し続ける。ctx が終わるまで戻らない。
func (s *Supervisor) Run(done <-chan struct{}, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			s.Tick()
		}
	}
}

func (s *Supervisor) audit(id, action, target, detail, outcome string) {
	_, _ = audit.Append(s.db, audit.Entry{
		Actor: "campd", Action: action, Target: target,
		SessionID: id, Detail: detail, Outcome: outcome,
	})
}

// resolveCwd は cwd を実パスに直して、ディレクトリであることを確かめる。
//
// **symlink を解いてから見る。** 解かずに文字列で照合すると、M29 の許可リストは
// リンク1本で外れる。ここではまだ許可リストを見ないが、**解く場所を先に決めておく**。
func resolveCwd(cwd string) (string, error) {
	if cwd == "" {
		return "", errors.New("cwd が要る")
	}
	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("cwd は絶対パスで指定する: %s", cwd)
	}
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", fmt.Errorf("cwd を辿れない: %w", err)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("cwd がディレクトリではない: %s", real)
	}
	return real, nil
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 乱数が取れないなら黙って弱い値を使わない。
		panic("乱数が取れない: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
