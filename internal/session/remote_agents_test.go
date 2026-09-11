package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// M42: 向こうのホストでも全エージェント。偽の ssh（remote_test.go）の上で Codex を向こうに起こし、
// しるし（CAMP_SESSION）で別のセッションへ逃げた残りを止めることを縛る。

// newRemoteCodexRig は向こうに偽の app-server を置いた接続先を用意する。向こうの置き場は
// $CODEX_HOME（向こうの sh が名乗る）で、偽物もそこで起きたと名乗る（env で上書きできる）。
func newRemoteCodexRig(t *testing.T, env ...string) (*remoteRig, string) {
	t.Helper()
	home, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("CODEX_HOME", home)
	bin, logPath := fakeCodex(t, append([]string{"CAMP_FAKE_CODEX_HOMEOUT=" + home}, env...)...)
	r := newRemoteRigWith(t, remoteClaudeBody, func(a *Agent) {
		// 実行面が Codex を名乗るため（手元の実体。向こうでは台帳の場所を使う）。
		a.Codex, a.CodexHome, a.CodexWithoutScope = bin, home, true
	})
	codexNoErr(t, SetAgentPath(r.db, "far", AgentCodex, bin))
	return r, logPath
}

func TestCodexStartsOnAnotherHostWithAPerm(t *testing.T) {
	r, logPath := newRemoteCodexRig(t)
	rec, err := r.s.StartWith("test", "far", r.root, AgentCodex, PermAsk)
	codexNoErr(t, err)
	got := r.waitState(t, rec.ID, StateIdle)
	if got.Agent != AgentCodex || got.Host != "far" || got.RemotePID == 0 {
		t.Fatalf("向こうの Codex の行が違う: %+v", got)
	}
	waitFor(t, 5*time.Second, func() bool { r, _ := get(r.db, rec.ID); return r.AgentSessionID == "thr-fake-1" })
	starts := sent(t, logPath, "thread/start")
	if len(starts) != 1 {
		t.Fatalf("thread/start が %d 回", len(starts))
	}
	if p, _ := starts[0]["params"].(map[string]any); p["approvalPolicy"] != "untrusted" || p["cwd"] != r.root {
		t.Fatalf("向こうへ確認の度合い・場所が渡っていない: %v", p)
	}
	codexNoErr(t, r.s.Input(rec.ID, "ask"))
	waitFor(t, 5*time.Second, func() bool { return len(pending(t, r.s, rec.ID)) == 1 })
	codexNoErr(t, r.s.Approve(rec.ID, "0", "allow", ""))
	waitFor(t, 5*time.Second, func() bool { return liveState(r.s, rec.ID) == StateIdle })
	if decisions(t, logPath)["0"] != "accept" {
		t.Fatal("許したのに向こうへ届いていない")
	}
	codexNoErr(t, r.s.Stop(rec.ID, StopTerminate))
	r.waitState(t, rec.ID, StateExited)
}

// **向こうでも、本人の置き場で起きたかを照らす。** 名乗った置き場と違えば話し始めない。
func TestARemoteCodexInAnotherHomeIsRefused(t *testing.T) {
	r, _ := newRemoteCodexRig(t, "CAMP_FAKE_CODEX_HOMEOUT=/somewhere/else")
	rec, err := r.s.StartWith("test", "far", r.root, AgentCodex, PermCLI)
	codexNoErr(t, err)
	end := r.waitState(t, rec.ID, StateExited)
	if end.EndCause != EndStartFailed || !strings.Contains(end.ExitReason, "置き場") {
		t.Fatalf("別の置き場で起きたのに話し始めた: %s / %s", end.EndCause, end.ExitReason)
	}
}

// escapingClaudeBody は、別のセッションへ逃がした孫を残し、話しかけられると自分を SIGKILL する。
// **Codex が異常終了したときに、Codex が起こしたコマンドが ppid 1 で残る**（2026-09-12、`rp` で実測）形。
const escapingClaudeBody = `#!/bin/sh
echo $$ > @DIR@/claude.pid
setsid sleep 300 </dev/null >/dev/null 2>&1 &
echo $! > @DIR@/escaped.pid
echo '{"type":"system","subtype":"init","session_id":"far-1"}'
while IFS= read -r line; do
  case "$line" in
    *'"type":"user"'*) kill -9 $$ ;;
  esac
done
`

// **別のセッションへ逃げた残りは、しるしで探して止める。** しるしの無い同じユーザーのプロセスは撃たない。
func TestLeftoversThatEscapedTheSessionAreFoundByTheMark(t *testing.T) {
	bystander := exec.Command("setsid", "sleep", "300")
	codexNoErr(t, bystander.Start())
	defer func() { bystander.Process.Kill(); bystander.Wait() }()

	r := newRemoteRig(t, escapingClaudeBody)
	rec := r.start(t, r.root)
	got := r.waitState(t, rec.ID, StateIdle)
	escaped := r.pidFrom(t, "escaped.pid")
	defer killIfAlive(escaped)
	if sessionOf(escaped) == got.RemotePID || sessionOf(escaped) == 0 {
		t.Fatalf("孫が別のセッションへ逃げていない（sid %d）。確かめたいことを確かめられない", sessionOf(escaped))
	}
	// 手元からは向こうへ入り直せない（reapScript は走らない）ようにして、**向こうの sh の終わりの
	// 後始末だけで**止まることを見る（reapScript の側は下のテスト）。
	codexNoErr(t, os.WriteFile(filepath.Join(r.dir, "unreachable"), nil, 0o600))
	codexNoErr(t, r.s.Input(rec.ID, "go"))
	waitFor(t, 5*time.Second, func() bool { return !procAlive(escaped) })
	if !procAlive(bystander.Process.Pid) {
		t.Fatal("しるしの無いプロセスまで止めた")
	}
}

// permClaudeBody は向こうの claude。受け取った引数を書き、渡された --permission-mode を最初の入力の
// あとの system/init で名乗る（本物と同じく、渡さなければ default）。
const permClaudeBody = `#!/bin/sh
printf '%s\n' "$*" > @DIR@/claude-args
mode=default
while [ $# -gt 0 ]; do
  case "$1" in
    --permission-mode) mode=$2; shift 2 ;;
    *) shift ;;
  esac
done
while IFS= read -r line; do
  case "$line" in
    *'"type":"user"'*)
      echo '{"type":"system","subtype":"init","session_id":"far-p","permissionMode":"'"$mode"'"}'
      echo '{"type":"result","subtype":"success","session_id":"far-p"}' ;;
  esac
done
`

// **向こうでも確認の度合いが駆動器の引数で渡り、起こしたときに照らされる。**
func TestClaudeStartsOnAnotherHostWithAPerm(t *testing.T) {
	r := newRemoteRig(t, permClaudeBody)
	rec, err := r.s.StartWith("test", "far", r.root, AgentClaude, PermEdits)
	codexNoErr(t, err)
	r.waitState(t, rec.ID, StateIdle)
	b, _ := os.ReadFile(filepath.Join(r.dir, "claude-args"))
	if !strings.Contains(string(b), "--permission-mode acceptEdits") {
		t.Fatalf("向こうへ駆動器の引数が渡っていない: %q", b)
	}
	codexNoErr(t, r.s.Input(rec.ID, "go"))
	waitFor(t, 5*time.Second, func() bool { g, _ := get(r.db, rec.ID); return g.AgentSessionID == "far-p" })
	waitFor(t, 5*time.Second, func() bool { return liveState(r.s, rec.ID) == StateIdle })
	codexNoErr(t, r.s.Stop(rec.ID, StopTerminate))
	r.waitState(t, rec.ID, StateExited)
}

// **向こうの sh が居なくなっていても**、reapScript はしるしを持つものを止める（campd の再起動後の
// 孤児の始末でも、しるしは台帳のセッション id から渡る）。しるしの違うものは撃たない。
func TestTheReaperStopsWhatCarriesTheMarkEvenAfterTheShellIsGone(t *testing.T) {
	sess := newID()
	start := func(mark string) *exec.Cmd {
		c := exec.Command("setsid", "sleep", "300")
		c.Env = append(os.Environ(), "CAMP_SESSION="+mark)
		codexNoErr(t, c.Start())
		go c.Wait()
		return c
	}
	mine, other := start(sess), start(newID())
	defer func() { mine.Process.Kill(); other.Process.Kill() }()

	out, err := exec.Command("sh", "-c", reapScript, "camp", "999999", "-", "-", "-", sess).Output()
	codexNoErr(t, err)
	if res, detail := parseReap(string(out)); res != RemoteKilled {
		t.Fatalf("しるしを持つものを止めていない: %s %s（%s）", res, detail, out)
	}
	waitFor(t, 5*time.Second, func() bool { return !procAlive(mine.Process.Pid) })
	if !procAlive(other.Process.Pid) {
		t.Fatal("しるしの違うものまで止めた")
	}
	// しるしの形でないものは使わない（向こうの grep に渡る）。
	out, _ = exec.Command("sh", "-c", reapScript, "camp", "999999", "-", "-", "-", ".*").Output()
	if res, _ := parseReap(string(out)); res != RemoteGone || !procAlive(other.Process.Pid) {
		t.Fatalf("しるしの形でないもので探した: %s", out)
	}
}

// 名乗りは版 3 だけを採る。末尾の key=value は読むが、形の崩れたものは採らない。
func TestTheHeaderIsVersionThree(t *testing.T) {
	line := "CAMP-REMOTE\t3\tn1\t42\t7\tb\t-\t/r\t/r/c\thome=/h\thomereal=-"
	o, ok := parseHeader(line, "far", "n1")
	if !ok || o.PID != 42 || o.Home != "/h" || o.HomeReal != "" || o.Cwd != "/r/c" {
		t.Fatalf("版 3 の名乗りを読めない: %+v %v", o, ok)
	}
	for _, bad := range []string{
		"CAMP-REMOTE\t2\tn1\t42\t7\tb\t-\t/r\t/r/c",
		"CAMP-REMOTE\t3\tn2\t42\t7\tb\t-\t/r\t/r/c",
		"CAMP-REMOTE\t3\tn1\t42\t7\tb\t-\t/r\t/r/c\tbroken",
	} {
		if _, ok := parseHeader(bad, "far", "n1"); ok {
			t.Fatalf("採ってはいけない名乗りを採った: %q", bad)
		}
	}
}

var _ = syscall.SIGKILL
