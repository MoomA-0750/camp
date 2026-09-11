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

// Codex のセッション駆動（Phase 3.6、3.7 で本人の置き場へ戻した）。偽の app-server は fakecodex_test.go。

func codexNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// withFakeCodex は実行面に偽の app-server と、使い捨ての「本人の置き場」を持たせる。
// 偽物は、その置き場で起きたと名乗る（env で上書きできる。あとに書いたものが勝つ）。
func withFakeCodex(t *testing.T, env ...string) (func(*Agent), string) {
	t.Helper()
	home := t.TempDir()
	bin, logPath := fakeCodex(t, append([]string{"CAMP_FAKE_CODEX_HOMEOUT=" + home}, env...)...)
	return func(a *Agent) {
		a.Codex, a.CodexHome = bin, home
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
// **方針は何も渡さない**（本人の設定のまま＝CLI と同じ。D-030）。
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
	for _, k := range []string{"approvalPolicy", "approvalsReviewer", "sandbox"} {
		if _, set := p[k]; set {
			t.Fatalf("CLI と同じはずなのに %s を渡した: %v", k, p)
		}
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

// **頼んだ場所で起きたかを照らしてから話し始める。** 違えば起こさない。
func TestCodexDoesNotStartUnlessItStartedWhereAsked(t *testing.T) {
	for _, c := range []struct{ env, want string }{
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
				t.Fatal("頼んだ場所で起きていないのに話し始めた")
			}
		})
	}
}

// **本人の置き場で起きたことを照らす。** CLI と同じ設定で動かすと約束しているので、
// 別の置き場（別の設定・別のログイン）で起きていたら話し始めない。
func TestCodexMustStartInTheUsersOwnHome(t *testing.T) {
	db := newDB(t)
	s, _, logPath := wireCodex(t, db, "CAMP_FAKE_CODEX_HOMEOUT="+t.TempDir())
	r := startCodexFails(t, s, db)
	if r.EndCause != EndStartFailed || !strings.Contains(r.ExitReason, "本人の置き場ではない") {
		t.Fatalf("別の置き場で起きたのに話し始めた: %s / %s", r.EndCause, r.ExitReason)
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

// ---------------------------------------------------------------- 取り下げ

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
		cs.classify([]byte(`{"method":"item/started","params":{"item":{"type":"fileChange","id":"i1","changes":[{"path":"/w/a","diff":"a\n"}]}}}`))
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

// 駆動器を名乗らない実行面（Phase 3.7 より前）は claude だけ。**Phase 3.6 の実行面は codex を
// 名乗るが、それでも頼まない**——専用の置き場で untrusted 固定のまま起こすのを「CLI と同じ」として
// 書いてしまう（`codex exec` のレビュー、2026-09-12）。
func TestAnOldExecutionSideIsNotAskedForCodex(t *testing.T) {
	old := &agentConn{}
	if old.can(AgentCodex) || !old.can(AgentClaude) {
		t.Fatal("名乗らない実行面の読み方が違う")
	}
	p36 := &agentConn{agents: []string{AgentClaude, AgentCodex}} // Phase 3.6 の実行面
	if p36.can(AgentCodex) {
		t.Fatal("駆動器を名乗らない実行面に Codex を頼める")
	}
	if !p36.can(AgentClaude) {
		t.Fatal("古い実行面の claude まで断っている")
	}
	// 駆動器を名乗れば頼める。
	named := &agentConn{agents: []string{AgentClaude, AgentCodex}}
	named.infos = namedInfos(named, []AgentInfo{{Name: AgentCodex, Label: "Codex",
		Perms: []string{PermCLI, PermAsk}}})
	if !named.can(AgentCodex) {
		t.Fatal("名乗った実行面に頼めない")
	}
}

// 向こうのホストで起こせないエージェント（駆動器の説明の Remote が false）は campd が断る。
// Codex は M42 で向こうでも起こせるようにしたので、テストの中の3つ目のエージェントで確かめる。
func TestCodexOnAnotherHostAndUnknownAgentsAreRefused(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	if _, err := s.StartAgent("test", "rp", "/data/x", agentFake3); err == nil ||
		!strings.Contains(err.Error(), "向こうのホスト") {
		t.Fatalf("向こうのホストで起こせないエージェントを起こそうとした: %v", err)
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
// **設定が途中で変わっても止めない。** 記録するだけ（2026-09-12、本人）。
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
	ev = cs.classify([]byte(`{"method":"thread/settings/updated","params":{}}`))
	if ev.note == "" || ev.err != "" || ev.turnEnd {
		t.Fatalf("設定の変化を記録していない、または止める扱いにした: %+v", ev)
	}
	// 自動審査の開始は「自動で判断」のふつうの流れ。止めも記録もしない。
	ev = cs.classify([]byte(`{"method":"item/autoApprovalReview/started","params":{}}`))
	if ev.note != "" || ev.err != "" || ev.turnEnd {
		t.Fatalf("自動審査の開始を特別扱いした: %+v", ev)
	}
}

// 設定の変化は止めずに、監査に「記録」として残る。
func TestASettingsChangeIsRecordedNotStopped(t *testing.T) {
	db := newDB(t)
	s, _, _ := wireCodex(t, db)
	rec := startCodexHere(t, s, db)
	s.dispatchForTest(Msg{T: MsgFrame, Session: rec.ID, Token: s.live[rec.ID].token,
		Kind: "thread/settings/updated", Note: "設定が途中で変わった（thread/settings/updated）。止めずに記録した"})
	if !auditHas(t, db, "session.note", "止めずに記録した") {
		t.Fatal("設定の変化が記録に残っていない")
	}
	if state(t, db, rec.ID) == StateExited {
		t.Fatal("設定の変化で止めた")
	}
}

// **断った通知は見えなくなる。** Camp が見張る通知を断ってはいけない。
func TestTheOptOutNeverHidesWhatCampWatches(t *testing.T) {
	watched := []string{"turn/started", "turn/completed", "error", "thread/closed",
		"item/started", "item/completed", "item/fileChange/patchUpdated", "serverRequest/resolved",
		"thread/tokenUsage/updated"}
	for m := range codexSettingsChanged {
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
	// **なぜ断ったかを言う**（差分がまだ来ていない。読めない差分とは分けて書く）。
	if ev.ask != nil || !strings.Contains(joined(ev.replies), "decline") || !strings.Contains(ev.err, "来ていない") {
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

// **照らすのは作業場所とスレッド。欠けていても断る。** 方針は本人の設定のまま（照らさない）。
func TestVerifyCodexStartChecksWhereItStarted(t *testing.T) {
	good := `{"thread":{"id":"01a0-x"},"approvalPolicy":"never","approvalsReviewer":"auto_review",` +
		`"sandbox":{"type":"dangerFullAccess"},"cwd":"/w"}`
	if th, err := verifyCodexStart(json.RawMessage(good), "/w"); err != nil || th != "01a0-x" {
		t.Fatalf("本人の設定のままの応答を断った: %v", err)
	}
	for name, bad := range map[string]string{
		"cwd無し":  strings.Replace(good, `,"cwd":"/w"`, ``, 1),
		"cwd違い":  strings.Replace(good, `"cwd":"/w"`, `"cwd":"/"`, 1),
		"スレッド":   strings.Replace(good, `"01a0-x"`, `"a b"`, 1),
		"スレッド無し": strings.Replace(good, `"thread":{"id":"01a0-x"},`, ``, 1),
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

// 起こす口は、本人の置き場を触らない（CODEX_HOME を渡さない。CLI と同じ）。どのエージェントも
// 実体の後ろに駆動器の引数をそのまま付け、scope を使うなら systemd-run で包む。
func TestTheDefaultCommandUsesTheUsersOwnSettings(t *testing.T) {
	a := NewAgent("x", "/usr/bin/claude-fake")
	a.Codex, a.CodexWithoutScope = "/usr/bin/true", true
	for _, scope := range []bool{false, true} {
		a.Scope = scope
		for _, name := range []string{AgentClaude, AgentCodex} {
			d := drivers[name]
			args, err := d.Argv(PermCLI)
			codexNoErr(t, err)
			l, err := d.Launch(a)
			codexNoErr(t, err)
			argv := append([]string{l.Bin}, args...)
			cmd := a.defaultCommand(name, "s1", argv)
			for _, e := range cmd.Env {
				if strings.HasPrefix(e, "CODEX_HOME=") {
					t.Fatalf("%s: CODEX_HOME を差し替えた: %s", name, e)
				}
			}
			if !strings.HasSuffix(strings.Join(cmd.Args, " "), strings.Join(argv, " ")) {
				t.Fatalf("%s: 起こし方が違う: %v", name, cmd.Args)
			}
			if wrapped := cmd.Args[0] == "systemd-run"; wrapped != scope {
				t.Fatalf("%s: scope=%v なのに %v", name, scope, cmd.Args)
			}
		}
	}
	if args, _ := (codexDriver{}).Argv(PermCLI); strings.Join(args, " ") != "app-server" {
		t.Fatalf("Codex の起こし方が違う: %v", args)
	}
}

// ---------------------------------------------------------------- 承認の中身（Phase 3.6 の outer gate の指摘 4）

// **起こした場所の外で走らせようとする承認は、断らずに印を付けて見せる**（D-030。決めるのは本人）。
func TestACommandApprovalOutsideTheStartPlaceIsShownWithAMark(t *testing.T) {
	db := newDB(t)
	s, _, logPath := wireCodex(t, db, "CAMP_FAKE_CODEX_ASKCWD=/etc")
	rec := startCodexHere(t, s, db)
	codexNoErr(t, s.Input(rec.ID, "ask"))
	waitFor(t, 5*time.Second, func() bool { return len(pending(t, s, rec.ID)) == 1 })
	w, _ := s.Waiting(rec.ID)
	if !strings.Contains(w[0].Detail, `"outside":true`) {
		t.Fatalf("起こした場所の外なのに印が無い: %s", w[0].Detail)
	}
	codexNoErr(t, s.Approve(rec.ID, w[0].RequestID, "allow", ""))
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	if decisions(t, logPath)["0"] != "accept" {
		t.Fatal("許したのに届いていない")
	}
}

// 何を・どこで・どのスレッドで、が揃わない承認は出さない。外なら印を付けて出す。
func TestACommandApprovalMustSayWhatWhereAndWhichThread(t *testing.T) {
	cs := newCodexState()
	cs.thread, cs.root = "t", "/w"
	for name, params := range map[string]string{
		"command 無し": `{"threadId":"t","cwd":"/w"}`,
		"command が空": `{"threadId":"t","cwd":"/w","command":" "}`,
		"cwd 無し":     `{"threadId":"t","command":"x"}`,
		"cwd が相対":    `{"threadId":"t","command":"x","cwd":"w"}`,
		"別のスレッド":     `{"threadId":"u","command":"x","cwd":"/w"}`,
	} {
		ev := cs.classify([]byte(`{"id":1,"method":"item/commandExecution/requestApproval","params":` + params + `}`))
		if ev.ask != nil || !strings.Contains(joined(ev.replies), "decline") {
			t.Fatalf("%s の承認を画面に出した", name)
		}
	}
	for cwd, outside := range map[string]bool{"/w/sub": false, "/w/../etc": true} {
		ev := cs.classify([]byte(`{"id":2,"method":"item/commandExecution/requestApproval","params":` +
			`{"threadId":"t","command":"x","cwd":"` + cwd + `"}}`))
		if ev.ask == nil {
			t.Fatalf("%s の揃った承認を断った: %s", cwd, ev.err)
		}
		if got := strings.Contains(string(ev.ask.Detail), `"outside":true`); got != outside {
			t.Fatalf("%s: 外の印=%v（%v のはず）", cwd, got, outside)
		}
	}
}

// ---------------------------------------------------------------- 後始末（Phase 3.6 の outer gate の指摘 1）

// **scope に残ったものを数えて止める。止め切れなければ数を返す。**
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

// filepath を使っている箇所（テストの中で置き場を作る）が残っているかの確かめ用。
var _ = filepath.Join
