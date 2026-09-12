package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ファイルの役割。source_files.role に入る。
//
// 判定は「その時点の中身」から毎回やり直す。稼働中のセッションは
// 生まれた直後 RoleStub と見分けが付かず、最初のプロンプトが書かれた
// 瞬間に RoleMain に変わるため、役割を一度決めて固定してはいけない。
const (
	RoleMain     = "main"           // user/assistant 行を持つ。実体のある会話
	RoleSidecar  = "resume-sidecar" // 会話行が無く、他ファイルの session_id として参照されている
	RoleSubagent = "subagent"       // <sessionId>/subagents/agent-*.jsonl
	RoleStub     = "stub"           // 会話行が無く、参照もされていない。起動しただけで捨てられたセッション
	RoleEmpty    = "empty"          // 0バイト、または1行も無い
)

// FileSummary は1ファイルを読み切って得た要約。
// messages を作るのに必要な生データは持たない（メモリに載せないため）。
type FileSummary struct {
	Path  string
	Rel   string
	Size  int64
	Dev   int64
	Inode int64
	MTime time.Time

	// Capped は「この1回では読み切らなかった」。蓋（limit）で止めたときだけ立つ。
	// 次回は ingested_offset から続きを読む。
	Capped bool

	// Seed は**この範囲を読み始める時点の、解釈器の状態**。
	//
	// Codex は `turn_context` の model・cwd を後の行へ持ち回る。差分だけ読むときに空から
	// 始めると、前回のうちに `turn_context` を読み終えていた場合、後から追記された行の
	// model・cwd・版が空になる（usage は空の model で計上される）。Claude は状態を持たない
	// ので、これは Codex にだけ起きる差だった（2026-09-12、codex のフェーズレビュー 4）。
	//
	// **要約と書き込みが同じ種から始まるように、summary に残す。** 読み終えた後の状態で
	// 書き込み側を始めると、範囲の途中で変わった新しい model を前の行へ逆に当ててしまう。
	Seed ParserSeed `json:"seed,omitempty"`

	// SessionID は会話の同一性。通常はファイル名だが、subagent の場合は
	// 行の sessionId が親を指すため、そこから採る。
	SessionID string
	AgentID   string
	Role      string

	// SessionKey は sessions.id に使う値。subagent だけ合成する。
	// 行の sessionId は親と同じ値なので、そのままでは主キーにできない。
	SessionKey string

	Lines           int
	HasConversation bool
	IsSidechain     bool

	// RunIDs は session_id（実行ごとのID）を出現順に重複なく並べたもの。
	// main ファイルではこれが resume の回数になる。
	RunIDs []string

	FirstCWD string
	LastCWD  string
	// CWDs はこのファイルに現れた作業ディレクトリ全部。1セッションが
	// 複数のディレクトリを跨ぐことがあるので（実測で最大11個）、
	// projects は最初と最後だけでなくここから作る。
	CWDs      []string
	FirstTS   string
	LastTS    string
	GitBranch string

	CLIVersion       string
	Model            string // 最後に見た model。**誰も書いていなかった last_model を埋める**
	AITitle          string
	LastPrompt       string
	LastPromptLeaf   string
	Mode             string
	PermissionMode   string
	BridgeSessionID  string
	FirstUserMessage string
	TotalCostUSD     *float64

	EndOffset int64
	Pending   []byte
	Broken    int

	// ResumeSHA は EndOffset 直前256バイトのハッシュ。次回の再開点の検証に使う。
	ResumeSHA string
	// Rotated は前回の状態が使えず先頭から読み直したことを示す。
	// source_files の世代を進めるかどうかの唯一の判断材料。
	Rotated bool `json:"-"`
}

// resumeWindow は再開点の検証に使う窓の大きさ。
const resumeWindow = 256

// resumeSHA は offset 直前の resumeWindow バイトのハッシュを返す。
// 追記専用ならこの範囲は不変なので、変わっていたら書き直されている。
func resumeSHA(path string, offset int64) string {
	if offset <= 0 {
		return ""
	}
	start := offset - resumeWindow
	if start < 0 {
		start = 0
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, offset-start)
	if _, err := f.ReadAt(buf, start); err != nil {
		return ""
	}
	return WindowSHA(buf)
}

// ResumeWindow は再開点の窓の大きさ。**向こうのホストへ「この大きさで寄こせ」と頼む**のに要る。
const ResumeWindow = resumeWindow

// WindowSHA は再開点の窓のハッシュ。
//
// **向こうのホストから運んだ窓も、必ずこれに通す**（M47）。向こうで計算させると、
// 取り方がずれても気づけない（そもそも向こうに sha256sum があるとも限らない）。
func WindowSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// rotatedFrom は「前回見たファイルと同じ実体か」を決める。
//
// **dev（デバイス番号）は見ない。** st_dev はマウントごとにカーネルが振る値で、
// 再起動や再マウントで変わる。実測（2026-09-03）で、再起動後に dev が 51→35 と
// 変わっただけで inode も中身も同じ 72 ファイル全部が「別の実体」と判定され、
// 世代が進んで**コーパス全体が二重に取り込まれた**（33,621行 322MB →
// 69,630行 529MB）。**再起動のたびにDBが倍になる。**
//
// **inode も見ない（2026-09-03 追加）。** dev と同じ理由で、st_ino も
// 「同じファイル」の証明にならない。rsync は既定で一時ファイルへ書いて rename
// するので新しい inode になり、バックアップからの復元や別マシンへの移動も同じ。
// 中身が1バイトも変わっていないのに世代が上がると、
//
//   - その行が全部もう一度 messages へ入る（一意制約が (source_file_id, byte_offset)
//     なので重複と見なされない。uuid は resume でファイルを跨いで正当に重複するため
//     一意にできない）
//   - 古い世代に紐づいた tombstone が効かなくなり、**消したものが戻る**
//
// の2つが同時に起きる。2026-09-03 の outer gate で両方とも実測した。
//
// 消して作り直したファイルが同じ位置に違う中身を持つ場合は、resumeSHA
// （前回の再開点の直前256バイト）が食い違う。切り詰めは size で分かる。
// **identity は中身だけで足りる。**
func rotatedFrom(prior *Prior, size int64, resume func() string) bool {
	if prior == nil || prior.Offset <= 0 {
		return false
	}
	switch {
	case size < prior.Offset:
		return true // 切り詰められた
	case resume() != prior.ResumeSHA:
		return true // 同じ位置に違う中身が来た
	}
	return false
}

// SummaryVersion は要約キャッシュの世代。パーサを直したら上げる。
// 上げると保存済みの要約が捨てられ、全ファイルが先頭から読み直される。
const SummaryVersion = 1

// Prior は前回の走査で保存した状態。差分だけ読むために使う。
type Prior struct {
	Offset    int64
	Dev       int64
	Inode     int64
	Version   int
	ResumeSHA string
	Summary   []byte // FileSummary の JSON
}

// Corpus は1回のスキャンで見えたファイル全体。
type Corpus struct {
	Root  string
	Agent string // どの取り込み器で見たか。sessions.agent にこの値が入る
	Files []*FileSummary

	// Unreadable は権限で開けなかったファイル。**黙って落とさず、数えて返す。**
	// ACL が配り直される前の新しいセッションが主にここに来る。
	Unreadable []string

	// Deferred は「そこに在るが、今回は運ばなかった」ファイル（向こうのホストの蓋）。
	// **消えた印を付けない**ために、名前だけ後段へ伝える。
	Deferred []string

	// runOwner は session_id -> それを書いた会話の SessionID。
	// resume サイドカーの親を引くのに使う。
	runOwner map[string]string

	// bySessionKey は sessions.id -> そのセッションの主ファイル。
	bySessionKey map[string]*FileSummary
}

// Survey は root 以下の .jsonl を全部読み、役割まで決めて返す。
//
// 分類は全ファイルを見終わるまで確定できない。「参照されているか」が
// 他ファイルの中身に依存するので、1ファイルずつ完結させられない。
// そのため取り込みは2パスになる（ここで分類 → 別パスで messages を書く）。
func Survey(col Collector, root string, prior map[string]*Prior) (*Corpus, error) {
	return SurveyWith(col, root, prior, 0)
}

// SurveyWith は手元の root を1ファイルにつき limit バイトまでで走査する（0 は蓋なし）。
func SurveyWith(col Collector, root string, prior map[string]*Prior, limit int64) (*Corpus, error) {
	fset, err := LocalFiles{Root: root}.Open()
	if err != nil {
		return nil, err
	}
	defer fset.Close()
	return SurveyFrom(col, fset, prior, limit)
}

// SurveyFrom は読み手から走査する。**置き場がどこかは読み手が決める**——
// 向こうのホストでは $HOME や環境変数から向こうが解決して名乗るので、
// campd は頼む時点では知らない（M47）。
func SurveyFrom(col Collector, fset FileSet, prior map[string]*Prior, limit int64) (*Corpus, error) {
	c := &Corpus{
		Agent:        col.Name(),
		runOwner:     map[string]string{},
		bySessionKey: map[string]*FileSummary{},
	}

	// 前回の位置も一緒に渡す。**向こうのホストでは一覧と再開点を1回で取る**——
	// 同じ実体かを見るためだけに、ファイルの数だけ繋ぎ直さないため。
	at := map[string]int64{}
	for path, p := range prior {
		if p != nil && p.Offset > 0 {
			at[path] = p.Offset
		}
	}
	lst, err := fset.List(at)
	if err != nil {
		return nil, err
	}
	c.Root = lst.Root
	// 読めなかったものは数えて飛ばし、Corpus に持って上へ伝える（読み手が集める）。
	c.Unreadable = append(c.Unreadable, lst.Unreadable...)
	// 「在るが今回は運ばなかった」ものも伝える。**消えた印を付けさせないため。**
	c.Deferred = append(c.Deferred, lst.Deferred...)

	for _, fi := range lst.Files {
		if !col.Wants(fi.Path) {
			continue
		}
		fsum, err := summarize(col, lst.Root, fi, prior[fi.Path], limit, fset)
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				c.Unreadable = append(c.Unreadable, fi.Path)
				continue
			}
			if errors.Is(err, fs.ErrNotExist) {
				continue // 歩いている最中に消えた
			}
			return nil, err
		}
		c.Files = append(c.Files, fsum)
	}

	// 参照関係を集める。自分自身への参照（初回 run は session_id が
	// sessionId と同じ）は親の証拠にならないので除く。
	for _, f := range c.Files {
		for _, rid := range col.RunRefs(f) {
			c.runOwner[rid] = f.SessionID
		}
	}

	for _, f := range c.Files {
		f.Role = col.Classify(f, c.runOwner)
		f.SessionKey = col.SessionKey(f)
		if f.SessionKey != "" && f.Role != RoleSidecar {
			c.bySessionKey[f.SessionKey] = f
		}
	}
	return c, nil
}

// classify は役割を決める。判定の順序に意味がある。
func classify(f *FileSummary, runOwner map[string]string) string {
	if f.AgentID != "" {
		return RoleSubagent
	}
	if f.Size == 0 || f.Lines == 0 {
		return RoleEmpty
	}
	if f.HasConversation {
		return RoleMain
	}
	if _, referenced := runOwner[f.SessionID]; referenced {
		return RoleSidecar
	}
	// 会話行が無く、誰からも参照されていない。remote-control を起動して
	// 何も送らずに終えた類。セッションとしては実在するが中身は無い。
	return RoleStub
}

// sessionKey は sessions.id に使う値を返す。
// subagent は行の sessionId が親と同じなので、agentId を足して分ける。
func sessionKey(f *FileSummary) string {
	switch f.Role {
	case RoleSubagent:
		return f.SessionID + "." + f.AgentID
	case RoleSidecar:
		return "" // セッションを作らない。runs になる
	default:
		return f.SessionID
	}
}

// ParentSession はサイドカーが属する会話の SessionID を返す。
func (c *Corpus) ParentSession(f *FileSummary) string {
	return c.runOwner[f.SessionID]
}

// dropSessions は、書かないと決めたセッションのファイルを走査結果から外す。
//
// **「消えた」印は付けない**——ファイルは向こうに在って、読めてもいる。書かなかった
// だけなので、Deferred（在るが今回は運ばなかった）へ回す。
func (c *Corpus) dropSessions(keys []string) {
	drop := make(map[string]bool, len(keys))
	for _, k := range keys {
		drop[k] = true
	}
	keep := c.Files[:0:0]
	for _, f := range c.Files {
		key := f.SessionKey
		if f.Role == RoleSidecar {
			key = c.ParentSession(f)
		}
		if drop[key] {
			c.Deferred = append(c.Deferred, f.Path)
			continue
		}
		keep = append(keep, f)
	}
	c.Files = keep
}

// SessionFile は sessions.id からその主ファイルを引く。
func (c *Corpus) SessionFile(key string) *FileSummary { return c.bySessionKey[key] }

// summarize は1ファイルを読み切って要約する。
//
// limit は**この1回で読む上限**（0 は蓋なし）。向こうのホストの記録を少しずつ運ぶために使う。
// **蓋は必ずここ（要約する側）で切る。** 台帳へ行を入れる側は要約が決めた終点に合わせるので、
// 逆向き（書き込む側にだけ蓋を掛ける）にすると ingested_offset と resume_sha が食い違い、
// 次回それが「同じ位置に違う中身」と読まれて世代が進む（2026-09-03 の事故と同じ経路）。
func summarize(col Collector, root string, fi FileInfo, prior *Prior, limit int64, fset FileSet) (*FileSummary, error) {
	path := fi.Path
	rel, _ := filepath.Rel(root, path)

	f := &FileSummary{}
	start := int64(0)

	// 前回の要約が使えるなら、そこから追記分だけ読む。
	// size が前回位置より小さければ切り詰められているので先頭から。
	// **dev・inode は見ない**（rotatedFrom のコメント）。台帳には残すので持ち回るだけ。
	rotated := rotatedFrom(prior, fi.Size, func() string {
		return fset.Window(path, prior.Offset)
	})
	if prior != nil && prior.Offset > 0 {
		if !rotated && prior.Version == SummaryVersion && len(prior.Summary) > 0 {
			if err := json.Unmarshal(prior.Summary, f); err == nil {
				start = prior.Offset
			} else {
				f = &FileSummary{}
			}
		}
	}
	f.Rotated = rotated
	// **この範囲を読み始める時点の状態を控える。** 前回の要約から復元した値がそれ。
	// 先頭から読み直すとき（rotated・prior 無し）は空のまま。
	f.Seed = ParserSeed{Model: f.Model, CWD: f.LastCWD, Ver: f.CLIVersion}

	f.Path = path
	f.Rel = rel
	f.Size = fi.Size
	f.MTime = fi.MTime
	f.Dev, f.Inode = fi.Dev, fi.Inode
	f.Broken = 0

	col.Identify(root, path, f)
	if f.Size == 0 {
		return f, nil
	}
	if start > 0 && start == f.Size {
		f.EndOffset = start
		f.ResumeSHA = fset.Window(path, start)
		return f, nil // 追記なし
	}

	seenRun := map[string]struct{}{}
	for _, r := range f.RunIDs {
		seenRun[r] = struct{}{}
	}
	stop := int64(0)
	if limit > 0 {
		stop = start + limit
	}
	rc, err := fset.Reader(path, start, stop)
	if err != nil {
		return nil, err
	}
	res, walkErr := WalkReader(rc, start, stop, col.NewParser(f), func(l *Line) error {
		f.Lines++
		col.Absorb(f, l, seenRun)
		return nil
	})
	rc.Close()
	if walkErr != nil {
		return nil, walkErr
	}
	f.Broken = len(res.Broken)
	f.EndOffset = res.EndOffset
	f.Capped = stop > 0 && res.EndOffset < f.Size
	f.Pending = res.Pending
	f.ResumeSHA = fset.Window(path, res.EndOffset)

	col.Identify(root, path, f) // sessionId が1行も無い subagent への保険
	return f, nil
}

// hasJSONLSuffix は走査で拾う拡張子か。**置き場の形はエージェントで違うが、
// どちらも JSONL**（Claude は projects/**.jsonl、Codex は sessions/**/rollout-*.jsonl）。
func hasJSONLSuffix(path string) bool { return strings.HasSuffix(path, ".jsonl") }

// claudeIdentify はファイルの名前と場所から、会話の同一性とサブエージェントの id を決める。
// **Claude の形**: <sessionId>.jsonl と <sessionId>/subagents/agent-<agentId>.jsonl。
// 既に決まっているものは触らない（走査のあとの保険で2度呼ばれる）。
func claudeIdentify(path string, f *FileSummary) {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if strings.Contains(filepath.ToSlash(filepath.Dir(path)), "/subagents") {
		if f.AgentID == "" {
			f.AgentID = strings.TrimPrefix(base, "agent-")
		}
		if f.SessionID == "" && f.Lines > 0 {
			f.SessionID = base // 行から採れなかったときの保険
		}
		return
	}
	if f.SessionID == "" {
		f.SessionID = base
	}
}

// absorb は1行から要約に必要な値を吸い上げる。
func (f *FileSummary) absorb(l *Line, seenRun map[string]struct{}) {
	if l.SessionID != "" && f.AgentID != "" {
		// subagent の sessionId は親を指す。これが親セッションへの唯一のリンク。
		f.SessionID = l.SessionID
	}
	if l.RunID != "" {
		if _, dup := seenRun[l.RunID]; !dup {
			seenRun[l.RunID] = struct{}{}
			f.RunIDs = append(f.RunIDs, l.RunID)
		}
	}
	if l.Type == "user" || l.Type == "assistant" {
		f.HasConversation = true
	}
	if l.IsSidechain {
		f.IsSidechain = true
	}
	if l.AgentID != "" {
		f.AgentID = l.AgentID
	}

	if l.CWD != "" {
		if f.FirstCWD == "" {
			f.FirstCWD = l.CWD
		}
		if f.LastCWD != l.CWD && !contains(f.CWDs, l.CWD) {
			f.CWDs = append(f.CWDs, l.CWD)
		}
		f.LastCWD = l.CWD
	}
	if l.Timestamp != "" {
		if f.FirstTS == "" {
			f.FirstTS = l.Timestamp
		}
		f.LastTS = l.Timestamp
	}
	if l.GitBranch != "" {
		f.GitBranch = l.GitBranch
	}
	if l.CLIVersion != "" {
		f.CLIVersion = l.CLIVersion
	}
	if l.Message != nil && l.Message.Model != "" {
		f.Model = l.Message.Model
	}

	switch l.Type {
	case "ai-title":
		if l.AITitle != "" {
			f.AITitle = l.AITitle
		}
	case "last-prompt":
		if l.LastPrompt != "" {
			f.LastPrompt = l.LastPrompt
		}
		if l.LeafUUID != "" {
			f.LastPromptLeaf = l.LeafUUID
		}
	case "mode":
		if l.Mode != "" {
			f.Mode = l.Mode
		}
	case "permission-mode":
		if l.PermissionMode != "" {
			f.PermissionMode = l.PermissionMode
		}
	case "bridge-session":
		if l.BridgeSession != "" {
			f.BridgeSessionID = l.BridgeSession
		}
	case "cost-state":
		if l.TotalCostUSD != nil {
			f.TotalCostUSD = l.TotalCostUSD
		}
	case "user":
		if f.FirstUserMessage == "" && !l.IsMeta {
			f.FirstUserMessage = firstText(l)
		}
	}
}

// firstText は user 行の最初のテキストを短く取り出す。一覧の見出し用。
func firstText(l *Line) string {
	if l.Message == nil {
		return ""
	}
	blocks, err := l.Message.Blocks()
	if err != nil {
		return ""
	}
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			return truncate(strings.TrimSpace(b.Text), 500)
		}
	}
	return ""
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Timestamps はセッションの開始・更新時刻を返す。
// 行にタイムスタンプが1つも無いファイル（mode/bridge-session だけの stub）は
// ファイルの mtime で代用する。sessions.started_at は NOT NULL なので必ず埋める。
func (f *FileSummary) Timestamps() (started, updated string) {
	mt := f.MTime.Format(time.RFC3339Nano)
	started, updated = f.FirstTS, f.LastTS
	if started == "" {
		started = mt
	}
	if updated == "" {
		updated = mt
	}
	return
}

// ParserVersion は messages から派生行（message_blocks・usage・session_files）を
// 作るパーサの世代。**抽出の結果が変わる直しをしたら上げる。**
//
// 0 は「分からない」を意味する予約値で、M21 より前に取り込んだ行に付く。
// 上げ忘れるより、上げすぎて作り直すほうが安い。
const ParserVersion = 1
