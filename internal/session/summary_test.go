package session

import (
	"encoding/json"
	"testing"
)

// 流れの1行の一言（M40。以前は画面が Claude と Codex を読み分けていた。RuntimeDetail.test.tsx から
// 移した）。

func TestCodexLinesAreSummarizedByItsDriver(t *testing.T) {
	d := codexDriver{}
	for _, c := range []struct{ frame, want string }{
		{`{"method":"item/completed","params":{"item":{"type":"agentMessage","text":"fake-ok"}}}`, "fake-ok"},
		{`{"method":"item/completed","params":{"item":{"type":"userMessage","content":[{"type":"text","text":"hello"}]}}}`, "hello"},
		{`{"method":"item/completed","params":{"item":{"type":"commandExecution","status":"declined","command":"zsh -lc 'rm x'"}}}`,
			"[declined] zsh -lc 'rm x'"},
		{`{"method":"turn/completed","params":{"turn":{"status":"interrupted"}}}`, "ターンが終わった（interrupted）"},
		{`{"method":"item/completed","params":{"item":{"type":"mcpToolCall","server":"cua_repl","tool":"js","status":"completed"}}}`,
			"[MCP cua_repl/js] completed"},
		{`{"id":"camp-3","error":{"message":"枠切れ"}}`, "エラー: 枠切れ"},
		{`{"id":0,"method":"item/commandExecution/requestApproval","params":{"command":"touch a"}}`, "承認を求めている: touch a"},
	} {
		if got, own := d.Summary("", json.RawMessage(c.frame)); got != c.want || own {
			t.Errorf("%s → %q（own=%v）。%q のはず", c.frame, got, own, c.want)
		}
	}
}

func TestClaudeLinesAreSummarizedByItsDriver(t *testing.T) {
	d := claudeDriver{}
	got, _ := d.Summary("assistant", json.RawMessage(`{"type":"assistant","message":{"content":[`+
		`{"type":"text","text":"見る"},{"type":"tool_use","name":"Read"},{"type":"thinking"}]}}`))
	if got != "見る [Read] [thinking]" {
		t.Fatalf("発言と道具を畳めていない: %q", got)
	}
	if got, _ := d.Summary("user", json.RawMessage(`{"type":"user","message":{"content":"こんにちは"}}`)); got != "こんにちは" {
		t.Fatalf("文字列の発言を畳めていない: %q", got)
	}
	// **Camp 自身の問い合わせは会話ではない。** 画面が畳めるよう、そう印を付ける。
	if _, own := d.Summary("control_response", json.RawMessage(`{"type":"control_response"}`)); !own {
		t.Fatal("Camp 自身の問い合わせの答えに印が無い")
	}
}
