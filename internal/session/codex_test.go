package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/MoomA-0750/camp/internal/store"
)

// Phase 3.6（Codex のセッション駆動）。偽の app-server は fakecodex_test.go。

func codexNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// withFakeCodex は実行面に偽の app-server と、使い捨ての「本人の置き場」を持たせる。
func withFakeCodex(t *testing.T, env ...string) (func(*Agent), string) {
	t.Helper()
	bin, logPath := fakeCodex(t, env...)
	src := t.TempDir()
	codexNoErr(t, os.WriteFile(filepath.Join(src, "auth.json"), []byte(`{"fake":true}`), 0o600))
	codexNoErr(t, os.WriteFile(filepath.Join(src, "config.toml"), []byte("model = \"fake\"\n"), 0o600))
	home := filepath.Join(t.TempDir(), "codex-home")
	return func(a *Agent) {
		a.Codex, a.CodexHome, a.CodexSource = bin, home, src
		a.CodexWithoutScope = true // 偽物なので scope 無しで通す
	}, logPath
}

func wireCodex(t *testing.T, db *store.DB, env ...string) (*Supervisor, *Agent, string) {
	t.Helper()
	s := New(db)
	opt, logPath := withFakeCodex(t, env...)
	return s, attach(t, s, fakeClaude(t), opt), logPath
}

func startCodexHere(t *testing.T, s *Supervisor, db *store.DB) Record {
	t.Helper()
	rec, err := s.StartAgent("test", "", allowHere(t, db), AgentCodex)
	codexNoErr(t, err)
	waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	return rec
}

// startCodexFails は起こして、起こせなかった理由を返す。
func startCodexFails(t *testing.T, s *Supervisor, db *store.DB) Record {
	t.Helper()
	rec, err := s.StartAgent("test", "", allowHere(t, db), AgentCodex)
	codexNoErr(t, err)
	waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateExited })
	r, _ := get(db, rec.ID)
	return r
}

// sent は偽物が受け取った行のうち、method が一致するもの。
func sent(t *testing.T, logPath, method string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, m := range received(t, logPath) {
		if m["method"] == method {
			out = append(out, m)
		}
	}
	return out
}

func decisions(t *testing.T, logPath string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, m := range received(t, logPath) {
		res, ok := m["result"].(map[string]any)
		if !ok {
			continue
		}
		d, _ := res["decision"].(string)
		b, _ := json.Marshal(m["id"])
		out[string(b)] = d
	}
	return out
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func joined(bs [][]byte) string { return string(bytes.Join(bs, []byte("\n"))) }

func historyReason(t *testing.T, db *store.DB, id, reqID string) string {
	t.Helper()
	rows, err := ApprovalHistory(db, id, 50)
	codexNoErr(t, err)
	for _, a := range rows {
		if a.RequestID == reqID {
			return a.Reason
		}
	}
	return "（無い）"
}

// ---------------------------------------------------------------- 起こす・話す・止める

// 1本の Codex のセッションが starting→idle→running→idle→exited を通る。
// **話し始める前の手順で、Camp の方針を渡している。**
func TestACodexSessionRunsThroughTheStateMachine(t *testing.T) {
	db := newDB(t)
	s, _, logPath := wireCodex(t, db)
	rec := startCodexHere(t, s, db)

	r, _ := get(db, rec.ID)
	if r.Agent != AgentCodex {
		t.Fatalf("台帳のエージェントが %q", r.Agent)
	}
	if r.ClaudeID != "thr-fake-1" {
		t.Fatalf("スレッド id が結びついていない: %q", r.ClaudeID)
	}

	codexNoErr(t, s.Input(rec.ID, "hello"))
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })

	init := sent(t, logPath, "initialize")
	if len(init) != 1 || !strings.Contains(mustJSON(init[0]), "item/agentMessage/delta") {
		t.Fatalf("途中経過を断っていない: %v", init)
	}
	start := sent(t, logPath, "thread/start")
	if len(start) != 1 {
		t.Fatalf("thread/start が %d 回", len(start))
	}
	p := start[0]["params"].(map[string]any)
	if p["approvalPolicy"] != "untrusted" || p["approvalsReviewer"] != "user" ||
		p["sandbox"] != "workspace-write" {
		t.Fatalf("渡した方針が違う: %v", p)
	}
	turns := sent(t, logPath, "turn/start")
	if len(turns) != 1 || !strings.Contains(mustJSON(turns[0]), `"threadId":"thr-fake-1"`) ||
		!strings.Contains(mustJSON(turns[0]), "hello") {
		t.Fatalf("入力がスレッドへ渡っていない: %v", turns)
	}

	codexNoErr(t, s.Stop(rec.ID, StopTerminate))
	waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateExited })
}

// **承認は1ターンに何度でも来る。待ち行列のまま**（Claude と同じ）。
// 答えの id は受け取った型のまま返す——整数の 0 を "0" で返すと対応づかない。
func TestCodexApprovalsAreAQueueAndAnswersKeepTheIDType(t *testing.T) {
	db := newDB(t)
	s, _, logPath := wireCodex(t, db)
	rec := startCodexHere(t, s, db)

	codexNoErr(t, s.Input(rec.ID, "two"))
	waitFor(t, 5*time.Second, func() bool { return len(pending(t, s, rec.ID)) == 2 })
	w, err := s.Waiting(rec.ID)
	codexNoErr(t, err)
	for _, a := range w {
		if a.Tool != "codex:command" || !strings.Contains(a.Detail, "touch") {
			t.Fatalf("承認の中身が画面へ渡っていない: %+v", a)
		}
	}
	codexNoErr(t, s.Approve(rec.ID, "0", "allow", ""))
	codexNoErr(t, s.Approve(rec.ID, "1", "deny", "だめ"))
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })

	got := decisions(t, logPath)
	if got["0"] != "accept" || got["1"] != "decline" {
		t.Fatalf("答えが違う（id は数のまま返すはず）: %v", got)
	}
	if _, bad := got[`"0"`]; bad {
		t.Fatal("整数の id を文字列で返している")
	}
}

// **ファイル変更の承認には、差分が添わる。** 要求そのものには無い（実測）。
func TestACodexFileChangeApprovalShowsTheDiff(t *testing.T) {
	db := newDB(t)
	s, _, _ := wireCodex(t, db)
	rec := startCodexHere(t, s, db)

	codexNoErr(t, s.Input(rec.ID, "patch"))
	waitFor(t, 5*time.Second, func() bool { return len(pending(t, s, rec.ID)) == 1 })
	w, _ := s.Waiting(rec.ID)
	if w[0].Tool != "codex:fileChange" || !strings.Contains(w[0].Detail, "/w/notes.txt") ||
		!strings.Contains(w[0].Detail, `hello\n`) {
		t.Fatalf("差分が承認に添っていない: %+v", w[0])
	}
	codexNoErr(t, s.Approve(rec.ID, w[0].RequestID, "allow", ""))
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
}

// **答えられない要求には、すぐ断りを返す。** 黙ると Codex は待ち続ける。
func TestCodexRequestsCampCannotAnswerAreRefused(t *testing.T) {
	db := newDB(t)
	s, _, logPath := wireCodex(t, db)
	rec := startCodexHere(t, s, db)

	codexNoErr(t, s.Input(rec.ID, "perm"))
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	refused := false
	for _, m := range received(t, logPath) {
		if _, ok := m["error"]; ok && m["id"] == float64(0) {
			refused = true
		}
	}
	if !refused {
		t.Fatal("権限の拡張要求に断りを返していない")
	}
	if len(pending(t, s, rec.ID)) != 0 {
		t.Fatal("答えられない要求を承認として出している")
	}
	if !auditHas(t, db, "session.frame", "答えられない要求を断った") {
		t.Fatal("断ったことが監査に残っていない")
	}
}

// **効いた方針を照らしてから話し始める。** 違えば起こさない。
func TestCodexDoesNotStartUnlessThePolicyTookEffect(t *testing.T) {
	for _, c := range []struct{ env, want string }{
		{"CAMP_FAKE_CODEX_POLICY=never", "承認の方針"},
		{"CAMP_FAKE_CODEX_REVIEWER=auto_review", "承認を見る"},
		{"CAMP_FAKE_CODEX_SANDBOX=dangerFullAccess", "sandbox が"},
		{"CAMP_FAKE_CODEX_CWD=/elsewhere", "作業場所"},
		{"CAMP_FAKE_CODEX_STARTERR=1", "Codex が断った"},
	} {
		t.Run(c.env, func(t *testing.T) {
			db := newDB(t)
			s, _, logPath := wireCodex(t, db, c.env)
			r := startCodexFails(t, s, db)
			if r.EndCause != EndStartFailed || !strings.Contains(r.ExitReason, c.want) {
				t.Fatalf("起こせなかった理由が違う: %s / %s", r.EndCause, r.ExitReason)
			}
			if len(sent(t, logPath, "turn/start")) != 0 {
				t.Fatal("方針が効いていないのに話し始めた")
			}
		})
	}
}

// **Camp の置き場で起きたことを照らす。** 本人の置き場で起きても方針は Camp が渡した値に
// なるので、thread/start の照合では気づけない（Fable の実装後レビュー 1）。
func TestCodexMustStartInCampsOwnHome(t *testing.T) {
	db := newDB(t)
	s, _, logPath := wireCodex(t, db, "CAMP_FAKE_CODEX_HOMEOUT="+t.TempDir())
	r := startCodexFails(t, s, db)
	if r.EndCause != EndStartFailed || !strings.Contains(r.ExitReason, "Camp の置き場ではない") {
		t.Fatalf("本人の置き場で起きたのに話し始めた: %s / %s", r.EndCause, r.ExitReason)
	}
	if len(sent(t, logPath, "thread/start")) != 0 {
		t.Fatal("置き場を照らす前にスレッドを始めた")
	}
	// 名乗らないときは、そう言って断る（「別の置き場で起きた」と取り違えさせない）。
	if err := verifyCodexHome(json.RawMessage(`{"userAgent":"x"}`), t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "名乗らない") {
		t.Fatalf("置き場を名乗らないのに、そうと言わずに通した／断った: %v", err)
	}
}

// 話し始める前の手順に時間を切る。**黙った子を待ち続けない。**
func TestAHandshakeThatNeverAnswersIsGivenUp(t *testing.T) {
	db := newDB(t)
	s := New(db)
	opt, _ := withFakeCodex(t, "CAMP_FAKE_CODEX_SILENT=1")
	attach(t, s, fakeClaude(t), opt, func(a *Agent) { a.HeaderWait = 300 * time.Millisecond })
	r := startCodexFails(t, s, db)
	if r.EndCause != EndStartFailed || !strings.Contains(r.ExitReason, "待っても") {
		t.Fatalf("黙った子を待ち続けた: %s / %s", r.EndCause, r.ExitReason)
	}
}

// 話し始める前に来た要求にも断りを返す（まだ誰も答えられない）。
func TestARequestBeforeTheThreadStartsIsRefused(t *testing.T) {
	db := newDB(t)
	s, _, logPath := wireCodex(t, db, "CAMP_FAKE_CODEX_EARLYASK=1")
	startCodexHere(t, s, db)
	refused := false
	for _, m := range received(t, logPath) {
		if _, ok := m["error"]; ok && m["id"] == float64(0) {
			refused = true
		}
	}
	if !refused {
		t.Fatal("話し始める前の要求に黙った")
	}
}

// 中断はターン id を添えて投げる。ターンは interrupted で終わり、次の入力を受ける。
func TestCodexInterruptEndsTheTurn(t *testing.T) {
	db := newDB(t)
	s, _, logPath := wireCodex(t, db)
	rec := startCodexHere(t, s, db)

	codexNoErr(t, s.Input(rec.ID, "hang"))
	codexNoErr(t, s.Stop(rec.ID, StopInterrupt))
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	ints := sent(t, logPath, "turn/interrupt")
	if len(ints) != 1 || !strings.Contains(mustJSON(ints[0]), `"turnId":"turn-1"`) {
		t.Fatalf("中断にターン id が無い: %v", ints)
	}
}

// **Codex の中断は効くが、工具は残る。** ターンが長すぎて Camp が中断したなら、続けて止める。
func TestACodexTurnThatRanTooLongIsStoppedAfterTheInterrupt(t *testing.T) {
	db := newDB(t)
	s, _, _ := wireCodex(t, db)
	rec := startCodexHere(t, s, db)
	s.TurnAfter = 10 * time.Millisecond

	codexNoErr(t, s.Input(rec.ID, "hang"))
	time.Sleep(30 * time.Millisecond)
	s.Tick()
	waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateExited })
	r, _ := get(db, rec.ID)
	if r.EndCause != EndTurnTimeout {
		t.Fatalf("終わり方が %s（turn_timeout のはず）", r.EndCause)
	}
}

// ---------------------------------------------------------------- 取り下げ（Fable の実装後レビュー 2）

// **訊いたあとで差分が変わったら、訊いた中身で許させない。** 断って、台帳からも取り下げる。
func TestAChangedDiffAfterAskingIsDeclined(t *testing.T) {
	db := newDB(t)
	s, _, logPath := wireCodex(t, db)
	rec := startCodexHere(t, s, db)

	codexNoErr(t, s.Input(rec.ID, "moved"))
	waitFor(t, 5*time.Second, func() bool { return historyReason(t, db, rec.ID, "0") == ByWithdrawn })
	if len(pending(t, s, rec.ID)) != 0 {
		t.Fatal("取り下げた承認が待ちのまま残っている")
	}
	if decisions(t, logPath)["0"] != "decline" {
		t.Fatal("差分が変わった承認を断っていない")
	}
	if err := s.Approve(rec.ID, "0", "allow", ""); err == nil {
		t.Fatal("取り下げた承認に、あとから答えられた")
	}
	if !auditHas(t, db, "tool.withdrawn", "差分が変わった") {
		t.Fatal("取り下げが監査に残っていない")
	}
}

// 答えないうちに Codex 側で片付いた承認も、台帳から取り下げる。
func TestAnApprovalSettledByCodexIsWithdrawn(t *testing.T) {
	db := newDB(t)
	s, _, _ := wireCodex(t, db)
	rec := startCodexHere(t, s, db)

	codexNoErr(t, s.Input(rec.ID, "settle"))
	waitFor(t, 5*time.Second, func() bool { return historyReason(t, db, rec.ID, "0") == ByWithdrawn })
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	if len(pending(t, s, rec.ID)) != 0 {
		t.Fatal("片付いた承認が待ちのまま残っている")
	}
}

// **子へ届かなかった答えを、届いたことにしない。**
func TestAnAnswerThatCannotReachTheChildIsWithdrawn(t *testing.T) {
	db := newDB(t)
	s, _, _ := wireCodex(t, db)
	rec := startCodexHere(t, s, db)
	// 実行面が見ていない承認（campd の台帳にだけある）。
	codexNoErr(t, ask(db, rec.ID, "99", "codex:command", `{}`, time.Now()))
	codexNoErr(t, s.Approve(rec.ID, "99", "allow", ""))
	waitFor(t, 5*time.Second, func() bool { return historyReason(t, db, rec.ID, "99") == ByWithdrawn })
	if !auditHas(t, db, "tool.withdrawn", "届かなかった") {
		t.Fatal("届かなかったことが監査に残っていない")
	}
}

// **書き込みの範囲を広げる承認（grantRoot）は、許す／断るに収まらないので断る。**
func TestAGrantRootApprovalIsDeclined(t *testing.T) {
	db := newDB(t)
	s, _, logPath := wireCodex(t, db)
	rec := startCodexHere(t, s, db)

	codexNoErr(t, s.Input(rec.ID, "grant"))
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	if len(pending(t, s, rec.ID)) != 0 || decisions(t, logPath)["0"] != "decline" {
		t.Fatal("grantRoot の承認を画面に出した")
	}
	if !auditHas(t, db, "session.frame", "grantRoot") {
		t.Fatal("断った理由が監査に無い")
	}
}

// item の id の無い patchUpdated で、コマンドの承認まで巻き込まない。同じ item の承認は全部断る。
func TestPatchUpdatedOnlyTouchesItsOwnItem(t *testing.T) {
	cs := newCodexState()
	cs.classify([]byte(`{"id":0,"method":"item/commandExecution/requestApproval","params":{"command":"x","cwd":"/w"}}`))
	ev := cs.classify([]byte(`{"method":"item/fileChange/patchUpdated","params":{"changes":[]}}`))
	if len(ev.replies) != 0 || len(cs.asks) != 1 {
		t.Fatalf("item の id が無いのにコマンドの承認を断った: %s", joined(ev.replies))
	}
	for _, id := range []string{"1", "2"} {
		cs.classify([]byte(`{"method":"item/started","params":{"item":{"type":"fileChange","id":"i1","changes":[{"path":"/w/a"}]}}}`))
		cs.classify([]byte(`{"id":` + id + `,"method":"item/fileChange/requestApproval","params":{"itemId":"i1"}}`))
	}
	ev = cs.classify([]byte(`{"method":"item/fileChange/patchUpdated","params":{"itemId":"i1","changes":[{"path":"/w/b"}]}}`))
	if len(ev.replies) != 2 || len(ev.withdrawn) != 2 || len(cs.asks) != 1 {
		t.Fatalf("同じ item の承認を全部断っていない: replies=%d withdrawn=%v 残り=%d",
			len(ev.replies), ev.withdrawn, len(cs.asks))
	}
}

// ---------------------------------------------------------------- 頼んでよい相手か

// **Codex を起こせない実行面に Codex を頼まない。** 古い実行面は agent を読まずに claude を起こす。
func TestCodexIsNotAskedOfAnExecutionSideThatCannotStartIt(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db) // Codex を持たない実行面
	if _, err := s.StartAgent("test", "", allowHere(t, db), AgentCodex); err == nil ||
		!strings.Contains(err.Error(), "起こせない") {
		t.Fatalf("Codex を起こせない実行面に頼んだ: %v", err)
	}

	// scope を使わない構成の実行面も、Codex は起こせないと名乗る。
	db2 := newDB(t)
	s2 := New(db2)
	opt, _ := withFakeCodex(t)
	attach(t, s2, fakeClaude(t), opt, func(a *Agent) { a.CodexWithoutScope = false })
	if _, err := s2.StartAgent("test", "", allowHere(t, db2), AgentCodex); err == nil {
		t.Fatal("scope 無しの実行面に Codex を頼んだ")
	}
}

// 名乗らない（Phase 3.6 より前の）実行面は claude だけ。
func TestAnOldExecutionSideIsNotAskedForCodex(t *testing.T) {
	old := &agentConn{}
	if old.can(AgentCodex) || !old.can(AgentClaude) {
		t.Fatal("名乗らない実行面の読み方が違う")
	}
	if !(&agentConn{agents: []string{AgentClaude, AgentCodex}}).can(AgentCodex) {
		t.Fatal("名乗った実行面に頼めない")
	}
}

func TestCodexOnAnotherHostAndUnknownAgentsAreRefused(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	if _, err := s.StartAgent("test", "rp", "/data/x", AgentCodex); err == nil ||
		!strings.Contains(err.Error(), "向こうのホスト") {
		t.Fatalf("向こうのホストで Codex を起こそうとした: %v", err)
	}
	if _, err := s.StartAgent("test", "", allowHere(t, db), "gemini"); err == nil {
		t.Fatal("知らないエージェントを通した")
	}
}

// **頼んだエージェントと違うものが起きたら、止める。**
func TestStartedWithAnotherAgentIsStopped(t *testing.T) {
	db := newDB(t)
	s := New(db)
	rec := Record{ID: "cx", Agent: AgentCodex, Cwd: "/tmp", State: StateStarting,
		RequestedBy: "test", CreatedAt: now(), UpdatedAt: now()}
	codexNoErr(t, insert(db, rec))
	s.live["cx"] = &liveSession{rec: rec, token: "tok", last: time.Now(), asked: map[string]bool{}}
	st, _ := Starttime(os.Getpid())
	c := &Control{s: s, allowUID: -1}
	// 古い実行面は agent を名乗らない（claude と読む）。
	c.dispatch(&agentConn{c: nopConn{}, who: "test"}, Msg{T: MsgStarted, Session: "cx",
		Token: "tok", PID: os.Getpid(), Started: st, BootID: BootID()})
	r, _ := get(db, "cx")
	if r.State != StateExited || r.EndCause != EndStartFailed ||
		!strings.Contains(r.ExitReason, "codex を頼んだのに claude") {
		t.Fatalf("違うエージェントを走らせたまま: %s %s %s", r.State, r.EndCause, r.ExitReason)
	}
}

// 引き取り直しでもエージェントを照らす。**待っている承認は台帳に採る**（Fable 5）。
func TestReadoptChecksTheAgentAndAdoptsWaitingApprovals(t *testing.T) {
	db := newDB(t)
	s := New(db)
	self := os.Getpid()
	st, _ := Starttime(self)
	mustInsert(t, db, "cx", StateRunning, self, st, BootID())
	_, err := db.Exec(`update runtime_sessions set agent='codex' where id='cx'`)
	codexNoErr(t, err)
	c := &Control{s: s, allowUID: -1}

	c.readopt([]Held{{ID: "cx", Token: "t", PID: self, Started: st, BootID: BootID(),
		Agent: AgentClaude}})
	if len(s.Live()) != 0 {
		t.Fatal("台帳は codex なのに claude の子を引き取った")
	}
	if !auditHas(t, db, "session.readopt", "台帳は codex なのに claude") {
		t.Fatal("断った理由が監査に無い")
	}

	c.readopt([]Held{{ID: "cx", Token: "t", PID: self, Started: st, BootID: BootID(),
		Agent: AgentCodex, Waiting: []HeldAsk{{ReqID: "7", Tool: "codex:command",
			Detail: json.RawMessage(`{"agent":"codex"}`)}}}})
	if len(s.Live()) != 1 {
		t.Fatal("正しい名乗りを引き取れていない")
	}
	if got := pending(t, s, "cx"); len(got) != 1 || got[0] != "7" {
		t.Fatalf("campd が居ない間に来た承認が台帳に無い: %v", got)
	}
}

// ---------------------------------------------------------------- 実行面の中（単体）

// **見た id 以外には答えない。** campd から来た文字列を JSON に埋めると中身を差し込める。
func TestCodexAnswersOnlyRequestsItSaw(t *testing.T) {
	cs := newCodexState()
	ev := cs.classify([]byte(`{"id":0,"method":"item/commandExecution/requestApproval","params":{"command":"x","cwd":"/w"}}`))
	if ev.ask == nil || ev.ask.ReqID != "0" {
		t.Fatalf("承認として畳めていない: %+v", ev)
	}
	for _, bad := range []string{`0,"result":{"decision":"accept"}`, "5", `"0"`} {
		if _, err := cs.approve(bad, "allow"); err == nil {
			t.Fatalf("見ていない id %q に答えた", bad)
		}
	}
	b, err := cs.approve("0", "deny")
	codexNoErr(t, err)
	if !strings.Contains(string(b), `"id":0`) || !strings.Contains(string(b), "decline") {
		t.Fatalf("断りの形が違う: %s", b)
	}
	if _, err := cs.approve("0", "allow"); err == nil {
		t.Fatal("同じ承認に二度答えた")
	}
}

// **ターンの終わりを取りこぼさない。** 取りこぼすと 60 分 running のまま（Fable 6）。
func TestEveryWayACodexTurnEndsIsFolded(t *testing.T) {
	cs := newCodexState()
	cs.thread = "t"
	in, err := cs.input("x")
	codexNoErr(t, err)
	var req struct {
		ID string `json:"id"`
	}
	json.Unmarshal(in, &req)
	ev := cs.classify([]byte(`{"id":"` + req.ID + `","error":{"code":-1,"message":"枠切れ"}}`))
	if !ev.turnEnd || !strings.Contains(ev.err, "枠切れ") {
		t.Fatalf("turn/start の失敗を終わりに畳んでいない: %+v", ev)
	}
	for line, end := range map[string]bool{
		`{"method":"error","params":{"willRetry":true,"error":{"message":"a"}}}`:  false,
		`{"method":"error","params":{"willRetry":false,"error":{"message":"a"}}}`: true,
		`{"method":"thread/closed","params":{"threadId":"t"}}`:                    true,
		`{"method":"turn/completed","params":{"turn":{"status":"failed"}}}`:       true,
	} {
		if got := cs.classify([]byte(line)).turnEnd; got != end {
			t.Fatalf("%s: turnEnd=%v（%v のはず）", line, got, end)
		}
	}
	for _, m := range []string{"thread/settings/updated", "item/autoApprovalReview/started"} {
		if ev := cs.classify([]byte(`{"method":"` + m + `","params":{}}`)); !ev.kill {
			t.Fatalf("方針が変わった（%s）のに止めない", m)
		}
	}
}

// **断った通知は見えなくなる。** Camp が見張る通知を断ってはいけない。
func TestTheOptOutNeverHidesWhatCampWatches(t *testing.T) {
	watched := []string{"turn/started", "turn/completed", "error", "thread/closed",
		"item/started", "item/completed", "item/fileChange/patchUpdated", "serverRequest/resolved",
		"thread/tokenUsage/updated"}
	for m := range codexPolicyChanged {
		watched = append(watched, m)
	}
	for _, m := range watched {
		if contains(codexOptOut, m) {
			t.Fatalf("見張る通知 %s を断っている", m)
		}
	}
}

// **中身を見せられない承認は、許させずに断る。**
func TestAnUnknownOrOversizedDiffIsDeclinedNotShown(t *testing.T) {
	cs := newCodexState()
	ev := cs.classify([]byte(`{"id":3,"method":"item/fileChange/requestApproval","params":{"itemId":"i1"}}`))
	if ev.ask != nil || !strings.Contains(joined(ev.replies), "decline") || !strings.Contains(ev.err, "差分") {
		t.Fatalf("差分の無い承認を出した: %+v", ev)
	}
	big := strings.Repeat("x", maxApprovalDetail)
	cs.classify([]byte(`{"method":"item/started","params":{"item":{"type":"fileChange","id":"i2",` +
		`"changes":[{"path":"/w/a","diff":"` + big + `"}]}}}`))
	ev = cs.classify([]byte(`{"id":4,"method":"item/fileChange/requestApproval","params":{"itemId":"i2"}}`))
	if ev.ask != nil || !strings.Contains(joined(ev.replies), "decline") || !strings.Contains(ev.err, "大きさ") {
		t.Fatalf("途中までの差分で許させようとした: %+v", ev.err)
	}
}

// 中断はターン id が要る。先に頼まれたら、ターンが始まったところで投げる。
func TestAnInterruptBeforeTheTurnIsKnownIsSentWhenItStarts(t *testing.T) {
	cs := newCodexState()
	cs.thread = "t"
	if b := cs.interrupt(); b != nil {
		t.Fatalf("ターン id が無いのに中断を投げた: %s", b)
	}
	ev := cs.classify([]byte(`{"method":"turn/started","params":{"turn":{"id":"turn-9"}}}`))
	if r := joined(ev.replies); !strings.Contains(r, "turn/interrupt") || !strings.Contains(r, "turn-9") {
		t.Fatalf("後回しにした中断を投げていない: %s", r)
	}
}

// **照合は構造ごと。欄が欠けていても断る。**
func TestVerifyCodexStartChecksTheWholeShape(t *testing.T) {
	good := `{"thread":{"id":"01a0-x"},"approvalPolicy":"untrusted","approvalsReviewer":"user",` +
		`"sandbox":{"type":"workspaceWrite","writableRoots":[],"networkAccess":false},` +
		`"activePermissionProfile":null,"cwd":"/w"}`
	if th, err := verifyCodexStart(json.RawMessage(good), "/w"); err != nil || th != "01a0-x" {
		t.Fatalf("正しい応答を断った: %v", err)
	}
	for name, bad := range map[string]string{
		"方針":        strings.Replace(good, `"untrusted"`, `"on-request"`, 1),
		"方針の型":      strings.Replace(good, `"untrusted"`, `{"granular":{}}`, 1),
		"見る者":       strings.Replace(good, `"user"`, `"auto_review"`, 1),
		"sandbox":   strings.Replace(good, `"workspaceWrite"`, `"dangerFullAccess"`, 1),
		"ネット":       strings.Replace(good, `"networkAccess":false`, `"networkAccess":true`, 1),
		"書ける場所":     strings.Replace(good, `"writableRoots":[]`, `"writableRoots":["/"]`, 1),
		"書ける場所無し":   strings.Replace(good, `"writableRoots":[],`, ``, 1),
		"プロファイル":    strings.Replace(good, `"activePermissionProfile":null`, `"activePermissionProfile":{"id":"x"}`, 1),
		"cwd無し":     strings.Replace(good, `,"cwd":"/w"`, ``, 1),
		"cwd違い":     strings.Replace(good, `"cwd":"/w"`, `"cwd":"/"`, 1),
		"sandbox無し": strings.Replace(good, `"sandbox":{"type":"workspaceWrite","writableRoots":[],"networkAccess":false},`, ``, 1),
		"スレッド":      strings.Replace(good, `"01a0-x"`, `"a b"`, 1),
	} {
		if _, err := verifyCodexStart(json.RawMessage(bad), "/w"); err == nil {
			t.Fatalf("%s が違うのに通した: %s", name, bad)
		}
	}
}

// hello で名乗るのは、本当に起こせるものだけ。
func TestTheExecutionSideNamesCodexOnlyWhenItCanStartIt(t *testing.T) {
	a := NewAgent("x", "y")
	a.Codex = ""
	if contains(a.agents(), AgentCodex) {
		t.Fatal("codex の実体が無いのに名乗った")
	}
	a.Codex = os.Args[0]
	a.Scope = true
	if !contains(a.agents(), AgentCodex) {
		t.Fatal("起こせるのに名乗らない")
	}
	a.Scope, a.CodexWithoutScope = false, false
	if contains(a.agents(), AgentCodex) {
		t.Fatal("scope を使わない構成で名乗った")
	}
}

// ---------------------------------------------------------------- Camp 専用の置き場

// **道具は本人と同じ。「今後訊かない」と「信頼済みの場所」は持ち込まない。本人の置き場に書かない。**
func TestTheCampCodexHomeCarriesToolsButNotRulesOrTrust(t *testing.T) {
	src := t.TempDir()
	conf := `model = "gpt-x"
default_permissions = "from-vaults"

[projects."/home/me"]
trust_level = "trusted"

[permissions.from-vaults]
extends = ":workspace"

[permissions.from-vaults.filesystem]
"/home/me/git" = "write"

[features]
hooks = true

[desktop]
keep = true

[marketplaces.openai-bundled]
source_type = "local"
source = "/home/me/.codex/.tmp/bundled-marketplaces/openai-bundled"

[marketplaces.openai-primary-runtime]
source_type = "local"

[mcp_servers.node_repl]
command = "node"
args = [
  "repl",
]

[mcp_servers.node_repl.env]
A = "1"

[tui]
status_line = []
`
	codexNoErr(t, os.WriteFile(filepath.Join(src, "config.toml"), []byte(conf), 0o600))
	codexNoErr(t, os.WriteFile(filepath.Join(src, "auth.json"), []byte(`{}`), 0o600))
	for _, d := range []string{"plugins", "skills", "rules"} {
		codexNoErr(t, os.Mkdir(filepath.Join(src, d), 0o700))
	}
	codexNoErr(t, os.WriteFile(filepath.Join(src, "rules", "default.rules"), []byte(`x`), 0o600))
	before, _ := os.ReadFile(filepath.Join(src, "config.toml"))

	home := filepath.Join(t.TempDir(), "h")
	codexNoErr(t, prepareCodexHome(home, src))
	out, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	for _, keep := range []string{`model = "gpt-x"`, "[features]", "[mcp_servers.node_repl]",
		"[mcp_servers.node_repl.env]", `"repl",`, "[marketplaces.openai-primary-runtime]"} {
		if !bytes.Contains(out, []byte(keep)) {
			t.Fatalf("道具の設定 %q を落とした:\n%s", keep, out)
		}
	}
	for _, gone := range []string{"projects", "trust_level", "default_permissions", "permissions.",
		"/home/me/git", "[desktop]", "openai-bundled", "[tui]"} {
		if bytes.Contains(out, []byte(gone)) {
			t.Fatalf("持ち込まないはずの %q が残っている:\n%s", gone, out)
		}
	}
	for _, name := range []string{"auth.json", "plugins", "skills"} {
		if cur, err := os.Readlink(filepath.Join(home, name)); err != nil || cur != filepath.Join(src, name) {
			t.Fatalf("%s が本人の置き場へのリンクになっていない: %q %v", name, cur, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(home, "rules")); err == nil {
		t.Fatal("「今後訊かない」を持ち込んだ")
	}
	if after, _ := os.ReadFile(filepath.Join(src, "config.toml")); !bytes.Equal(before, after) {
		t.Fatal("本人の config.toml を書き換えた")
	}

	// Codex がここへ書き足した「信頼済みの場所」は、次に起こすときに消える。
	f, _ := os.OpenFile(filepath.Join(home, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("\n[projects.\"/w\"]\ntrust_level = \"trusted\"\n")
	f.Close()
	codexNoErr(t, prepareCodexHome(home, src))
	if out, _ := os.ReadFile(filepath.Join(home, "config.toml")); bytes.Contains(out, []byte("trust_level")) {
		t.Fatal("書き足された信頼済みの場所が残った")
	}

	// rules が現れたら起こさない。
	codexNoErr(t, os.Mkdir(filepath.Join(home, "rules"), 0o700))
	if err := prepareCodexHome(home, src); err == nil || !strings.Contains(err.Error(), "rules") {
		t.Fatalf("rules があるのに起こす: %v", err)
	}
	os.Remove(filepath.Join(home, "rules"))

	// ログインの写しを置かない。
	os.Remove(filepath.Join(home, "auth.json"))
	codexNoErr(t, os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{}`), 0o600))
	if err := prepareCodexHome(home, src); err == nil || !strings.Contains(err.Error(), "symlink ではない") {
		t.Fatalf("ログインの写しを受け入れた: %v", err)
	}
}

// **綴りが違っても、本人の置き場そのものは使わない**（Fable の実装後レビュー 3）。
// rules の無い本人の置き場でも止まること——rules の検査に偶然頼らない。
func TestTheUsersOwnHomeIsNotUsedUnderAnotherName(t *testing.T) {
	src := t.TempDir()
	conf := "model = \"x\"\n[projects.\"/keep\"]\ntrust_level = \"trusted\"\n"
	codexNoErr(t, os.WriteFile(filepath.Join(src, "config.toml"), []byte(conf), 0o600))
	codexNoErr(t, os.WriteFile(filepath.Join(src, "auth.json"), []byte(`{}`), 0o600))
	alias := filepath.Join(t.TempDir(), "alias")
	codexNoErr(t, os.Symlink(src, alias))
	for _, home := range []string{src, alias, src + "/./", filepath.Join(alias, ".")} {
		if err := prepareCodexHome(home, src); err == nil {
			t.Fatalf("%s（本人の置き場）を Camp の置き場にした", home)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(src, "config.toml")); string(got) != conf {
		t.Fatal("本人の config.toml を書き換えた")
	}
}

// **TOML として同じ意味の書き方でも落とす。** 複数行の値の中は見出しと読まない（Fable の実装後レビュー 4）。
func TestTheConfigFilterReadsTOMLNotJustLines(t *testing.T) {
	src := `projects."/x".trust_level = "trusted"
projects = { "/y" = { trust_level = "trusted" } }
default_permissions = """
from-vaults
"""
developer_instructions = """
[projects."/in-a-string"]
"""
model = "keep"

[ projects . "/z" ]
trust_level = "trusted"

["projects"."/q"]
trust_level = "trusted"

[ 'permissions' . "p" ]
x = 1

[mcp_servers.keep]
args = [
  ["nested"],
  "[projects.fake]",
]
env = { A = "[b]" }

[marketplaces."openai-bundled"]
source = "/home/me/.codex"

[projects."/w"] # 注
trust_level = "trusted"
`
	out := string(filterCodexConfig([]byte(src)))
	for _, keep := range []string{`model = "keep"`, `developer_instructions = """`,
		`[projects."/in-a-string"]`, "[mcp_servers.keep]", `["nested"],`, `"[projects.fake]",`,
		`env = { A = "[b]" }`} {
		if !strings.Contains(out, keep) {
			t.Fatalf("残すはずの %q を落とした:\n%s", keep, out)
		}
	}
	for _, gone := range []string{"trust_level", "from-vaults", `"/z"`, `"/q"`, "permissions",
		"x = 1", "openai-bundled", "/home/me/.codex"} {
		if strings.Contains(out, gone) {
			t.Fatalf("落とすはずの %q が残っている:\n%s", gone, out)
		}
	}
}

// ---------------------------------------------------------------- codex の outer gate の指摘

// **許した場所の外で走らせようとする承認は、見せずに断る**（指摘 4）。
func TestACommandApprovalOutsideTheAllowedPlaceIsDeclined(t *testing.T) {
	db := newDB(t)
	s, _, logPath := wireCodex(t, db, "CAMP_FAKE_CODEX_ASKCWD=/etc")
	rec := startCodexHere(t, s, db)
	codexNoErr(t, s.Input(rec.ID, "ask"))
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	if len(pending(t, s, rec.ID)) != 0 || decisions(t, logPath)["0"] != "decline" {
		t.Fatal("許した場所の外で走る承認を画面に出した")
	}
	if !auditHas(t, db, "session.frame", "許した場所の外") {
		t.Fatal("断った理由が監査に無い")
	}
}

// 何を・どこで・どのスレッドで、が揃わない承認は出さない。
func TestACommandApprovalMustSayWhatWhereAndWhichThread(t *testing.T) {
	cs := newCodexState()
	cs.thread, cs.root = "t", "/w"
	for name, params := range map[string]string{
		"command 無し":   `{"threadId":"t","cwd":"/w"}`,
		"command が空":   `{"threadId":"t","cwd":"/w","command":" "}`,
		"cwd 無し":       `{"threadId":"t","command":"x"}`,
		"cwd が相対":      `{"threadId":"t","command":"x","cwd":"w"}`,
		"外（.. で抜ける）": `{"threadId":"t","command":"x","cwd":"/w/../etc"}`,
		"別のスレッド":      `{"threadId":"u","command":"x","cwd":"/w"}`,
	} {
		ev := cs.classify([]byte(`{"id":1,"method":"item/commandExecution/requestApproval","params":` + params + `}`))
		if ev.ask != nil || !strings.Contains(joined(ev.replies), "decline") {
			t.Fatalf("%s の承認を画面に出した", name)
		}
	}
	ev := cs.classify([]byte(`{"id":2,"method":"item/commandExecution/requestApproval","params":` +
		`{"threadId":"t","command":"x","cwd":"/w/sub"}}`))
	if ev.ask == nil {
		t.Fatalf("揃った承認を断った: %s", ev.err)
	}
}

// **scope に残ったものを数えて止める。止め切れなければ数を返す**（指摘 1）。
func TestLeftoversInTheScopeAreStoppedAndCounted(t *testing.T) {
	// 本当に止まるもの。
	sleeper := exec.Command("sleep", "60")
	sleeper.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // Codex のコマンドと同じく別のセッション
	codexNoErr(t, sleeper.Start())
	var gone atomic.Bool
	go func() { sleeper.Wait(); gone.Store(true) }()
	stopped := 0
	a := NewAgent("x", "y")
	a.ScopeStop = func(string) error { stopped++; return nil } // systemd には触らない
	a.ScopeProcs = func(string) ([]int, error) {
		if gone.Load() {
			return nil, nil
		}
		return []int{sleeper.Process.Pid}, nil
	}
	if left, known := a.sweepScope("camp-session-x.scope"); left != 0 || !known {
		t.Fatalf("残りを止められていない: left=%d known=%v", left, known)
	}
	if stopped != 1 || !gone.Load() {
		t.Fatalf("scope を止めてから1つずつ止める、になっていない（stop=%d gone=%v）", stopped, gone.Load())
	}

	// 止め切れないもの（居ない pid を返し続ける）。
	a.ScopeProcs = func(string) ([]int, error) { return []int{1 << 30}, nil }
	if left, known := a.sweepScope("camp-session-x.scope"); left != 1 || !known {
		t.Fatalf("止め切れないのに数えていない: left=%d known=%v", left, known)
	}
	// 確かめられないもの。
	a.ScopeProcs = func(string) ([]int, error) { return nil, errors.New("systemd に訊けない") }
	if _, known := a.sweepScope("camp-session-x.scope"); known {
		t.Fatal("確かめられないのに確かめたと言った")
	}
}

// **止め切れていないものを「子が自分で終わった」と書かない。**
func TestAnExitWithLeftoversIsNotRecordedAsFinished(t *testing.T) {
	db := newDB(t)
	s := New(db)
	for i, left := range []int{2, -1} {
		id := fmt.Sprintf("lx%d", i)
		rec := Record{ID: id, Agent: AgentCodex, Cwd: "/tmp", State: StateIdle,
			RequestedBy: "test", CreatedAt: now(), UpdatedAt: now()}
		codexNoErr(t, insert(db, rec))
		s.live[id] = &liveSession{rec: rec, token: "tok", last: time.Now(), asked: map[string]bool{}}
		s.dispatchForTest(Msg{T: MsgExited, Session: id, Token: "tok", Code: 0,
			Reason: "終わった" + leftoverNote(left), Leftover: left})
		r, _ := get(db, id)
		if r.State != StateExited || r.EndCause != EndStopTimeout {
			t.Fatalf("残り %d なのに %s / %s と書いた", left, r.State, r.EndCause)
		}
	}
}
