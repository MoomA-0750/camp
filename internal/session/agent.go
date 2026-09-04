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
	mu    sync.Mutex
	dead  bool
}

// NewAgent は実行面を作る。
func NewAgent(sock, claude string) *Agent {
	a := &Agent{Sock: sock, Claude: claude, Scope: true, kids: map[string]*child{}}
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
func (a *Agent) Dial(version string) error {
	c, err := net.Dial("unix", a.Sock)
	if err != nil {
		return fmt.Errorf("制御口へ繋げない（%s）: %w", a.Sock, err)
	}
	a.conn = c
	if err := a.send(Msg{T: MsgHello, Version: version}); err != nil {
		c.Close()
		return err
	}
	return nil
}

// Run は campd の指示を受け続ける。接続が切れるまで戻らない。
func (a *Agent) Run() error {
	defer a.stopAll("制御口が切れた")

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
	if _, err := resolveCwd(m.Cwd); err != nil {
		a.send(Msg{T: MsgFailed, Session: m.Session, Token: m.Token, Error: err.Error()})
		return
	}
	cmd := a.Command(m.Session, m.Cwd)
	cmd.Dir = m.Cwd
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
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var f map[string]any
		if err := json.Unmarshal(line, &f); err != nil {
			continue
		}
		m := Msg{T: MsgFrame, Session: k.id, Token: k.token, Kind: FrameKind(f)}
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
