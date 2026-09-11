package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Agent は実行面。**本人のユーザーで動き、`claude` を起こすことだけをする。**
//
// DB へ繋ぐ口を持たない。持たせないことが、この分割の全部。
// campd（camp ユーザー）はサンドボックスの都合で `claude` を起こせないので
// （dev/active/phase3-baseline.md 4節）、起こす役はこちらに居る。
type Agent struct {
	Sock   string // 制御口のパス
	Claude string // `claude` の実体
	Scope  bool   // systemd の transient scope で包むか

	// Command はテストで差し替える。既定は systemd-run で包んだ `claude`。
	Command func(id, cwd string) *exec.Cmd

	// LogDir はフレームの落とし先。**子の隣**（本人のユーザーの領域）。
	LogDir string
	// SSHConfig は読む場所。**読むだけ。書き戻す経路をこの型に持たせない。**
	SSHConfig string

	// リモート起動（agent_remote.go）。
	SSH       string // ssh の実体。テストで差し替える
	SSHKeygen string // 固定する鍵の指紋を読む
	// SSHFile は ssh に -F で渡す config。空なら ssh の既定に任せる
	// （既定のときに -F を付けると /etc/ssh/ssh_config が読まれなくなる）。
	SSHFile    string
	HeaderWait time.Duration // 向こうの sh が名乗るまで・Codex が話し始めるまで
	ReapWait   time.Duration // 向こうを見に行く ssh 1本
	ReapRetry  time.Duration // 見に行けなかったとき、もう一度行くまで

	// Codex（codex.go）。
	Codex       string // codex の実体。空なら Codex は起こさない
	CodexHome   string // Camp 専用の置き場（CODEX_HOME にする）
	CodexSource string // 本人の置き場（道具とログインを借りる。**書かない**）
	// CodexCommand はテストで差し替える。既定は systemd-run で包んだ `codex app-server`。
	CodexCommand func(id string) *exec.Cmd
	// CodexWithoutScope は scope 無しでも Codex を起こしてよいか。**テストの偽物のためだけ。**
	CodexWithoutScope bool

	// scope の後始末を確かめる口（scope.go）。テストで差し替える。nil なら systemd に訊く。
	ScopeProcs func(scope string) ([]int, error)
	ScopeStop  func(scope string) error

	mu   sync.Mutex
	kids map[string]*child
	// stopWanted は「まだ生まれていない子」への停止指示。
	//
	// **起動は非同期なので、止めろが先に着くことがある。** そのとき黙って
	// 捨てると、campd は stopping のまま、子は走り続ける（2026-09-04 の
	// outer gate で実測。止めたつもりで止まっていない、が一番悪い）。
	stopWanted map[string]string
	conn       net.Conn
	enc        sync.Mutex
}

type child struct {
	id    string
	token string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	scope string
	log   *Log
	// pending は子へ投げた制御要求の返事待ち。子が返す request_id で対応づける。
	pending map[string]chan []byte
	mu      sync.Mutex
	dead    bool
	// turn は「入力を渡してから result が返るまで」。
	// **campd の引き取り直しに要る**——idle と言ってしまうと、応答生成中の
	// 子に入力を重ねて送られる。
	turn bool
	// logBroken は落とし先へ書けなくなったか。**黙って続けない。**
	logBroken bool
	// remote は ssh の向こうの子。このマシンの子なら nil。
	remote *RemoteOwner
	// deliberate は Camp が止めに入ったか。**止めて ssh が 255 で終わったのを、
	// 接続が切れたと読まないため。**
	deliberate bool
	// codex は Codex の子の状態（codex.go）。Claude の子なら nil。
	codex *codexState
	// dropped は落とし先が溢れて捨てた数（最後に campd へ言った値）。
	dropped int64
}

// agent は子のエージェント。
func (k *child) agent() string {
	if k.codex != nil {
		return AgentCodex
	}
	return AgentClaude
}

// NewAgent は実行面を作る。Codex は Codex の実体を入れたときだけ起こせる。
func NewAgent(sock, claude string) *Agent {
	a := &Agent{Sock: sock, Claude: claude, Scope: true,
		LogDir: DefaultLogDir(), SSHConfig: DefaultSSHConfig(),
		SSH: "ssh", SSHKeygen: "ssh-keygen", SSHFile: os.Getenv("CAMP_SSH_CONFIG"),
		HeaderWait: headerWait, ReapWait: reapWait, ReapRetry: 3 * time.Second,
		CodexHome: DefaultCodexHome(), CodexSource: DefaultCodexSource(),
		kids: map[string]*child{}, stopWanted: map[string]string{}}
	a.Command = a.defaultCommand
	a.CodexCommand = a.defaultCodexCommand
	return a
}

// defaultCommand は `claude` を transient scope で包んで起こす。
//
// **孫まで数えて止めるため。** セッションはテスト・LSP・ssh を産む。
// プロセスグループでは、自分で切り離した孫を取り逃がす。cgroup なら取り逃がさない。
func (a *Agent) defaultCommand(id, cwd string) *exec.Cmd {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-prompt-tool", "stdio",
	}
	if !a.Scope {
		return exec.Command(a.Claude, args...)
	}
	full := append([]string{
		"--user", "--scope", "--quiet", "--collect",
		"--unit", scopeName(id),
		"--", a.Claude,
	}, args...)
	return exec.Command("systemd-run", full...)
}

func scopeName(id string) string { return "camp-session-" + id + ".scope" }

// debugFrames は流れているフレームの種類を stderr に出す。**中身は出さない。**
var debugFrames = os.Getenv("CAMP_AGENT_DEBUG") != ""

// Dial は制御口へ繋いで hello を送る。
//
// **いま抱えている子を名乗る。** campd を入れ替えたときに、走っている
// セッションを殺さずに引き取り直してもらうため。
func (a *Agent) Dial(version string) error {
	c, err := net.Dial("unix", a.Sock)
	if err != nil {
		return fmt.Errorf("制御口へ繋げない（%s）: %w", a.Sock, err)
	}
	a.conn = c
	// **起こせるエージェントも名乗る。** 名乗らないと、campd は Codex を頼んでよいか
	// 分からない（古い実行面は claude を起こしてしまう）。
	if err := a.send(Msg{T: MsgHello, Version: version, Held: a.held(),
		Agents: a.agents()}); err != nil {
		c.Close()
		return err
	}
	return nil
}

// held はいま抱えている子の一覧。
func (a *Agent) held() []Held {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Held, 0, len(a.kids))
	for id, k := range a.kids {
		k.mu.Lock()
		dead := k.dead
		k.mu.Unlock()
		if dead || k.cmd.Process == nil {
			continue
		}
		pid := k.cmd.Process.Pid
		st, _ := Starttime(pid)
		k.mu.Lock()
		state := StateIdle
		if k.turn {
			state = StateRunning
		}
		k.mu.Unlock()
		// 向こうの身元も名乗る。**started が campd に届く前に campd が落ちると、
		// 台帳は向こうの pid を知らないまま**になり、あとで始末できない（codex の指摘）。
		h := Held{ID: id, Token: k.token, PID: pid, Started: st, BootID: BootID(),
			Scope: k.scope, State: state, RemoteOwner: k.remote, Agent: k.agent()}
		if k.codex != nil {
			// **待っている承認も名乗る。** campd が居ない間に来たものは台帳に無い。
			h.Waiting = k.codex.waiting()
		}
		out = append(out, h)
	}
	return out
}

// Serve は繋ぎ直しながら動き続ける。**campd の入れ替えで子を殺さない。**
//
// ただし見張る者が居ないまま走らせ続けもしない。orphanGrace を過ぎたら止める
// ——誰も見ていないセッションが、承認を待ったまま延々と残るのが一番悪い。
func (a *Agent) Serve(version string) error {
	lost := time.Time{}
	for {
		if err := a.Dial(version); err != nil {
			if lost.IsZero() {
				lost = time.Now()
			}
			if n := len(a.held()); n > 0 && time.Since(lost) > orphanGrace {
				fmt.Fprintf(os.Stderr,
					"camp agent: campd が %v 戻らない。抱えている %d 本を止める\n",
					orphanGrace, n)
				a.stopAll("campd が戻らない")
			}
			time.Sleep(reconnectWait)
			continue
		}
		lost = time.Time{}
		fmt.Fprintf(os.Stderr, "camp agent: %s へ繋いだ（抱えている子 %d 本）\n",
			a.Sock, len(a.held()))
		if err := a.Run(); err != nil {
			return err // 断られた（uid 違い・2つ目）。繋ぎ直しても同じ
		}
		fmt.Fprintln(os.Stderr, "camp agent: 制御口が切れた。子は殺さずに繋ぎ直す")
		time.Sleep(reconnectWait)
	}
}

const (
	reconnectWait = 3 * time.Second
	// orphanGrace は campd が戻るのを待つ長さ。
	orphanGrace = 10 * time.Minute
)

// Run は campd の指示を受け続ける。接続が切れるまで戻らない。
// Run は campd の指示を受け続ける。接続が切れたら nil を返す。
//
// **切れても子は殺さない。** 殺すのは Serve が見切りをつけたときだけ
// （campd の入れ替えは数秒で終わるので、そこで殺すと毎回セッションが飛ぶ）。
func (a *Agent) Run() error {
	done := make(chan struct{})
	defer close(done)
	go a.heartbeat(done)

	sc := bufio.NewScanner(a.conn)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	for sc.Scan() {
		var m Msg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		switch m.T {
		case MsgWelcome:
			// 受理された。
		case MsgStart:
			switch agent := agentOr(m.Agent); {
			case agent == AgentCodex && m.Remote == nil:
				go a.startCodex(m)
			case agent != AgentClaude:
				// **知らないものを claude で代わりに起こさない。**
				a.send(Msg{T: MsgFailed, Session: m.Session, Token: m.Token,
					Error: fmt.Sprintf("この実行面は %s を %s で起こせない", agent,
						map[bool]string{true: "向こうのホスト", false: "このマシン"}[m.Remote != nil])})
			case m.Remote != nil:
				go a.startRemote(m)
			default:
				go a.start(m)
			}
		case MsgInput:
			a.toChild(m, userFrame(m.Text))
		case MsgApprove:
			a.toChild(m, approveFrame(m.ReqID, m.Behavior, m.Text))
		case MsgStop:
			go a.stop(m)
		case MsgReap:
			go a.reap(m)
		case MsgTail:
			go a.tail(m)
		case MsgSSHScan:
			go a.scanSSH(m)
		case MsgSSHResolve:
			go a.resolveSSH(m)
		case MsgControl:
			go a.control(m)
		case MsgError:
			fmt.Fprintln(os.Stderr, "campd:", m.Error)
			if strings.Contains(m.Error, "既に繋がっている") ||
				strings.Contains(m.Error, "uid") {
				return fmt.Errorf("campd に断られた: %s", m.Error)
			}
		}
	}
	return sc.Err()
}

func (a *Agent) heartbeat(done <-chan struct{}) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			a.send(Msg{T: MsgPing})
		}
	}
}

func (a *Agent) send(m Msg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	a.enc.Lock()
	defer a.enc.Unlock()
	_, err = a.conn.Write(append(b, '\n'))
	return err
}

// start は子を起こす。
func (a *Agent) start(m Msg) {
	// **campd 側でも照合しているが、ここでも見る。** 境界として数えるのは
	// campd 側だけ（同じユーザーで動く以上、ここの検査は迂回できる）。
	// それでも、campd の取り違えをそのまま実行しないだけの価値はある。
	real, err := resolveCwd(m.Cwd)
	if err != nil {
		a.send(Msg{T: MsgFailed, Session: m.Session, Token: m.Token, Error: err.Error()})
		return
	}
	if m.Root == "" || !under(real, m.Root) {
		a.send(Msg{T: MsgFailed, Session: m.Session, Token: m.Token,
			Error: fmt.Sprintf("許した場所（%s）の外を渡された: %s", m.Root, real)})
		return
	}
	cmd := a.Command(m.Session, real)
	cmd.Dir = real
	stdin, err := cmd.StdinPipe()
	if err != nil {
		a.send(Msg{T: MsgFailed, Session: m.Session, Token: m.Token, Error: err.Error()})
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		a.send(Msg{T: MsgFailed, Session: m.Session, Token: m.Token, Error: err.Error()})
		return
	}
	cmd.Stderr = os.Stderr
	// scope を使わない場合でも、せめて自分のプロセスグループから切る。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		a.send(Msg{T: MsgFailed, Session: m.Session, Token: m.Token, Error: err.Error()})
		return
	}
	pid := cmd.Process.Pid
	st, _ := Starttime(pid)

	// **照合したパスと、実際に降りた場所が同じか。**
	//
	// 照合してから exec するまでの間に、ディレクトリを rename や symlink で
	// 差し替えられると、同じ文字列が別の場所を指しうる（TOCTOU）。
	// 起こしたあとに /proc/<pid>/cwd を読めば、実際どこに居るかが分かる。
	// 違えば、その場で止める。
	if where, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err == nil {
		if !under(where, m.Root) {
			cmd.Process.Kill()
			a.send(Msg{T: MsgFailed, Session: m.Session, Token: m.Token,
				Error: fmt.Sprintf("起こした先が許した場所の外だった（%s）。止めた", where)})
			return
		}
	} else {
		// 読めなかったことを「合っていた」と読ませない。
		fmt.Fprintf(os.Stderr,
			"camp agent: %s の実際の cwd を確かめられない: %v\n", m.Session[:8], err)
	}

	k := &child{id: m.Session, token: m.Token, cmd: cmd, stdin: stdin,
		pending: map[string]chan []byte{}}
	if a.Scope {
		k.scope = scopeName(m.Session)
	}
	// **落とし先を先に開く。** 開けなければ drain は行き場を失い、
	// パイプが詰まって子が止まる。黙って進めない。
	if lg, err := OpenLog(a.LogDir, m.Session); err == nil {
		k.log = lg
	} else {
		// **campd にも言う。** stderr にしか出さないと、画面は
		// 「フレームは来ているのに中身が無い」を「中身が無かった」と読む。
		k.logBroken = true
		fmt.Fprintf(os.Stderr, "camp agent: 落とし先を開けない（%v）。フレームは残らない\n", err)
		a.send(Msg{T: MsgDropped, Session: m.Session, Token: m.Token, Dropped: -1,
			Error: "落とし先を開けない: " + err.Error()})
	}
	a.mu.Lock()
	a.kids[m.Session] = k
	wanted, wasAsked := a.stopWanted[m.Session]
	delete(a.stopWanted, m.Session)
	a.mu.Unlock()

	a.send(Msg{T: MsgStarted, Session: m.Session, Token: m.Token,
		PID: pid, Started: st, BootID: BootID(), Scope: k.scope})

	// **生まれる前に止めろと言われていたなら、生まれた直後に止める。**
	if wasAsked {
		fmt.Fprintf(os.Stderr, "camp agent: %s は生まれる前に止めろと言われていた\n",
			m.Session[:8])
		go a.stop(Msg{Session: m.Session, Token: m.Token, Mode: wanted})
	}

	// **stdout は常時読む。** 読まないと子が詰まる（M27 で永続化する）。
	go a.drain(k, stdout)

	err = cmd.Wait()
	code, reason := 0, "終わった"
	if err != nil {
		reason = err.Error()
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	k.mu.Lock()
	k.dead = true
	k.mu.Unlock()
	// **子が終わっても、scope に残りが居るかもしれない。** 数えて止め、止め切れなければそう言う。
	left := a.leftovers(k)
	if left != 0 {
		reason += leftoverNote(left)
	}
	if k.log != nil {
		k.log.Close()
	}
	a.mu.Lock()
	delete(a.kids, m.Session)
	a.mu.Unlock()
	a.send(Msg{T: MsgExited, Session: m.Session, Token: m.Token, Code: code, Reason: reason,
		Leftover: left})
}

// drain は子の stdout を読み続け、種類だけを campd へ渡す。
//
// **中身はまだ渡さない。** 境界を越える量を決めるのは M27 の仕事で、
// ここで無制限に流すと、あとで絞るのが難しくなる。
func (a *Agent) drain(k *child, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	a.drainScanner(k, sc)
}

// drainScanner は読みかけの scanner から続ける。リモートでは向こうの sh の
// 名乗り（1行目）を読んだあとで、同じ scanner をここへ渡す。
func (a *Agent) drainScanner(k *child, sc *bufio.Scanner) {
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var f map[string]any
		if err := json.Unmarshal(line, &f); err != nil {
			continue
		}
		kind := FrameKind(f)
		// 子からの制御応答は、待っている者へ回す。**画面へは流さない。**
		if kind == "control_response" {
			deliverControl(k, f)
		}
		a.record(k, kind, line)
		if kind == "result" {
			k.mu.Lock()
			k.turn = false
			k.mu.Unlock()
		}
		m := Msg{T: MsgFrame, Session: k.id, Token: k.token, Kind: kind,
			TurnEnd: kind == "result", Ask: kind == "control_request/can_use_tool"}
		if s, ok := f["session_id"].(string); ok {
			m.ClaudeID = s
		}
		if debugFrames {
			fmt.Fprintf(os.Stderr, "camp agent: %s %s\n", k.id[:8], m.Kind)
		}
		if m.Kind == "control_request/can_use_tool" {
			if id, ok := f["request_id"].(string); ok {
				m.ReqID = id
			}
			if req, ok := f["request"].(map[string]any); ok {
				if n, ok := req["tool_name"].(string); ok {
					m.Text = n
				}
				// **何を承認しようとしているかは、画面に出さないと答えられない。**
				// 承認要求だけは中身を渡す（他のフレームは種類だけ）。
				if b, err := json.Marshal(req); err == nil && len(b) <= maxApprovalDetail {
					m.Frame = b
				}
			}
		}
		a.send(m)
	}
}

// FrameKind は stream-json の1フレームを「種類」に畳む。
func FrameKind(f map[string]any) string {
	t, _ := f["type"].(string)
	switch t {
	case "control_request":
		if req, ok := f["request"].(map[string]any); ok {
			if s, ok := req["subtype"].(string); ok {
				return "control_request/" + s
			}
		}
		return "control_request/?"
	case "system":
		if s, ok := f["subtype"].(string); ok {
			return "system/" + s
		}
		return "system/?"
	case "":
		return "?"
	}
	return t
}

// record は1行を落とし先へ残す。**読み手より先に、必ず落とす。** ここが読み手待ちに
// なると子が詰まる。Claude と Codex で同じ。
func (a *Agent) record(k *child, kind string, line []byte) {
	if k.log == nil {
		return
	}
	if _, err := k.log.Append(kind, line); err != nil {
		// **一度でも書けなくなったら、そう言う。**
		// 黙って続けると、落とし先には穴があるのに gap も立たない。
		fmt.Fprintf(os.Stderr, "camp agent: 落とせない: %v\n", err)
		k.mu.Lock()
		first := !k.logBroken
		k.logBroken = true
		k.mu.Unlock()
		if first {
			a.send(Msg{T: MsgDropped, Session: k.id, Token: k.token,
				Dropped: -1, Error: "落とし先へ書けない: " + err.Error()})
		}
	}
	if _, _, d := k.log.Stats(); d > k.dropped {
		k.dropped = d
		a.send(Msg{T: MsgDropped, Session: k.id, Token: k.token, Dropped: d})
	}
}

func (a *Agent) toChild(m Msg, frame []byte) {
	a.mu.Lock()
	k := a.kids[m.Session]
	a.mu.Unlock()
	if k == nil || k.token != m.Token {
		return
	}
	if k.codex != nil {
		// Codex は JSON-RPC。Claude 用に組んだ frame は使わず、状態から組み直す。
		var err error
		switch m.T {
		case MsgInput:
			frame, err = k.codex.input(m.Text)
		case MsgApprove:
			frame, err = k.codex.approve(m.ReqID, m.Behavior)
		default:
			err = fmt.Errorf("Codex へ渡せない種類: %s", m.T)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "camp agent: %s: %v\n", k.id[:8], err)
			if m.T == MsgInput {
				// **渡せなかったターンを、終わったターンとして返す。** 黙ると campd は
				// running のまま入力を受けなくなる。
				a.send(Msg{T: MsgFrame, Session: k.id, Token: k.token, Kind: "camp/input_failed",
					TurnEnd: true, Error: "入力を渡せなかった: " + err.Error()})
			}
			if m.T == MsgApprove {
				// **届かなかった答えを、届いたことにしない。** campd の台帳は「本人が答えた」の
				// ままになるので、取り下げとして返す（Fable の実装後レビュー 2）。
				a.send(Msg{T: MsgFrame, Session: k.id, Token: k.token, Kind: "camp/withdrawn",
					ReqID: m.ReqID, Withdrawn: true, Error: "答えが子へ届かなかった: " + err.Error()})
			}
			return
		}
	}
	k.mu.Lock()
	dead := k.dead
	var err error
	if !dead {
		if m.T == MsgInput {
			k.turn = true // result が返るまでターン中
		}
		_, err = k.stdin.Write(append(frame, '\n'))
	}
	k.mu.Unlock()
	if dead || err == nil {
		// 死んでいれば exited が来て、待っていた承認はそこで閉じる。
		return
	}
	// **書けなかったことを、届いたことにしない**（codex の outer gate の指摘 2）。
	// campd の台帳は「本人が答えた」「入力を渡した」のまま残るので、そう返す。
	fmt.Fprintf(os.Stderr, "camp agent: %s へ書けない: %v\n", k.id[:8], err)
	switch m.T {
	case MsgApprove:
		a.send(Msg{T: MsgFrame, Session: k.id, Token: k.token, Kind: "camp/withdrawn",
			ReqID: m.ReqID, Withdrawn: true, Error: "答えを子へ書けなかった: " + err.Error()})
	case MsgInput:
		a.send(Msg{T: MsgFrame, Session: k.id, Token: k.token, Kind: "camp/input_failed",
			TurnEnd: true, Error: "入力を子へ書けなかった: " + err.Error()})
	}
}

// DenyReason は拒否の理由。**空にしない。**
func DenyReason(message string) string {
	if strings.TrimSpace(message) == "" {
		return "本人が拒否した"
	}
	return message
}

func userFrame(text string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": text},
	})
	return b
}

func approveFrame(reqID, behavior, message string) []byte {
	resp := map[string]any{"behavior": behavior}
	if behavior == "deny" {
		// **理由を空で送らない。**
		//
		// 空だと、CLI は中身の無い tool_result（is_error: true）を会話へ
		// 積む。API はそれを 400 で弾き、**その1件が履歴に残る以上、
		// 以後どの発言も通らなくなる**——セッションが死ぬ。
		// 2026-09-07 に本人が踏んだ:
		//   messages.15.content.0.tool_result: content cannot be empty
		//   if `is_error` is true
		resp["message"] = DenyReason(message)
	}
	b, _ := json.Marshal(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": reqID,
			"response":   resp,
		},
	})
	return b
}

// stop は止める。interrupt は制御フレーム、terminate は scope ごと。
func (a *Agent) stop(m Msg) {
	a.mu.Lock()
	k := a.kids[m.Session]
	if k == nil {
		// **まだ生まれていない。覚えておく。** 捨てると止まらないまま残る。
		a.stopWanted[m.Session] = m.Mode
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	if k.token != m.Token {
		return
	}
	if m.Mode == StopInterrupt && k.codex != nil {
		// Codex の中断はターン id が要る。まだ無ければ、来たところで投げる（codex.go）。
		// **走っていた工具は残る**（実測）。確実に止めるのは terminate。
		if b := k.codex.interrupt(); b != nil {
			if err := k.write(b); err != nil {
				fmt.Fprintf(os.Stderr, "camp agent: %s を中断できない: %v\n", k.id[:8], err)
			}
		}
		return
	}
	if m.Mode == StopInterrupt {
		b, _ := json.Marshal(map[string]any{
			"type":       "control_request",
			"request_id": "camp-stop-" + m.Session,
			"request":    map[string]any{"subtype": "interrupt"},
		})
		k.mu.Lock()
		if !k.dead {
			k.stdin.Write(append(b, '\n'))
		}
		k.mu.Unlock()
		return
	}
	a.killChild(k)
}

// kill は孫まで止める。
func (k *child) kill() {
	if k.scope != "" {
		// **scope を止めれば孫まで消える。** これが cgroup で包んだ理由。
		exec.Command("systemctl", "--user", "stop", k.scope).Run()
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.dead || k.cmd.Process == nil {
		return
	}
	// プロセスグループごと。scope が無い場合の受け皿。
	syscall.Kill(-k.cmd.Process.Pid, syscall.SIGTERM)
}

func (a *Agent) stopAll(why string) {
	a.mu.Lock()
	kids := make([]*child, 0, len(a.kids))
	for _, k := range a.kids {
		kids = append(kids, k)
	}
	a.mu.Unlock()
	for _, k := range kids {
		fmt.Fprintf(os.Stderr, "camp agent: %s を止める（%s）\n", k.id, why)
		a.killChild(k)
	}
}

// reap は前回の残り（孤児）を始末する。
//
// **campd に言われた pid をそのまま撃たない。** 起動時刻が一致しなければ、
// それは同じ番号を取った他人のプロセスで、撃てば無関係なものを殺す。
func (a *Agent) reap(m Msg) {
	if m.RemoteOwner != nil {
		a.reapOrphanRemote(m)
		return
	}
	o := Owner{PID: m.PID, Started: m.Started, BootID: m.BootID}
	alive, known := o.Alive()
	if !known {
		a.send(Msg{T: MsgReaped, Session: m.Session, Reason: "確かめられなかった"})
		return
	}
	// scope に触るのは、そのセッションの scope 名のときだけ（campd の台帳の値でも照らす）。
	scope := ""
	if m.Scope == scopeName(m.Session) {
		scope = m.Scope
	}
	if !alive {
		// **本体は居なくても、scope に残りが居るかもしれない**（Codex のコマンドは別の
		// セッションに居る）。数えて止め、止め切れなければそう言う。
		left, known := a.sweepScope(scope)
		if !known {
			left = -1
		}
		reason := "もう居なかった"
		if left != 0 {
			reason += leftoverNote(left)
		}
		a.send(Msg{T: MsgReaped, Session: m.Session, Reason: reason, Leftover: left})
		return
	}
	if scope != "" {
		exec.Command("systemctl", "--user", "stop", scope).Run()
	}
	if a2, _ := o.Alive(); a2 {
		syscall.Kill(-m.PID, syscall.SIGTERM)
		time.Sleep(2 * time.Second)
		if a3, _ := o.Alive(); a3 {
			syscall.Kill(-m.PID, syscall.SIGKILL)
		}
	}
	left, known := a.sweepScope(scope)
	if !known {
		left = -1
	}
	reason := "止めた"
	if left != 0 {
		reason += leftoverNote(left)
	}
	a.send(Msg{T: MsgReaped, Session: m.Session, Reason: reason, Leftover: left})
}

// tail は画面が要求した範囲だけを返す。**境界を越えるのはここだけ。**
func (a *Agent) tail(m Msg) {
	a.mu.Lock()
	k := a.kids[m.Session]
	a.mu.Unlock()
	out := Msg{T: MsgTailRes, Session: m.Session, ReqID: m.ReqID}
	var lg *Log
	if k != nil && k.token == m.Token {
		lg = k.log
	} else if logExists(a.LogDir, m.Session) {
		// 終わったセッションでも、落とし先は残っている。読むだけなら開き直す。
		//
		// **無ければ開かない。** OpenLog は空のファイルを作るので、
		// でたらめな id で呼ばれるたびにゴミが増えるし、返す答えが
		// 「まだ何も流れていない」になる——**「見ていないから0」を
		// 「無いから0」と読ませる形**。
		if l, err := OpenLog(a.LogDir, m.Session); err == nil {
			defer l.Close()
			lg = l
		}
	}
	if lg == nil {
		out.Error = "そのセッションの落とし先が無い（走っていないか、id が違う）"
		a.send(out)
		return
	}
	lines, gap, err := lg.Tail(m.Since, m.Limit)
	if err != nil {
		out.Error = err.Error()
		a.send(out)
		return
	}
	_, newest, dropped := lg.Stats()
	out.Lines, out.Gap, out.Seq, out.Dropped = lines, gap, newest, dropped
	a.send(out)
}

// scanSSH は `~/.ssh/config` を**読んで**返す。
//
// campd（camp ユーザー）は `~/.ssh` を開けない——開けるようにもしない。
// 鍵の置き場に読み取りを配れば、境界がそのぶん薄くなる。
func (a *Agent) scanSSH(m Msg) {
	out := Msg{T: MsgSSHRes, ReqID: m.ReqID}
	hosts, err := ReadSSHConfig(a.SSHConfig)
	if err != nil {
		out.Error = err.Error()
	} else {
		out.SSHHosts = hosts
	}
	a.send(out)
}

// deliverControl は子の制御応答を、待っている者へ渡す。
func deliverControl(k *child, f map[string]any) {
	resp, ok := f["response"].(map[string]any)
	if !ok {
		return
	}
	id, _ := resp["request_id"].(string)
	if id == "" {
		return
	}
	k.mu.Lock()
	ch := k.pending[id]
	delete(k.pending, id)
	k.mu.Unlock()
	if ch == nil {
		return
	}
	b, err := json.Marshal(resp["response"])
	if err != nil {
		b = []byte("null")
	}
	select {
	case ch <- b:
	default:
	}
}

// control は子へ制御フレームを1つ投げて、答えを返す。
//
// 残量（get_usage / get_context_usage）はこの経路でしか取れない。
// **モデル呼び出しは起きない**ので、押すたびにトークンを使うことはない。
func (a *Agent) control(m Msg) {
	out := Msg{T: MsgCtlRes, Session: m.Session, ReqID: m.ReqID}
	a.mu.Lock()
	k := a.kids[m.Session]
	a.mu.Unlock()
	if k == nil || k.token != m.Token {
		out.Error = "そのセッションは走っていない"
		a.send(out)
		return
	}
	if k.codex != nil {
		a.controlCodex(k, m)
		return
	}
	childReq := "camp-ctl-" + newID()
	ch := make(chan []byte, 1)
	k.mu.Lock()
	dead := k.dead
	if !dead {
		k.pending[childReq] = ch
	}
	k.mu.Unlock()
	if dead {
		out.Error = "子はもう居ない"
		a.send(out)
		return
	}
	defer func() {
		k.mu.Lock()
		delete(k.pending, childReq)
		k.mu.Unlock()
	}()

	b, _ := json.Marshal(map[string]any{
		"type": "control_request", "request_id": childReq,
		"request": map[string]any{"subtype": m.Kind},
	})
	k.mu.Lock()
	_, err := k.stdin.Write(append(b, '\n'))
	k.mu.Unlock()
	if err != nil {
		out.Error = err.Error()
		a.send(out)
		return
	}
	select {
	case payload := <-ch:
		out.Frame = payload
	case <-time.After(10 * time.Second):
		// **返ってこないことを「空」と読まない。**
		out.Error = "子が答えない"
	}
	a.send(out)
}
