package session

import (
	"encoding/json"
	"strings"
)

// Summary は Codex（JSON-RPC）の1行を一言に畳む。途中経過は来ない（実行面が断っている）ので、
// 完成した item とターンの終わり・承認・エラーだけを文にする。
func (codexDriver) Summary(_ string, frame json.RawMessage) (string, bool) {
	var f struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(frame, &f) != nil {
		return "", false
	}
	if f.Method == "" {
		if f.Error != nil && f.Error.Message != "" {
			return cut("エラー: "+f.Error.Message, 300), false
		}
		return "", false
	}
	var p struct {
		Item *struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Status  string `json:"status"`
			Command string `json:"command"`
			Server  string `json:"server"`
			Tool    string `json:"tool"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Changes []struct {
				Path string `json:"path"`
			} `json:"changes"`
		} `json:"item"`
		Turn *struct {
			Status string `json:"status"`
		} `json:"turn"`
		Command string `json:"command"`
		Error   *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(f.Params, &p)
	switch f.Method {
	case "item/completed":
		it := p.Item
		if it == nil {
			return "", false
		}
		switch it.Type {
		case "agentMessage":
			return cut(it.Text, 300), false
		case "userMessage":
			texts := make([]string, 0, len(it.Content))
			for _, c := range it.Content {
				texts = append(texts, c.Text)
			}
			return cut(strings.Join(texts, " "), 300), false
		case "commandExecution":
			return cut("["+it.Status+"] "+it.Command, 300), false
		case "fileChange":
			paths := make([]string, 0, len(it.Changes))
			for _, c := range it.Changes {
				paths = append(paths, c.Path)
			}
			return "[" + it.Status + "] " + cut(strings.Join(paths, ", "), 300), false
		case "mcpToolCall":
			return "[MCP " + it.Server + "/" + it.Tool + "] " + it.Status, false
		}
		return "[" + it.Type + "]", false
	case "turn/completed":
		status := "?"
		if p.Turn != nil && p.Turn.Status != "" {
			status = p.Turn.Status
		}
		return "ターンが終わった（" + status + "）", false
	case "item/commandExecution/requestApproval":
		return cut("承認を求めている: "+p.Command, 300), false
	case "item/fileChange/requestApproval":
		return "承認を求めている: ファイル変更", false
	case "error":
		msg := ""
		if p.Error != nil {
			msg = p.Error.Message
		}
		return cut("エラー: "+msg, 300), false
	}
	return "", false
}
