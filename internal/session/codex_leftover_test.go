package session

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// Phase 3.6 の outer gate で codex が指摘したことを縛る（指摘 1・2）。

// readMsg は実行面が送った1通を読む。
func readMsg(t *testing.T, c net.Conn) Msg {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		t.Fatalf("実行面から何も来ない: %v", err)
	}
	var m Msg
	codexNoErr(t, json.Unmarshal(line, &m))
	return m
}

// **子が終わっても scope に残りが居るなら、「止めた」「自分で終わった」と書かない。**
// Claude と Codex の両方で。scope は偽物（systemd には触らせない）。
func TestExitsWithLeftoversAreRecordedAsGivenUp(t *testing.T) {
	for _, agent := range []string{AgentClaude, AgentCodex} {
		t.Run(agent, func(t *testing.T) {
			db := newDB(t)
			s := New(db)
			opt, _ := withFakeCodex(t)
			claude := fakeClaude(t)
			attach(t, s, claude, opt, func(a *Agent) {
				a.Scope = true // scope 名を付けさせる
				// scope で包まずに起こす（systemd には触らない）。
				a.Command = func(_, _ string, argv []string) *exec.Cmd {
					c := exec.Command(argv[0], argv[1:]...)
					c.Env = append(os.Environ(), "CODEX_HOME="+a.CodexHome)
					return c
				}
				a.ScopeStop = func(string) error { return nil }
				// 止めても居なくならない残り（居ない pid を返し続ける）。
				a.ScopeProcs = func(string) ([]int, error) { return []int{1 << 30}, nil }
			})
			rec, err := s.StartAgent("test", "", allowHere(t, db), agent)
			codexNoErr(t, err)
			waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
			codexNoErr(t, s.Stop(rec.ID, StopTerminate))
			waitFor(t, 15*time.Second, func() bool { return state(t, db, rec.ID) == StateExited })
			r, _ := get(db, rec.ID)
			if r.EndCause != EndStopTimeout || !strings.Contains(r.ExitReason, "止められなかった") {
				t.Fatalf("残りがあるのに %s / %s と書いた", r.EndCause, r.ExitReason)
			}
		})
	}
}

// 孤児を始末したと言われても、残りがあるなら「始末した」と書かない。
func TestAReapWithLeftoversIsNotRecordedAsReaped(t *testing.T) {
	db := newDB(t)
	s := New(db)
	dead := exec.Command("true")
	codexNoErr(t, dead.Run())
	mustInsert(t, db, "rp", StateOrphaned, dead.Process.Pid, 1, BootID())
	s.dispatchForTest(Msg{T: MsgReaped, Session: "rp", Reason: "もう居なかった" + leftoverNote(1),
		Leftover: 1})
	if r, _ := get(db, "rp"); r.State != StateExited || r.EndCause != EndStopTimeout {
		t.Fatalf("残りがあるのに %s / %s と書いた", r.State, r.EndCause)
	}
}

// **孤児の始末で触る scope は、そのセッションのものだけ。** 台帳の値でも、名前を照らす。
func TestTheReaperOnlyTouchesItsOwnScope(t *testing.T) {
	a := NewAgent("x", "y")
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	a.conn = c1
	var mu sync.Mutex
	var asked []string
	a.ScopeStop = func(string) error { return nil }
	a.ScopeProcs = func(s string) ([]int, error) {
		mu.Lock()
		asked = append(asked, s)
		mu.Unlock()
		return nil, nil
	}
	dead := exec.Command("true")
	codexNoErr(t, dead.Run())
	for _, sc := range []string{"user-other.scope", scopeName("sess1")} {
		go a.reap(Msg{T: MsgReap, Session: "sess1", PID: dead.Process.Pid, Started: 1,
			BootID: BootID(), Scope: sc})
		readMsg(t, c2)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0] != scopeName("sess1") {
		t.Fatalf("別の unit の scope を見に行った: %v", asked)
	}
}

type failingStdin struct{}

func (failingStdin) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (failingStdin) Close() error              { return nil }

// **子へ書けなかった答えを、届いたことにしない**（指摘 2）。取り下げとして返す。
func TestAnAnswerThatCannotBeWrittenIsReportedAsWithdrawn(t *testing.T) {
	a := NewAgent("x", "y")
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	a.conn = c1
	cs := newCodexState()
	cs.asks["0"] = HeldAsk{ReqID: "0", Tool: "codex:command"}
	a.kids["sessabcdef"] = &child{id: "sessabcdef", token: "t", stdin: failingStdin{},
		conv: &codexConv{cs: cs}, name: AgentCodex, pending: map[string]chan []byte{}}
	go a.toChild(Msg{T: MsgApprove, Session: "sessabcdef", Token: "t", ReqID: "0",
		Behavior: "allow"})
	m := readMsg(t, c2)
	if !m.Withdrawn || m.ReqID != "0" || !strings.Contains(m.Error, "書けなかった") {
		t.Fatalf("書けなかった答えを取り下げとして返していない: %+v", m)
	}
}
