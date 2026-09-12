// Package ingest は Claude Code / Codex の会話記録を読み取る。
package ingest

import (
	"encoding/json"
)

// Line は JSONL の1行。
//
// 重要: sessionId と session_id は別物である。
//   - SessionID (sessionId)  … 会話の同一性。--resume を跨いで不変。ファイル名と一致する
//   - RunID     (session_id) … 実行ごとの同一性。--resume のたびに変わる
//
// 両方を1つのフィールドに畳むと、resume サイドカーが幽霊セッションとして現れる。
type Line struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"` // system 行のみ

	UUID              string `json:"uuid"`
	ParentUUID        string `json:"parentUuid"`
	LogicalParentUUID string `json:"logicalParentUuid"` // compact_boundary の修復リンク

	SessionID string `json:"sessionId"`
	RunID     string `json:"session_id"`

	Timestamp  string `json:"timestamp"` // ai-title/mode/permission-mode/bridge-session/atis-latch には無い
	CWD        string `json:"cwd"`
	GitBranch  string `json:"gitBranch"`
	CLIVersion string `json:"version"`

	IsSidechain      bool `json:"isSidechain"`
	IsMeta           bool `json:"isMeta"`
	IsCompactSummary bool `json:"isCompactSummary"`

	AgentID   string `json:"agentId"`
	RequestID string `json:"requestId"`
	PromptID  string `json:"promptId"`
	Effort    string `json:"effort"`
	UserType  string `json:"userType"`

	IsAPIErrorMessage bool `json:"isApiErrorMessage"`

	Message *Message `json:"message"`

	// 型ごとの小さなペイロード
	AITitle        string          `json:"aiTitle"`         // ai-title
	LastPrompt     string          `json:"lastPrompt"`      // last-prompt
	LeafUUID       string          `json:"leafUuid"`        // last-prompt
	Mode           string          `json:"mode"`            // mode
	PermissionMode string          `json:"permissionMode"`  // permission-mode
	BridgeSession  string          `json:"bridgeSessionId"` // bridge-session
	Operation      string          `json:"operation"`       // queue-operation
	Content        json.RawMessage `json:"content"`         // queue-operation / system

	// file-history-delta / -snapshot は sessionId を持たない。
	// 同一ファイル内の MessageID -> uuid でしか結合できない。
	MessageID         string    `json:"messageId"`
	SnapshotMessageID string    `json:"snapshotMessageId"`
	TrackingPath      string    `json:"trackingPath"`
	Backup            *Backup   `json:"backup"`
	Snapshot          *Snapshot `json:"snapshot"`

	// cost-state はセッションのコストロールアップをタダでくれる
	TotalCostUSD      *float64 `json:"totalCostUSD"`
	TotalLinesAdded   *int64   `json:"totalLinesAdded"`
	TotalLinesRemoved *int64   `json:"totalLinesRemoved"`

	// toolUseResult と attachment は形が一定しない。
	// オブジェクトのこともあれば素の文字列や配列のこともあるので、
	// 厳密な型で受けるとパースが落ちる（実コーパスで26件確認）。
	ToolUseResult json.RawMessage `json:"toolUseResult"`
	Attachment    json.RawMessage `json:"attachment"`

	// Limits はプラン枠の観測を **limits が読む形**（`{"rate_limits":{窓の名前:{used_percentage,resets_at}}}`）
	// に直したもの。Codex は記録の中にプラン枠を埋めているので、取り込みのときに拾える（M46）。
	// Claude は statusLine の別経路なので、ここは空のまま。
	Limits []byte `json:"-"`

	// Raw は元の行そのもの。加工せずそのまま保存する（D-010）。
	Raw []byte `json:"-"`
	// Offset はファイル先頭からのバイト位置。表示順と追尾の再開点を兼ねる。
	Offset int64 `json:"-"`

	// Degraded は厳密なデコードに失敗して最小限の項目だけ拾ったことを示す。
	// Raw は失われていないので、パーサを直せば後から作り直せる。
	Degraded   bool   `json:"-"`
	DegradedBy string `json:"-"`
}

type Message struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Usage   *Usage          `json:"usage"`
	Content json.RawMessage `json:"content"` // 文字列 または ブロック配列
}

type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`

	CacheCreation *struct {
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
	} `json:"cache_creation"`

	OutputTokensDetails *struct {
		ThinkingTokens int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`

	ServerToolUse *struct {
		WebSearchRequests int64 `json:"web_search_requests"`
		WebFetchRequests  int64 `json:"web_fetch_requests"`
	} `json:"server_tool_use"`

	ServiceTier  string          `json:"service_tier"`
	Speed        string          `json:"speed"`
	InferenceGeo string          `json:"inference_geo"`
	Iterations   json.RawMessage `json:"iterations"`
}

type Backup struct {
	BackupFileName string `json:"backupFileName"`
	Version        int    `json:"version"`
	BackupTime     string `json:"backupTime"`
	RealParentDir  string `json:"realParentDir"`
}

type Snapshot struct {
	MessageID          string          `json:"messageId"`
	Timestamp          string          `json:"timestamp"`
	TrackedFileBackups json.RawMessage `json:"trackedFileBackups"` // 累積。そのまま保存すると O(n^2)
}

// ToolResultFilePath は toolUseResult がオブジェクトで filePath を持つときだけ返す。
// 文字列・配列・欠落のいずれでも空文字を返す。
func (l *Line) ToolResultFilePath() string {
	w, _ := l.ToolResultPaths()
	return w
}

// ToolResultPaths は toolUseResult から触ったファイルの絶対パスを読み取る。
//
// 置き場所がツールで違う。Edit と Write は filePath 直下に置くが、
// Read だけは file.filePath と1段深い。filePath だけを見ると、
// 読んだだけのファイル（実測 617件・180パス）が丸ごと落ちる。
func (l *Line) ToolResultPaths() (written, read string) {
	if len(l.ToolUseResult) == 0 {
		return "", ""
	}
	var v struct {
		FilePath string `json:"filePath"`
		File     *struct {
			FilePath string `json:"filePath"`
		} `json:"file"`
	}
	if err := json.Unmarshal(l.ToolUseResult, &v); err != nil {
		return "", ""
	}
	if v.File != nil {
		read = v.File.FilePath
	}
	return v.FilePath, read
}

// ToolResultUseID は message.content の tool_result が指す tool_use の id を返す。
// これを辿らないと、その結果を生んだツールの名前が分からない。
func (l *Line) ToolResultUseID() string {
	if l.Message == nil {
		return ""
	}
	blocks, err := l.Message.Blocks()
	if err != nil {
		return ""
	}
	for _, b := range blocks {
		if b.Type == "tool_result" && b.ToolUseID != "" {
			return b.ToolUseID
		}
	}
	return ""
}

// AttachmentInfo は attachment の type と filename を返す。形が違えば空。
func (l *Line) AttachmentInfo() (kind, filename string) {
	if len(l.Attachment) == 0 {
		return "", ""
	}
	var v struct {
		Type     string `json:"type"`
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(l.Attachment, &v); err != nil {
		return "", ""
	}
	return v.Type, v.Filename
}

// Block は message.content の1要素。
type Block struct {
	Type      string          `json:"type"` // text | thinking | tool_use | tool_result | image
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"` // tool_use
	ID        string          `json:"id"`   // tool_use
	ToolUseID string          `json:"tool_use_id"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"` // tool_result: 文字列 または ブロック配列
}

// Blocks は message.content をブロック配列として返す。
// content が素の文字列の場合は text ブロック1つに正規化する。
func (m *Message) Blocks() ([]Block, error) {
	if m == nil || len(m.Content) == 0 {
		return nil, nil
	}
	var blocks []Block
	if err := json.Unmarshal(m.Content, &blocks); err == nil {
		return blocks, nil
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err != nil {
		return nil, err
	}
	return []Block{{Type: "text", Text: s}}, nil
}

// AbsPath は file-history 行から編集対象の絶対パスを組み立てる。
// realParentDir + basename(trackingPath)。大文字小文字は変えない。
func (l *Line) AbsPath() string {
	if l.Backup == nil || l.Backup.RealParentDir == "" || l.TrackingPath == "" {
		return ""
	}
	base := l.TrackingPath
	for i := len(base) - 1; i >= 0; i-- {
		if base[i] == '/' {
			base = base[i+1:]
			break
		}
	}
	return l.Backup.RealParentDir + "/" + base
}
