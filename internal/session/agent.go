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

	// Command は子を起こすコマンドを作る。argv は実体と駆動器の引数。既定は systemd-run の
	// scope で包む。テストで差し替える。
	Command func(agent, id string, argv []string) *exec.Cmd

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
	Codex string // codex の実体。空なら Codex は起こさない
	// CodexHome は本人の Codex の置き場（CLI と同じ。$CODEX_HOME か ~/.codex）。
	// Codex がここで起きたかを話し始める前に照らす。**Camp は書き換えない。**
	CodexHome string
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
	// conv は子との会話（conversation.go）。**子とはこれを通してしか話さない。**
	conv Conversation
	// name はエージェント。空は claude。
	name string
	// dropped は落とし先が溢れて捨てた数（最後に campd へ言った値）。
	dropped int64
}

// agent は子のエージェント。
func (k *child) agent() string { return agentOr(k.name) }

// NewAgent は実行面を作る。Codex は Codex の実体を入れたときだけ起こせる。
func NewAgent(sock, claude string) *Agent {
	a := &Agent{Sock: sock, Claude: claude, Scope: true,
		LogDir: DefaultLogDir(), SSHConfig: DefaultSSHConfig(),
		SSH: "ssh", SSHKeygen: "ssh-keygen", SSHFile: os.Getenv("CAMP_SSH_CONFIG"),
		HeaderWait: headerWait, ReapWait: reapWait, ReapRetry: 3 * time.Second,
		CodexHome: DefaultCodexHome(),
		kids:      map[string]*child{}, stopWanted: map[string]string{}}
	a.Command = a.defaultCommand
	return a
}

// defaultCommand は子を transient scope で包んで起こす。**環境は実行面のまま**（CLI と同じ設定・
// 同じ置き場で動かす。D-030）。
//
// **孫まで数えて止めるため。** セッションはテスト・LSP・ssh を産む。
// プロセスグループでは、自分で切り離した孫を取り逃がす。cgroup なら取り逃がさない
// （Codex の子は別のプロセスグループ・別のセッションに居る。実測）。
func (a *Agent) defaultCommand(_, id string, argv []string) *exec.Cmd {
	if !a.Scope {
		return exec.Command(argv[0], argv[1:]...)
	}
	full := append([]string{
		"--user", "--scope", "--quiet", "--collect",
		"--unit", scopeName(id),
		"--",
	}, argv...)
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
		Agents: a.agents(), Drivers: a.driverInfos()}); err != nil {
		c.Close()
		return err
	}
	return nil
}

// driverInfos は起こせるエージェントの説明（hello で名乗る）。
func (a *Agent) driverInfos() []AgentInfo {
	names := a.agents()
	out := make([]AgentInfo, 0, len(names))
	for _, n := range names {
		out = append(out, drivers[n].Info())
	}
	return out
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
		if k.conv != nil {
			// **待っている承認も名乗る。** campd が居ない間に来たものは台帳に無い。
			h.Waiting = k.conv.Waiting()
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
			agent, perm := agentOr(m.Agent), permOr(m.Perm)
			if d, ok := drivers[agent]; ok && !contains(d.Info().Perms, perm) {
				// **頼まれた度合いで起こせないなら、別の度合いで代わりに起こさない。**
				a.send(Msg{T: MsgFailed, Session: m.Session, Token: m.Token,
					Error: fmt.Sprintf("この実行面は %s を確認の度合い %s で起こせない", agent, perm)})
				continue
			}
			switch {
			case !validAgent(agent) || (m.Remote != nil && !drivers[agent].Info().Remote):
				// **知らないものを claude で代わりに起こさない。** 向こうのホストで起こせるかは駆動器の説明で。
				a.send(Msg{T: MsgFailed, Session: m.Session, Token: m.Token,
					Error: fmt.Sprintf("この実行面は %s を %s で起こせない", agent,
						map[bool]string{true: "向こうのホスト", false: "このマシン"}[m.Remote != nil])})
			case m.Remote != nil:
				go a.startRemote(m)
			default:
				go a.start(m)
			}
		case MsgInput, MsgApprove:
			a.toChild(m)
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

// toChild は入力・承認の答えを子へ渡す。**子へ書く1行は会話が組む**（campd から来た文字列を
// そのまま埋めない）。
func (a *Agent) toChild(m Msg) {
	a.mu.Lock()
	k := a.kids[m.Session]
	a.mu.Unlock()
	if k == nil || k.token != m.Token {
		return
	}
	var frame []byte
	var err error
	switch m.T {
	case MsgInput:
		frame, err = k.conv.Input(m.Text)
	case MsgApprove:
		frame, err = k.conv.Answer(m.ReqID, m.Behavior, m.Text)
	default:
		err = fmt.Errorf("子へ渡せない種類: %s", m.T)
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
	k.mu.Lock()
	dead := k.dead
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
	if m.Mode == StopInterrupt {
		// 今は投げられない中断（Codex はターン id が要る）は、来たところで会話が投げる。
		// **走っていた工具が残るエージェントがある**（Codex。実測）。確実に止めるのは terminate。
		if b := k.conv.Interrupt(); b != nil {
			if err := k.write(b); err != nil {
				fmt.Fprintf(os.Stderr, "camp agent: %s を中断できない: %v\n", k.id[:8], err)
			}
		}
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

// control は子へ問い合わせを1つ投げて、答えを返す（残量。会話の Query）。
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
	a.controlConv(k, m)
}
