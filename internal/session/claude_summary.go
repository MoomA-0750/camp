package session

import (
	"encoding/json"
	"strings"
)

// Summary は stream-json の1行を一言に畳む。発言は文、道具は [名前]、結果は [結果]。
// **全文はここでは出さない**（会話そのものは取り込まれたあと「セッション」側で読む）。
func (claudeDriver) Summary(kind string, frame json.RawMessage) (string, bool) {
	if kind == "control_response" {
		return "", true // Camp が子へ投げた問い合わせ（残量など）の答え。会話ではない
	}
	var f struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(frame, &f) != nil || len(f.Message.Content) == 0 {
		return "", false
	}
	var s string
	if json.Unmarshal(f.Message.Content, &s) == nil {
		return cut(s, 300), false
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Name string `json:"name"`
	}
	if json.Unmarshal(f.Message.Content, &blocks) != nil {
		return "", false
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, cut(b.Text, 200))
		case "tool_use":
			parts = append(parts, "["+b.Name+"]")
		case "tool_result":
			parts = append(parts, "[結果]")
		default:
			parts = append(parts, "["+b.Type+"]")
		}
	}
	return cut(strings.Join(parts, " "), 300), false
}
