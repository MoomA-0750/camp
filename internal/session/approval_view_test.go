package session

import (
	"encoding/json"
	"strings"
	"testing"
)

// 承認の中身の共通の形（M40）。**画面はこの形だけを見る**ので、どちらのエージェントの承認も
// 何を・どこで・どう変えるかが同じ欄に収まることを縛る。

func viewOf(t *testing.T, detail []byte) AskView {
	t.Helper()
	var d struct {
		View *AskView `json:"view"`
	}
	if err := json.Unmarshal(detail, &d); err != nil || d.View == nil {
		t.Fatalf("中身に共通の形が無い: %s", detail)
	}
	return *d.View
}

func TestClaudeApprovalsGetTheCommonShape(t *testing.T) {
	for _, c := range []struct {
		tool, input, what, patch string
	}{
		{"Write", `{"file_path":"/w/a.txt","content":"one\ntwo\n"}`, "file", "+one\n+two\n"},
		{"Edit", `{"file_path":"/w/a.txt","old_string":"a","new_string":"b"}`, "file", "-a\n+b\n"},
		{"MultiEdit", `{"file_path":"/w/a.txt","edits":[{"old_string":"a","new_string":"b"},` +
			`{"old_string":"c","new_string":"d"}]}`, "file", "-a\n+b\n-c\n+d\n"},
	} {
		v := claudeAskView(c.tool, json.RawMessage(c.input))
		if v.What != c.what || len(v.Changes) != 1 || v.Changes[0].Path != "/w/a.txt" ||
			v.Changes[0].Patch != c.patch {
			t.Errorf("%s を差分に直せていない: %+v", c.tool, v)
		}
	}
	if v := claudeAskView("Bash", json.RawMessage(`{"command":"touch x","description":"作る"}`)); v.What != "command" ||
		v.Command != "touch x" || v.Reason != "作る" {
		t.Errorf("コマンドを直せていない: %+v", v)
	}
	// 知らない道具は、入力そのままを見せる。
	if v := claudeAskView("WebFetch", json.RawMessage(`{"url":"https://example.com"}`)); v.What != "tool" ||
		v.Tool != "WebFetch" || !strings.Contains(string(v.Input), "example.com") {
		t.Errorf("知らない道具の入力が見えない: %+v", v)
	}

	// 中身の欄に入り、元の要求も残る。
	req := map[string]any{"subtype": "can_use_tool", "tool_name": "Bash",
		"input": map[string]any{"command": "ls"}}
	if b := claudeAskDetail("Bash", req); viewOf(t, b).Command != "ls" || !strings.Contains(string(b), `"tool_name"`) {
		t.Fatalf("元の要求と共通の形が並んでいない: %s", b)
	}
	// **大きすぎれば共通の形を諦め、それでも大きすぎれば中身を渡さない。**
	half := map[string]any{"tool_name": "Write", "input": map[string]any{"file_path": "/w/a",
		"content": strings.Repeat("x", maxApprovalDetail/2+64)}}
	if b := claudeAskDetail("Write", half); b == nil || strings.Contains(string(b), `"view"`) {
		t.Fatal("共通の形を諦めて元の要求だけ渡す、になっていない")
	}
	huge := map[string]any{"tool_name": "Write", "input": map[string]any{"file_path": "/w/a",
		"content": strings.Repeat("x", maxApprovalDetail+64)}}
	if b := claudeAskDetail("Write", huge); b != nil {
		t.Fatal("画面へ渡せる大きさを越えた中身を渡した")
	}
}

func TestCodexApprovalsGetTheCommonShape(t *testing.T) {
	cs := newCodexState()
	cs.root = "/w"
	ev := cs.classify([]byte(`{"id":0,"method":"item/commandExecution/requestApproval",` +
		`"params":{"command":"ls","cwd":"/etc","reason":"見たい"}}`))
	if ev.ask == nil {
		t.Fatalf("承認を出していない: %s", ev.err)
	}
	if v := viewOf(t, ev.ask.Detail); v.What != "command" || v.Command != "ls" || v.Cwd != "/etc" ||
		!v.Outside || v.Reason != "見たい" {
		t.Fatalf("コマンドの承認が共通の形に収まっていない: %+v", v)
	}

	cs.classify([]byte(`{"method":"item/started","params":{"item":{"type":"fileChange","id":"i1",` +
		`"changes":[{"path":"/w/a","kind":{"type":"update","move_path":"/w/b"},"diff":"-a\n+b\n"}]}}}`))
	ev = cs.classify([]byte(`{"id":1,"method":"item/fileChange/requestApproval","params":{"itemId":"i1"}}`))
	if ev.ask == nil {
		t.Fatalf("ファイル変更の承認を出していない: %s", ev.err)
	}
	if v := viewOf(t, ev.ask.Detail); v.What != "file" || len(v.Changes) != 1 || v.Changes[0].Path != "/w/a" ||
		v.Changes[0].Kind != "update → /w/b" || v.Changes[0].Patch != "-a\n+b\n" {
		t.Fatalf("ファイル変更の承認が共通の形に収まっていない: %+v", v)
	}

	// **差分の欠けた変更は、見せずに断る。**
	cs.classify([]byte(`{"method":"item/started","params":{"item":{"type":"fileChange","id":"i2",` +
		`"changes":[{"path":"/w/c"}]}}}`))
	ev = cs.classify([]byte(`{"id":2,"method":"item/fileChange/requestApproval","params":{"itemId":"i2"}}`))
	if ev.ask != nil || !strings.Contains(joined(ev.replies), "decline") || !strings.Contains(ev.err, "読めない") {
		t.Fatalf("差分の欠けた変更を見せた: %+v", ev)
	}
}
