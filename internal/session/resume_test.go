package session

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 終わったセッションの続きから起こす（M48、2026-09-13）。
//
// **実測でこうなっている**（2026-09-13、`dev/scripts/probe_claude_resume.py`・
// `probe_codex_resume.py`）:
//
//	Claude  `-p --resume <id>`        文脈が続き、session_id も記録の JSONL も元のまま
//	Codex   `thread/resume {threadId}` 文脈が続き、スレッド id も rollout も元のまま
//
// だから台帳は**新しい行を作り**、エージェント側の id は元の行から引き継いで渡す。
// 元の行は「どう終わったか」ごとそのまま残す（上書きしない）。

// 続きから起こすと、元の行の id をエージェントへ渡し、新しい行に元を控える。
func TestAResumedSessionCarriesTheAgentsOwnID(t *testing.T) {
	bin, argsLog := fakeClaudePerm(t)
	db := newDB(t)
	s := New(db)
	attach(t, s, bin)

	rec, err := s.StartWith("test", "", allowHere(t, db), AgentClaude, PermCLI)
	codexNoErr(t, err)
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	// Claude は最初の入力のあとに system/init で名乗る。名乗るまで続きから起こせない。
	codexNoErr(t, s.Input(rec.ID, "go"))
	waitFor(t, 5*time.Second, func() bool { r, _ := get(db, rec.ID); return r.AgentSessionID == "fake-p" })
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	codexNoErr(t, s.Stop(rec.ID, StopTerminate))
	waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateExited })

	next, err := s.ResumeWith("test", rec.ID)
	codexNoErr(t, err)
	waitFor(t, 5*time.Second, func() bool { return state(t, db, next.ID) == StateIdle })

	if next.ID == rec.ID {
		t.Fatal("元の行を使い回した（新しい行のはず）")
	}
	if next.ResumedFrom != rec.ID {
		t.Fatalf("続きの元が %q（%s のはず）", next.ResumedFrom, rec.ID)
	}
	// **エージェント側の id を引数で渡している。**
	b, _ := os.ReadFile(argsLog)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, "--resume fake-p") {
		t.Fatalf("--resume を渡していない: %s", last)
	}
	// **元の行から続きを引ける。** 画面が「生き返った」ように見せるのに使う。
	if r, _ := get(db, rec.ID); r.ResumedBy != next.ID {
		t.Fatalf("元の行から続きを引けない: %q", r.ResumedBy)
	}
	// **元の行の終わり方は残る。** 生き返らせて使い回すと、ここが消える。
	if r, _ := get(db, rec.ID); r.State != StateExited || r.EndCause != EndUserStop {
		t.Fatalf("元の行が書き換わった: %s / %s", r.State, r.EndCause)
	}
	codexNoErr(t, s.Stop(next.ID, StopTerminate))
	waitFor(t, 10*time.Second, func() bool { return state(t, db, next.ID) == StateExited })
}

// 走っているもの・エージェント側の id を名乗らないまま終わったもの・無い行は続けない。
func TestOnlyAFinishedNamedSessionIsResumed(t *testing.T) {
	bin, _ := fakeClaudePerm(t)
	db := newDB(t)
	s := New(db)
	attach(t, s, bin)

	rec, err := s.StartWith("test", "", allowHere(t, db), AgentClaude, PermCLI)
	codexNoErr(t, err)
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	// **走っているものは続けない。** 同じ会話を2つ開くと、Claude は「別の端末で走っている」
	// として拒み、Codex は読み込み済みのスレッドへの上書きを無視する。
	if _, err := s.ResumeWith("test", rec.ID); err == nil ||
		!strings.Contains(err.Error(), "まだ終わっていない") {
		t.Fatalf("走っているものを続けた: %v", err)
	}
	codexNoErr(t, s.Stop(rec.ID, StopTerminate))
	waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateExited })

	// 一度も話していないので、Claude は session_id を名乗らないまま終わっている。
	if _, err := s.ResumeWith("test", rec.ID); err == nil ||
		!strings.Contains(err.Error(), "id を名乗らないまま") {
		t.Fatalf("id の無い行を続けた: %v", err)
	}
	if _, err := s.ResumeWith("test", "nope"); err == nil {
		t.Fatal("無い行を続けた")
	}
}

// **続きからを名乗らない実行面へは頼まない。** 読まれずに落ちると、続きのつもりで
// 新しい会話が始まり、本人は気づけない（画面には起きたとしか出ない）。
func TestAnOldExecutionSideIsNotAskedToResume(t *testing.T) {
	db := newDB(t)
	s := New(db)
	sock := filepath.Join(t.TempDir(), "a.sock")
	c, err := s.Listen(sock, "", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	go c.Serve()
	t.Cleanup(func() { c.Close() })

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	// **駆動器を名乗らない古い実行面**（Phase 3.7 より前）。campd は手元の駆動器から補うが、
	// そのとき Resume は落とす（control.go の info）。
	line, err := json.Marshal(Msg{T: MsgHello, Version: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return s.AgentConnected() })

	// 続きから起こせる形の行を、台帳に直に作る（子は起こさない）。
	cwd := allowHere(t, db)
	codexNoErr(t, insert(db, Record{ID: "old1", Agent: AgentClaude, Perm: PermCLI, Cwd: cwd,
		State: StateStarting, RequestedBy: "test", CreatedAt: now(), UpdatedAt: now()}))
	codexNoErr(t, setClaudeID(db, "old1", "sid-1"))
	codexNoErr(t, finish(db, "old1", 0, "", EndUserStop, true))

	if _, err := s.ResumeWith("test", "old1"); err == nil ||
		!strings.Contains(err.Error(), "続きから起こせない") {
		t.Fatalf("古い実行面へ続きを頼んだ: %v", err)
	}
}

// Codex は `thread/resume` で同じスレッドを続ける。**cwd は渡さない**（元のスレッドのもの）。
func TestCodexResumesTheSameThread(t *testing.T) {
	db := newDB(t)
	s, _, logPath := wireCodex(t, db)

	rec, err := s.StartWith("test", "", allowHere(t, db), AgentCodex, PermCLI)
	codexNoErr(t, err)
	waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	// Codex は握手のうちにスレッド id が決まるので、話さなくても名乗っている。
	waitFor(t, 10*time.Second, func() bool { r, _ := get(db, rec.ID); return r.AgentSessionID != "" })
	src, _ := get(db, rec.ID)
	codexNoErr(t, s.Stop(rec.ID, StopTerminate))
	waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateExited })

	next, err := s.ResumeWith("test", rec.ID)
	codexNoErr(t, err)
	waitFor(t, 10*time.Second, func() bool { return state(t, db, next.ID) == StateIdle })

	res := sent(t, logPath, "thread/resume")
	if len(res) != 1 {
		t.Fatalf("thread/resume が %d 回", len(res))
	}
	p, _ := res[0]["params"].(map[string]any)
	if p["threadId"] != src.AgentSessionID {
		t.Fatalf("続けたスレッドが %v（%s のはず）", p["threadId"], src.AgentSessionID)
	}
	// 続きのときは thread/start を投げない（新しい会話を始めてしまう）。
	if starts := sent(t, logPath, "thread/start"); len(starts) != 1 {
		t.Fatalf("thread/start が %d 回（最初の1回だけのはず）", len(starts))
	}
	codexNoErr(t, s.Stop(next.ID, StopTerminate))
	waitFor(t, 10*time.Second, func() bool { return state(t, db, next.ID) == StateExited })
}

// **別のスレッドを続けたら話し始めない。** 別の会話の続きを本人に見せることになる。
func TestCodexResumingAnotherThreadIsRefused(t *testing.T) {
	db := newDB(t)
	s, _, _ := wireCodex(t, db, "CAMP_FAKE_CODEX_RESUMEID=thr-other")

	rec, err := s.StartWith("test", "", allowHere(t, db), AgentCodex, PermCLI)
	codexNoErr(t, err)
	waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	waitFor(t, 10*time.Second, func() bool { r, _ := get(db, rec.ID); return r.AgentSessionID != "" })
	codexNoErr(t, s.Stop(rec.ID, StopTerminate))
	waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateExited })

	next, err := s.ResumeWith("test", rec.ID)
	codexNoErr(t, err)
	waitFor(t, 10*time.Second, func() bool { return state(t, db, next.ID) == StateExited })
	r, _ := get(db, next.ID)
	if r.EndCause != EndStartFailed || !strings.Contains(r.ExitReason, "別のスレッド") {
		t.Fatalf("別の会話の続きを始めてしまった: %s / %s", r.EndCause, r.ExitReason)
	}
}
