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

	// budgets はセッションごとの「記録してよい件数」。
	// 実行面が起こす audit の増え方を押さえる。
	budgets map[string]*budget
	// reapAsked は向こうの孤児を見に行かせた時刻。繋がらないホストを
	// Tick のたびに叩かないため。
	reapAsked map[string]time.Time

	// readRecords は向こうのホストの記録を読む差し込み口（M47）。**campd が外から挿す**
	// ——ここは取り込みを知らない（`internal/ingest` を import しない）。
	readRecords ReadRecords
	// reading は記録の読みが走っている最中か。**同じ周期が重ならないため。**
	// 携帯の回線では1周に何分もかかりうる。
	reading bool

	// テストで時間を進めるために差し替える。
	Now func() time.Time
	// テストで待たずに済ませるために差し替える。
	IdleAfter time.Duration
	TurnAfter time.Duration
	// ParkAfter は承認を待つ長さ。**0 は「期限切れにしない」**（既定。New は入れない）。
	// CLI にも承認の期限は無い（D-030、本人の決定 2026-09-12。approval.go の parkLimit を見よ）。
	ParkAfter  time.Duration
	StartAfter time.Duration
	StopAfter  time.Duration
	// RemoteReapEvery は向こうを確かめられなかった孤児を見に行き直す間隔。
	RemoteReapEvery time.Duration
}

type liveSession struct {
	rec   Record
	token string
	// last は最後に何かが起きた時刻。アイドルの判定に使う。
	last time.Time
	// turn はターンが始まった時刻。ゼロならターン中ではない。
	turn time.Time
	// interrupted はターンが長すぎて中断を投げた時刻。ゼロなら投げていない。
	// **中断が効かなければ、しばらく待ってから孫まで止める。**
	interrupted time.Time
	// asked は待っている承認。M28 で画面に出す。
	asked map[string]bool
	// logBroken は落とし先へ書けなくなったか。画面に出す。
	logBroken bool
}

// New は supervisor を作る。
func New(db *store.DB) *Supervisor {
	return &Supervisor{
		db:         db,
		live:       map[string]*liveSession{},
		waits:      map[string]chan Msg{},
		budgets:    map[string]*budget{},
		reapAsked:  map[string]time.Time{},
		maxConc:    defaultMaxConcurrent,
		Now:        time.Now,
		IdleAfter:  idleTimeout,
		TurnAfter:  turnTimeout,
		StartAfter: startGrace,
		StopAfter:  stopGrace,

		RemoteReapEvery: remoteReapEvery,
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
			_ = markOrphaned(s.db, r.ID, EndUnseen)
			s.audit(r.ID, "session.orphan", r.Cwd,
				fmt.Sprintf("campd の再起動後も pid %d が生きている", r.PID), audit.OK)
		case r.Host != "":
			// **手元の ssh が居ないだけでは、向こうの子が終わったとは言えない。**
			// stdin を読んでいない子は ssh が死んでも生き残る（実測）。
			// 孤児にして、実行面が繋がってきたら見に行かせる。
			orphans++
			_ = markOrphaned(s.db, r.ID, EndUnseen)
			s.audit(r.ID, "session.orphan", r.Host+":"+r.Cwd,
				fmt.Sprintf("手元の ssh（pid %d）は居ないが、向こうはまだ確かめていない", r.PID), audit.OK)
		default:
			ghosts++
			_ = finish(s.db, r.ID, -1, "campd の再起動後に居なかった", EndUnseen, false)
			// **待っていた承認を「待っている」まま残さない。** 残すと、あとで
			// 期限切れとして閉じられ、「答えずに放置した」に数えられてしまう。
			s.CloseApprovals(r.ID)
			s.audit(r.ID, "session.ghost", r.Cwd,
				fmt.Sprintf("pid %d はもう居ない", r.PID), audit.OK)
		}
	}
	return ghosts, orphans, unknown, nil
}

// Start はこのマシンに新しいセッションを起こす。
func (s *Supervisor) Start(requestedBy, cwd string) (Record, error) {
	return s.StartOn(requestedBy, "", cwd)
}

// StartOn は host（空ならこのマシン）に Claude Code のセッションを起こす。
func (s *Supervisor) StartOn(requestedBy, host, cwd string) (Record, error) {
	return s.StartAgent(requestedBy, host, cwd, AgentClaude)
}

// StartAgent は host（空ならこのマシン）に agent のセッションを、確認の度合い「CLI と同じ」で起こす。
func (s *Supervisor) StartAgent(requestedBy, host, cwd, agent string) (Record, error) {
	return s.StartWith(requestedBy, host, cwd, agent, PermCLI)
}

// StartWith は host（空ならこのマシン）に agent のセッションを、確認の度合い perm で起こす。
// **起こすのは実行面だが、決めるのはここ。**
func (s *Supervisor) StartWith(requestedBy, host, cwd, agent, perm string) (Record, error) {
	return s.startWith(requestedBy, host, cwd, agent, perm, resumeOf{})
}

// resumeOf は「どの会話の、どの行の続きか」。空なら新しく起こす（M48、2026-09-13）。
type resumeOf struct {
	// AgentID はエージェント自身のセッション id（Claude の session_id・Codex のスレッド id）。
	// **これを実行面へ渡す。** 実測（2026-09-13）: 続きから起こしてもこの id は変わらず、
	// 記録も同じファイルへ追記される——取り込みは何も変えなくてよい。
	AgentID string
	// From は続きの元になった台帳の行（runtime_sessions.id）。記録として残すだけ。
	From string
}

func (s *Supervisor) startWith(requestedBy, host, cwd, agent, perm string, res resumeOf) (Record, error) {
	agent, perm = agentOr(agent), permOr(perm)
	if !validAgent(agent) {
		err := fmt.Errorf("知らないエージェント: %q", agent)
		s.audit("", "session.start", cwd, err.Error(), audit.Denied)
		return Record{}, err
	}
	if !validPerm(perm) {
		err := fmt.Errorf("知らない確認の度合い: %q", perm)
		s.audit("", "session.start", cwd, err.Error(), audit.Denied)
		return Record{}, err
	}
	if info := drivers[agent].Info(); host != "" && !info.Remote {
		// 向こうのホストで起こせないエージェント（Codex は M42 まで）。
		err := fmt.Errorf("%s はまだ向こうのホストでは起こせない（このマシンだけ）", info.Label)
		s.audit("", "session.start", host+":"+cwd, err.Error(), audit.Denied)
		return Record{}, err
	}
	// **照合するのは campd 側。** 実行面は本人のユーザーで動くので、
	// そこでの照合は迂回できる。ここが唯一の境界。
	var real, root, target string
	var spec *RemoteSpec
	if host == "" {
		r, err := CheckCwd(s.db, cwd)
		if err != nil {
			s.audit("", "session.start", cwd, err.Error(), audit.Denied)
			return Record{}, err
		}
		if root, err = matchedRoot(s.db, r); err != nil {
			return Record{}, err
		}
		real, target = r, r
	} else {
		// 向こうのパスは campd には実パスに直せない。文字の上で決め、
		// symlink は向こうの sh が解いてから塞ぐ（remote.go）。
		var err error
		real, root, spec, err = checkRemote(s.db, agent, host, cwd)
		if err != nil {
			s.audit("", "session.start", host+":"+cwd, err.Error(), audit.Denied)
			return Record{}, err
		}
		target = host + ":" + real
	}

	id := newID()
	token := newID() + newID()
	t := s.Now().UTC()
	rec := Record{
		ID: id, Agent: agent, Perm: perm, Cwd: real, State: StateStarting, RequestedBy: requestedBy,
		CreatedAt: t.Format(time.RFC3339), UpdatedAt: t.Format(time.RFC3339),
		Host: host, ResumedFrom: res.From,
	}

	// **枠は数えたその場で押さえる。**
	// 数えてから錠を外して、それから live に入れると、同時に来た要求が
	// 全部同じ空き枠を見て、上限を越えて起こしてしまう。
	s.mu.Lock()
	if s.agent == nil {
		s.mu.Unlock()
		s.audit("", "session.start", target, ErrNoAgent.Error(), audit.Denied)
		return Record{}, ErrNoAgent
	}
	if !s.agent.can(agent) {
		// **古い実行面に Codex を頼まない。** 読まれない欄は黙って落ち、claude が起きる。
		s.mu.Unlock()
		err := fmt.Errorf("いまの実行面は %s を起こせない（codex が無いか、実行面が古い。camp-agent を入れ替える）", agent)
		s.audit("", "session.start", target, err.Error(), audit.Denied)
		return Record{}, err
	}
	if !s.agent.canPerm(agent, perm) {
		// **名乗っていない度合いを頼まない。** 読まれずに落ち、本人の設定のまま起きる。
		s.mu.Unlock()
		err := fmt.Errorf("いまの実行面は %s を確認の度合い %s で起こせない（実行面が古い。camp-agent を入れ替える）", agent, perm)
		s.audit("", "session.start", target, err.Error(), audit.Denied)
		return Record{}, err
	}
	if host == "" && s.agent.remoteOnly(agent) {
		// **手元に実体が無い。** 向こうのホストでなら起こせる（M49）。頼めば実行面が
		// 断るが、ここで止めれば理由がはっきり出る。
		s.mu.Unlock()
		err := fmt.Errorf("いまの実行面は %s をこのマシンでは起こせない（実体が無い）。向こうのホストでなら起こせる", agent)
		s.audit("", "session.start", target, err.Error(), audit.Denied)
		return Record{}, err
	}
	if res.AgentID != "" && !s.agent.canResume(agent) {
		// **続きからを名乗らない実行面へは頼まない。** 読まれずに落ちると、続きのつもりで
		// 新しい会話が始まってしまう（M48）。
		s.mu.Unlock()
		err := fmt.Errorf("いまの実行面は %s を続きから起こせない（実行面が古い。camp-agent を入れ替える）", agent)
		s.audit("", "session.start", target, err.Error(), audit.Denied)
		return Record{}, err
	}
	if n := len(s.live); n >= s.maxConc {
		s.mu.Unlock()
		err := fmt.Errorf("同時に走らせる上限（%d本）に達している", s.maxConc)
		s.audit("", "session.start", target, err.Error(), audit.Denied)
		return Record{}, err
	}
	ac := s.agent
	s.live[id] = &liveSession{rec: rec, token: token, last: s.Now(), asked: map[string]bool{}}
	s.mu.Unlock()

	// **起こす前に書く。** 起こしてから書くと、その隙に campd が落ちたときに
	// 誰も知らない子が残る。
	if err := insert(s.db, rec); err != nil {
		s.mu.Lock()
		delete(s.live, id)
		s.mu.Unlock()
		return Record{}, err
	}

	s.audit(id, "session.start", target, "実行面へ起動を依頼した（"+agent+"、確認の度合い "+perm+"）", audit.OK)

	if err := ac.send(Msg{T: MsgStart, Session: id, Token: token, Cwd: real, Root: root,
		Remote: spec, Agent: agent, Perm: perm, Resume: res.AgentID}); err != nil {
		// **「届かなかった」を「起きなかった」と確定しない。**
		// 途中まで書けていれば実行面は子を起こしている。ここで exited と
		// 書くと、あとから来る started も、繋ぎ直しの名乗りも弾いてしまい、
		// 生きた子が台帳から外れる。starting のまま残し、started が来なければ
		// 起動猶予（Tick）で閉じる。
		s.audit(id, "session.start", target,
			"実行面へ届いたか分からない: "+err.Error(), audit.Error)
		return rec, err
	}
	return rec, nil
}

// ResumeWith は終わったセッションの続きから起こす（M48、2026-09-13）。
//
// **新しい行を作り、元の行はそのまま残す**（本人の決定 2026-09-13）。終わった行を生き返らせると、
// どう終わったか・いつ・待たせたまま終わった承認が上書きされ、過去が消える。プロセスとの
// 1 対 1（pid・起動時刻・boot_id・scope で所有権を見る）も崩れる。**画面では「元のものが
// 生き返った」ように見せる**（台帳は別の行のまま）。
//
// 起こす場所・確認の度合い・エージェント・ホストは**元の行から引き継ぐ**。場所は引き継いだ
// うえで startWith が改めて許可を照らす（許した場所が狭まっているかもしれない）。
func (s *Supervisor) ResumeWith(requestedBy, from string) (Record, error) {
	src, err := Get(s.db, from)
	if err != nil {
		return Record{}, err
	}
	// **走っているものは続けない。** 同じ会話を2つ開くと、Claude は「別の端末で走っている」
	// として拒み、Codex は読み込み済みのスレッドへの上書きを無視する（どちらも実体の文言）。
	if src.Live() {
		return Record{}, fmt.Errorf("そのセッションはまだ終わっていない（%s）。止めてから続ける", src.State)
	}
	// **エージェント自身の id が要る。** これが無い行は、子が名乗る前に終わっている。
	if src.ClaudeID == "" {
		return Record{}, fmt.Errorf("そのセッションはエージェント側の id を名乗らないまま終わった。続きから起こせない")
	}
	// Phase 3.6 の Codex の行（legacy）は専用の置き場で起こしていたので、続けられない。
	if !validPerm(permOr(src.Perm)) {
		return Record{}, fmt.Errorf("そのセッションは確認の度合い %s で起きていた。続きから起こせない", src.Perm)
	}
	return s.startWith(requestedBy, src.Host, src.Cwd, src.Agent, src.Perm,
		resumeOf{AgentID: src.ClaudeID, From: src.ID})
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
	ls.interrupted = time.Time{}
	ls.last = s.Now()
	token := ls.token
	s.mu.Unlock()

	_ = setState(s.db, id, StateRunning)
	s.audit(id, "session.input", "", fmt.Sprintf("%d バイト", len(text)), audit.OK)
	return agent.send(Msg{T: MsgInput, Session: id, Token: token, Text: text})
}

// Stop は止める。mode は StopInterrupt か StopTerminate。**本人が止めるときの口。**
//
// 中断（interrupt）はターンを止めるだけで、セッションは終わらない。
// **状態も変えない**——running なら result が来て idle に戻る。2026-09-11 まで
// 中断でも stopping にしていたので、中断した途端に入力を受けなくなり、
// 2分後には「止まらない」として生きている子を exited と書いていた。
func (s *Supervisor) Stop(id, mode string) error {
	return s.stop(id, mode, EndUserStop)
}

// stop は止める。cause は、terminate でこのまま終わったときの理由。
func (s *Supervisor) stop(id, mode, cause string) error {
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
	if mode == StopTerminate {
		ls.rec.State = StateStopping
	}
	ls.last = s.Now()
	token := ls.token
	s.mu.Unlock()

	detail := ""
	if mode == StopTerminate {
		_ = markStopping(s.db, id, cause)
		detail = cause
	}
	s.audit(id, "session.stop", mode, detail, audit.OK)
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

	if behavior == "deny" {
		// **理由を空のまま先へ渡さない。** 空だと子の会話が壊れて、
		// 以後どの発言も通らなくなる（agent.go の approveFrame 参照）。
		message = DenyReason(message)
	}
	if err := agent.send(Msg{
		T: MsgApprove, Session: id, Token: token,
		ReqID: reqID, Behavior: behavior, Text: message,
	}); err != nil {
		// **届かなかったなら、答えたことにしない。**
		// DB だけ「回答済み」にすると、子は待ったまま、画面は答えたと出て、
		// 同じ承認へ答え直すこともできなくなる。
		if err2 := reopen(s.db, id, reqID); err2 != nil {
			s.audit(id, "tool.approve", reqID,
				"届かず、戻すのにも失敗した: "+err2.Error(), audit.Error)
		} else {
			s.audit(id, "tool.approve", reqID,
				"実行面へ届かなかったので待ちに戻した: "+err.Error(), audit.Error)
		}
		return fmt.Errorf("実行面へ届かなかった: %w", err)
	}

	outcome := audit.OK
	if behavior == "deny" {
		outcome = audit.Denied
	}
	s.audit(id, "tool.approve", reqID, message, outcome)
	return nil
}

// Pending は待っている承認の request_id。**DB から読む。**
//
// エラーを空に畳まない。読めなかったことを「待っている承認は無い」と
// 読ませると、子が止まったまま画面には何も出ない。
func (s *Supervisor) Pending(id string) ([]string, error) {
	rows, err := openApprovals(s.db, id)
	if err != nil {
		return nil, approvalError("読み取り", err)
	}
	out := make([]string, 0, len(rows))
	for _, a := range rows {
		out = append(out, a.RequestID)
	}
	return out, nil
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

// AgentStale は実行面が campd と違うビルドで動いているか（build.go）。
//
// **止めない。知らせるだけ。** 古い実行面でも動くことは動く。ただし直したはずの不具合が
// 直っていないので、画面に出さないと気づけない（2026-09-12、向こうのホストの記録が
// 読めないまま「最後に読めた」と出ていた）。
func (s *Supervisor) AgentStale() (stale bool, build string) {
	s.mu.Lock()
	a := s.agent
	s.mu.Unlock()
	if a == nil {
		return false, ""
	}
	return buildMismatch(selfBuild(), a.build), a.build
}

// fail は起こせなかった・止まらなかったセッションを閉じる。
// **控えてある理由より、こちらが正しい**（止めろと言ったが止まらなかった、など）。
func (s *Supervisor) fail(id, reason, cause string) {
	s.mu.Lock()
	delete(s.live, id)
	s.mu.Unlock()
	_ = finish(s.db, id, -1, reason, cause, true)
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

// withdrawAsk は実行面が取り下げた承認を閉じる（Codex）。
//
// 実行面が断った（訊いたあとで差分が変わった）・Codex 側で片付いた・答えが子へ届かなかった。
// **台帳を「待っている」や「本人が許した」のまま残さない**——実際には子へ届いていない。
func (s *Supervisor) withdrawAsk(m Msg) {
	s.mu.Lock()
	if ls := s.live[m.Session]; ls != nil {
		delete(ls.asked, m.ReqID)
	}
	s.mu.Unlock()
	ok, err := withdraw(s.db, m.Session, m.ReqID, s.Now())
	if err != nil {
		s.audit(m.Session, "tool.approve", m.ReqID, "取り下げを記録できない: "+err.Error(), audit.Error)
		return
	}
	if ok {
		s.auditFromAgent(m.Session, "tool.withdrawn", m.ReqID, m.Error, audit.Denied)
	}
}

// Tick は時間切れを回収する。呼ぶ側が周期を決める（テストでは直接呼ぶ）。
func (s *Supervisor) Tick() {
	type action struct {
		id, mode, why, cause string
	}
	var todo []action
	var dead []action

	s.mu.Lock()
	now := s.Now()
	for id, ls := range s.live {
		switch ls.rec.State {
		case StateStarting:
			if now.Sub(ls.last) > s.StartAfter {
				dead = append(dead, action{id: id, why: "起動が確認できないまま時間切れ",
					cause: EndStartFailed})
			}
		case StateRunning:
			if s.TurnAfter <= 0 || ls.turn.IsZero() || now.Sub(ls.turn) <= s.TurnAfter {
				break
			}
			// **まず中断。効かなければ孫まで止める。** 中断を投げっぱなしにすると、
			// 効かない子は running のまま枠を1つ握り続ける。
			switch {
			case ls.interrupted.IsZero():
				ls.interrupted = now
				todo = append(todo, action{id, StopInterrupt, "ターンが長すぎる", EndTurnTimeout})
			case now.Sub(ls.interrupted) > s.StopAfter:
				todo = append(todo, action{id, StopTerminate,
					"ターンが長すぎ、中断も効かない", EndTurnTimeout})
			}
		case StateIdle:
			if s.IdleAfter > 0 && now.Sub(ls.last) > s.IdleAfter {
				todo = append(todo, action{id, StopTerminate,
					"何も来ないまま時間が経った", EndIdleTimeout})
			}
		case StateStopping:
			// **止めろと言ったのに止まらない。** 実行面が受け取り損ねた・
			// 子が signal を無視した、どちらもありうる。放っておくと
			// stopping のまま永久に残るので、期限を切って諦める。
			if now.Sub(ls.last) > s.StopAfter {
				dead = append(dead, action{id: id, why: "止めろと言ったのに止まらない",
					cause: EndStopTimeout})
			}
		}
	}
	s.mu.Unlock()

	// **期限切れを、期限切れとして答える。ただし既定では期限を見ない**（ParkAfter は 0。
	// Phase 3 の「`claude` 自身が5分で諦める」は誤りだった。approval.go の parkLimit を見よ）。
	var late []Approval
	if s.ParkAfter > 0 {
		got, err := expired(s.db, s.Now())
		if err != nil {
			// **読めなかったことを「期限切れは無い」と読ませない。**
			s.audit("", "tool.approve", "", "期限切れを数えられない: "+err.Error(), audit.Error)
		}
		late = got
	}
	{
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
				if err := agent.send(Msg{T: MsgApprove, Session: a.SessionID, Token: token,
					ReqID: a.RequestID, Behavior: "deny",
					Text: "期限切れ（Camp が待てる時間を過ぎた）"}); err != nil {
					// 届かなくても、期限が切れたこと自体は動かない。
					// ただし**子には伝わっていない**ので、そう書く。
					s.audit(a.SessionID, "tool.approve", a.Tool,
						"期限切れの拒否が子へ届いていない: "+err.Error(), audit.Error)
				}
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
		s.fail(a.id, a.why, a.cause)
	}
	for _, a := range todo {
		s.audit(a.id, "session.timeout", a.mode, a.why, audit.Timeout)
		_ = s.stop(a.id, a.mode, a.cause)
	}
}

// sweepOrphans は orphaned の行を見に行き、もう居ないものを閉じる。
//
// 生きているものは触らない——**動いているものを「終わった」と書かない**のは
// Reconcile と同じ。判定できなかったものも触らない。
func (s *Supervisor) sweepOrphans() {
	rows, err := listLive(s.db)
	if err != nil {
		s.audit("", "session.reconcile", "",
			"孤児を見に行けない: "+err.Error(), audit.Error)
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
		if r.Host != "" {
			// **手元の ssh が居ないことは、向こうが終わったことを意味しない。**
			// 実行面に見に行かせる。繋がらなければ、しばらくしてまた行かせる。
			s.askRemoteReap(r, false)
			continue
		}
		// 実行面が落ちて孤児にしたのなら、控えてある「実行面が落ちた」が残る。
		_ = finish(s.db, r.ID, -1, "見張る者が居ないうちに終わっていた", EndUnseen, false)
		s.CloseApprovals(r.ID)
		s.audit(r.ID, "session.ghost", "",
			fmt.Sprintf("孤児にしていた pid %d はもう居ない", r.PID), audit.OK)
	}
}

// askRemoteReap は向こうの孤児を実行面に見に行かせる。
// force でなければ、前に行かせてから RemoteReapEvery 経つまで行かせない。
func (s *Supervisor) askRemoteReap(r Record, force bool) {
	s.mu.Lock()
	agent := s.agent
	last := s.reapAsked[r.ID]
	due := agent != nil && (force || last.IsZero() || s.Now().Sub(last) >= s.RemoteReapEvery)
	if due {
		s.reapAsked[r.ID] = s.Now()
	}
	s.mu.Unlock()
	if !due {
		return
	}
	// **掃除でも行き先を照らす。** 起こすときは実行面が `ssh -G` と固定を照らしているのに、
	// 掃除の経路はそれを飛ばして繋いでいた（Fable の M47 設計レビュー 4）。`~/.ssh/config` の
	// HostName を書き換えれば、固定と違う先へ定期的に繋ぎに行けてしまう。
	msg := Msg{T: MsgReap, Session: r.ID, PID: r.PID, Started: r.Started,
		BootID: r.BootID, RemoteOwner: r.remoteOwner()}
	if r.Host != "" {
		if d, err := getDestination(s.db, r.Host); err == nil && d.Pinned.Pinned() {
			msg.Remote = &RemoteSpec{Alias: r.Host, Pin: *d.Pinned}
		}
	}
	_ = agent.send(msg)
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

// auditFromAgent は**実行面が起こした**記録を残す。枠を使い切っていれば残さない。
//
// campd 自身が起こす記録（起動を決めた・許可リストを見た）は絞らない。
// 絞るのは、境界の外から何度でも起こせるものだけ。
func (s *Supervisor) auditFromAgent(id, action, target, detail, outcome string) {
	ok, first := s.mayRecord(id)
	if ok {
		s.audit(id, action, target, detail, outcome)
		return
	}
	if first {
		s.audit(id, action, target,
			fmt.Sprintf("1分あたり %d 件を越えたので、しばらく記録しない", maxAuditPerMin),
			audit.Denied)
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
		s.summarize(id, m.Lines)
		return TailResult{Lines: m.Lines, Gap: m.Gap, Newest: m.Seq, Dropped: m.Dropped}, nil
	case <-time.After(15 * time.Second):
		// **返ってこないことを「空」と読まない。**
		return TailResult{}, errors.New("実行面が返事をしない")
	}
}

// budget は1セッションぶんの記録の枠。
type budget struct {
	window time.Time
	n      int
	// said は「枠を使い切った」を1度だけ記録するための印。
	said bool
}

// mayRecord は、このセッションについてまだ記録してよいかを返す。
//
// 使い切ったときは false を返し、**そのことを1度だけ記録する**
// （黙って捨てると「見ていないから0」になる）。
func (s *Supervisor) mayRecord(id string) (ok, firstRefusal bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.budgets[id]
	if b == nil {
		b = &budget{window: s.Now()}
		s.budgets[id] = b
	}
	if s.Now().Sub(b.window) >= time.Minute {
		b.window, b.n, b.said = s.Now(), 0, false
	}
	if b.n >= maxAuditPerMin {
		if !b.said {
			b.said = true
			return false, true
		}
		return false, false
	}
	b.n++
	return true, false
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
	// **どのエージェントにも同じ問い合わせだけを通す**（D-031）。mcp_status は Codex に同じものが
	// 無く、画面も使っていないので外した（Fable の設計レビュー）。
	switch subtype {
	case "get_usage", "get_context_usage":
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
