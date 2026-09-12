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

	// rec_list / rec_read（campd → 実行面）。接続先と固定は Remote に載せる（start と同じ）。
	//
	// RecHomeEnv・RecHomeDefault・RecSub は**置き場の決め方**（駆動器の RemoteLaunch と、
	// 取り込み器の置き場）。パスそのものを運ばないのは、自由なパスを打ち込む口を作らないため
	// （本人の決定 2026-09-12）。向こうの sh がこの規則で解決し、実パスを名乗る。
	RecHomeEnv     string `json:"rec_home_env,omitempty"`
	RecHomeDefault string `json:"rec_home_default,omitempty"`
	RecSub         string `json:"rec_sub,omitempty"`
	// RecAt は「このパスのこの位置の窓も欲しい」。一覧と1回で取るため。
	RecAt map[string]int64 `json:"rec_at,omitempty"`
	// RecWin は窓の大きさ。**向こうで計算させない**——運んだ中身を手元で同じ関数に
	// かけるので、ハッシュの取り方がずれようがない（向こうに sha256sum があるとも限らない）。
	RecWin int64 `json:"rec_win,omitempty"`
	// RecWant は取り寄せる範囲の束。RecCap は1回の返事の合計の蓋。
	RecWant []RecRange `json:"rec_want,omitempty"`
	RecCap  int64      `json:"rec_cap,omitempty"`

	// rec_list_result / rec_read_result（実行面 → campd）
	//
	// RecRoot は**向こうが解決して名乗った置き場の実パス**。
	RecRoot   string            `json:"rec_root,omitempty"`
	RecFiles  []RecFile         `json:"rec_files,omitempty"`
	RecWindow map[string][]byte `json:"rec_window,omitempty"` // 位置の直前 RecWin バイトの中身
	RecData   map[string][]byte `json:"rec_data,omitempty"`   // 頼んだ範囲の中身
	// RecMore は「蓋で切った。続きがある」。
	RecMore bool `json:"rec_more,omitempty"`

	// start（campd → 実行面）: どこへ繋ぐか。nil ならこのマシン。
	Remote *RemoteSpec `json:"remote,omitempty"`
	// started / reap: 向こうで起きた子の身元。**campd は確かめられない。**
	RemoteOwner *RemoteOwner `json:"remote_owner,omitempty"`
	// exited / reaped: 向こうを見に行った結果（RemoteGone など）。
	RemoteEnd string `json:"remote_end,omitempty"`
	// exited: SSH の接続が切れて終わったか。
	ConnLost bool `json:"conn_lost,omitempty"`
	// exited: 子が終わったあと scope に残り、止められなかったものの数。-1 は確かめられなかった。
	// **止め切れていないものを「終わった」と書かないため**（scope.go）。
	Leftover int `json:"leftover,omitempty"`

	// ssh_resolve / ssh_resolved: `ssh -G` で alias がいまどこを指すか
	Alias    string    `json:"alias,omitempty"`
	Resolved *Resolved `json:"resolved,omitempty"`

	// hello / welcome
	Version string `json:"version,omitempty"`
	// Build は実行面の**実行ファイルの指紋**（build.go）。campd は自分のものと照らし、
	// 違えば画面に出す。**止めはしない**——古い実行面でも動くことは動く。
	// 版の文字列（Version）では足りない: campd と実行面は同じバイナリで、どちらも同じ
	// 文字列を名乗るため（2026-09-12、実行面だけ古いまま動いていたのに気づけなかった）。
	Build string `json:"build,omitempty"`
	// Held は実行面がいま抱えている子。**campd を入れ替えても殺さないため。**
	Held []Held `json:"held,omitempty"`

	// Agent は起こすエージェント（start）・起こしたエージェント（started）。
	// **空は claude と読む**——Phase 3.6 より前の実行面は名乗らない。
	Agent string `json:"agent,omitempty"`
	// Agents は実行面が起こせるエージェント（hello）。空なら claude だけ（古い実行面）。
	Agents []string `json:"agents,omitempty"`
	// Drivers はその駆動器の説明（hello）。**Agents とは別の欄にしてある**——古い campd・古い
	// 実行面と hello が読めなくならないように（Fable の設計レビュー）。名乗らない実行面の分は
	// campd が手元の駆動器から補い、確認の度合いは cli だけとみなす。
	Drivers []AgentInfo `json:"drivers,omitempty"`
	// Perm は確認の度合い。start で頼み、started で実行面が「こう起こした」と名乗る。
	// **空は cli と読む**（Phase 3.7 より前の実行面は名乗らない）。
	Perm string `json:"perm,omitempty"`
	// Resume は続きから起こすときの、エージェント自身のセッション id（start。M48、2026-09-13）。
	// 空なら新しく起こす。**古い実行面はこの欄を読まない**ので、campd は名乗らない実行面へ
	// 再開を頼まない（読まれずに落ちると、続きのつもりで新しい会話が始まってしまう）。
	Resume string `json:"resume,omitempty"`

	// frame（実行面 → campd）: 駆動器が畳んだ意味。**種類の文字列を campd が読み分けない**
	// （Claude の result と Codex の turn/completed を、どちらも TurnEnd で伝える）。
	TurnEnd bool `json:"turn_end,omitempty"`
	Ask     bool `json:"ask,omitempty"` // 承認の要求。ReqID・Text（工具）・Frame（中身）を添える
	// Interrupted は中断でターンが終わった（Codex）。**走っていた工具は残りうる。**
	Interrupted bool `json:"interrupted,omitempty"`
	// Note は止めずに記録するだけの一言（frame）。設定が途中で変わった等。**監査にはエラーでなく
	// 記録として残す**（2026-09-12、本人: 途中の変化は止めずに見せる）。
	Note string `json:"note,omitempty"`
	// Withdrawn は ReqID の承認が、答えを待つものではなくなった（Codex）。実行面が断った
	// （訊いたあとで差分が変わった）・Codex 側で片付いた・答えが子へ届かなかった。
	// **campd の台帳を「待っている」のまま残さない**（Fable の実装後レビュー 2）。
	Withdrawn bool `json:"withdrawn,omitempty"`
	// Halt は、頼んだ確認の度合いで起きていなかった（frame。理由は Error）。**campd は孫まで止めて
	// 「起こせなかった」と書く**（起こしたときに1回だけ照らす。途中の変化は Note で記録するだけ）。
	Halt bool `json:"halt,omitempty"`
}

// HeldAsk は実行面が抱えている、まだ答えていない承認1つ。
//
// 引き取り直しで名乗る。**campd が入れ替わっている間に来た承認が台帳から消えないように**
// （Fable の設計レビュー 5）。
type HeldAsk struct {
	ReqID  string          `json:"request_id"`
	Tool   string          `json:"tool"`
	Detail json.RawMessage `json:"detail,omitempty"`
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
	// RemoteOwner は向こうの子（リモートのとき）。台帳と照らしてから採る。
	RemoteOwner *RemoteOwner `json:"remote_owner,omitempty"`
	// Agent はその子のエージェント。空は claude（古い実行面）。**台帳と違えば採らない。**
	Agent string `json:"agent,omitempty"`
	// Waiting はその子がまだ答えを待っている承認。campd が居ない間に来たものも名乗る。
	Waiting []HeldAsk `json:"waiting,omitempty"`
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

	MsgSSHResolved = "ssh_resolved"

	// 向こうのホストの記録（M47）。**読むだけ。向こうへは書かない。**
	MsgRecListRes = "rec_list_result"
	MsgRecReadRes = "rec_read_result"
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

	// MsgSSHResolve は `ssh -G <alias>` の結果を寄こせ。**繋がない。**
	// 許すときに行き先を固定するのに使う。
	MsgSSHResolve = "ssh_resolve"

	// MsgRecList は向こうのホストの記録の一覧を寄こせ。**繋ぐ。**
	// 置き場は向こうが $HOME と環境変数から解決して名乗る（campd は知らない）。
	// 前回の位置を添えると、同じ実体かを見るための窓も1回で返る。
	MsgRecList = "rec_list"
	// MsgRecRead は記録の中身を範囲で寄こせ。**まとめて頼む**——1本ずつ繋ぎ直すと、
	// 変わっていない記録40本でも40回 ssh することになる。
	MsgRecRead = "rec_read"
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

	// **DBを太らせる経路にだけ上限を置く。**
	//
	// 2026-09-04 の outer gate で実測: 上限が無いと 5,756 フレーム/秒で
	// 承認要求を流し込め、3.5秒で approvals 2万行・audit 2万行・DB 13MB。
	// **監査ログは保持ポリシーの対象外**なので、そのまま溜まり続ける。
	// M25.5 の outer gate で報告口に対して見つけたのと同じ形が、
	// 新しい口でそのまま再発していた。
	//
	// **ただし、フレームそのものは絞らない。** 最初そうしたら、よく喋る子の
	// セッションで制御口が切れた（実測 18,000 フレーム/秒。工具の出力を
	// そのまま流すセッションはこれくらい出る）。**子が饒舌だからという
	// 理由で見張りを切るのは、防いでいるものより悪い。** フレームの量は
	// 実行面側の落とし先が上限で押さえている。
	//
	// 絞るのは「DBに行が増える経路」だけ。
	maxAuditPerMin = 120 // 1セッションあたり、実行面が起こす記録
	// 1セッションが同時に待てる承認の数。ふつうは1ターンに数件。
	maxOpenApprovals = 64

	// 4コア。実測で子1本 314MB・2プロセス（孫の出る仕事ではもっと増える）。
	// 上限は運用で下げられるが、既定は控えめにする。
	defaultMaxConcurrent = 4

	// **放置とターンの長さで Camp から止めない**（D-030、本人の決定 2026-09-12）。
	// CLI のセッションは開けっぱなしにでき、auto mode の1ターンは1時間を超える。
	// 0 は「見ない」。運用で入れたければ Supervisor.IdleAfter・TurnAfter に長さを入れる。
	idleTimeout = 0                // 何も来なくなってから（0 = 閉じない）
	turnTimeout = 0                // 1ターンが終わらない（0 = 止めない）
	startGrace  = 60 * time.Second // started が返ってこない
	stopGrace   = 2 * time.Minute  // 止めろと言ったのに止まらない

	// 向こうの sh が名乗るまで待つ長さ。ssh の接続（ConnectTimeout 20秒）と
	// ログインを含む。startGrace より短くしておく。
	headerWait = 45 * time.Second
	// 向こうを見に行く ssh 1本の長さ。
	reapWait = 30 * time.Second
	// 向こうを確かめられなかった孤児を、もう一度見に行くまでの間。
	remoteReapEvery = 5 * time.Minute
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
