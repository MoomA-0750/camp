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
	// waits は実行面へ投げた問い合わせの返事待ち。request_id で対応づける。
	waits map[string]chan Msg

	// テストで時間を進めるために差し替える。
	Now func() time.Time
	// テストで待たずに済ませるために差し替える。
	IdleAfter  time.Duration
	TurnAfter  time.Duration
	StartAfter time.Duration
	StopAfter  time.Duration
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
		waits:      map[string]chan Msg{},
		maxConc:    defaultMaxConcurrent,
		Now:        time.Now,
		IdleAfter:  idleTimeout,
		TurnAfter:  turnTimeout,
		StartAfter: startGrace,
		StopAfter:  stopGrace,
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
	// **照合するのは campd 側。** 実行面は本人のユーザーで動くので、
	// そこでの照合は迂回できる。ここが唯一の境界。
	real, err := CheckCwd(s.db, cwd)
	if err != nil {
		s.audit("", "session.start", cwd, err.Error(), audit.Denied)
		return Record{}, err
	}
	root, err := matchedRoot(s.db, real)
	if err != nil {
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

	if err := agent.send(Msg{T: MsgStart, Session: id, Token: token, Cwd: real, Root: root}); err != nil {
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
	delete(ls.asked, reqID)
	ls.last = s.Now()
	token := ls.token
	s.mu.Unlock()

	// **二重に答えさせない。** 判定は DB で行う（campd を入れ替えても効く）。
	// 同じ承認へ2回答えると、2回目は子に届かず宙に浮く。
	first, err := answer(s.db, id, reqID, behavior, ByUser, s.Now())
	if err != nil {
		return approvalError("記録", err)
	}
	if !first {
		return fmt.Errorf("その承認はもう答えてある（または待っていない）: %s", reqID)
	}

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

// Pending は待っている承認の request_id。**DB から読む。**
func (s *Supervisor) Pending(id string) []string {
	rows, err := openApprovals(s.db, id)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, a := range rows {
		out = append(out, a.RequestID)
	}
	return out
}

// Waiting は待っている承認を中身つきで返す。画面はこれを出す。
func (s *Supervisor) Waiting(id string) ([]Approval, error) {
	return openApprovals(s.db, id)
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
	s.CloseApprovals(id)
	s.audit(id, "session.exit", "", reason, audit.Error)
}

// CloseApprovals は宙に浮いた承認を閉じる。**待っていたものを残さない。**
func (s *Supervisor) CloseApprovals(id string) {
	n, err := closeOpen(s.db, id, s.Now())
	if err != nil || n == 0 {
		return
	}
	s.audit(id, "tool.approve", "",
		fmt.Sprintf("セッションが終わったので %d 件を拒否として閉じた", n), audit.Denied)
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
		case StateStopping:
			// **止めろと言ったのに止まらない。** 実行面が受け取り損ねた・
			// 子が signal を無視した、どちらもありうる。放っておくと
			// stopping のまま永久に残るので、期限を切って諦める。
			if now.Sub(ls.last) > s.StopAfter {
				dead = append(dead, action{id: id, why: "止めろと言ったのに止まらない"})
			}
		}
	}
	s.mu.Unlock()

	// **期限切れを、期限切れとして答える。**
	// 放っておくと `claude` 自身のパーク期限（5分）で子が勝手に諦め、
	// 何が起きたか分からない記録になる。
	if late, err := expired(s.db, s.Now()); err == nil {
		for _, a := range late {
			if ok, err := answer(s.db, a.SessionID, a.RequestID, "deny", ByTimeout, s.Now()); err != nil || !ok {
				continue
			}
			s.audit(a.SessionID, "tool.approve", a.Tool,
				"期限切れ。拒否として扱った", audit.Timeout)
			s.mu.Lock()
			ls, agent := s.live[a.SessionID], s.agent
			token := ""
			if ls != nil {
				delete(ls.asked, a.RequestID)
				token = ls.token
			}
			s.mu.Unlock()
			if agent != nil && token != "" {
				agent.send(Msg{T: MsgApprove, Session: a.SessionID, Token: token,
					ReqID: a.RequestID, Behavior: "deny",
					Text: "期限切れ（Camp が待てる時間を過ぎた）"})
			}
		}
	}

	// **孤児を、孤児のまま置き去りにしない。**
	//
	// 実行面が落ちると、その子は stdin が閉じて自分で終わる（CLI の仕様。
	// 2026-09-04 に実測）。落ちた瞬間はまだ生きているので orphaned にするが、
	// そのあと誰も見に行かないと、台帳は永久に「孤児」のまま残る。
	// **「見ていないから孤児」を「孤児だから孤児」と読ませない。**
	s.sweepOrphans()

	for _, a := range dead {
		s.fail(a.id, a.why)
	}
	for _, a := range todo {
		s.audit(a.id, "session.timeout", a.mode, a.why, audit.Timeout)
		_ = s.Stop(a.id, a.mode)
	}
}

// sweepOrphans は orphaned の行を見に行き、もう居ないものを閉じる。
//
// 生きているものは触らない——**動いているものを「終わった」と書かない**のは
// Reconcile と同じ。判定できなかったものも触らない。
func (s *Supervisor) sweepOrphans() {
	rows, err := listLive(s.db)
	if err != nil {
		return
	}
	for _, r := range rows {
		if r.State != StateOrphaned {
			continue
		}
		s.mu.Lock()
		adopted := s.live[r.ID] != nil
		s.mu.Unlock()
		if adopted {
			continue // 引き取り直されている
		}
		alive, known := r.Owner().Alive()
		if alive || !known {
			continue
		}
		_ = finish(s.db, r.ID, -1, "見張る者が居ないうちに終わっていた")
		s.CloseApprovals(r.ID)
		s.audit(r.ID, "session.ghost", "",
			fmt.Sprintf("孤児にしていた pid %d はもう居ない", r.PID), audit.OK)
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

// TailResult は画面が受け取る形。
type TailResult struct {
	Lines []Line `json:"lines"`
	// Gap はカーソルより前が落ちていたか。**黙って飛ばさない。**
	Gap     bool  `json:"gap"`
	Newest  int64 `json:"newest"`
	Dropped int64 `json:"dropped"`
}

// Tail は実行面に、画面が要求した範囲だけを出させる。
//
// **campd はフレームを溜めない。** 溜めると、境界を越える量が読み手と無関係に
// 決まってしまう。要求されたぶんだけ、その都度もらう。
func (s *Supervisor) Tail(id string, since int64, limit int) (TailResult, error) {
	s.mu.Lock()
	agent := s.agent
	token := ""
	if ls := s.live[id]; ls != nil {
		token = ls.token
	}
	s.mu.Unlock()
	if agent == nil {
		return TailResult{}, ErrNoAgent
	}

	req := newID()
	ch := make(chan Msg, 1)
	s.mu.Lock()
	s.waits[req] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.waits, req)
		s.mu.Unlock()
	}()

	if err := agent.send(Msg{T: MsgTail, Session: id, Token: token,
		ReqID: req, Since: since, Limit: limit}); err != nil {
		return TailResult{}, err
	}
	select {
	case m := <-ch:
		if m.Error != "" {
			return TailResult{}, fmt.Errorf("%s", m.Error)
		}
		return TailResult{Lines: m.Lines, Gap: m.Gap, Newest: m.Seq, Dropped: m.Dropped}, nil
	case <-time.After(15 * time.Second):
		// **返ってこないことを「空」と読まない。**
		return TailResult{}, errors.New("実行面が返事をしない")
	}
}

// deliver は返事を待っている者へ渡す。
func (s *Supervisor) deliver(m Msg) {
	s.mu.Lock()
	ch := s.waits[m.ReqID]
	s.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- m:
	default:
	}
}

// matchedRoot は real を通した許可リストの行を返す。実行面へ渡して二重に照合させる。
func matchedRoot(db *store.DB, real string) (string, error) {
	list, err := ListAllowed(db)
	if err != nil {
		return "", err
	}
	best := ""
	for _, a := range list {
		if under(real, a.Path) && len(a.Path) > len(best) {
			best = a.Path
		}
	}
	if best == "" {
		return "", ErrNotAllowed{Path: real}
	}
	return best, nil
}

// Control は走っているセッションへ制御フレームを1つ投げて、答えを返す。
//
// 使えるのは**読み取りだけ**。`set_permission_mode` のような、子の振る舞いを
// 変えるものはここから出せない——画面の1クリックで承認の要否が変わると、
// 監査ログの意味が薄くなる。
func (s *Supervisor) Control(id, subtype string) ([]byte, error) {
	switch subtype {
	case "get_usage", "get_context_usage", "mcp_status":
	default:
		return nil, fmt.Errorf("この制御は出せない: %s", subtype)
	}
	s.mu.Lock()
	agent := s.agent
	token := ""
	if ls := s.live[id]; ls != nil {
		token = ls.token
	}
	s.mu.Unlock()
	if agent == nil {
		return nil, ErrNoAgent
	}
	if token == "" {
		return nil, fmt.Errorf("そのセッションは走っていない: %s", id)
	}

	req := newID()
	ch := make(chan Msg, 1)
	s.mu.Lock()
	s.waits[req] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.waits, req)
		s.mu.Unlock()
	}()
	if err := agent.send(Msg{T: MsgControl, Session: id, Token: token,
		ReqID: req, Kind: subtype}); err != nil {
		return nil, err
	}
	select {
	case m := <-ch:
		if m.Error != "" {
			return nil, fmt.Errorf("%s", m.Error)
		}
		return m.Frame, nil
	case <-time.After(15 * time.Second):
		return nil, errors.New("実行面が返事をしない")
	}
}

// Capacity はいま何本走っていて、上限がいくつか。**同時実行の警告に使う。**
func (s *Supervisor) Capacity() (running, max int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.live), s.maxConc
}
