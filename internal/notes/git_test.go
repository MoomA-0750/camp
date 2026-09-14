package notes

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 本物の git と、GitHub 役の裸リポジトリで確かめる。

func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

type repo struct {
	origin, work, other string
	disk                *Disk
	git                 *Git
	known               []string // campd が覚える Camp の commit（試験では commit と push の結果から足す）
}

// commit は Camp として commit し、作った commit を覚える。
func (r *repo) commit(t *testing.T, entries ...Entry) CommitResult {
	t.Helper()
	res, err := r.git.Commit(entries)
	if err != nil {
		t.Fatal(err)
	}
	if res.Commit != "" && len(res.Mismatch) == 0 {
		r.known = append(r.known, res.Commit)
	}
	return res
}

// push は覚えた commit で push し、作った merge も覚える。
func (r *repo) push(t *testing.T) PushResult {
	t.Helper()
	res, err := r.git.Push(r.known)
	if err != nil {
		t.Fatal(err)
	}
	if res.MergeSHA != "" {
		r.known = append(r.known, res.MergeSHA)
	}
	return res
}

// newRepo は origin（裸）・work（Camp が書く作業コピー）・other（ほかの端末）を作る。
func newRepo(t *testing.T) *repo {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	base := t.TempDir()
	r := &repo{origin: filepath.Join(base, "origin.git"), work: filepath.Join(base, "work"), other: filepath.Join(base, "other")}
	sh(t, base, "init", "-q", "--bare", "-b", "master", r.origin)
	sh(t, base, "clone", "-q", r.origin, r.work)
	for _, p := range []string{"Human/Logs", "Inbox"} {
		os.MkdirAll(filepath.Join(r.work, p), 0o755)
	}
	put(t, r.work, "Human/Logs/a.md", "a\n", 0o644)
	put(t, r.work, "Human/Logs/b.md", "b\n", 0o644)
	sh(t, r.work, "add", ".")
	sh(t, r.work, "commit", "-qm", "init")
	sh(t, r.work, "push", "-q", "origin", "master")
	sh(t, base, "clone", "-q", r.origin, r.other)
	d, err := OpenDisk(r.work)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	r.disk, r.git = d, &Git{Disk: d}
	return r
}

// write は Camp として書き、待ち行の1件を返す。
func (r *repo) write(t *testing.T, id int64, rel, body string) Entry {
	t.Helper()
	base := ""
	create := true
	if b, err := os.ReadFile(filepath.Join(r.work, rel)); err == nil {
		base, create = Sum(b), false
	}
	res, err := r.disk.Write(rel, base, []byte(body), create, false)
	if err != nil || (res.Status != StatusWritten && res.Status != StatusSame) {
		t.Fatalf("write %s: %+v %v", rel, res, err)
	}
	return Entry{ID: id, Path: rel, SHA: res.DiskSHA}
}

func TestCommitTakesOnlyCampPathsAndLeavesOthersAlone(t *testing.T) {
	r := newRepo(t)
	// 名前にグロブの文字を含むノートと、それに当たってしまう名前のノート。
	put(t, r.work, "Human/Logs/x1.md", "x1\n", 0o644)
	sh(t, r.work, "add", "Human/Logs/x1.md")
	sh(t, r.work, "commit", "-qm", "x1")
	put(t, r.work, "Human/Logs/x1.md", "エージェントの x1\n", 0o644)
	// エージェントの書きかけ: b.md を stage、新しいファイルを未追跡で。
	put(t, r.work, "Human/Logs/b.md", "エージェントが stage した\n", 0o644)
	sh(t, r.work, "add", "Human/Logs/b.md")
	put(t, r.work, "Inbox/agent.md", "未追跡\n", 0o644)

	e1 := r.write(t, 7, "Human/Logs/a.md", "a\n本人が書いた\n")
	// グロブの文字の名前は Camp では新しく作らない（wikilink に書けない）が、よそで作られたものは直せる。未追跡のまま。
	put(t, r.work, "Human/Logs/x[0-9].md", "よそで作った\n", 0o644)
	e2 := r.write(t, 8, "Human/Logs/x[0-9].md", "グロブの名前\n")
	e3 := r.write(t, 9, "Inbox/新しい.md", "新しいノート\n")

	res, err := r.git.Commit([]Entry{e1, e2, e3})
	if err != nil {
		t.Fatal(err)
	}
	if res.Commit == "" || len(res.Done) != 3 || len(res.Overwritten) != 0 || len(res.Mismatch) != 0 {
		t.Fatalf("%+v", res)
	}
	files := sh(t, r.work, "show", "--name-only", "--format=", "HEAD")
	want := "Human/Logs/a.md\nHuman/Logs/x[0-9].md\nInbox/新しい.md"
	if files != want && strings.ReplaceAll(files, "\"", "") != want {
		// git は日本語のパスを引用符と8進で出すことがあるので、-z でも確かめる。
		z := sh(t, r.work, "-c", "core.quotepath=false", "show", "--name-only", "--format=", "HEAD")
		if z != want {
			t.Fatalf("commit に入ったファイルが違う:\n%s", z)
		}
	}
	msg := sh(t, r.work, "log", "-1", "--format=%(trailers:key=Camp-Commit,valueonly)")
	if msg != "7,8,9" {
		t.Fatalf("トレーラー: %q", msg)
	}
	// エージェントの stage と書きかけはそのまま。
	st := sh(t, r.work, "-c", "core.quotepath=false", "status", "--porcelain")
	for _, w := range []string{"M  Human/Logs/b.md", " M Human/Logs/x1.md", "?? Inbox/agent.md"} {
		if !strings.Contains(st, w) {
			t.Errorf("エージェントの変更が変わった（%q が無い）:\n%s", w, st)
		}
	}
}

func TestCommitSkipsWhatAnotherWriterOverwrote(t *testing.T) {
	r := newRepo(t)
	e := r.write(t, 1, "Human/Logs/a.md", "本人の版\n")
	// commit を待つ間にエージェントが上書きした。
	put(t, r.work, "Human/Logs/a.md", "エージェントの版\n", 0o644)
	gone := r.write(t, 2, "Human/Logs/b.md", "本人の b\n")
	os.Remove(filepath.Join(r.work, "Human/Logs/b.md"))

	res, err := r.git.Commit([]Entry{e, gone})
	if err != nil {
		t.Fatal(err)
	}
	if res.Commit != "" || len(res.Done) != 0 || len(res.Overwritten) != 2 {
		t.Fatalf("上書きされたものを commit した: %+v", res)
	}
	if res.Overwritten[0].SHA != Sum([]byte("エージェントの版\n")) || res.Overwritten[1].SHA != "" {
		t.Fatalf("%+v", res.Overwritten)
	}
	if n := sh(t, r.work, "rev-list", "--count", "HEAD"); n != "1" {
		t.Fatalf("commit が増えた: %s", n)
	}
}

func TestCommitAlreadyInHeadIsDone(t *testing.T) {
	r := newRepo(t)
	e := r.write(t, 3, "Human/Logs/a.md", "同じ\n")
	sh(t, r.work, "commit", "-qam", "エージェントが先に commit した")
	res, err := r.git.Commit([]Entry{e})
	if err != nil || res.Commit != "" || len(res.Done) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestCommitWaitsDuringMergeOrDetachedHead(t *testing.T) {
	r := newRepo(t)
	e := r.write(t, 1, "Human/Logs/a.md", "本人\n")
	// detached HEAD
	sh(t, r.work, "checkout", "-q", "--detach")
	if _, err := r.git.Commit([]Entry{e}); !IsBusy(err) {
		t.Fatalf("detached HEAD で commit した: %v", err)
	}
	sh(t, r.work, "checkout", "-q", "master")
	// merge の途中
	gitDir := filepath.Join(r.work, ".git")
	os.WriteFile(filepath.Join(gitDir, "MERGE_HEAD"), []byte(sh(t, r.work, "rev-parse", "HEAD")+"\n"), 0o644)
	if _, err := r.git.Commit([]Entry{e}); !IsBusy(err) {
		t.Fatalf("merge の途中で commit した: %v", err)
	}
	os.Remove(filepath.Join(gitDir, "MERGE_HEAD"))
	if res, err := r.git.Commit([]Entry{e}); err != nil || res.Commit == "" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestPushOnlyWhenEverythingPendingIsCamps(t *testing.T) {
	r := newRepo(t)
	if res := r.push(t); res.Kind != PushNothing {
		t.Fatalf("%+v", res)
	}
	// エージェントの確認待ちの commit
	put(t, r.work, "Human/Logs/b.md", "エージェント\n", 0o644)
	sh(t, r.work, "commit", "-qam", "エージェントの commit")
	e := r.write(t, 1, "Human/Logs/a.md", "本人\n")
	r.commit(t, e)
	if res := r.push(t); res.Kind != PushAgentPending || res.Pending != 1 {
		t.Fatalf("エージェントの commit ごと出した: %+v", res)
	}
	if sh(t, r.origin, "rev-list", "--count", "master") != "1" {
		t.Fatal("GitHub 役に出た")
	}
	// エージェントの分が出たあとなら、Camp の分を出す。
	sh(t, r.work, "push", "-q", "origin", "HEAD~1:refs/heads/master")
	if res := r.push(t); res.Kind != PushDone {
		t.Fatalf("%+v", res)
	}
	if sh(t, r.origin, "rev-parse", "master") != sh(t, r.work, "rev-parse", "HEAD") {
		t.Fatal("出ていない")
	}
}

func TestPushMergesWhenGitHubMovedAndStopsOnConflict(t *testing.T) {
	r := newRepo(t)
	// ほかの端末が別のファイルを push した。
	put(t, r.other, "Human/Logs/b.md", "スマホで書いた\n", 0o644)
	sh(t, r.other, "commit", "-qam", "2609131200")
	sh(t, r.other, "push", "-q", "origin", "master")
	// 作業コピーには、関係ないファイルのエージェントの書きかけがある。
	put(t, r.work, "Inbox/agent.md", "書きかけ\n", 0o644)
	e := r.write(t, 1, "Human/Logs/a.md", "本人\n")
	r.commit(t, e)
	if res := r.push(t); res.Kind != PushDone || !res.Merged || res.MergeSHA == "" {
		t.Fatalf("取り込んで出していない: %+v", res)
	}
	if got, _ := os.ReadFile(filepath.Join(r.work, "Inbox/agent.md")); string(got) != "書きかけ\n" {
		t.Fatal("書きかけが消えた")
	}

	// 今度は同じ行をぶつける。
	sh(t, r.other, "pull", "-q", "--no-rebase", "origin", "master")
	put(t, r.other, "Human/Logs/a.md", "スマホの a\n", 0o644)
	sh(t, r.other, "commit", "-qam", "2609131300")
	sh(t, r.other, "push", "-q", "origin", "master")
	e = r.write(t, 2, "Human/Logs/a.md", "本人の a\n")
	r.commit(t, e)
	head := sh(t, r.work, "rev-parse", "HEAD")
	if res := r.push(t); res.Kind != PushConflict {
		t.Fatalf("%+v", res)
	}
	if r.git.inMerge() || sh(t, r.work, "rev-parse", "HEAD") != head {
		t.Fatal("戻していない")
	}
	if got, _ := os.ReadFile(filepath.Join(r.work, "Human/Logs/a.md")); string(got) != "本人の a\n" {
		t.Fatalf("本人の版が消えた: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(r.work, "Inbox/agent.md")); string(got) != "書きかけ\n" {
		t.Fatal("書きかけが消えた")
	}
}

func TestPushDoesNotMergeOverDirtyOverlap(t *testing.T) {
	r := newRepo(t)
	put(t, r.other, "Human/Logs/b.md", "スマホ\n", 0o644)
	sh(t, r.other, "commit", "-qam", "phone")
	sh(t, r.other, "push", "-q", "origin", "master")
	e := r.write(t, 1, "Human/Logs/a.md", "本人\n")
	r.commit(t, e)
	put(t, r.work, "Human/Logs/b.md", "エージェントの書きかけ\n", 0o644)
	if res := r.push(t); res.Kind != PushBehindDirty {
		t.Fatalf("%+v", res)
	}
	if got, _ := os.ReadFile(filepath.Join(r.work, "Human/Logs/b.md")); string(got) != "エージェントの書きかけ\n" {
		t.Fatal("書きかけが消えた")
	}
}

func TestPushHookRefusalIsNotMistakenForBehind(t *testing.T) {
	r := newRepo(t)
	hooks := filepath.Join(r.work, ".githooks")
	os.MkdirAll(hooks, 0o755)
	os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\n[ -n \"$SKIP_PUSH_GUARD\" ] && exit 0\necho 'pre-push: refusing to push -- credential written inline: パスワード: …' >&2\nexit 1\n"), 0o755)
	sh(t, r.work, "config", "core.hooksPath", hooks)
	t.Setenv("SKIP_PUSH_GUARD", "1") // Camp の経路では外れないこと
	e := r.write(t, 1, "Human/Logs/a.md", "本人\n")
	r.commit(t, e)
	if res := r.push(t); res.Kind != PushHook || !strings.Contains(res.Detail, "refusing") {
		t.Fatalf("%+v", res)
	}
	if sh(t, r.origin, "rev-list", "--count", "master") != "1" {
		t.Fatal("守りを抜けて出た")
	}
}

// Camp の commit を見分けるのは覚えた id。トレーラーを真似た commit・amend した commit は出さない（outer gate）。
func TestPushDoesNotTrustTheTrailerText(t *testing.T) {
	r := newRepo(t)
	e := r.write(t, 1, "Human/Logs/a.md", "本人\n")
	r.commit(t, e)
	// エージェントが Camp の commit に足し忘れを amend した。
	put(t, r.work, "Human/Logs/b.md", "エージェントが足した\n", 0o644)
	sh(t, r.work, "commit", "-q", "--amend", "--no-edit", "-a")
	if res := r.push(t); res.Kind != PushAgentPending {
		t.Fatalf("amend した commit を出した: %+v", res)
	}
	// トレーラーを真似た commit。
	sh(t, r.work, "reset", "-q", "--hard", "origin/master")
	put(t, r.work, "Human/Logs/b.md", "真似た\n", 0o644)
	sh(t, r.work, "commit", "-qam", "agent\n\nCamp-Commit: 1")
	if res := r.push(t); res.Kind != PushAgentPending {
		t.Fatalf("トレーラーを真似た commit を出した: %+v", res)
	}
	if sh(t, r.origin, "rev-list", "--count", "master") != "1" {
		t.Fatal("GitHub 役に出た")
	}
}

// ほかの merge の途中（エージェントが手で解いている）なら、戻さずに待つ。
func TestPushDoesNotAbortSomeoneElsesMerge(t *testing.T) {
	r := newRepo(t)
	put(t, r.other, "Human/Logs/a.md", "スマホの a\n", 0o644)
	sh(t, r.other, "commit", "-qam", "phone")
	sh(t, r.other, "push", "-q", "origin", "master")
	e := r.write(t, 1, "Human/Logs/b.md", "本人の b\n")
	r.commit(t, e)
	// エージェントが別の枝を merge してぶつかり、手で解いている途中。
	sh(t, r.work, "checkout", "-q", "-b", "side", "HEAD~1")
	put(t, r.work, "Human/Logs/b.md", "枝の b\n", 0o644)
	sh(t, r.work, "commit", "-qam", "side")
	sh(t, r.work, "checkout", "-q", "master")
	cmd := exec.Command("git", "-C", r.work, "merge", "side")
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	cmd.Run() // ぶつかる
	put(t, r.work, "Human/Logs/b.md", "エージェントが手で解いた\n", 0o644)
	if res := r.push(t); res.Kind != PushBusy {
		t.Fatalf("%+v", res)
	}
	if !r.git.inMerge() {
		t.Fatal("エージェントの merge を戻した")
	}
	if got, _ := os.ReadFile(filepath.Join(r.work, "Human/Logs/b.md")); string(got) != "エージェントが手で解いた\n" {
		t.Fatalf("手で解いた中身が消えた: %q", got)
	}
}

// 照らしたあとでエージェントが commit しても、出すのは照らした commit まで（outer gate の codex）。
func TestPushSendsOnlyWhatWasChecked(t *testing.T) {
	r := newRepo(t)
	e := r.write(t, 1, "Human/Logs/a.md", "本人\n")
	camp := r.commit(t, e).Commit
	beforePush = func() {
		put(t, r.work, "Human/Logs/b.md", "照らしたあとのエージェント\n", 0o644)
		sh(t, r.work, "commit", "-qam", "agent")
	}
	t.Cleanup(func() { beforePush = nil })
	if res := r.push(t); res.Kind != PushDone {
		t.Fatalf("%+v", res)
	}
	if got := sh(t, r.origin, "rev-parse", "master"); got != camp {
		t.Fatalf("GitHub 役が %s（Camp の commit は %s）。エージェントの commit まで出た", got, camp)
	}
}
