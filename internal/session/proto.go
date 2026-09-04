package session

import (
	"encoding/json"
	"time"
)

// 実行面と campd のあいだで流れるものの全部。1行1JSON。
//
// **union にせず1つの構造体にしてある。** 種類ごとに型を分けると、
// 「この種類ではこの欄を見ない」という約束が型の外に出てしまう。
// ここでは代わりに、受け取り側が種類ごとに必要な欄だけを検査する。
type Msg struct {
	T       string `json:"t"`
	Session string `json:"session,omitempty"`

	// Token はセッションごとの短命の合鍵。**campd が発行し、実行面が持ち回る。**
	//
	// これが防ぐのは**取り違え**だけ——別のセッションの承認に答えたり、
	// 起こしてもいないセッションのフレームを流し込んだりできなくする。
	// **偽造は防げない。** 実行面は人間と同じユーザーで動き、AIエージェントも
	// 同じユーザーで動くので、鍵はメモリから読める。そう書かない。
	Token string `json:"token,omitempty"`

	// start（campd → 実行面）
	Cwd  string   `json:"cwd,omitempty"`
	Argv []string `json:"argv,omitempty"`
	// Root は cwd を通した許可リストの行。**実行面がもう一度照合する。**
	// 境界として数えるのは campd 側の照合だけだが、campd の取り違えを
	// そのまま実行しないだけの価値はある。
	Root string `json:"root,omitempty"`

	// started（実行面 → campd）
	PID     int    `json:"pid,omitempty"`
	Started uint64 `json:"proc_started,omitempty"`
	BootID  string `json:"boot_id,omitempty"`
	Scope   string `json:"scope,omitempty"`

	// input / approve（campd → 実行面）
	Text     string `json:"text,omitempty"`
	ReqID    string `json:"request_id,omitempty"`
	Behavior string `json:"behavior,omitempty"`
	Mode     string `json:"mode,omitempty"`

	// frame（実行面 → campd）。M26 では種類だけ数える。中身の永続化は M27。
	Frame json.RawMessage `json:"frame,omitempty"`
	Kind  string          `json:"kind,omitempty"`
	// ClaudeID は子が system/init で名乗った session_id。**campd が採番した
	// Session とは別物。**これがあって初めて、起こしたものと、あとで
	// 取り込まれる会話記録が繋がる。
	ClaudeID string `json:"claude_id,omitempty"`

	// exited / failed / error
	Code   int    `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`

	// tail（campd → 実行面）と tail_result（実行面 → campd）
	Since   int64  `json:"since,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Lines   []Line `json:"lines,omitempty"`
	Gap     bool   `json:"gap,omitempty"`
	Seq     int64  `json:"seq,omitempty"`
	Dropped int64  `json:"dropped,omitempty"`

	// ssh_scan / ssh_result
	SSHHosts []SSHHost `json:"ssh_hosts,omitempty"`

	// hello / welcome
	Version string `json:"version,omitempty"`
	// Held は実行面がいま抱えている子。**campd を入れ替えても殺さないため。**
	Held []Held `json:"held,omitempty"`
}

// Held は実行面が抱えている子1つ。campd はこれを見て引き取り直す。
//
// **中身をそのまま信じない。** pid と起動時刻は campd が /proc で確かめる。
type Held struct {
	ID      string `json:"id"`
	Token   string `json:"token"`
	PID     int    `json:"pid"`
	Started uint64 `json:"proc_started"`
	BootID  string `json:"boot_id"`
	Scope   string `json:"scope"`
	State   string `json:"state"`
}

// 実行面 → campd
const (
	MsgHello   = "hello"   // 繋いだ。以後この接続が実行面
	MsgStarted = "started" // 子が起きた。pid と起動時刻を添える
	MsgFrame   = "frame"   // 子から来たフレーム
	MsgExited  = "exited"  // 子が終わった
	MsgFailed  = "failed"  // 起こせなかった
	MsgReaped  = "reaped"  // 孤児を始末した
	MsgPing    = "ping"    // 生きている。**止まった実行面に気づくため**
	MsgTailRes = "tail_result"
	MsgDropped = "dropped" // 溢れて捨てた。**黙って消さない**
	MsgSSHRes  = "ssh_result"
	MsgCtlRes  = "control_result"
)

// campd → 実行面
const (
	MsgWelcome = "welcome"
	MsgStart   = "start"
	MsgInput   = "input"
	MsgStop    = "stop"
	MsgApprove = "approve"
	MsgReap    = "reap"     // 孤児を始末しろ
	MsgTail    = "tail"     // 画面が要求した範囲だけ寄こせ
	MsgSSHScan = "ssh_scan" // ~/.ssh/config を**読んで**寄こせ
	MsgControl = "control"  // 子へ制御フレームを1つ投げて、答えを寄こせ
	MsgError   = "error"
)

// 状態機械。**この5つ以外の状態を作らない。**
const (
	StateStarting = "starting"
	StateIdle     = "idle"
	StateRunning  = "running"
	StateStopping = "stopping"
	StateExited   = "exited"

	// StateOrphaned は「子は生きているのに、見張っている者が居ない」。
	// campd を再起動したときにだけ現れる。exited とは違う——**まだ動いている**。
	StateOrphaned = "orphaned"
)

// 停止の仕方。docs/30-session-protocol.md の「正しい停止順序」に対応する。
const (
	StopInterrupt = "interrupt" // 制御フレームで止める。result が返る
	StopTerminate = "terminate" // scope ごと止める。孫まで消える
)

// 境界の外は信用しない側なので、上限を置く。
const (
	maxLine     = 1 << 20 // 1行の長さ
	maxArgv     = 64
	maxArgLen   = 4096
	maxText     = 256 * 1024 // ユーザーの入力1回
	maxSessions = 16         // 同時に見張るセッション

	// 4コア。実測で子1本 314MB・2プロセス（孫の出る仕事ではもっと増える）。
	// 上限は運用で下げられるが、既定は控えめにする。
	defaultMaxConcurrent = 4

	idleTimeout = 30 * time.Minute // 何も来なくなってから
	turnTimeout = 60 * time.Minute // 1ターンが終わらない
	startGrace  = 60 * time.Second // started が返ってこない
	stopGrace   = 2 * time.Minute  // 止めろと言ったのに止まらない
)

// validState は知らない状態を弾く。
func validState(s string) bool {
	switch s {
	case StateStarting, StateIdle, StateRunning, StateStopping, StateExited, StateOrphaned:
		return true
	}
	return false
}

// jsonDecode はテストからも使う薄い包み。
func jsonDecode(b []byte, v any) error { return json.Unmarshal(b, v) }
