package ingest

// 取り込み器（Collector）——エージェントごとの「記録の読み方」。
//
// **本体は共通のまま。** 差分読み（prior・rotated・resumeSHA）・DB への書き込み
// （projects/sessions/source_files/runs/messages）・派生（usage・blocks・files・cost-state）・
// 伏字化・件数の更新は、どのエージェントでも同じ手順で走る。エージェントごとに違うのは
// 「どのファイルを見るか」「ファイル名から誰の記録と読むか」「1行をどう解釈するか」
// 「要約に何を積むか」「役割と主キーをどう決めるか」だけなので、そこをここに閉じる（D-031）。
//
// **`Line` は共通の中間形として残す。** 派生の処理（newUsageRow・blocks・files・cost-state）が
// すべて `*Line` を見ているので、Codex の行も `Line` へ翻訳して渡す。そうすれば派生に手を
// 入れずに済む（M44 は振る舞いを変えない作り替え）。
//
// 駆動器（internal/session の Driver）と同じ考え方。追加は「1つ足してレジストリに載せる」だけ。
type Collector interface {
	// Name は取り込み器の名前。**台帳の `sessions.agent` に書く値でもある**
	// （語彙は `claude` / `codex`。internal/session・usage_windows と揃える。本人の決定 2026-09-12）。
	Name() string

	// DefaultRoot は記録の置き場（本人の設定のまま。CLI と同じ場所）。
	DefaultRoot(home string) string

	// RecordSub は**エージェントの置き場から見た、記録の置き場の名前**（M47）。
	//
	// 向こうのホストの記録を読むとき、campd は向こうの $HOME も環境変数も知らない。
	// そこでパスではなく規則を渡し、向こうの sh に解決させて名乗らせる（本人の決定
	// 2026-09-12）。置き場そのもの（$CLAUDE_CONFIG_DIR / $CODEX_HOME と既定）は
	// 駆動器が持っているので、取り込み器はその下の名前だけを言う。
	RecordSub() string

	// Wants は走査で拾うファイルか。拡張子や置き場の形はエージェントで違う。
	Wants(path string) bool

	// Identify はファイルの場所と名前から、会話の同一性（SessionID）と
	// サブエージェントの id を決める。**中身を読む前に呼ぶ。**
	Identify(root, path string, f *FileSummary)

	// NewParser は**ファイル1本ぶんの**行の解釈を作る。1行ずつ呼ばれる。
	//
	// **状態を持てる形にしてある。** Codex は `turn_context` に出てくる model を後の行へ持ち回る
	// 必要がある（`usage.model` は NOT NULL で、実測では turn_context が token_usage_record より
	// 先に来る）。Claude は状態を持たないので、そのまま ParseLine を返す。
	// f はこの範囲を読み始める時点の要約（`Seed` に前回の終わりの状態が入っている）。
	// **状態を持たない取り込み器は無視してよい**（Claude）。
	NewParser(f *FileSummary) ParseFunc

	// Absorb は1行から要約に要る値を吸う。seenRun は run の重複を防ぐ覚え。
	Absorb(f *FileSummary, l *Line, seenRun map[string]struct{})

	// RunRefs はそのファイルが参照している run の id。**自分自身は除く**
	// （初回 run は会話の id と同じ値なので、親の証拠にならない）。
	RunRefs(f *FileSummary) []string

	// Classify は役割（main / resume-sidecar / subagent / stub / empty）を決める。
	// runOwner は run の id -> それを書いた会話の SessionID。
	Classify(f *FileSummary, runOwner map[string]string) string

	// SessionKey は sessions.id に使う値。空なら「セッションを作らない」。
	SessionKey(f *FileSummary) string
}

// collectors は名前で引ける取り込み器。**エージェントを足すのはここに1行。**
var collectors = map[string]Collector{
	AgentClaude: claudeCollector{},
}

// AgentClaude は Claude Code の取り込み器の名前。台帳の `sessions.agent` にもこの値が入る
// （語彙は internal/session・usage_windows と同じ `claude` / `codex`）。
const AgentClaude = "claude"

// CollectorFor は名前から取り込み器を返す。
func CollectorFor(name string) (Collector, bool) {
	c, ok := collectors[name]
	return c, ok
}

// CollectorNames は名乗っている取り込み器の名前。
func CollectorNames() []string {
	out := make([]string, 0, len(collectors))
	for n := range collectors {
		out = append(out, n)
	}
	return out
}

// claudeCollector は Claude Code の記録（~/.claude/projects の JSONL）。
//
// **中身は既存の関数をそのまま呼ぶ。** M44 で振る舞いを変えないため、
// 判断のコードはここへ移さず、呼び先に置いたままにしてある。
type claudeCollector struct{}

var _ Collector = claudeCollector{}

func (claudeCollector) Name() string { return AgentClaude }

// RecordSub は `~/.claude` の下の `projects`（DefaultRoot と同じ置き場を、規則で言い直したもの）。
func (claudeCollector) RecordSub() string { return "projects" }

func (claudeCollector) DefaultRoot(home string) string { return home + "/.claude/projects" }

func (claudeCollector) Wants(path string) bool { return hasJSONLSuffix(path) }

func (claudeCollector) Identify(root, path string, f *FileSummary) { claudeIdentify(path, f) }

// NewParser は状態を持たない（Claude の行は1行で完結する）。
func (claudeCollector) NewParser(*FileSummary) ParseFunc { return ParseLine }

// ParserSeed は解釈器が行をまたいで持ち回る値。**エージェントに依らない形**にしてある。
type ParserSeed struct {
	Model string `json:"model,omitempty"`
	CWD   string `json:"cwd,omitempty"`
	Ver   string `json:"ver,omitempty"`
}

func (claudeCollector) Absorb(f *FileSummary, l *Line, seenRun map[string]struct{}) {
	f.absorb(l, seenRun)
}

// RunRefs は resume のたびに変わる id（session_id）の並び。自分自身は除く。
func (claudeCollector) RunRefs(f *FileSummary) []string {
	out := make([]string, 0, len(f.RunIDs))
	for _, rid := range f.RunIDs {
		if rid != f.SessionID {
			out = append(out, rid)
		}
	}
	return out
}

func (claudeCollector) Classify(f *FileSummary, runOwner map[string]string) string {
	return classify(f, runOwner)
}

func (claudeCollector) SessionKey(f *FileSummary) string { return sessionKey(f) }

// corpusParse は、その Corpus を作った取り込み器の行の解釈を**1本ぶん**作る。
// messages を書く2周目は Corpus しか持っていないので、ここで引き直す。
// **ファイルごとに呼ぶこと**——解釈が状態を持つことがある（Codex の model）。
func corpusParse(c *Corpus, f *FileSummary) ParseFunc {
	if col, ok := CollectorFor(c.Agent); ok {
		return col.NewParser(f)
	}
	return ParseLine
}
