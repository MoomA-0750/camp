package session

import (
	"net"
	"strings"
	"testing"
)

// Claude の会話（M40 の (2)）。畳み方は Phase 3.6 までの drain と同じで、承認は見た id にだけ答える。

func openClaude(t *testing.T) *claudeConv {
	t.Helper()
	return claudeDriver{}.Open(OpenOpts{Session: "sessabcdef"}).(*claudeConv)
}

// **見た id にだけ答える。** 二度は答えない。断る理由は空にしない。
func TestClaudeAnswersOnlyApprovalsItSaw(t *testing.T) {
	c := openClaude(t)
	if _, err := c.Answer("req-1", "allow", ""); err == nil {
		t.Fatal("見ていない承認に答えた")
	}
	ev := c.Fold([]byte(`{"type":"control_request","request_id":"req-1","request":` +
		`{"subtype":"can_use_tool","tool_name":"Write","input":{"file_path":"x"}}}`))
	if ev.Ask == nil || ev.Ask.ReqID != "req-1" || ev.Ask.Tool != "Write" ||
		!strings.Contains(string(ev.Ask.Detail), "file_path") {
		t.Fatalf("承認を畳めていない: %+v", ev.Ask)
	}
	if w := c.Waiting(); len(w) != 1 || w[0].ReqID != "req-1" {
		t.Fatalf("待っている承認を名乗れない: %+v", w)
	}
	b, err := c.Answer("req-1", "deny", "")
	if err != nil || !strings.Contains(string(b), "本人が拒否した") {
		t.Fatalf("断りの理由が空のまま: %s %v", b, err)
	}
	if _, err := c.Answer("req-1", "allow", ""); err == nil {
		t.Fatal("同じ承認に二度答えた")
	}
	if len(c.Waiting()) != 0 {
		t.Fatal("答えた承認がまだ待っている")
	}
}

// 畳み方は今までの drain と同じ。読めない行は捨てる、session_id はその行にあるときだけ、
// 制御応答は待っている者へ。
func TestClaudeFoldKeepsTheOldShape(t *testing.T) {
	c := openClaude(t)
	if !c.Fold([]byte(`not json`)).Drop {
		t.Fatal("読めない行を通した")
	}
	ev := c.Fold([]byte(`{"type":"result","subtype":"success","session_id":"abc"}`))
	if !ev.TurnEnd || ev.Kind != "result" || ev.SessionID != "abc" || c.SessionID() != "abc" {
		t.Fatalf("result を畳めていない: %+v", ev)
	}
	ev = c.Fold([]byte(`{"type":"assistant","message":{}}`))
	if ev.SessionID != "" || ev.Ask != nil || ev.TurnEnd || ev.Deliver != "" {
		t.Fatalf("ただの発言に意味を足した: %+v", ev)
	}
	ev = c.Fold([]byte(`{"type":"control_response","response":{"subtype":"success",` +
		`"request_id":"camp-ctl-1","response":{"x":1}}}`))
	if ev.Deliver != "camp-ctl-1" || string(ev.Payload) != `{"x":1}` {
		t.Fatalf("制御応答を待っている者へ回せない: %+v", ev)
	}
	req, key, now, err := c.Query("get_usage")
	if err != nil || now != nil || !strings.HasPrefix(key, "camp-ctl-") ||
		!strings.Contains(string(req), key) || !strings.Contains(string(req), `"get_usage"`) {
		t.Fatalf("問い合わせを組めていない: %s %s %v", req, key, err)
	}
	if b := c.Interrupt(); !strings.Contains(string(b), "camp-stop-sessabcdef") ||
		!strings.Contains(string(b), `"interrupt"`) {
		t.Fatalf("中断を組めていない: %s", b)
	}
}

// **見ていない承認への答えを、届いたことにしない。** 取り下げとして返す。
func TestAClaudeAnswerToAnUnseenApprovalIsWithdrawn(t *testing.T) {
	a := NewAgent("x", "y")
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	a.conn = c1
	a.kids["sessabcdef"] = &child{id: "sessabcdef", token: "t", stdin: failingStdin{},
		name: AgentClaude, conv: openClaude(t), pending: map[string]chan []byte{}}
	go a.toChild(Msg{T: MsgApprove, Session: "sessabcdef", Token: "t", ReqID: "nope",
		Behavior: "allow"})
	m := readMsg(t, c2)
	if !m.Withdrawn || m.ReqID != "nope" || !strings.Contains(m.Error, "待っていない") {
		t.Fatalf("見ていない承認への答えを取り下げとして返していない: %+v", m)
	}
}
