package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 確認の度合い（M41）。5つとも「渡したもの」と「照らすもの」を、偽の claude・偽の app-server で縛る。
// **起こしたときに1回だけ照らし、違えば止める。途中の変化は止めずに記録する**（本人の決定）。

// fakeClaudePerm は、渡された --permission-mode を最初の入力のあとの system/init で名乗る偽の claude
// （本物と同じく、渡さなければ default）。受け取った引数を argsLog に書く。
// CAMP_FAKE_CLAUDE_MODE があれば、それを名乗る（違う度合いで起きたふり）。CAMP_FAKE_CLAUDE_MODE2 が
// あれば、2ターン目からそれを名乗る（途中で変わったふり）。
func fakeClaudePerm(t *testing.T) (bin, argsLog string) {
	t.Helper()
	dir := t.TempDir()
	argsLog = filepath.Join(dir, "args")
	bin = filepath.Join(dir, "fake-claude")
	body := `#!/bin/sh
printf '%s\n' "$*" >> ` + shQuote(argsLog) + `
mode=default
while [ $# -gt 0 ]; do
  case "$1" in
    --permission-mode) mode=$2; shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$CAMP_FAKE_CLAUDE_MODE" ] && mode=$CAMP_FAKE_CLAUDE_MODE
n=0
while IFS= read -r line; do
  case "$line" in
    *'"type":"user"'*)
      n=$((n+1))
      m=$mode
      if [ -n "$CAMP_FAKE_CLAUDE_MODE2" ] && [ $n -ge 2 ]; then m=$CAMP_FAKE_CLAUDE_MODE2; fi
      echo '{"type":"system","subtype":"init","session_id":"fake-p","permissionMode":"'"$m"'"}'
      echo '{"type":"result","subtype":"success","session_id":"fake-p"}' ;;
  esac
done
`
	codexNoErr(t, os.WriteFile(bin, []byte(body), 0o755))
	return bin, argsLog
}

func TestEachPermIsPassedToClaudeAndChecked(t *testing.T) {
	bin, argsLog := fakeClaudePerm(t)
	db := newDB(t)
	s := New(db)
	attach(t, s, bin)
	for _, perm := range allPerms {
		rec, err := s.StartWith("test", "", allowHere(t, db), AgentClaude, perm)
		codexNoErr(t, err)
		waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
		codexNoErr(t, s.Input(rec.ID, "go"))
		waitFor(t, 5*time.Second, func() bool { r, _ := get(db, rec.ID); return r.AgentSessionID == "fake-p" })
		waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
		if r, _ := get(db, rec.ID); r.Perm != perm {
			t.Fatalf("%s: 台帳の度合いが %q", perm, r.Perm)
		}
		b, _ := os.ReadFile(argsLog)
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		last := lines[len(lines)-1]
		if want, ok := claudeModes[perm]; ok != strings.Contains(last, "--permission-mode "+want) ||
			(perm == PermCLI && strings.Contains(last, "--permission-mode")) {
			t.Fatalf("%s: 渡した引数が違う: %s", perm, last)
		}
		codexNoErr(t, s.Stop(rec.ID, StopTerminate))
		waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateExited })
		if r, _ := get(db, rec.ID); r.EndCause != EndUserStop {
			t.Fatalf("%s: 照らして止めてしまった: %s / %s", perm, r.EndCause, r.ExitReason)
		}
	}
}

// **頼んだ度合いで起きていなければ止める。** 「起こせなかった」と書き、理由を監査に残す。
func TestAClaudeStartedWithAnotherPermIsStopped(t *testing.T) {
	bin, _ := fakeClaudePerm(t)
	t.Setenv("CAMP_FAKE_CLAUDE_MODE", "bypassPermissions")
	db := newDB(t)
	s := New(db)
	attach(t, s, bin)
	rec, err := s.StartWith("test", "", allowHere(t, db), AgentClaude, PermAsk)
	codexNoErr(t, err)
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	codexNoErr(t, s.Input(rec.ID, "go"))
	waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateExited })
	if r, _ := get(db, rec.ID); r.EndCause != EndStartFailed {
		t.Fatalf("違う度合いで起きたのに %s / %s と書いた", r.EndCause, r.ExitReason)
	}
	if !auditHas(t, db, "session.perm", "bypassPermissions") {
		t.Fatal("止めた理由が監査に無い")
	}
}

// cli は照らさない（本人の設定のまま。何を名乗っても止めない）。
func TestTheCLIPermIsNotChecked(t *testing.T) {
	bin, _ := fakeClaudePerm(t)
	t.Setenv("CAMP_FAKE_CLAUDE_MODE", "bypassPermissions")
	db := newDB(t)
	s := New(db)
	attach(t, s, bin)
	rec, err := s.StartWith("test", "", allowHere(t, db), AgentClaude, PermCLI)
	codexNoErr(t, err)
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	codexNoErr(t, s.Input(rec.ID, "go"))
	waitFor(t, 5*time.Second, func() bool { r, _ := get(db, rec.ID); return r.AgentSessionID == "fake-p" })
	time.Sleep(200 * time.Millisecond)
	if st := state(t, db, rec.ID); st != StateIdle {
		t.Fatalf("cli なのに照らして止めた: %s", st)
	}
}

// **途中で変わっても止めない。** 記録として残す（CLI では止まらない。D-030）。
func TestAClaudePermChangeMidSessionIsRecordedNotStopped(t *testing.T) {
	bin, _ := fakeClaudePerm(t)
	t.Setenv("CAMP_FAKE_CLAUDE_MODE2", "plan")
	db := newDB(t)
	s := New(db)
	attach(t, s, bin)
	rec, err := s.StartWith("test", "", allowHere(t, db), AgentClaude, PermAsk)
	codexNoErr(t, err)
	for i := 0; i < 2; i++ {
		waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
		codexNoErr(t, s.Input(rec.ID, "go"))
		waitFor(t, 5*time.Second, func() bool { r, _ := get(db, rec.ID); return r.AgentSessionID == "fake-p" })
	}
	waitFor(t, 5*time.Second, func() bool { return auditHasSilent(db, "session.note", "default → plan") })
	if st := state(t, db, rec.ID); st == StateExited {
		t.Fatal("途中で変わっただけで止めた")
	}
}

func TestEachPermIsPassedToCodexAndChecked(t *testing.T) {
	db := newDB(t)
	s, _, logPath := wireCodex(t, db)
	for i, perm := range allPerms {
		rec, err := s.StartWith("test", "", allowHere(t, db), AgentCodex, perm)
		codexNoErr(t, err)
		waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
		starts := sent(t, logPath, "thread/start")
		if len(starts) != i+1 {
			t.Fatalf("%s: thread/start が %d 回", perm, len(starts))
		}
		p, _ := starts[i]["params"].(map[string]any)
		for k, v := range codexPerms[perm] {
			if p[k] != v {
				t.Fatalf("%s: thread/start の %s が %v（%v のはず）: %v", perm, k, p[k], v, p)
			}
		}
		for _, k := range []string{"approvalPolicy", "approvalsReviewer", "sandbox"} {
			if _, sentIt := p[k]; sentIt && codexPerms[perm][k] == nil {
				t.Fatalf("%s: 頼んでいない %s を渡した: %v", perm, k, p)
			}
		}
		codexNoErr(t, s.Stop(rec.ID, StopTerminate))
		waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateExited })
	}
}

// 知らない度合いは campd が断る。表示だけの legacy は書けない。（向こうのホストでの度合いは M42 で
// 選べるようにした。remote_agents_test.go）
func TestAnUnknownOrRemotePermIsRefused(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	// 断るのは campd が値を照らすところ（実行面が名乗っているかを見る前）。
	if _, err := s.StartWith("test", "", allowHere(t, db), AgentClaude, "yolo"); err == nil ||
		!strings.Contains(err.Error(), "知らない確認の度合い") {
		t.Fatalf("知らない度合いを通した: %v", err)
	}
	if err := insert(db, Record{ID: "lg", Agent: AgentCodex, Perm: PermLegacy, Cwd: "/w",
		State: StateStarting, RequestedBy: "test", CreatedAt: now(), UpdatedAt: now()}); err == nil {
		t.Fatal("表示だけの legacy を台帳に書けた")
	}
}

// sandbox も照らす（渡したものだけ）。
func TestACodexStartedInAnotherSandboxIsRefused(t *testing.T) {
	db := newDB(t)
	s, _, _ := wireCodex(t, db, "CAMP_FAKE_CODEX_SANDBOX=readOnly")
	rec, err := s.StartWith("test", "", allowHere(t, db), AgentCodex, PermEdits)
	codexNoErr(t, err)
	waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateExited })
	r, _ := get(db, rec.ID)
	if r.EndCause != EndStartFailed || !strings.Contains(r.ExitReason, "sandbox が readOnly") {
		t.Fatalf("違う sandbox で起きたのに話し始めた: %s / %s", r.EndCause, r.ExitReason)
	}
}

// **頼んだ度合いで起きていなければ、話し始めない。**
func TestACodexStartedWithAnotherPermIsRefused(t *testing.T) {
	db := newDB(t)
	s, _, _ := wireCodex(t, db, "CAMP_FAKE_CODEX_POLICY=never")
	rec, err := s.StartWith("test", "", allowHere(t, db), AgentCodex, PermAsk)
	codexNoErr(t, err)
	waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateExited })
	r, _ := get(db, rec.ID)
	if r.EndCause != EndStartFailed || !strings.Contains(r.ExitReason, "approvalPolicy が never") {
		t.Fatalf("違う度合いで起きたのに話し始めた: %s / %s", r.EndCause, r.ExitReason)
	}
}
