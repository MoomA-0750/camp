package ingest

import (
	"io/fs"
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
func Survey(root string) (*Corpus, error) {
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
		fsum, err := summarize(root, path, d)
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
func summarize(root, path string, d fs.DirEntry) (*FileSummary, error) {
	info, err := d.Info()
	if err != nil {
		return nil, err
	}
	rel, _ := filepath.Rel(root, path)

	f := &FileSummary{
		Path:  path,
		Rel:   rel,
		Size:  info.Size(),
		MTime: info.ModTime().UTC(),
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		f.Dev = int64(st.Dev)
		f.Inode = int64(st.Ino)
	}

	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if strings.Contains(filepath.ToSlash(filepath.Dir(path)), "/subagents") {
		f.AgentID = strings.TrimPrefix(base, "agent-")
	} else {
		f.SessionID = base
	}
	if f.Size == 0 {
		return f, nil
	}

	seenRun := map[string]struct{}{}
	res, walkErr := WalkFile(path, func(l *Line) error {
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
