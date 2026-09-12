package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// **3つ目のエージェント**（テストの中だけ。M40 の inner gate）。
//
// 登録はこのファイルの init の1行だけで、campd（supervisor・control）・実行面の本流・API には
// 手を入れない。起こす・話す・承認・残量・流れの一言・引き取り直しの名乗り・中断・止めるが、
// それで通ることを縛る——**エージェント名の決め打ちが残っていないことの証拠**（D-031）。
//
// 子は sh で書いた偽物。1行1JSON:
//
//	子 → {"ev":"hello","sid":…} / {"ev":"done"} / {"ev":"done","interrupted":true}
//	     {"ev":"ask","id":…,"tool":…,"what":…} / {"ev":"reply","id":…,"data":{…}}
//	子 ← {"say":…} / {"answer":…,"ok":…} / {"interrupt":true} / {"query":…,"kind":…}

const agentFake3 = "fake3"

// fake3Bin は偽物の実体。空なら起こせない（hello で名乗らない）ので、ほかのテストには出ない。
var fake3Bin atomic.Value

func init() { drivers[agentFake3] = fake3Driver{} }

type fake3Driver struct{}

func (fake3Driver) Info() AgentInfo {
	// cli のほかに、まだ campd の知らない度合いも名乗る（名乗らない実行面・知らない度合いを
	// campd が頼まないことを縛るため。driver_test.go）。
	return AgentInfo{Name: agentFake3, Label: "Fake Three", Perms: []string{PermCLI, "ask"},
		Notes: []string{"テストの中だけの駆動器"}}
}

func (fake3Driver) Launch(*Agent) (Launch, error) {
	b, _ := fake3Bin.Load().(string)
	if b == "" {
		return Launch{}, errors.New("fake3 の実体が無い")
	}
	return Launch{Bin: b}, nil
}

// **続きからは名乗らない**（Info の Resume が false）。名乗らない駆動器へ campd が再開を
// 頼まないことを、これで縛れる（resume_test.go）。
func (fake3Driver) Argv(perm, _ string) ([]string, error) {
	if perm != PermCLI {
		return nil, fmt.Errorf("fake3 は確認の度合い %s を扱わない", perm)
	}
	return []string{"--fake"}, nil
}

func (fake3Driver) Open(OpenOpts) Conversation { return &fake3Conv{asks: map[string]HeldAsk{}} }

func (fake3Driver) Usage(usage, _ json.RawMessage) UsageView {
	var u struct {
		Left float64 `json:"left"`
	}
	json.Unmarshal(usage, &u)
	return UsageView{Windows: []UsageWindow{{Label: "フェイク枠", Percent: u.Left}}}
}

func (fake3Driver) RemoteLaunch() RemoteLaunch { return RemoteLaunch{Name: "fake3"} }

func (fake3Driver) Summary(kind string, _ json.RawMessage) (string, bool) {
	return "fake3:" + kind, kind == "reply"
}

type fake3Conv struct {
	mu   sync.Mutex
	sid  string
	asks map[string]HeldAsk
	n    int
}

func (c *fake3Conv) Begin(BeginOpts) error { return nil }

func (c *fake3Conv) Fold(line []byte) Event {
	var f struct {
		Ev, Sid, ID, Tool, What string
		Interrupted             bool
		Data                    json.RawMessage
	}
	if json.Unmarshal(line, &f) != nil {
		return Event{Drop: true}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ev := Event{Kind: f.Ev}
	switch f.Ev {
	case "hello":
		c.sid, ev.SessionID = f.Sid, f.Sid
	case "done":
		ev.TurnEnd, ev.Interrupted = true, f.Interrupted
	case "ask":
		d, _ := json.Marshal(map[string]any{"view": AskView{What: "command", Command: f.What}})
		a := HeldAsk{ReqID: f.ID, Tool: f.Tool, Detail: d}
		c.asks[f.ID] = a
		ev.Ask = &a
	case "reply":
		ev.Deliver, ev.Payload = f.ID, f.Data
	}
	return ev
}

func (c *fake3Conv) Input(text string) ([]byte, error) {
	return json.Marshal(map[string]string{"say": text})
}

func (c *fake3Conv) Answer(reqID, behavior, _ string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.asks[reqID]; !ok {
		return nil, fmt.Errorf("その承認は待っていない: %s", reqID)
	}
	delete(c.asks, reqID)
	return json.Marshal(map[string]any{"answer": reqID, "ok": behavior == "allow"})
}

func (c *fake3Conv) Interrupt() []byte {
	b, _ := json.Marshal(map[string]bool{"interrupt": true})
	return b
}

func (c *fake3Conv) Query(kind string) ([]byte, string, json.RawMessage, error) {
	c.mu.Lock()
	c.n++
	key := fmt.Sprintf("q%d", c.n)
	c.mu.Unlock()
	b, _ := json.Marshal(map[string]string{"query": key, "kind": kind})
	return b, key, nil, nil
}

func (c *fake3Conv) Waiting() []HeldAsk {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]HeldAsk, 0, len(c.asks))
	for _, a := range c.asks {
		out = append(out, a)
	}
	return out
}

func (c *fake3Conv) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sid
}

// writeFake3 は偽物の子を置く。
func writeFake3(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake3")
	body := `#!/bin/sh
echo '{"ev":"hello","sid":"f3-1"}'
while IFS= read -r line; do
  case "$line" in
    *'"interrupt"'*) echo '{"ev":"done","interrupted":true}' ;;
    *'"query"'*)
      key=$(printf '%s' "$line" | sed 's/.*"query":"\([^"]*\)".*/\1/')
      printf '{"ev":"reply","id":"%s","data":{"left":42}}\n' "$key" ;;
    *'"answer"'*) echo '{"ev":"done"}' ;;
    *'"say":"ask'*) echo '{"ev":"ask","id":"q-1","tool":"Touch","what":"touch x"}' ;;
    *'"say":"hang'*) : ;;
    *'"say"'*) echo '{"ev":"done"}' ;;
  esac
done
`
	codexNoErr(t, os.WriteFile(p, []byte(body), 0o755))
	return p
}

func TestAThirdAgentWorksWithoutTouchingCampd(t *testing.T) {
	fake3Bin.Store(writeFake3(t))
	t.Cleanup(func() { fake3Bin.Store("") })
	db := newDB(t)
	s := New(db)
	ag := attach(t, s, fakeClaude(t))

	// 実行面が名乗り、campd が画面へ出す。
	var info *AgentInfo
	for _, in := range s.Agents() {
		if in.Name == agentFake3 {
			info = &in
		}
	}
	if info == nil || info.Label != "Fake Three" || len(info.Notes) != 1 {
		t.Fatalf("3つ目のエージェントが名乗られていない: %+v", s.Agents())
	}

	// 起こす。
	rec, err := s.StartAgent("test", "", allowHere(t, db), agentFake3)
	codexNoErr(t, err)
	waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	waitFor(t, 5*time.Second, func() bool { r, _ := get(db, rec.ID); return r.AgentSessionID == "f3-1" })
	if r, _ := get(db, rec.ID); r.Agent != agentFake3 || r.AgentLabel != "Fake Three" {
		t.Fatalf("台帳の行が違う: %+v", r)
	}

	// 話す。
	codexNoErr(t, s.Input(rec.ID, "hello"))
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })

	// 承認。**引き取り直しで名乗る**（実行面が抱えている承認）。
	codexNoErr(t, s.Input(rec.ID, "ask"))
	waitFor(t, 5*time.Second, func() bool { return len(pending(t, s, rec.ID)) == 1 })
	w, _ := s.Waiting(rec.ID)
	if w[0].Tool != "Touch" || !strings.Contains(w[0].Detail, "touch x") {
		t.Fatalf("承認の中身が違う: %+v", w[0])
	}
	named := false
	for _, h := range ag.held() {
		named = named || (h.ID == rec.ID && h.Agent == agentFake3 && len(h.Waiting) == 1)
	}
	if !named {
		t.Fatalf("抱えている子と承認を名乗れない: %+v", ag.held())
	}
	codexNoErr(t, s.Approve(rec.ID, w[0].RequestID, "allow", ""))
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })

	// 残量（共通の形に直るところまで）。
	b, err := s.Control(rec.ID, "get_usage")
	codexNoErr(t, err)
	if v := UsageViewOf(agentFake3, b, nil); len(v.Windows) != 1 || v.Windows[0].Percent != 42 {
		t.Fatalf("残量が共通の形に直らない: %s → %+v", b, v)
	}

	// 流れの一言（駆動器が畳む）。Camp 自身の問い合わせには印。
	tr, err := s.Tail(rec.ID, 0, 100)
	codexNoErr(t, err)
	summarized, own := false, false
	for _, ln := range tr.Lines {
		summarized = summarized || ln.Summary == "fake3:ask"
		own = own || (ln.Kind == "reply" && ln.Own)
	}
	if !summarized || !own {
		t.Fatalf("流れの一言が駆動器から来ていない: %+v", tr.Lines)
	}

	// 中断（このエージェントは工具を残さないので、続けて止めはしない）と、止める。
	codexNoErr(t, s.Input(rec.ID, "hang"))
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateRunning })
	codexNoErr(t, s.Stop(rec.ID, StopInterrupt))
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	codexNoErr(t, s.Stop(rec.ID, StopTerminate))
	waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateExited })
}
