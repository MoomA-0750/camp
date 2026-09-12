package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/MoomA-0750/camp/internal/audit"
)

// Control は実行面と話す口。
//
// 報告口（internal/report）が一方通行の追記専用なのに対し、こちらは双方向。
// **だからこそ、繋げる相手を絞る。** socket は 0660 で campreport グループ、
// さらに uid を指定して照合する。名乗りは見ない（SO_PEERCRED で決める）。
type Control struct {
	s    *Supervisor
	ln   net.Listener
	path string
	// allowUID は実行面になってよい uid。**負なら誰でも。**
	// ゼロ値は 0（root だけ）になるので、`Listen` 以外でこの型を作るときは
	// 必ず明示する。
	allowUID int

	mu     sync.Mutex
	closed bool
}

// agentConn は繋がっている実行面。**同時に1つだけ。**
//
// 2つ繋げると、どちらが本物かを campd が決められない（両方とも同じ uid で
// 動いている）。先に繋いだほうを本物として、あとは断る。
type agentConn struct {
	c   net.Conn
	who string
	mu  sync.Mutex
	// agents はこの実行面が起こせるエージェント（hello で名乗る）。
	// **名乗らない古い実行面は claude だけ。** Codex を頼むと claude が起きてしまう（Fable 7）。
	agents []string
	// infos は hello で名乗った駆動器の説明。
	infos map[string]AgentInfo
}

// announced は hello で名乗った起こせる名前か。**名乗らない古い実行面は claude だけ。**
func (a *agentConn) announced(agent string) bool {
	if len(a.agents) == 0 {
		return agent == AgentClaude
	}
	return contains(a.agents, agent)
}

// can は agent を起こせる実行面か。
//
// **駆動器を名乗らない実行面（Phase 3.7 より前）には claude しか頼まない。** Phase 3.6 の実行面は
// codex を名乗るが、Codex を専用の置き場で approvalPolicy untrusted 固定のまま起こす。それを
// 「CLI と同じ」として台帳に書いてしまう（started の perm も空で返るので照らしても気づけない）。
// claude は古い実行面でも本人の設定のまま起こすので、cli として扱ってよい。
func (a *agentConn) can(agent string) bool {
	if !a.announced(agent) {
		return false
	}
	if _, named := a.infos[agent]; !named {
		return agent == AgentClaude
	}
	return true
}

// info はその実行面での agent の説明。**駆動器を名乗らない古い実行面の claude は、手元の駆動器から
// 補い、確認の度合いは cli だけとみなす**——古い実行面は perm を読まずに本人の設定のまま起こす
// （claude 以外は can が断る）。
func (a *agentConn) info(agent string) (AgentInfo, bool) {
	if !a.can(agent) {
		return AgentInfo{}, false
	}
	d, ok := drivers[agent]
	if !ok {
		return AgentInfo{}, false
	}
	if in, named := a.infos[agent]; named {
		return in, true
	}
	local := d.Info()
	local.Perms = []string{PermCLI}
	return local, true
}

// leavesTools は、中断でターンが終わっても走っていた工具が残るエージェントか。
//
// **起こせるかどうかと切り離して見る**——引き取り直した子は、いまの実行面が起こせない
// エージェントのこともある。どちらか（手元の駆動器・実行面の名乗り）が残ると言えば残るとみなす
// （止めすぎるほうへ倒す）。
func (a *agentConn) leavesTools(agent string) bool {
	if d, ok := drivers[agent]; ok && d.Info().InterruptLeavesTools {
		return true
	}
	return a.infos[agent].InterruptLeavesTools
}

// canPerm は agent を確認の度合い perm で起こせる実行面か。
func (a *agentConn) canPerm(agent, perm string) bool {
	in, ok := a.info(agent)
	return ok && contains(in.Perms, perm)
}

// namedInfos は hello の Drivers から、知っている駆動器・知っている度合いだけを採る。
func namedInfos(a *agentConn, ds []AgentInfo) map[string]AgentInfo {
	out := map[string]AgentInfo{}
	for _, d := range ds {
		// **can ではなく announced で見る。** can は「駆動器を名乗ったか」を見るので、
		// いま組み立てている最中の infos を参照してしまう。
		if !validAgent(d.Name) || !a.announced(d.Name) {
			continue
		}
		perms := []string{}
		for _, p := range d.Perms {
			if validPerm(p) {
				perms = append(perms, p)
			}
		}
		d.Perms = perms
		out[d.Name] = d
	}
	return out
}

func (a *agentConn) send(m Msg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > maxLine {
		return fmt.Errorf("送る行が長すぎる（%d バイト）", len(b))
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err = a.c.Write(append(b, '\n'))
	return err
}

// Listen は制御口を開く。allowUID が 0 以上ならその uid 以外を断る。
func (s *Supervisor) Listen(path, group string, allowUID int) (*Control, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	if len(path) >= 108 {
		return nil, fmt.Errorf(
			"制御口のパスが長すぎる（%d バイト、上限 107）: %s", len(path), path)
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	if group != "" {
		if err := regroupSock(dir, path, group); err != nil {
			ln.Close()
			os.Remove(path)
			return nil, err
		}
	}
	return &Control{s: s, ln: ln, path: path, allowUID: allowUID}, nil
}

func regroupSock(dir, path, group string) error {
	g, err := user.LookupGroup(group)
	if err != nil {
		return fmt.Errorf("グループ %s が無い: %w", group, err)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return err
	}
	for _, p := range []string{dir, path} {
		if err := os.Chown(p, -1, gid); err != nil {
			return fmt.Errorf("%s を %s のものにできない: %w", p, group, err)
		}
	}
	return os.Chmod(dir, 0o750)
}

// Addr は開いている socket のパス。
func (c *Control) Addr() string { return c.path }

// Close は口を閉じる。
func (c *Control) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	err := c.ln.Close()
	os.Remove(c.path)
	return err
}

// Serve は受け付け続ける。
func (c *Control) Serve() error {
	for {
		conn, err := c.ln.Accept()
		if err != nil {
			c.mu.Lock()
			closed := c.closed
			c.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		go c.handle(conn)
	}
}

func (c *Control) handle(conn net.Conn) {
	defer conn.Close()

	uid, pid, err := peerUID(conn)
	if err != nil {
		writeMsg(conn, Msg{T: MsgError, Error: "呼び出し元を確かめられない"})
		return
	}
	if c.allowUID >= 0 && int(uid) != c.allowUID {
		writeMsg(conn, Msg{T: MsgError, Error: "この uid は実行面になれない"})
		c.s.audit("", "agent.connect", fmt.Sprintf("uid:%d", uid),
			"許された uid ではない", audit.Denied)
		return
	}
	who := fmt.Sprintf("uid:%d pid:%d", uid, pid)

	a := &agentConn{c: conn, who: who}

	// **最初の1行は hello でなければならない。** 名乗る前に指示は受けない。
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if !sc.Scan() {
		return
	}
	var hello Msg
	if err := json.Unmarshal(sc.Bytes(), &hello); err != nil || hello.T != MsgHello {
		writeMsg(conn, Msg{T: MsgError, Error: "最初に hello が要る"})
		return
	}

	c.s.mu.Lock()
	if c.s.agent != nil {
		c.s.mu.Unlock()
		writeMsg(conn, Msg{T: MsgError, Error: "実行面は既に繋がっている"})
		c.s.audit("", "agent.connect", who, "既に繋がっているので断った", audit.Denied)
		return
	}
	a.agents = hello.Agents
	a.infos = namedInfos(a, hello.Drivers)
	c.s.agent = a
	c.s.mu.Unlock()

	c.s.audit("", "agent.connect", who, fmt.Sprintf("実行面が繋がった（version=%s、起こせる=%v）",
		hello.Version, agentsOf(a)), audit.OK)
	a.send(Msg{T: MsgWelcome})
	// **引き取り直しが先。** 先に孤児を始末すると、実行面がまだ抱えている
	// 子まで殺してしまう（campd を入れ替えるたびにセッションが飛ぶ）。
	c.readopt(hello.Held)
	c.reapOrphans(a)

	defer c.dropAgent(a)

	for {
		// 繋ぎっぱなしで黙っているだけの接続を許さない。
		conn.SetReadDeadline(time.Now().Add(2 * idleHeartbeat))
		if !sc.Scan() {
			return
		}
		var m Msg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			a.send(Msg{T: MsgError, Error: "読めない行"})
			continue
		}
		c.dispatch(a, m)
	}
}

// idleHeartbeat は実行面が生きていることを示す間隔。
const idleHeartbeat = 30 * time.Second

// dropAgent は実行面が落ちたときの後始末。
//
// **子は死んでいない。** 見張る者だけが居なくなったので、走っていたものは
// 孤児になる。「終わった」とは書かない。
func (c *Control) dropAgent(a *agentConn) {
	c.s.mu.Lock()
	if c.s.agent != a {
		c.s.mu.Unlock()
		return
	}
	c.s.agent = nil
	ids := make([]string, 0, len(c.s.live))
	for id := range c.s.live {
		ids = append(ids, id)
	}
	c.s.live = map[string]*liveSession{}
	c.s.mu.Unlock()

	c.s.audit("", "agent.disconnect", a.who,
		fmt.Sprintf("実行面が落ちた。見張っていたのは %d 本", len(ids)), audit.Error)
	for _, id := range ids {
		// **子は実行面と一緒に終わる**（stdin が閉じる）。それを仕様とした
		// （2026-09-11、本人）。このあと Tick が居なくなったのを見て閉じるときに、
		// 理由が「実行面が落ちた」になるよう控えておく。
		_ = markOrphaned(c.s.db, id, EndAgentLost)
		c.s.audit(id, "session.orphan", "", "実行面が落ちた", audit.Error)
	}
}

// readopt は実行面がまだ抱えている子を引き取り直す。
//
// **名乗りをそのまま信じない。** pid と起動時刻は campd が /proc で確かめる。
// 確かめられなければ引き取らない——「たぶん生きている」で台帳を進めない。
func (c *Control) readopt(held []Held) {
	s := c.s
	for _, h := range held {
		r, err := get(s.db, h.ID)
		if err != nil {
			s.audit(h.ID, "session.readopt", "", "台帳に無い", audit.Error)
			continue
		}
		if r.State == StateExited {
			s.audit(h.ID, "session.readopt", "", "台帳では終わっている", audit.Error)
			continue
		}
		// **台帳が既に知っている pid と一致すること。**
		// 一致を見ないと、偽の実行面が自分の持つ生きた pid を名乗って、
		// 他人のセッションの行を乗っ取れる（そのあと偽の exited や
		// 承認要求を campd 自身に書かせられる）。
		if r.PID != 0 && (h.PID != r.PID || h.Started != r.Started) {
			s.audit(h.ID, "session.readopt", strconv.Itoa(h.PID),
				fmt.Sprintf("台帳の pid %d/%d と違うものを名乗った", r.PID, r.Started),
				audit.Denied)
			continue
		}
		if c.allowUID >= 0 {
			if uid, ok := OwnerUID(h.PID); ok && uid != c.allowUID {
				s.audit(h.ID, "session.readopt", strconv.Itoa(h.PID),
					fmt.Sprintf("uid %d のプロセスを名乗った（実行面は %d）", uid, c.allowUID),
					audit.Denied)
				continue
			}
		}
		o := Owner{PID: h.PID, Started: h.Started, BootID: h.BootID}
		alive, known := o.Alive()
		if !known {
			s.audit(h.ID, "session.readopt", strconv.Itoa(h.PID),
				"生死を確かめられないので引き取らない", audit.Error)
			continue
		}
		if !alive {
			s.audit(h.ID, "session.readopt", strconv.Itoa(h.PID),
				"抱えていると言われたが、そのプロセスは居ない", audit.Error)
			continue
		}
		// **scope 名を名乗らせない。** これは後で
		// `systemctl --user stop <scope>` に渡る。任意の名前を通すと、
		// 同じユーザーの無関係な unit を止められる。
		scope := r.Scope
		if h.Scope != "" {
			if h.Scope != scopeName(h.ID) {
				s.audit(h.ID, "session.readopt", h.Scope,
					"知らない scope 名を名乗ったので採らない", audit.Denied)
			} else {
				scope = h.Scope
			}
		}
		// **向こうの身元も台帳と照らす。** started が届く前に campd が落ちていれば、
		// 台帳は向こうの pid をまだ知らない。そのときは名乗りを採って書く。
		// 知っているなら、違うものは採らない（手元の pid と同じ理由）。
		ro := h.RemoteOwner
		if r.Host != "" {
			why := ""
			switch {
			case ro == nil || ro.PID <= 0 || ro.Started == 0:
				why = "向こうの身元を名乗らなかった"
			case ro.Host != r.Host:
				why = fmt.Sprintf("台帳は %s なのに %s の子を名乗った", r.Host, ro.Host)
			case !under(ro.Cwd, ro.Root):
				why = "向こうで降りた先が許した場所の外と名乗った"
			case r.RemotePID != 0 && (ro.PID != r.RemotePID || ro.Started != r.RemoteStarted):
				why = fmt.Sprintf("台帳の向こうの pid %d/%d と違うものを名乗った", r.RemotePID, r.RemoteStarted)
			}
			if why != "" {
				s.audit(h.ID, "session.readopt", r.Host, why, audit.Denied)
				continue
			}
		} else if ro != nil {
			s.audit(h.ID, "session.readopt", ro.Host, "このマシンの行なのに向こうの子を名乗った", audit.Denied)
			continue
		}
		// **エージェントも台帳と照らす。** Claude の行を Codex の子で乗っ取らせない（逆も）。
		if got, want := agentOr(h.Agent), agentOr(r.Agent); got != want {
			s.audit(h.ID, "session.readopt", got,
				fmt.Sprintf("台帳は %s なのに %s の子を名乗った", want, got), audit.Denied)
			continue
		}
		s.mu.Lock()
		if s.live[h.ID] != nil {
			s.mu.Unlock()
			continue
		}
		// **ターンの途中かもしれない。** idle と決めてしまうと、応答生成中の
		// 子に入力を重ねて送れる。実行面が running と言うなら、そのまま扱う
		// （result が来るまで入力を受けない）。分からないときも running 側に倒す。
		r.State = StateRunning
		if h.State == StateIdle {
			r.State = StateIdle
		}
		r.PID, r.Started, r.BootID, r.Scope = o.PID, o.Started, o.BootID, scope
		if ro != nil {
			r.Cwd, r.RemotePID, r.RemoteStarted = ro.Cwd, ro.PID, ro.Started
			r.RemoteBootID, r.RemoteScope = ro.BootID, ro.Scope
		}
		s.live[h.ID] = &liveSession{rec: r, token: h.Token, last: s.Now(),
			turn: turnStartFor(r.State, s.Now()), asked: map[string]bool{}}
		s.mu.Unlock()
		_ = setOwner(s.db, h.ID, o, scope)
		if ro != nil {
			_ = setRemote(s.db, h.ID, *ro)
		}
		_ = setState(s.db, h.ID, r.State)
		// 見張りは戻った。「見張りが外れていた」の控えはもう理由にならない。
		_ = clearOrphanCause(s.db, h.ID)
		s.audit(h.ID, "session.readopt", strconv.Itoa(h.PID),
			"実行面がまだ抱えていたので引き取り直した（"+r.State+"）", audit.OK)
		s.adoptWaiting(h)
	}
}

// adoptWaiting は実行面が名乗った「待っている承認」を台帳に採る。
//
// **campd が居ない間に来た承認は、台帳に無い。** 採らないと画面に出ず、期限切れの拒否も
// 掛からず、子は答えを待ったまま長く止まる（Fable の設計レビュー 5）。知っているものは足さない。
func (s *Supervisor) adoptWaiting(h Held) {
	if len(h.Waiting) == 0 {
		return
	}
	known := map[string]bool{}
	if rows, err := ApprovalHistory(s.db, h.ID, 500); err == nil {
		for _, a := range rows {
			known[a.RequestID] = true
		}
	}
	for _, w := range h.Waiting {
		if w.ReqID == "" || known[w.ReqID] {
			continue
		}
		s.recordAsk(Msg{Session: h.ID, Token: h.Token, ReqID: w.ReqID, Text: w.Tool, Frame: w.Detail})
		s.mu.Lock()
		if ls := s.live[h.ID]; ls != nil {
			ls.asked[w.ReqID] = true
		}
		s.mu.Unlock()
	}
}

// turnStartFor は running で引き取ったときにターンの起点を入れる。
// ゼロのままだと、終わらないターンを回収できない。
func turnStartFor(state string, now time.Time) time.Time {
	if state == StateRunning {
		return now
	}
	return time.Time{}
}

// reapOrphans は前回の残りを実行面に始末してもらう。
func (c *Control) reapOrphans(a *agentConn) {
	rows, err := listLive(c.s.db)
	if err != nil {
		return
	}
	for _, r := range rows {
		if r.State != StateOrphaned {
			continue
		}
		// 引き取り直したものは孤児ではない。
		c.s.mu.Lock()
		adopted := c.s.live[r.ID] != nil
		c.s.mu.Unlock()
		if adopted {
			continue
		}
		if r.Host != "" {
			c.s.askRemoteReap(r, true)
			continue
		}
		a.send(Msg{T: MsgReap, Session: r.ID, PID: r.PID,
			Started: r.Started, BootID: r.BootID, Scope: r.Scope})
	}
}

// dispatchHook はテストが campd に届いた Msg の列を採るための口（golden_test.go）。本番では nil。
var dispatchHook func(Msg)

func (c *Control) dispatch(a *agentConn, m Msg) {
	if dispatchHook != nil {
		dispatchHook(m)
	}
	s := c.s
	switch m.T {
	case MsgStarted:
		ls, ok := s.check(m)
		if !ok {
			a.send(Msg{T: MsgError, Session: m.Session, Error: "知らないセッション"})
			return
		}
		// **実行面の言う pid を鵜呑みにしない。** campd 自身が /proc を読んで
		// 起動時刻を確かめる。読めない環境では「確かめられなかった」と記録する
		// ——「見ていないから合っている」にはしない。
		// **名乗られた pid が、実行面と同じユーザーのものか。**
		//
		// campd は「そのプロセスが本当に実行面の子か」までは確かめられない。
		// だが、無関係な system のプロセスを指させることは防げる——
		// 台帳に入ると、あとで reap されたときに本当に止めてしまう。
		if c.allowUID >= 0 {
			if uid, ok := OwnerUID(m.PID); !ok {
				s.audit(m.Session, "session.started", strconv.Itoa(m.PID),
					"pid の持ち主を確かめられない", audit.Error)
			} else if uid != c.allowUID {
				s.audit(m.Session, "session.started", strconv.Itoa(m.PID),
					fmt.Sprintf("uid %d のプロセスを名乗った（実行面は %d）", uid, c.allowUID),
					audit.Denied)
				s.fail(m.Session, "実行面と違うユーザーのプロセスを名乗った", EndStartFailed)
				return
			}
		}
		o := Owner{PID: m.PID, Started: m.Started, BootID: m.BootID}
		if st, err := Starttime(m.PID); err == nil {
			if st != m.Started {
				s.audit(m.Session, "session.started", strconv.Itoa(m.PID),
					fmt.Sprintf("実行面の言う起動時刻(%d)が /proc(%d) と違う", m.Started, st),
					audit.Error)
				o.Started = st // **自分で読んだほうを採る**
			}
		} else {
			s.audit(m.Session, "session.started", strconv.Itoa(m.PID),
				"起動時刻を自分で確かめられなかった: "+err.Error(), audit.Error)
		}
		if o.BootID == "" {
			o.BootID = BootID()
		}

		// **頼んだエージェントが起きたか。** 古い実行面は agent を読まずに claude を起こす
		// （Fable 7）。台帳と違うものを走らせたままにしない。
		s.mu.Lock()
		host, want, wantPerm := ls.rec.Host, agentOr(ls.rec.Agent), permOr(ls.rec.Perm)
		s.mu.Unlock()
		why := ""
		if got := agentOr(m.Agent); got != want {
			why = fmt.Sprintf("%s を頼んだのに %s が起きた", want, got)
		} else if got := permOr(m.Perm); got != wantPerm {
			// **頼んだ確認の度合いで起きたか。** 違う度合いで走らせたままにしない。
			why = fmt.Sprintf("確認の度合い %s を頼んだのに %s で起きた", wantPerm, got)
		}
		if why != "" {
			_ = a.send(Msg{T: MsgStop, Session: m.Session, Token: m.Token, Mode: StopTerminate})
			s.audit(m.Session, "session.started", strconv.Itoa(m.PID), why, audit.Denied)
			s.fail(m.Session, why, EndStartFailed)
			return
		}

		// **向こうの身元は確かめられない。** 実行面の報告を記録するだけ。
		// ただし、頼んだホストのものか・許した場所の中と言っているかは照らす。
		ro := m.RemoteOwner
		if host != "" {
			why := ""
			switch {
			case ro == nil || ro.PID <= 0:
				why = "向こうの身元が来ない"
			case ro.Host != host:
				why = fmt.Sprintf("頼んだのは %s なのに %s の子を名乗った", host, ro.Host)
			case !under(ro.Cwd, ro.Root):
				why = fmt.Sprintf("向こうで降りた先が許した場所の外（%s）", ro.Cwd)
			}
			if why != "" {
				_ = a.send(Msg{T: MsgStop, Session: m.Session, Token: m.Token, Mode: StopTerminate})
				s.audit(m.Session, "session.started", host, why, audit.Denied)
				s.fail(m.Session, why, EndStartFailed)
				return
			}
		} else if ro != nil {
			_ = a.send(Msg{T: MsgStop, Session: m.Session, Token: m.Token, Mode: StopTerminate})
			s.fail(m.Session, "このマシンに頼んだのに、向こうの子を名乗った", EndStartFailed)
			return
		}

		s.mu.Lock()
		ls.rec.State = StateIdle
		ls.rec.PID, ls.rec.Started, ls.rec.BootID, ls.rec.Scope = o.PID, o.Started, o.BootID, m.Scope
		if ro != nil {
			ls.rec.Cwd = ro.Cwd
			ls.rec.RemotePID, ls.rec.RemoteStarted = ro.PID, ro.Started
			ls.rec.RemoteBootID, ls.rec.RemoteScope = ro.BootID, ro.Scope
		}
		ls.last = s.Now()
		s.mu.Unlock()

		// **idle にするのは最後。** setOwner が state を idle にするので、先に書くと、
		// 台帳を読んだ者が「起きた」のに向こうの pid やセッション id がまだ無い行を見る
		// （2026-09-12、並列度を下げたテストで実測。セッション id の書き込みを間に足して窓が広がった）。
		// **書けなかったら言う。** 台帳に持ち主が残らないと、campd を入れ替えたあとに
		// 引き取り直せず、向こうの残りも始末できない（codex exec のレビュー、2026-09-12）。
		if m.ClaudeID != "" {
			// Codex は話し始める前にスレッド id が決まっている。
			if err := setClaudeID(s.db, m.Session, m.ClaudeID); err != nil {
				s.audit(m.Session, "session.started", host,
					"エージェントのセッション id を台帳に書けない: "+err.Error(), audit.Error)
			}
		}
		if ro != nil {
			if err := setRemote(s.db, m.Session, *ro); err != nil {
				s.audit(m.Session, "session.started", host,
					"向こうの持ち主を台帳に書けない: "+err.Error(), audit.Error)
			}
		}
		if err := setOwner(s.db, m.Session, o, m.Scope); err != nil {
			s.audit(m.Session, "session.started", host,
				"持ち主を台帳に書けない: "+err.Error(), audit.Error)
		}
		if ro != nil {
			s.auditFromAgent(m.Session, "session.started", host,
				fmt.Sprintf("向こうの pid %d（scope=%s）", ro.PID, ro.Scope), audit.OK)
		}
		s.auditFromAgent(m.Session, "session.started", strconv.Itoa(m.PID),
			"scope="+m.Scope, audit.OK)

	case MsgFrame:
		ls, ok := s.check(m)
		if !ok {
			return
		}
		// 駆動器が畳んだ意味を読む。**古い実行面は欄を立てない**ので、Claude の種類も読む。
		turnEnd := m.TurnEnd || m.Kind == "result"
		isAsk := (m.Ask || m.Kind == "control_request/can_use_tool") && m.ReqID != ""
		s.mu.Lock()
		ls.last = s.Now()
		idle, escalate := false, false
		if turnEnd {
			// ターンが終わった。次の入力を受けられる。
			// **止めに入っているものは idle に戻さない**（孫まで止める途中）。
			if ls.rec.State == StateRunning {
				ls.rec.State = StateIdle
				idle = true
			}
			// **中断は効くが、走っていた工具は残る**エージェントがある（Codex。実測）。ターンが
			// 長すぎて Camp が中断を投げ、それで終わったなら、続けて止める。「中断が効かなければ
			// 止める」には届かないので（Fable 8）。どのエージェントかは駆動器の説明で決める。
			escalate = !ls.interrupted.IsZero() && a.leavesTools(agentOr(ls.rec.Agent))
			ls.turn = time.Time{}
			ls.interrupted = time.Time{}
		}
		if isAsk {
			ls.asked[m.ReqID] = true
		}
		s.mu.Unlock()

		if idle {
			_ = setState(s.db, m.Session, StateIdle)
		}
		if m.ClaudeID != "" {
			_ = setClaudeID(s.db, m.Session, m.ClaudeID)
		}
		if isAsk {
			s.recordAsk(m)
		}
		if m.Note != "" {
			// 止めずに記録する（設定が途中で変わった等）。エラーではない。枠の中で。
			s.auditFromAgent(m.Session, "session.note", m.Kind, m.Note, audit.OK)
		}
		if m.Withdrawn && m.ReqID != "" {
			// 実行面が取り下げた承認を、待っているまま残さない。
			s.withdrawAsk(m)
		} else if m.Halt {
			// **頼んだ確認の度合いで起きていない。** 違う度合いで走らせたままにしない（起こしたときに
			// 1回だけ照らす。本人の決定）。孫まで止めて「起こせなかった」と書く。
			s.audit(m.Session, "session.perm", m.Kind, m.Error, audit.Denied)
			_ = s.stop(m.Session, StopTerminate, EndStartFailed)
		} else if m.Error != "" {
			// 断った・方針が変わった・ターンが失敗した。**黙って流さない。**
			// 実行面が何度でも起こせるので枠の中で。
			s.auditFromAgent(m.Session, "session.frame", m.Kind, m.Error, audit.Error)
		}
		if escalate {
			s.audit(m.Session, "session.timeout", StopTerminate,
				"中断でターンは終わったが、このエージェントは走っていた工具を残すので止める", audit.Timeout)
			_ = s.stop(m.Session, StopTerminate, EndTurnTimeout)
		}

	case MsgExited:
		ls, ok := s.check(m)
		if !ok {
			return
		}
		s.mu.Lock()
		host := ls.rec.Host
		delete(s.live, m.Session)
		s.mu.Unlock()
		// 止めろと言ってあれば、控えてある理由（本人が止めた・放置で閉じた）が残る。
		// 何も言っていないのに終わったなら、子が自分で終わった。
		cause := EndSelf
		if m.ConnLost {
			cause = EndConnLost
		}
		if host != "" && (m.RemoteEnd == RemoteUnreachable || m.RemoteEnd == RemoteUnsupported) {
			// **向こうを確かめられないうちは「終わった」と書かない。**
			// 手元の ssh は終わったが、向こうの子はまだ工具を走らせているかもしれない。
			// 孤児にして、繋がり次第見に行かせる。承認はもう届けようがないので閉じる。
			_ = markOrphaned(s.db, m.Session, cause)
			s.CloseApprovals(m.Session)
			s.audit(m.Session, "session.orphan", host,
				"向こうを確かめられないので孤児として残す: "+m.Reason, audit.Error)
			return
		}
		override := false
		if m.Leftover != 0 {
			// **止め切れていない（確かめられない）ものを「子が自分で終わった」と書かない。**
			// 見張りを諦めた、と書く（Phase 3.6 の outer gate で codex が指摘）。
			cause, override = EndStopTimeout, true
		}
		_ = finish(s.db, m.Session, m.Code, m.Reason, cause, override)
		s.CloseApprovals(m.Session)
		out := audit.OK
		if m.Code != 0 || m.Leftover != 0 {
			out = audit.Error
		}
		s.audit(m.Session, "session.exit", strconv.Itoa(m.Code), m.Reason, out)

	case MsgFailed:
		if _, ok := s.check(m); !ok {
			return
		}
		s.fail(m.Session, m.Error, EndStartFailed)

	case MsgReaped:
		// **始末したという申告を、そのまま信じない。** /proc を見る。
		r, err := get(s.db, m.Session)
		if err != nil {
			return
		}
		alive, known := r.Owner().Alive()
		if r.Host != "" && !alive {
			s.reapedRemote(r, m)
			return
		}
		switch {
		case alive:
			s.audit(m.Session, "session.reap", strconv.Itoa(r.PID),
				"始末したと言われたが、まだ生きている", audit.Error)
		case !known:
			s.audit(m.Session, "session.reap", strconv.Itoa(r.PID),
				"始末したと言われたが、確かめられなかった", audit.Error)
		default:
			// 止め切れていない（確かめられない）残りがあるなら、「始末した」とは書かない。
			cause, out := EndReaped, audit.OK
			if m.Leftover != 0 {
				cause, out = EndStopTimeout, audit.Error
			}
			_ = finish(s.db, m.Session, m.Code, "孤児を始末した: "+m.Reason, cause, true)
			s.CloseApprovals(m.Session)
			s.audit(m.Session, "session.reap", strconv.Itoa(r.PID), m.Reason, out)
		}

	case MsgTailRes, MsgSSHRes, MsgCtlRes, MsgSSHResolved:
		s.deliver(m)

	case MsgDropped:
		if _, ok := s.check(m); !ok {
			return
		}
		// **溢れて捨てたことを記録に残す。** 画面に出ない範囲があることは、
		// あとから「無かった」と読み違えられる。
		// ただし、これも実行面が何度でも起こせるので枠の中で。
		if m.Dropped < 0 {
			// 溢れたのではなく、**落とし先そのものが壊れた**。
			// 「中身が無い」と「保存できなかった」を混ぜない。
			s.auditFromAgent(m.Session, "session.log_broken", "",
				"落とし先へ書けない: "+m.Error, audit.Error)
			s.mu.Lock()
			if ls := s.live[m.Session]; ls != nil {
				ls.logBroken = true
			}
			s.mu.Unlock()
			return
		}
		s.auditFromAgent(m.Session, "session.log_dropped", "",
			fmt.Sprintf("落とし先が溢れて %d 件捨てた", m.Dropped), audit.Error)

	case MsgPing:
		// 生きている合図。何もしない（読み取りの期限が延びるだけ）。

	case MsgHello:
		a.send(Msg{T: MsgError, Error: "hello は1回だけ"})

	default:
		a.send(Msg{T: MsgError, Error: "知らない種類: " + m.T})
	}
}

// reapedRemote は向こうの孤児を見に行った報告を受ける。
//
// **campd はこれを確かめられない**（向こうの /proc を読めるのは鍵を持つ側だけ）。
// 手元なら /proc で「本当に居ないか」を見るが、ここでは実行面の報告を記録するだけ。
func (s *Supervisor) reapedRemote(r Record, m Msg) {
	// **「確かめようがない」を「終わった」と書かない**（codex の指摘。以前は
	// unsupported を gone と同じに扱い、生きているかもしれない子を閉じていた）。
	// /proc の無いホストではそもそも起こさないので、ここへ来るのは異常なときだけ。
	switch m.RemoteEnd {
	case RemoteGone:
		// 控えてある理由（実行面が落ちた・SSH が切れた）が残る。
		_ = finish(s.db, r.ID, -1, "見張る者が居ないうちに終わっていた。向こう: "+m.Reason,
			EndUnseen, false)
	case RemoteKilled:
		// SSH が切れて残っていたものなら、「切れた」を残す（始末したことは理由の文に）。
		_ = finish(s.db, r.ID, -1, "孤児を始末した。向こう: "+m.Reason,
			EndReaped, r.EndCause != EndConnLost)
	default:
		s.audit(r.ID, "session.reap", r.Host, "向こうを確かめられない: "+m.Reason, audit.Error)
		return
	}
	s.mu.Lock()
	delete(s.reapAsked, r.ID)
	s.mu.Unlock()
	s.CloseApprovals(r.ID)
	s.audit(r.ID, "session.reap", r.Host, m.Reason, audit.OK)
}

// recordAsk は承認要求を DB に残す。**ここが実行面から DB を太らせる本線**
// なので、2つの上限を掛ける——同時に待てる数と、1分あたりの記録の数。
//
// 待ちを DB に置くのは、campd を入れ替えても「誰が何を訊かれていたか」が
// 消えないようにするため。
func (s *Supervisor) recordAsk(m Msg) {
	if open, err := openApprovals(s.db, m.Session); err != nil {
		s.audit(m.Session, "tool.ask", m.Text,
			"待っている承認を数えられない: "+err.Error(), audit.Error)
		return
	} else if len(open) >= maxOpenApprovals {
		if _, first := s.mayRecord(m.Session); first {
			s.audit(m.Session, "tool.ask", m.Text,
				fmt.Sprintf("待っている承認が %d 件を越えたので記録しない",
					maxOpenApprovals), audit.Denied)
		}
		return
	}
	if err := ask(s.db, m.Session, m.ReqID, m.Text, string(m.Frame), s.Now(), s.ParkAfter); err != nil {
		s.audit(m.Session, "tool.ask", m.Text, "承認の記録に失敗: "+err.Error(), audit.Error)
		return
	}
	s.auditFromAgent(m.Session, "tool.ask", m.Text, m.ReqID, audit.OK)
}

// check は「そのセッションを、その合鍵で触ってよいか」。
//
// **合鍵が防ぐのは取り違えだけ。** 同じユーザーで動く者はメモリから鍵を読めるので、
// なりすましは防げない。ここで止まるのは、鍵を持っていない別のセッションの
// 承認に答えることと、起こしてもいないセッションのフレームを流し込むこと。
func (s *Supervisor) check(m Msg) (*liveSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ls := s.live[m.Session]
	if ls == nil {
		return nil, false
	}
	if m.Token == "" || m.Token != ls.token {
		return nil, false
	}
	return ls, true
}

func writeMsg(c net.Conn, m Msg) {
	b, _ := json.Marshal(m)
	c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	c.Write(append(b, '\n'))
}

// peerUID は接続の向こう側を、名乗りではなくカーネルから取る。
func peerUID(c net.Conn) (uid uint32, pid int32, err error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, 0, fmt.Errorf("unix socket ではない")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, 0, err
	}
	if cerr != nil {
		return 0, 0, cerr
	}
	return cred.Uid, cred.Pid, nil
}

// dispatchForTest はテストから1通だけ流し込む。実行面を用意せずに済ませる。
func (s *Supervisor) dispatchForTest(m Msg) {
	c := &Control{s: s, allowUID: -1}
	c.dispatch(&agentConn{c: nil, who: "test"}, m)
}
