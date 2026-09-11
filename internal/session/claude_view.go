package session

import "encoding/json"

// Claude の承認（can_use_tool）を共通の形に直す。Edit・Write は差分に直す（Fable の設計レビュー）。

// claudeAskView は道具の名前と入力から、承認の中身の共通の形を作る。**読めないものは
// 道具の入力そのまま**（tool）として見せる。
func claudeAskView(tool string, input json.RawMessage) AskView {
	var in struct {
		Command     string  `json:"command"`
		Description string  `json:"description"`
		FilePath    string  `json:"file_path"`
		Content     *string `json:"content"`
		OldString   string  `json:"old_string"`
		NewString   string  `json:"new_string"`
		Edits       []struct {
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		} `json:"edits"`
	}
	json.Unmarshal(input, &in)
	switch tool {
	case "Bash":
		if in.Command != "" {
			return AskView{What: "command", Tool: tool, Command: in.Command, Reason: in.Description}
		}
	case "Write":
		if in.FilePath != "" && in.Content != nil {
			return AskView{What: "file", Tool: tool, Changes: []AskChange{{Path: in.FilePath,
				Kind: "write", Patch: linesWith("+", *in.Content)}}}
		}
	case "Edit":
		if in.FilePath != "" {
			return AskView{What: "file", Tool: tool, Changes: []AskChange{{Path: in.FilePath,
				Kind: "edit", Patch: linesWith("-", in.OldString) + linesWith("+", in.NewString)}}}
		}
	case "MultiEdit":
		if in.FilePath != "" && len(in.Edits) > 0 {
			patch := ""
			for _, e := range in.Edits {
				patch += linesWith("-", e.OldString) + linesWith("+", e.NewString)
			}
			return AskView{What: "file", Tool: tool, Changes: []AskChange{{Path: in.FilePath,
				Kind: "edit", Patch: patch}}}
		}
	}
	return AskView{What: "tool", Tool: tool, Input: input}
}

// claudeAskDetail は承認カードの中身: 元の要求に共通の形（view）を添えたもの。**大きすぎれば
// 共通の形を諦め、それでも大きすぎれば中身を渡さない**（Phase 3.6 までと同じ）。
func claudeAskDetail(tool string, req map[string]any) []byte {
	in, _ := json.Marshal(req["input"])
	with := make(map[string]any, len(req)+1)
	for k, v := range req {
		with[k] = v
	}
	with["view"] = claudeAskView(tool, in)
	if b, err := json.Marshal(with); err == nil && len(b) <= maxApprovalDetail {
		return b
	}
	if b, err := json.Marshal(req); err == nil && len(b) <= maxApprovalDetail {
		return b
	}
	return nil
}
