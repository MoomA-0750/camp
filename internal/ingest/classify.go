package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	sum := sha256.Sum256(buf)
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
// inode だけでは、消して作り直したファイルが同じ番号を再利用したときに
// 気づけない。そこは resumeSHA（前回の再開点の直前256バイト）が見ている。
// 中身が違えば必ず食い違うので、identity は inode と中身で足りる。
func rotatedFrom(prior *Prior, inode, size int64, resume func() string) bool {
	if prior == nil || prior.Offset <= 0 {
		return false
	}
	switch {
	case prior.Inode != inode:
		return true // 別の実体になった
	case size < prior.Offset:
		return true // 切り詰められた
	case resume() != prior.ResumeSHA:
		// 同じ inode のまま、前より長く書き直された。
		// size と inode だけ見ていると気づけない。
		return true
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
	Files []*FileSummary

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
func Survey(root string, prior map[string]*Prior) (*Corpus, error) {
	c := &Corpus{
		Root:         root,
		runOwner:     map[string]string{},
		bySessionKey: map[string]*FileSummary{},
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		fsum, err := summarize(root, path, d, prior[path])
		if err != nil {
			return err
		}
		c.Files = append(c.Files, fsum)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 参照関係を集める。自分自身への参照（初回 run は session_id が
	// sessionId と同じ）は親の証拠にならないので除く。
	for _, f := range c.Files {
		for _, rid := range f.RunIDs {
			if rid != f.SessionID {
				c.runOwner[rid] = f.SessionID
			}
		}
	}

	for _, f := range c.Files {
		f.Role = classify(f, c.runOwner)
		f.SessionKey = sessionKey(f)
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

// SessionFile は sessions.id からその主ファイルを引く。
func (c *Corpus) SessionFile(key string) *FileSummary { return c.bySessionKey[key] }

// summarize は1ファイルを読み切って要約する。
func summarize(root, path string, d fs.DirEntry, prior *Prior) (*FileSummary, error) {
	info, err := d.Info()
	if err != nil {
		return nil, err
	}
	rel, _ := filepath.Rel(root, path)

	f := &FileSummary{}
	start := int64(0)

	// 前回の要約が使えるなら、そこから追記分だけ読む。
	// (dev, inode) が変わっていたら別の実体なので先頭から。
	// size が前回位置より小さければ切り詰められているので先頭から。
	var dev, inode int64
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		dev, inode = int64(st.Dev), int64(st.Ino)
	}
	rotated := rotatedFrom(prior, inode, info.Size(), func() string {
		return resumeSHA(path, prior.Offset)
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

	f.Path = path
	f.Rel = rel
	f.Size = info.Size()
	f.MTime = info.ModTime().UTC()
	f.Dev, f.Inode = dev, inode
	f.Broken = 0

	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if strings.Contains(filepath.ToSlash(filepath.Dir(path)), "/subagents") {
		f.AgentID = strings.TrimPrefix(base, "agent-")
	} else {
		f.SessionID = base
	}
	if f.Size == 0 {
		return f, nil
	}
	if start > 0 && start == f.Size {
		f.EndOffset = start
		f.ResumeSHA = resumeSHA(path, start)
		return f, nil // 追記なし
	}

	seenRun := map[string]struct{}{}
	for _, r := range f.RunIDs {
		seenRun[r] = struct{}{}
	}
	res, walkErr := WalkFileFrom(path, start, func(l *Line) error {
		f.Lines++
		f.absorb(l, seenRun)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	f.Broken = len(res.Broken)
	f.EndOffset = res.EndOffset
	f.Pending = res.Pending
	f.ResumeSHA = resumeSHA(path, res.EndOffset)

	if f.SessionID == "" {
		f.SessionID = base // sessionId が1行も無い subagent への保険
	}
	return f, nil
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
