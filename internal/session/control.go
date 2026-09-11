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
	c.s.agent = a
	c.s.mu.Unlock()

	c.s.audit("", "agent.connect", who, "実行面が繋がった（version="+hello.Version+"）", audit.OK)
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
		s.live[h.ID] = &liveSession{rec: r, token: h.Token, last: s.Now(),
			turn: turnStartFor(r.State, s.Now()), asked: map[string]bool{}}
		s.mu.Unlock()
		_ = setOwner(s.db, h.ID, o, scope)
		_ = setState(s.db, h.ID, r.State)
		// 見張りは戻った。「見張りが外れていた」の控えはもう理由にならない。
		_ = clearOrphanCause(s.db, h.ID)
		s.audit(h.ID, "session.readopt", strconv.Itoa(h.PID),
			"実行面がまだ抱えていたので引き取り直した（"+r.State+"）", audit.OK)
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
		a.send(Msg{T: MsgReap, Session: r.ID, PID: r.PID,
			Started: r.Started, BootID: r.BootID, Scope: r.Scope})
	}
}

func (c *Control) dispatch(a *agentConn, m Msg) {
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

		s.mu.Lock()
		ls.rec.State = StateIdle
		ls.rec.PID, ls.rec.Started, ls.rec.BootID, ls.rec.Scope = o.PID, o.Started, o.BootID, m.Scope
		ls.last = s.Now()
		s.mu.Unlock()

		_ = setOwner(s.db, m.Session, o, m.Scope)
		s.auditFromAgent(m.Session, "session.started", strconv.Itoa(m.PID),
			"scope="+m.Scope, audit.OK)

	case MsgFrame:
		ls, ok := s.check(m)
		if !ok {
			return
		}
		s.mu.Lock()
		ls.last = s.Now()
		idle := false
		switch m.Kind {
		case "result":
			// ターンが終わった。次の入力を受けられる。
			// **止めに入っているものは idle に戻さない**（孫まで止める途中）。
			if ls.rec.State == StateRunning {
				ls.rec.State = StateIdle
				idle = true
			}
			ls.turn = time.Time{}
			ls.interrupted = time.Time{}
		case "control_request/can_use_tool":
			if m.ReqID != "" {
				ls.asked[m.ReqID] = true
			}
		}
		s.mu.Unlock()

		if idle {
			_ = setState(s.db, m.Session, StateIdle)
		}
		if m.ClaudeID != "" {
			_ = setClaudeID(s.db, m.Session, m.ClaudeID)
		}
		if m.Kind == "control_request/can_use_tool" && m.ReqID != "" {
			s.recordAsk(m)
		}

	case MsgExited:
		if _, ok := s.check(m); !ok {
			return
		}
		s.mu.Lock()
		delete(s.live, m.Session)
		s.mu.Unlock()
		// 止めろと言ってあれば、控えてある理由（本人が止めた・放置で閉じた）が残る。
		// 何も言っていないのに終わったなら、子が自分で終わった。
		_ = finish(s.db, m.Session, m.Code, m.Reason, EndSelf, false)
		s.CloseApprovals(m.Session)
		out := audit.OK
		if m.Code != 0 {
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
		switch {
		case alive:
			s.audit(m.Session, "session.reap", strconv.Itoa(r.PID),
				"始末したと言われたが、まだ生きている", audit.Error)
		case !known:
			s.audit(m.Session, "session.reap", strconv.Itoa(r.PID),
				"始末したと言われたが、確かめられなかった", audit.Error)
		default:
			_ = finish(s.db, m.Session, m.Code, "孤児を始末した: "+m.Reason, EndReaped, true)
			s.CloseApprovals(m.Session)
			s.audit(m.Session, "session.reap", strconv.Itoa(r.PID), m.Reason, audit.OK)
		}

	case MsgTailRes, MsgSSHRes, MsgCtlRes:
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
	if err := ask(s.db, m.Session, m.ReqID, m.Text, string(m.Frame), s.Now()); err != nil {
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
