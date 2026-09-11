package session

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Phase 3.7 の M40 の (0): hello の駆動器の説明と、確認の度合い（perm）の互換。

// **知らない駆動器・知らない度合いは、名乗られても採らない。**
func TestHelloDriversAreTakenOnlyForKnownAgentsAndPerms(t *testing.T) {
	a := &agentConn{agents: []string{AgentClaude, AgentCodex}}
	a.infos = namedInfos(a, []AgentInfo{
		{Name: AgentCodex, Label: "Codex", Perms: []string{PermCLI, "yolo"}},
		{Name: "gemini", Label: "Gemini", Perms: []string{PermCLI}},
	})
	if _, ok := a.infos["gemini"]; ok {
		t.Fatal("知らない駆動器の説明を採った")
	}
	in, ok := a.info(AgentCodex)
	if !ok || len(in.Perms) != 1 || in.Perms[0] != PermCLI {
		t.Fatalf("知らない度合いまで採った: %+v", in)
	}
	if a.canPerm(AgentCodex, "yolo") {
		t.Fatal("知らない度合いを頼める")
	}
	// 起こせると言っていないものの説明は採らない。
	b := &agentConn{agents: []string{AgentClaude}}
	if got := namedInfos(b, []AgentInfo{{Name: AgentCodex, Perms: []string{PermCLI}}}); len(got) != 0 {
		t.Fatalf("起こせないものの説明を採った: %v", got)
	}
}

// **駆動器を名乗らない古い実行面へは、claude を cli でしか頼まない。ほかのエージェントは頼まない。**
//
// claude は古い実行面でも本人の設定のまま起きるので cli と言える。Phase 3.6 の実行面は codex を
// 名乗るが、Codex を専用の置き場で approvalPolicy untrusted 固定のまま起こす——それを「CLI と同じ」
// として台帳に書いてしまい、started の perm も空で返るので照らしても気づけない
// （`codex exec` のレビュー、2026-09-12）。
func TestAnOldAgentIsAskedOnlyCLIAndOnlyClaude(t *testing.T) {
	old := &agentConn{}
	if !old.canPerm(AgentClaude, PermCLI) {
		t.Fatal("古い実行面に claude を cli で頼めない")
	}
	if old.canPerm(AgentClaude, "ask") || old.canPerm(AgentCodex, PermCLI) {
		t.Fatal("古い実行面に名乗っていないものを頼める")
	}
	p36 := &agentConn{agents: []string{AgentClaude, AgentCodex}} // Phase 3.6 の実行面
	if _, ok := p36.info(AgentCodex); ok {
		t.Fatal("駆動器を名乗らない実行面に Codex を頼める（Phase 3.6 の起こし方を cli と書いてしまう）")
	}
	if p36.canPerm(AgentCodex, PermCLI) || p36.can(AgentCodex) {
		t.Fatal("駆動器を名乗らない実行面の Codex を起こせると答えている")
	}
	in, ok := p36.info(AgentClaude)
	if !ok || len(in.Perms) != 1 || in.Perms[0] != PermCLI || in.Label != "Claude Code" {
		t.Fatalf("古い実行面の claude を手元の駆動器から補えていない: %+v", in)
	}
	// 名乗らなければ、手元の駆動器が知っているエージェントでも頼まない（cli も）。
	quiet := &agentConn{agents: []string{AgentClaude, agentFake3}}
	if quiet.canPerm(agentFake3, "ask") || quiet.canPerm(agentFake3, PermCLI) {
		t.Fatal("名乗らない実行面に、手元の駆動器の度合いをそのまま頼める")
	}
	// 名乗れば、その説明どおりに頼める。
	named := &agentConn{agents: []string{AgentClaude, agentFake3}}
	named.infos = namedInfos(named, []AgentInfo{{Name: agentFake3, Label: "Fake Three",
		Perms: []string{PermCLI, "ask"}}})
	if !named.canPerm(agentFake3, "ask") || !named.canPerm(agentFake3, PermCLI) {
		t.Fatal("名乗った度合いを頼めない")
	}
}

// **工具が残るエージェントを、名乗りで「残らない」にできない。** 起こせないエージェントでも見る。
func TestLeavesToolsCannotBeTurnedOffByHello(t *testing.T) {
	a := &agentConn{agents: []string{AgentClaude, AgentCodex}}
	a.infos = namedInfos(a, []AgentInfo{{Name: AgentCodex, Perms: []string{PermCLI}}})
	if !a.leavesTools(AgentCodex) {
		t.Fatal("名乗りで工具が残らないことにされた")
	}
	if !(&agentConn{}).leavesTools(AgentCodex) {
		t.Fatal("起こせない実行面では工具が残らないことになった（引き取り直した子）")
	}
	if (&agentConn{}).leavesTools(AgentClaude) {
		t.Fatal("Claude の中断を工具が残るものとして扱った")
	}
}

// 駆動器の説明は、全部 cli を直せる（D-031: 度合いを全部直せないエージェントは足さない。
// いまは cli だけ。M41 で5つにする）。名前は登録の鍵と同じ。
func TestEveryDriverNamesItselfAndTakesCLI(t *testing.T) {
	for name, d := range drivers {
		in := d.Info()
		if in.Name != name || in.Label == "" {
			t.Fatalf("%s の説明が登録と合わない: %+v", name, in)
		}
		if !contains(in.Perms, PermCLI) {
			t.Fatalf("%s が cli を直せない", name)
		}
	}
}

// **頼んだ確認の度合いと違う度合いで起きたら、止める。** 名乗らないのは cli と読む。
func TestStartedWithAnotherPermIsStopped(t *testing.T) {
	db := newDB(t)
	s := New(db)
	st, _ := Starttime(os.Getpid())
	c := &Control{s: s, allowUID: -1}
	for _, tc := range []struct {
		id, perm string
		fails    bool
	}{{"p1", "full", true}, {"p2", "", false}, {"p3", PermCLI, false}} {
		rec := Record{ID: tc.id, Agent: AgentCodex, Perm: PermCLI, Cwd: "/tmp", State: StateStarting,
			RequestedBy: "test", CreatedAt: now(), UpdatedAt: now()}
		codexNoErr(t, insert(db, rec))
		s.live[tc.id] = &liveSession{rec: rec, token: "tok", last: time.Now(), asked: map[string]bool{}}
		c.dispatch(&agentConn{c: nopConn{}, who: "test"}, Msg{T: MsgStarted, Session: tc.id,
			Token: "tok", PID: os.Getpid(), Started: st, BootID: BootID(), Agent: AgentCodex,
			Perm: tc.perm})
		r, _ := get(db, tc.id)
		if tc.fails {
			if r.State != StateExited || r.EndCause != EndStartFailed ||
				!strings.Contains(r.ExitReason, "確認の度合い cli を頼んだのに full") {
				t.Fatalf("違う度合いで走らせたまま: %s %s %s", r.State, r.EndCause, r.ExitReason)
			}
			continue
		}
		if r.State != StateIdle {
			t.Fatalf("perm=%q を cli と読めていない: %s %s", tc.perm, r.State, r.ExitReason)
		}
	}
}

// **名乗っていない度合いを campd は頼まない。** 起こせるエージェントでも、その度合いを
// 名乗っていなければ断る（読まれずに落ち、本人の設定のまま起きる）。
func TestAnUnnamedPermIsNotAskedFor(t *testing.T) {
	db := newDB(t)
	s := New(db)
	ac := &agentConn{c: nopConn{}, who: "test", agents: []string{AgentClaude}}
	ac.infos = namedInfos(ac, []AgentInfo{{Name: AgentClaude, Label: "Claude Code", Perms: []string{"yolo"}}})
	s.agent = ac
	if _, err := s.StartAgent("test", "", allowHere(t, db), AgentClaude); err == nil ||
		!strings.Contains(err.Error(), "確認の度合い cli") {
		t.Fatalf("名乗っていない度合いで頼んだ: %v", err)
	}
}

// **このマシンでしか起こせないエージェントを、向こうのホストで代わりに起こさない。**
func TestTheAgentRefusesToStartALocalOnlyAgentOnAnotherHost(t *testing.T) {
	srv, cli := net.Pipe()
	defer srv.Close()
	a := NewAgent("x", "/bin/true")
	a.conn = cli
	a.SSH = "/nonexistent/ssh" // 繋ぎに行ったら別の理由で落ちる
	go a.Run()
	b, _ := json.Marshal(Msg{T: MsgStart, Session: "s1", Token: "t", Cwd: "/w", Root: "/w",
		Agent: agentFake3, Remote: &RemoteSpec{Alias: "far"}})
	go srv.Write(append(b, '\n'))
	srv.SetReadDeadline(time.Now().Add(5 * time.Second))
	sc := bufio.NewScanner(srv)
	var got Msg
	for sc.Scan() {
		if json.Unmarshal(sc.Bytes(), &got) == nil && got.T != MsgPing {
			break
		}
	}
	if got.T != MsgFailed || !strings.Contains(got.Error, "この実行面は fake3 を 向こうのホスト で起こせない") {
		t.Fatalf("このマシンだけのエージェントを向こうで起こそうとした: %+v", got)
	}
}

// **実行面は、頼まれた度合いで起こせないなら、別の度合いで代わりに起こさない。**
func TestTheAgentRefusesAPermItDoesNotName(t *testing.T) {
	srv, cli := net.Pipe()
	defer srv.Close()
	a := NewAgent("x", "/bin/true")
	a.conn = cli
	var started atomic.Bool
	a.Command = func(_, _ string, _ []string) *exec.Cmd {
		started.Store(true)
		return exec.Command("/bin/true")
	}
	go a.Run()
	b, _ := json.Marshal(Msg{T: MsgStart, Session: "s1", Token: "t", Cwd: os.TempDir(),
		Root: os.TempDir(), Perm: "yolo"})
	go srv.Write(append(b, '\n'))
	srv.SetReadDeadline(time.Now().Add(5 * time.Second))
	sc := bufio.NewScanner(srv)
	var got Msg
	for sc.Scan() {
		if json.Unmarshal(sc.Bytes(), &got) == nil && got.T != MsgPing {
			break
		}
	}
	// 断るのは起こす手順に入る前（駆動器が引数を組むところまで行かない）。
	if got.T != MsgFailed || !strings.Contains(got.Error, "この実行面は claude を確認の度合い yolo で起こせない") {
		t.Fatalf("知らない度合いの起動を、起こす前に断っていない: %+v", got)
	}
	if started.Load() {
		t.Fatal("断ったのに子を起こした")
	}
}
