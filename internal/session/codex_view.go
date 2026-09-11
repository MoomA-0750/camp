package session

import (
	"encoding/json"
	"errors"
)

// Codex の承認を共通の形に直す（askDetail から呼ぶ）。

// codexChanges は item/started の changes（実測: [{path, kind:{type}, diff}]）を共通の形に直す。
// **読めない・欠けているものは断る**——見せられない中身で許させない。
func codexChanges(raw json.RawMessage) ([]AskChange, error) {
	var in []struct {
		Path string `json:"path"`
		Kind struct {
			Type     string `json:"type"`
			MovePath string `json:"move_path"`
		} `json:"kind"`
		Diff *string `json:"diff"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, errors.New("差分の形が違う")
	}
	if len(in) == 0 {
		return nil, errors.New("差分が空")
	}
	out := make([]AskChange, 0, len(in))
	for _, c := range in {
		if c.Path == "" || c.Diff == nil {
			return nil, errors.New("path か diff の無い変更がある")
		}
		kind := c.Kind.Type
		if c.Kind.MovePath != "" {
			kind += " → " + c.Kind.MovePath
		}
		out = append(out, AskChange{Path: c.Path, Kind: kind, Patch: *c.Diff})
	}
	return out, nil
}

// codexReason は承認の要求に Codex が添えた理由。
func codexReason(params json.RawMessage) string {
	var p struct {
		Reason *string `json:"reason"`
	}
	json.Unmarshal(params, &p)
	if p.Reason != nil {
		return *p.Reason
	}
	return ""
}
