package ingest

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// Codex の記録の取り込み器（Phase 3.8 の M45）。
//
// 置き場は本人の `~/.codex/sessions/YYYY/MM/DD/rollout-<時刻>-<uuid>.jsonl`（CLI と同じ場所。D-030）。
// 1行は `{timestamp, ordinal, type, payload}` で、type は6種:
//
//	session_meta        会話の頭。id・cwd・cli_version・git・source（cli / vscode / exec / subagent）
//	turn_context        そのターンの model・approval_policy・sandbox_policy・cwd
//	response_item       本文。message / reasoning / custom_tool_call / custom_tool_call_output
//	event_msg           途中経過。token_count に**プラン枠**（rate_limits）が載る（M46 で使う）
//	token_usage_record  ターンのトークン内訳
//	world_state         その他
//
// **Line へ翻訳して本体へ渡す。** 派生（usage・blocks・files）は全部 `*Line` を見ているので、
// ここで Claude の形に寄せておけば本体に手を入れずに済む（M44 の作り）。
const AgentCodex = "codex"

func init() { collectors[AgentCodex] = codexCollector{} }

type codexCollector struct{}

var _ Collector = codexCollector{}

func (codexCollector) Name() string { return AgentCodex }

func (codexCollector) DefaultRoot(home string) string { return home + "/.codex/sessions" }

// RecordSub は `~/.codex` の下の `sessions`（DefaultRoot と同じ置き場を、規則で言い直したもの）。
func (codexCollector) RecordSub() string { return "sessions" }

// Wants は `rollout-*.jsonl` だけ拾う。置き場には他のものも置かれうる。
func (codexCollector) Wants(path string) bool {
	return hasJSONLSuffix(path) && strings.HasPrefix(filepath.Base(path), "rollout-")
}

// Identify はファイル名からは決めない。**会話の id は `session_meta.id`**（行から採る）。
// ファイル名の uuid は同じ値のことが多いが、実測で 73本中 4本（すべて cli）食い違う。
// 行が1つも無いファイル（起こしただけ）のときだけ、名前を保険に使う。
func (codexCollector) Identify(root, path string, f *FileSummary) {
	if f.SessionID != "" || f.Lines == 0 {
		return
	}
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if i := strings.LastIndex(base, "-"); i > 0 && len(base) > i+1 {
		f.SessionID = base[i+1:]
	}
}

// codexLine は Codex の1行の外側。
type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// codexParser は1本ぶんの解釈。**model と cwd を持ち回る**——`turn_context` は本文より後に
// 来ることがあり（実測）、`usage.model` は NOT NULL。最後に見た値を後の行へ渡す。
type codexParser struct {
	model string
	cwd   string
	ver   string
}

// NewParser は**前回の終わりの状態から始める**。差分だけ読むとき、前回のうちに
// `turn_context` を読み終えていると、空から始めては model も cwd も拾えない
// （2026-09-12、codex のフェーズレビュー 4）。
func (codexCollector) NewParser(f *FileSummary) ParseFunc {
	p := &codexParser{}
	if f != nil {
		p.model, p.cwd, p.ver = f.Seed.Model, f.Seed.CWD, f.Seed.Ver
	}
	return p.parse
}

func (p *codexParser) parse(raw []byte, offset int64) (*Line, error) {
	var o codexLine
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, fmt.Errorf("offset %d: %w", offset, err)
	}
	l := &Line{Type: o.Type, Timestamp: o.Timestamp, Raw: raw, Offset: offset}

	switch o.Type {
	case "session_meta":
		var m struct {
			ID         string          `json:"id"`
			SessionID  string          `json:"session_id"`
			CWD        string          `json:"cwd"`
			CLIVersion string          `json:"cli_version"`
			Source     json.RawMessage `json:"source"`
			Git        *struct {
				Branch string `json:"branch"`
			} `json:"git"`
		}
		if err := json.Unmarshal(o.Payload, &m); err != nil {
			return degraded(l, err), nil
		}
		l.SessionID = firstNonEmpty(m.ID, m.SessionID)
		l.CWD, l.CLIVersion = m.CWD, m.CLIVersion
		if m.Git != nil {
			l.GitBranch = m.Git.Branch
		}
		p.cwd, p.ver = m.CWD, m.CLIVersion
		// subagent は親スレッドと、この記録自身の id を持つ。
		if parent, agent := codexSpawn(m.Source); parent != "" {
			l.SessionID = parent // 親への唯一のリンク（Claude の subagent と同じ扱い）
			l.AgentID = firstNonEmpty(agent, m.ID)
			l.IsSidechain = true
		}

	case "turn_context":
		// **オブジェクトの欄は生のまま受ける。** `collaboration_mode` は `{mode, settings}` で、
		// ほかにも dict / list の欄がある（`sandbox_policy`・`permission_profile`・`workspace_roots`）。
		// 素の文字列で受けると payload 全体のデコードが落ち、turn_id も model も拾えなくなる
		// （2026-09-12、実データで degraded になって気づいた）。
		var t struct {
			Model          string          `json:"model"`
			CWD            string          `json:"cwd"`
			ApprovalPolicy string          `json:"approval_policy"`
			Collaboration  json.RawMessage `json:"collaboration_mode"`
			TurnID         string          `json:"turn_id"`
		}
		if err := json.Unmarshal(o.Payload, &t); err != nil {
			return degraded(l, err), nil
		}
		if t.Model != "" {
			p.model = t.Model
		}
		if t.CWD != "" {
			p.cwd = t.CWD
		}
		l.CWD = p.cwd
		l.RunID = t.TurnID
		// 確認の度合いと協調のしかたは、Claude の mode / permission-mode と同じ欄へ。
		l.Mode, l.PermissionMode = codexMode(t.Collaboration), t.ApprovalPolicy

	case "response_item":
		// **工具の中身は content ではない。** `custom_tool_call` は `input`、
		// `custom_tool_call_output` は `output` に入る（2026-09-12、`rp` の実データで確認:
		// call 6件すべてに input、output 5件すべてに output があり、content は無い）。
		// content だけ見ていたので、Codex の工具が検索に1件も入っていなかった
		// （codex のフェーズレビュー 5）。
		var r struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			ID      string          `json:"id"`
			CallID  string          `json:"call_id"`
			Name    string          `json:"name"`
			Content json.RawMessage `json:"content"`
			Input   json.RawMessage `json:"input"`
			Output  json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(o.Payload, &r); err != nil {
			return degraded(l, err), nil
		}
		l.Subtype = r.Type
		l.CWD, l.CLIVersion = p.cwd, p.ver
		body := r.Content
		switch r.Type {
		case "custom_tool_call":
			body = r.Input
		case "custom_tool_call_output":
			body = r.Output
		}
		l.Message = &Message{ID: r.ID, Role: r.Role, Model: p.model,
			Content: codexText(r.Type, r.Name, r.CallID, body)}
		// 本体の「会話の行か」は type で見る（Claude は user / assistant）。揃えておく。
		if r.Type == "message" {
			switch r.Role {
			case "user", "developer":
				l.Type = "user"
			case "assistant":
				l.Type = "assistant"
			}
		}

	case "event_msg":
		// **プラン枠は記録の中にある**（`token_count` の `rate_limits`）。ただし形が違う:
		// Camp（statusLine の形）は「窓の名前 → {used_percentage, resets_at}」の map。
		// Codex は固定キーで、`primary` / `secondary` が {used_percent, resets_at, window_minutes}。
		// 実データでは primary=300分・secondary=10080分（＝5時間と7日）で、null の回もある。
		var e struct {
			Type       string `json:"type"`
			RateLimits *struct {
				Primary   *codexWindow `json:"primary"`
				Secondary *codexWindow `json:"secondary"`
			} `json:"rate_limits"`
		}
		if err := json.Unmarshal(o.Payload, &e); err != nil {
			return degraded(l, err), nil
		}
		l.Subtype = e.Type
		if e.RateLimits != nil {
			l.Limits = codexLimits(e.RateLimits.Primary, e.RateLimits.Secondary)
		}

	case "token_usage_record":
		var u struct {
			ResponseID string `json:"response_id"`
			TurnID     string `json:"turn_id"`
			Usage      *struct {
				Input     int64 `json:"input_tokens"`
				Cached    int64 `json:"cached_input_tokens"`
				Write     int64 `json:"cache_write_input_tokens"`
				Output    int64 `json:"output_tokens"`
				Reasoning int64 `json:"reasoning_output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(o.Payload, &u); err != nil {
			return degraded(l, err), nil
		}
		l.RunID = u.TurnID
		if u.Usage != nil && u.ResponseID != "" {
			l.Message = &Message{ID: u.ResponseID, Model: p.model, Usage: &Usage{
				InputTokens:              u.Usage.Input,
				OutputTokens:             u.Usage.Output,
				CacheReadInputTokens:     u.Usage.Cached,
				CacheCreationInputTokens: u.Usage.Write,
			}}
			if u.Usage.Reasoning > 0 {
				l.Message.Usage.OutputTokensDetails = &struct {
					ThinkingTokens int64 `json:"thinking_tokens"`
				}{ThinkingTokens: u.Usage.Reasoning}
			}
		}
	}
	return l, nil
}

// Absorb は要約へ積む。**Claude 版と同じ欄を埋める**ので、本体の書き込みはそのまま動く。
func (codexCollector) Absorb(f *FileSummary, l *Line, seenRun map[string]struct{}) {
	if l.SessionID != "" && f.SessionID == "" {
		f.SessionID = l.SessionID
	}
	if l.AgentID != "" {
		f.AgentID = l.AgentID
		f.IsSidechain = true
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
	// **model は Codex でもここで積む。** Claude 版の absorb に足しただけでは Codex は素通りして
	// `sessions.last_model` が空のままだった（2026-09-12、実データで気づいた）。
	if l.Message != nil && l.Message.Model != "" {
		f.Model = l.Message.Model
	}
	if l.Mode != "" {
		f.Mode = l.Mode
	}
	if l.PermissionMode != "" {
		f.PermissionMode = l.PermissionMode
	}
	if l.Type == "user" && f.FirstUserMessage == "" {
		f.FirstUserMessage = firstText(l)
	}
}

// RunRefs は turn の id。**Codex に resume のサイドカーは無い**ので、自分自身を除く細工は要らない。
func (codexCollector) RunRefs(f *FileSummary) []string { return nil }

// Classify は役割。sidecar は無い（1スレッド1ファイル。実測で 73本中の跨りゼロ）。
func (codexCollector) Classify(f *FileSummary, runOwner map[string]string) string {
	if f.AgentID != "" {
		return RoleSubagent
	}
	if f.Size == 0 || f.Lines == 0 {
		return RoleEmpty
	}
	if f.HasConversation {
		return RoleMain
	}
	return RoleStub
}

// SessionKey は Claude と同じ決め方（subagent だけ親と分ける）。
func (codexCollector) SessionKey(f *FileSummary) string { return sessionKey(f) }

// codexSpawn は subagent の記録から親スレッドと自分の呼び名を取り出す。
func codexSpawn(src json.RawMessage) (parent, agent string) {
	if len(src) == 0 {
		return "", ""
	}
	var s struct {
		Subagent *struct {
			ThreadSpawn *struct {
				ParentThreadID string `json:"parent_thread_id"`
				AgentPath      string `json:"agent_path"`
			} `json:"thread_spawn"`
		} `json:"subagent"`
	}
	if err := json.Unmarshal(src, &s); err != nil || s.Subagent == nil || s.Subagent.ThreadSpawn == nil {
		return "", ""
	}
	return s.Subagent.ThreadSpawn.ParentThreadID, s.Subagent.ThreadSpawn.AgentPath
}

// codexText は本文をブロックの形（Claude と同じ）に詰め直す。
// Codex の content は `[{type:"input_text"|"output_text", text}]`。工具は name と call_id を持つ。
func codexText(kind, name, callID string, content json.RawMessage) json.RawMessage {
	// **`tool_use` の中身は `input` に入れる。** 検索のブロックを作る側（blocks.go）は
	// `tool_use` なら `Input`、`tool_result` なら `Content` を見て、空なら索引しない。
	// Claude の形に合わせる（エージェントによる差を残さない）。
	type block struct {
		Type      string          `json:"type"`
		Text      string          `json:"text,omitempty"`
		Name      string          `json:"name,omitempty"`
		ID        string          `json:"id,omitempty"`
		ToolUseID string          `json:"tool_use_id,omitempty"`
		Input     json.RawMessage `json:"input,omitempty"`
		Content   json.RawMessage `json:"content,omitempty"`
	}
	var out []block
	switch kind {
	case "custom_tool_call":
		out = append(out, block{Type: "tool_use", Name: name, ID: callID, Input: content})
	case "custom_tool_call_output":
		out = append(out, block{Type: "tool_result", ToolUseID: callID, Content: content})
	default:
		var items []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(content, &items); err == nil {
			for _, it := range items {
				t := "text"
				if it.Type == "reasoning" || kind == "reasoning" {
					t = "thinking"
				}
				out = append(out, block{Type: t, Text: it.Text})
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return b
}

// codexWindow は Codex のプラン枠の1つの窓。**キーの名前が Camp と違う**（used_percent）。
type codexWindow struct {
	UsedPercent   *float64 `json:"used_percent"`
	ResetsAt      *int64   `json:"resets_at"`
	WindowMinutes *int     `json:"window_minutes"`
}

// codexWindowKind は窓の長さから Camp の窓の名前を決める。
// **長さが分かるので推測が要らない**（statusLine は名前から長さを逆算していた）。
// 既存の語彙（five_hour / seven_day）に写すと、画面の表示と started_at の逆算がそのまま効く。
func codexWindowKind(minutes *int) string {
	if minutes == nil {
		return ""
	}
	switch *minutes {
	case 300:
		return "five_hour"
	case 10080:
		return "seven_day"
	}
	return fmt.Sprintf("minutes_%d", *minutes) // 知らない長さはそのまま名前にする（画面はそのまま出す）
}

// codexLimits は Codex の窓を limits が読む形へ直す。
// 値が欠けている窓（実データで primary/secondary が null の回がある）は入れない。
func codexLimits(ws ...*codexWindow) []byte {
	out := map[string]map[string]any{}
	for _, w := range ws {
		if w == nil || w.UsedPercent == nil || w.ResetsAt == nil {
			continue
		}
		kind := codexWindowKind(w.WindowMinutes)
		if kind == "" {
			continue
		}
		out[kind] = map[string]any{"used_percentage": *w.UsedPercent, "resets_at": *w.ResetsAt}
	}
	if len(out) == 0 {
		return nil
	}
	b, err := json.Marshal(map[string]any{"rate_limits": out})
	if err != nil {
		return nil
	}
	return b
}

// codexMode は `collaboration_mode` から名前だけ取り出す。
// 実データは `{"mode": "...", "settings": {...}}`。古い記録では素の文字列のこともある。
func codexMode(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var o struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(raw, &o); err == nil {
		return o.Mode
	}
	return ""
}

func degraded(l *Line, err error) *Line {
	l.Degraded, l.DegradedBy = true, err.Error()
	return l
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
