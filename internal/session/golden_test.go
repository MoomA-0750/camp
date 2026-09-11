package session

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// **振る舞いを変えない作り替えの証拠**（Phase 3.7 の M40。Fable の設計レビュー 7）。
//
// 偽の claude・偽の app-server で決まった手順を踏み、campd に届いた Msg の列を
// testdata/golden/ に採っておく。駆動器の形へ作り替えたあとも、同じ列が届くことを比べる。
//
//	go test ./internal/session/ -run TestMsgSequencesStayTheSame -update-golden   # 採り直す
var updateGolden = flag.Bool("update-golden", false, "golden の Msg 列を採り直す")

// goldenMsg は比べる欄だけにした Msg。**毎回変わるもの（pid・起動時刻・合鍵・セッション id・
// 時刻）は落とす。**
type goldenMsg struct {
	T           string `json:"t"`
	Kind        string `json:"kind,omitempty"`
	Agent       string `json:"agent,omitempty"`
	AgentID     string `json:"agent_session_id,omitempty"`
	TurnEnd     bool   `json:"turn_end,omitempty"`
	Ask         bool   `json:"ask,omitempty"`
	ReqID       string `json:"request_id,omitempty"`
	Tool        string `json:"tool,omitempty"`
	Interrupted bool   `json:"interrupted,omitempty"`
	Withdrawn   bool   `json:"withdrawn,omitempty"`
	Note        string `json:"note,omitempty"`
	Error       string `json:"error,omitempty"`
	Code        int    `json:"code,omitempty"`
	Leftover    int    `json:"leftover,omitempty"`
}

func toGolden(m Msg) (goldenMsg, bool) {
	switch m.T {
	case MsgPing, MsgTailRes, MsgCtlRes, MsgSSHRes, MsgSSHResolved, MsgHello, MsgDropped:
		return goldenMsg{}, false
	}
	return goldenMsg{T: m.T, Kind: m.Kind, Agent: m.Agent, AgentID: m.ClaudeID, TurnEnd: m.TurnEnd,
		Ask: m.Ask, ReqID: m.ReqID, Tool: m.Text, Interrupted: m.Interrupted,
		Withdrawn: m.Withdrawn, Note: m.Note, Error: m.Error, Code: m.Code, Leftover: m.Leftover}, true
}

type goldenScenario struct {
	name  string
	agent string
	// steps は起こしたあとに踏む手順。"say:<文>" は入力して待つ、"allow"・"deny" は最初の承認に
	// 答えて待つ、"interrupt" は中断して待つ。
	steps []string
}

var goldenScenarios = []goldenScenario{
	{"claude-talk", AgentClaude, []string{"say:hello", "say:again"}},
	{"codex-talk", AgentCodex, []string{"say:hello"}},
	{"codex-allow", AgentCodex, []string{"say:ask", "allow"}},
	{"codex-deny-queue", AgentCodex, []string{"say:two", "allow", "deny"}},
	{"codex-file", AgentCodex, []string{"say:patch", "allow"}},
	{"codex-refuse", AgentCodex, []string{"say:perm"}},
	{"codex-interrupt", AgentCodex, []string{"say!:hang", "interrupt"}},
	{"codex-withdrawn", AgentCodex, []string{"say:moved"}},
}

func TestMsgSequencesStayTheSame(t *testing.T) {
	for _, sc := range goldenScenarios {
		t.Run(sc.name, func(t *testing.T) {
			got := runGolden(t, sc)
			path := filepath.Join("testdata", "golden", sc.name+".json")
			b, _ := json.MarshalIndent(got, "", "  ")
			if *updateGolden {
				codexNoErr(t, os.MkdirAll(filepath.Dir(path), 0o755))
				codexNoErr(t, os.WriteFile(path, append(b, '\n'), 0o644))
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("golden が無い（-update-golden で採る）: %v", err)
			}
			if strings.TrimSpace(string(want)) != strings.TrimSpace(string(b)) {
				t.Fatalf("campd に届く Msg の列が変わった。\n--- 前:\n%s\n--- 後:\n%s", want, b)
			}
		})
	}
}

func runGolden(t *testing.T, sc goldenScenario) []goldenMsg {
	t.Helper()
	var mu sync.Mutex
	var seen []goldenMsg
	var sessionID string
	dispatchHook = func(m Msg) {
		mu.Lock()
		defer mu.Unlock()
		if sessionID != "" && m.Session != sessionID {
			return
		}
		if g, ok := toGolden(m); ok {
			seen = append(seen, g)
		}
	}
	defer func() { dispatchHook = nil }()

	db := newDB(t)
	var s *Supervisor
	if sc.agent == AgentCodex {
		s, _, _ = wireCodex(t, db)
	} else {
		s, _ = wire(t, db)
	}
	rec, err := s.StartAgent("test", "", allowHere(t, db), sc.agent)
	codexNoErr(t, err)
	mu.Lock()
	sessionID = rec.ID
	mu.Unlock()
	idle := func() {
		waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	}
	idle()
	for _, step := range sc.steps {
		switch {
		case strings.HasPrefix(step, "say!:"):
			// 言うだけで待たない（終わらないターンを中断する手順）。
			codexNoErr(t, s.Input(rec.ID, strings.TrimPrefix(step, "say!:")))
		case strings.HasPrefix(step, "say:"):
			codexNoErr(t, s.Input(rec.ID, strings.TrimPrefix(step, "say:")))
			// 承認を待つ手順なら、承認が来るまで。そうでなければ idle まで。
			waitFor(t, 10*time.Second, func() bool {
				return state(t, db, rec.ID) == StateIdle || len(pending(t, s, rec.ID)) > 0 ||
					historyReason(t, db, rec.ID, "0") == ByWithdrawn
			})
		case step == "allow" || step == "deny":
			var ids []string
			waitFor(t, 10*time.Second, func() bool { ids = pending(t, s, rec.ID); return len(ids) > 0 })
			codexNoErr(t, s.Approve(rec.ID, ids[0], step, ""))
			time.Sleep(100 * time.Millisecond)
		case step == "interrupt":
			codexNoErr(t, s.Stop(rec.ID, StopInterrupt))
			idle()
		}
	}
	idle()
	// 止める前までの列を比べる（止めたあとの exited と、最後のフレームの前後は並びが揺れる）。
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	out := append([]goldenMsg(nil), seen...)
	mu.Unlock()
	codexNoErr(t, s.Stop(rec.ID, StopTerminate))
	waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateExited })
	return out
}
