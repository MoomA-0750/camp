package session

// 駆動器（Driver）。エージェント1種類ぶんの違いを、ここに書いた形に閉じ込める（D-031。
// 設計は dev/active/phase3.7-design.md）。実行面の本流・campd・画面は、この形しか知らない。
//
// **エージェントを足す・外すのは、drivers の行と、その駆動器のファイルだけ。**

import "encoding/json"

// 台帳・API に入る名前。
const (
	AgentClaude = "claude"
	AgentCodex  = "codex"
)

// 確認の度合い（セッションごとに本人が選ぶ。phase3.7-design.md）。**cli 以外は、渡したものだけを
// 起こしたときに1回照らす。** 名前はエージェントによらない Camp の語で、エージェントごとの渡し方は駆動器が持つ。
const (
	// PermCLI は既定。**何も渡さない**——本人の設定のまま、CLI と同じに動く（D-030）。
	PermCLI   = "cli"
	PermAsk   = "ask"   // 毎回訊く
	PermEdits = "edits" // 編集は訊かない
	PermAuto  = "auto"  // 自動で判断
	PermFull  = "full"  // 全部任せる
	// PermLegacy は Phase 3.6 の Camp 専用の置き場で起こした Codex の行（表示だけ。頼めない）。
	PermLegacy = "legacy"
)

// allPerms は頼める度合いの全部。
var allPerms = []string{PermCLI, PermAsk, PermEdits, PermAuto, PermFull}

// AgentInfo は駆動器の説明。実行面が hello で名乗り、campd と画面はこれだけを見て振る舞いを決める。
type AgentInfo struct {
	Name  string `json:"name"`  // 台帳・API の値
	Label string `json:"label"` // 画面の名前
	// Perms は直せる確認の度合い。**名乗っていない度合いを campd は頼まない。**
	Perms []string `json:"perms"`
	// Notes はそのエージェント固有の振る舞いの説明。画面にそのまま出す。
	Notes []string `json:"notes,omitempty"`
	// InterruptLeavesTools は、中断でターンが終わっても走っていた工具が残るか。残るなら、
	// Camp が時間切れで中断してそれで終わったとき、続けて止める。
	InterruptLeavesTools bool `json:"interrupt_leaves_tools,omitempty"`
	// Remote は向こうのホスト（ssh）でも起こせるか。
	Remote bool `json:"remote,omitempty"`
}

// Launch は起こすのに要る、実行面の設定のうちそのエージェントの分。
type Launch struct {
	Bin  string // 実体
	Home string // 置き場（照らす駆動器だけが使う）
}

// RemoteLaunch は向こうのホストで起こすのに要るもの。**向こうの sh にエージェントの名前を書かない**
// ために、駆動器が渡す（D-031。Fable の M42 レビュー）。
type RemoteLaunch struct {
	Name string // 向こうで探す実体の名前（台帳に場所が無ければ PATH と決まった場所で探す）
	// HomeEnv・HomeDefault は置き場の環境変数名と、無いときの $HOME からの相対パス。照らさないなら空。
	HomeEnv, HomeDefault string
}

// Driver はエージェント1種類ぶん。
type Driver interface {
	Info() AgentInfo
	// Launch は実行面の設定から、起こすのに要るものを取り出す。起こせない構成ならその理由。
	// **起こせると言ったものだけを hello で名乗る。**
	Launch(a *Agent) (Launch, error)
	// Argv は子の引数（実体の後ろ）。**このマシンでも向こうのホストでも同じものを使う**
	// （向こうは M42）。確認の度合いは、渡すものがある度合いだけ引数に足す（cli は何も足さない）。
	Argv(perm string) ([]string, error)
	// Open は子1本ぶんの会話を作る。
	Open(o OpenOpts) Conversation
	// Usage は残量の問い合わせ（get_usage・get_context_usage）の答えを共通の形に直す（campd 側）。
	Usage(usage, context json.RawMessage) UsageView
	// Summary は落とし先の1行を画面の一言に畳む（campd 側）。own は Camp 自身の問い合わせの
	// やりとり（会話ではないので画面が畳む）。
	Summary(kind string, frame json.RawMessage) (text string, own bool)
	// RemoteLaunch は向こうのホストで探す名前と置き場（Info の Remote が true のときだけ使う）。
	RemoteLaunch() RemoteLaunch
}

// drivers は起こせるエージェントの全部。**ここに無いものは通さない。**
var drivers = map[string]Driver{
	AgentClaude: claudeDriver{},
	AgentCodex:  codexDriver{},
}

func validAgent(a string) bool {
	_, ok := drivers[a]
	return ok
}

// agentOr は空を claude と読む。**古い実行面は agent を名乗らない**（Phase 3.6 より前）。
func agentOr(a string) string {
	if a == "" {
		return AgentClaude
	}
	return a
}

// permOr は空を cli と読む。**古い実行面は perm を名乗らない**（Phase 3.7 より前）。
// 名乗らない実行面へは cli 以外を頼まないので、読み違えは起きない（agentConn.info）。
func permOr(p string) string {
	if p == "" {
		return PermCLI
	}
	return p
}

// validPerm は頼める確認の度合いか（legacy は頼めない）。
func validPerm(p string) bool { return contains(allPerms, p) }

// agentsOf は実行面が起こせるエージェント（監査の文用）。
func agentsOf(a *agentConn) []string {
	if len(a.agents) == 0 {
		return []string{AgentClaude}
	}
	return a.agents
}
