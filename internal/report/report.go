// Package report は、**境界の外から監査ログへ追記するためだけ**の口。
//
// Phase 3 で Camp はセッションを起こす。そのセッションは人間と同じOSユーザーで
// 動き、環境を自由に横断する（それが狙いなので縛らない）。一方 campd と
// camp.sqlite は専用ユーザーのもので、そちらからは触れない。
//
// **監査される側が監査ログを書き換えられる場所にいてはいけない。**
// だからこの口が受け付けるのは追記だけにする。読み出しも、更新も、削除も、
// そういう命令が無い。返すのは採番された id だけ。
//
// 名乗りは信じない。actor は接続の資格情報（SO_PEERCRED）から campd が決める。
package report

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
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
	"github.com/MoomA-0750/camp/internal/limits"
	"github.com/MoomA-0750/camp/internal/store"
)

// Event は外から送れるものの全部。**これ以上は送れない。**
type Event struct {
	// Kind が "limits" のときだけ Payload を見る（statusLine の JSON を
	// そのまま渡す）。残量はプロンプトの描画時にしか手に入らず、それを
	// 観測できるのは人間のユーザー側だけなので、境界を越える必要がある。
	// **越えられるのはこの1種類だけ。**
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`

	Action  string `json:"action"`
	Target  string `json:"target"`
	Session string `json:"session"`
	// Detail は自由文。列名は detail_json だが、既存の書き手（retain.apply など）も
	// 素のテキストを入れているので合わせる。
	Detail  string `json:"detail"`
	Outcome string `json:"outcome"`
}

// Reply は返すものの全部。**中身は読ませない。**
type Reply struct {
	OK    bool   `json:"ok"`
	ID    int64  `json:"id,omitempty"`
	Error string `json:"error,omitempty"`
}

// Listener は追記専用の口。
type Listener struct {
	db   *store.DB
	ln   net.Listener
	path string

	sem chan struct{} // 同時接続の枠

	mu       sync.Mutex
	closed   bool
	window   time.Time // 全体の流量を測る窓
	inWindow int
}

// Listen は socket を開く。
//
// group が空でなければ、socket と親ディレクトリの**グループをそこへ移す**。
// campd は専用ユーザーで動き、報告する側は人間のユーザーなので、
// この2つを繋ぐのは共有グループしかない。mode は socket 0660 / ディレクトリ 0750。
// **その他のユーザーには開けない。**
func Listen(db *store.DB, path, group string) (*Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	// Linux の sun_path は 108 バイト。越えると bind が
	// 「invalid argument」としか言わないので、ここで名指しする。
	if len(path) >= 108 {
		return nil, fmt.Errorf(
			"報告口のパスが長すぎる（%d バイト、上限 107）: %s", len(path), path)
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		os.Remove(path) // 前回の残骸があると bind できない
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
		if err := regroup(dir, path, group); err != nil {
			ln.Close()
			os.Remove(path)
			return nil, err
		}
	}
	return &Listener{
		db: db, ln: ln, path: path,
		sem: make(chan struct{}, maxConns),
	}, nil
}

// allow は全体の流量に空きがあるかを見る。**接続をまたいで数える。**
func (l *Listener) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if time.Since(l.window) >= time.Second {
		l.window, l.inWindow = time.Now(), 0
	}
	if l.inWindow >= maxTotalPerSec {
		return false
	}
	l.inWindow++
	return true
}

// regroup は socket と親ディレクトリを共有グループのものにする。
func regroup(dir, path, group string) error {
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
	if err := os.Chmod(dir, 0o750); err != nil {
		return err
	}
	return nil
}

// Serve は受け付け続ける。Close されるまで戻らない。
func (l *Listener) Serve() error {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			l.mu.Lock()
			closed := l.closed
			l.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		// 枠が空いていなければ、その場で断って閉じる。**受けたまま溜め込まない。**
		select {
		case l.sem <- struct{}{}:
			go func() {
				defer func() { <-l.sem }()
				l.handle(c)
			}()
		default:
			writeReply(c, Reply{Error: "接続が多すぎる"})
			c.Close()
		}
	}
}

// Close は口を閉じる。
func (l *Listener) Close() error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	err := l.ln.Close()
	os.Remove(l.path)
	return err
}

// Addr は開いている socket のパス。
func (l *Listener) Addr() string { return l.path }

func (l *Listener) handle(c net.Conn) {
	defer c.Close()

	who, err := peerName(c)
	if err != nil {
		writeReply(c, Reply{Error: "呼び出し元を確かめられない"})
		return
	}

	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var n int
	window := time.Now()
	for {
		// **黙って握ったままの接続を許さない。**
		// 何も送らずに繋ぎっぱなしにするだけで枠を占有できてしまう。
		c.SetReadDeadline(time.Now().Add(maxIdle))
		if !sc.Scan() {
			return
		}
		if time.Since(window) >= time.Second {
			window, n = time.Now(), 0
		}
		n++
		if n > maxPerSec {
			writeReply(c, Reply{Error: "速すぎる。1秒あたりの上限を越えた"})
			return
		}
		if !l.allow() {
			writeReply(c, Reply{Error: "全体の流量が上限に達している"})
			continue
		}
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			writeReply(c, Reply{Error: "読めない行"})
			continue
		}
		id, err := l.append(who, e)
		if err != nil {
			writeReply(c, Reply{Error: err.Error()})
			continue
		}
		writeReply(c, Reply{OK: true, ID: id})
	}
}

// 追記しか受けないとはいえ、**無制限に受けてよいわけではない。**
// 境界の外は信用しない側なので、行を膨らませて監査ログを埋めることもできる。
// 長さと速さに上限を置く。
const (
	maxField  = 4096      // action / target / session の1つあたり
	maxDetail = 64 * 1024 // detail_json
	maxPerSec = 100       // 1接続あたりの追記
	maxConns  = 64        // 同時接続
	maxIdle   = 60 * time.Second

	// **全体の上限。接続ごとの上限だけでは意味が無い。**
	// 2026-09-04 の outer gate で実測: 8接続から 160 件/秒、1時間で約
	// 576,000 行。監査ログは保持ポリシーの対象外なので溜まり続け、
	// 本物の記録が埋まる。境界の外は信用しない側なので、全体で絞る。
	maxTotalPerSec = 20
)

func (l *Listener) append(who string, e Event) (int64, error) {
	if e.Kind == "limits" {
		return l.recordLimits(e)
	}
	if e.Action == "" {
		return 0, fmt.Errorf("action が要る")
	}
	for name, v := range map[string]string{
		"action": e.Action, "target": e.Target, "session": e.Session,
	} {
		if len(v) > maxField {
			return 0, fmt.Errorf("%s が長すぎる（%d バイト、上限 %d）", name, len(v), maxField)
		}
	}
	if len(e.Detail) > maxDetail {
		return 0, fmt.Errorf("detail が長すぎる（%d バイト、上限 %d）", len(e.Detail), maxDetail)
	}
	switch e.Outcome {
	case "", audit.OK, audit.Denied, audit.Error, audit.Timeout:
	default:
		return 0, fmt.Errorf("知らない outcome: %s", e.Outcome)
	}
	outcome := e.Outcome
	if outcome == "" {
		outcome = audit.OK
	}
	return audit.Append(l.db, audit.Entry{
		Actor:     who, // **名乗りではなく、カーネルが答えた身元**
		Action:    e.Action,
		Target:    e.Target,
		SessionID: e.Session,
		Detail:    e.Detail,
		Outcome:   outcome,
	})
}

// recordLimits は statusLine の観測を取り込む。**書けるのは残量だけ。**
// 返すのは記録できた窓の数で、DBの中身は返さない。
func (l *Listener) recordLimits(e Event) (int64, error) {
	if len(e.Payload) == 0 {
		return 0, fmt.Errorf("payload が要る")
	}
	if len(e.Payload) > maxDetail {
		return 0, fmt.Errorf("payload が長すぎる（%d バイト、上限 %d）", len(e.Payload), maxDetail)
	}
	got, err := limits.Record(l.db, bytes.NewReader(e.Payload),
		limits.AgentClaudeCode, limits.SourceStatusLine)
	if err != nil {
		if errors.Is(err, limits.ErrNoWindows) {
			return 0, nil // 窓が出ていないだけ。異常ではない
		}
		return 0, err
	}
	return int64(len(got)), nil
}

func writeReply(c net.Conn, r Reply) {
	b, _ := json.Marshal(r)
	c.Write(append(b, '\n'))
}

// peerName は接続の向こう側の身元を、送られてきた名前ではなくカーネルから取る。
func peerName(c net.Conn) (string, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return "", fmt.Errorf("unix socket ではない")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return "", err
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return "", err
	}
	if cerr != nil {
		return "", cerr
	}
	name := itoa(cred.Uid)
	if u := lookupUID(cred.Uid); u != "" {
		name = u
	}
	return fmt.Sprintf("uid:%s pid:%d", name, cred.Pid), nil
}
