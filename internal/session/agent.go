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

	mu   sync.Mutex
	kids map[string]*child
	conn net.Conn
	enc  sync.Mutex
}

type child struct {
	id    string
	token string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	scope string
	log   *Log
	mu    sync.Mutex
	dead  bool
}

// NewAgent は実行面を作る。
func NewAgent(sock, claude string) *Agent {
	a := &Agent{Sock: sock, Claude: claude, Scope: true,
		LogDir: DefaultLogDir(), kids: map[string]*child{}}
	a.Command = a.defaultCommand
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
	if err := a.send(Msg{T: MsgHello, Version: version, Held: a.held()}); err != nil {
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
		out = append(out, Held{ID: id, Token: k.token, PID: pid,
			Started: st, BootID: BootID(), Scope: k.scope, State: StateIdle})
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
			go a.start(m)
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

	k := &child{id: m.Session, token: m.Token, cmd: cmd, stdin: stdin}
	if a.Scope {
		k.scope = scopeName(m.Session)
	}
	// **落とし先を先に開く。** 開けなければ drain は行き場を失い、
	// パイプが詰まって子が止まる。黙って進めない。
	if lg, err := OpenLog(a.LogDir, m.Session); err == nil {
		k.log = lg
	} else {
		fmt.Fprintf(os.Stderr, "camp agent: 落とし先を開けない（%v）。フレームは残らない\n", err)
	}
	a.mu.Lock()
	a.kids[m.Session] = k
	a.mu.Unlock()

	a.send(Msg{T: MsgStarted, Session: m.Session, Token: m.Token,
		PID: pid, Started: st, BootID: BootID(), Scope: k.scope})

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
	if k.log != nil {
		k.log.Close()
	}
	a.mu.Lock()
	delete(a.kids, m.Session)
	a.mu.Unlock()
	a.send(Msg{T: MsgExited, Session: m.Session, Token: m.Token, Code: code, Reason: reason})
}

// drain は子の stdout を読み続け、種類だけを campd へ渡す。
//
// **中身はまだ渡さない。** 境界を越える量を決めるのは M27 の仕事で、
// ここで無制限に流すと、あとで絞るのが難しくなる。
func (a *Agent) drain(k *child, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	var dropped int64
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
		// **読み手より先に、必ず落とす。** ここが読み手待ちになると子が詰まる。
		if k.log != nil {
			if _, err := k.log.Append(kind, line); err != nil {
				fmt.Fprintf(os.Stderr, "camp agent: 落とせない: %v\n", err)
			}
			if _, _, d := k.log.Stats(); d > dropped {
				dropped = d
				a.send(Msg{T: MsgDropped, Session: k.id, Token: k.token, Dropped: d})
			}
		}
		m := Msg{T: MsgFrame, Session: k.id, Token: k.token, Kind: kind}
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

func (a *Agent) toChild(m Msg, frame []byte) {
	a.mu.Lock()
	k := a.kids[m.Session]
	a.mu.Unlock()
	if k == nil || k.token != m.Token {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.dead {
		return
	}
	k.stdin.Write(append(frame, '\n'))
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
		resp["message"] = message
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
	a.mu.Unlock()
	if k == nil || k.token != m.Token {
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
	k.kill()
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
		k.kill()
	}
}

// reap は前回の残り（孤児）を始末する。
//
// **campd に言われた pid をそのまま撃たない。** 起動時刻が一致しなければ、
// それは同じ番号を取った他人のプロセスで、撃てば無関係なものを殺す。
func (a *Agent) reap(m Msg) {
	o := Owner{PID: m.PID, Started: m.Started, BootID: m.BootID}
	alive, known := o.Alive()
	if !known {
		a.send(Msg{T: MsgReaped, Session: m.Session, Reason: "確かめられなかった"})
		return
	}
	if !alive {
		a.send(Msg{T: MsgReaped, Session: m.Session, Reason: "もう居なかった"})
		return
	}
	if m.Scope != "" {
		exec.Command("systemctl", "--user", "stop", m.Scope).Run()
	}
	if a2, _ := o.Alive(); a2 {
		syscall.Kill(-m.PID, syscall.SIGTERM)
		time.Sleep(2 * time.Second)
		if a3, _ := o.Alive(); a3 {
			syscall.Kill(-m.PID, syscall.SIGKILL)
		}
	}
	a.send(Msg{T: MsgReaped, Session: m.Session, Reason: "止めた"})
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
	} else {
		// 終わったセッションでも、落とし先は残っている。読むだけなら開き直す。
		if l, err := OpenLog(a.LogDir, m.Session); err == nil {
			defer l.Close()
			lg = l
		}
	}
	if lg == nil {
		out.Error = "落とし先が無い"
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
