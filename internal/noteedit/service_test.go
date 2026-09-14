package noteedit

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoomA-0750/camp/internal/notes"
	"github.com/MoomA-0750/camp/internal/store"
	"github.com/MoomA-0750/camp/internal/vault"
)

// 実行面の代わりに notes をそのまま呼ぶ（制御口の往復は session の試験で見ている）。
type direct struct {
	g     *notes.Git
	root  string
	calls map[string]int
}

func (d *direct) check(v string) error {
	if v != d.root {
		return errors.New("別の Vault")
	}
	return nil
}

func (d *direct) NoteWrite(v, path, base, body string, create, reauthed bool) (notes.Result, error) {
	d.calls["write"]++
	if err := d.check(v); err != nil {
		return notes.Result{}, err
	}
	return d.g.Disk.Write(path, base, []byte(body), create, reauthed)
}

func (d *direct) NoteCommit(v string, e []notes.Entry) (notes.CommitResult, error) {
	d.calls["commit"]++
	if err := d.check(v); err != nil {
		return notes.CommitResult{}, err
	}
	return d.g.Commit(e)
}

func (d *direct) NotePush(v string, known []string) (notes.PushResult, error) {
	d.calls["push"]++
	if err := d.check(v); err != nil {
		return notes.PushResult{}, err
	}
	return d.g.Push(known)
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

type rig struct {
	db     *store.DB
	svc    *Service
	w      *direct
	work   string
	origin string
	ids    map[string]int64
}

func newRig(t *testing.T) *rig {
	t.Helper()
	for k, v := range map[string]string{"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_AUTHOR_NAME": "t",
		"GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"} {
		t.Setenv(k, v)
	}
	base := t.TempDir()
	origin, work := filepath.Join(base, "origin.git"), filepath.Join(base, "vault")
	git(t, base, "init", "-q", "--bare", "-b", "master", origin)
	git(t, base, "clone", "-q", origin, work)
	files := map[string]string{
		"Human/Logs/2026-09-13.md":  "# 日記\n\n- 09:00 起きた\n\n## 夜\n\n- 22:00 寝る\n",
		"AGENTS.md":                 "指示\n",
		"Data/Health/2026-09-13.md": "---\nsteps: 1\n---\n",
		"Human/Projects/crlf.md":    "a\r\nb\r\n",
	}
	for p, b := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(work, p)), 0o755)
		os.WriteFile(filepath.Join(work, p), []byte(b), 0o644)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "-qm", "init")
	git(t, work, "push", "-q", "origin", "master")
	real, _ := filepath.EvalSymlinks(work)

	db, err := store.Open(filepath.Join(t.TempDir(), "camp.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := vault.Index(db, "test", real, "Vault"); err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	rows, _ := db.Query(`select id, path from notes`)
	for rows.Next() {
		var id int64
		var p string
		rows.Scan(&id, &p)
		ids[p] = id
	}
	rows.Close()

	d, err := notes.OpenDisk(real)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	w := &direct{g: &notes.Git{Disk: d}, root: real, calls: map[string]int{}}
	return &rig{db: db, w: w, work: real, origin: origin, ids: ids,
		svc: &Service{DB: db, W: w, IsBusy: notes.IsBusy, CommitAfter: time.Minute}}
}

func (r *rig) disk(t *testing.T, rel string) string {
	b, err := os.ReadFile(filepath.Join(r.work, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (r *rig) states(t *testing.T) string {
	rows, err := r.db.Query(`select state from note_writes order by id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return strings.Join(out, ",")
}

const log = "Human/Logs/2026-09-13.md"

func TestOpenReadsTheDiskNotTheIndex(t *testing.T) {
	r := newRig(t)
	// 索引のあとでエージェントが書いた。
	os.WriteFile(filepath.Join(r.work, log), []byte("エージェントの版\n"), 0o644)
	src, err := r.svc.Open(r.ids[log])
	if err != nil || src.Body != "エージェントの版\n" || src.SHA != notes.Sum([]byte("エージェントの版\n")) || !src.Editable {
		t.Fatalf("%+v %v", src, err)
	}
	for p, want := range map[string]string{"Data/Health/2026-09-13.md": "外", "Human/Projects/crlf.md": "CR"} {
		src, err := r.svc.Open(r.ids[p])
		if err != nil || src.Editable || !strings.Contains(src.ReadOnly, want) {
			t.Errorf("%s: %+v %v", p, src, err)
		}
	}
	if src, _ := r.svc.Open(r.ids["AGENTS.md"]); !src.Instruction {
		t.Fatalf("%+v", src)
	}
}

func TestSaveThenCommitAfterAQuietMinuteThenPush(t *testing.T) {
	r := newRig(t)
	src, _ := r.svc.Open(r.ids[log])
	body := src.Body + "- 10:00 書いた\n"
	res, err := r.svc.Save(r.ids[log], src.SHA, body, false)
	if err != nil || res.Status != "saved" {
		t.Fatalf("%+v %v", res, err)
	}
	// 自動保存でもう一度（同じ保存の続き）。
	body2 := body + "- 10:01 もう少し\n"
	if res, err := r.svc.Save(r.ids[log], res.SHA, body2, false); err != nil || res.Status != "saved" {
		t.Fatalf("%+v %v", res, err)
	}
	if r.disk(t, log) != body2 {
		t.Fatal("書いていない")
	}
	if st := r.states(t); st != "superseded,pending" {
		t.Fatalf("待ち行: %s", st)
	}
	// 1 分経たないうちは commit しない。
	r.svc.Tick(time.Now())
	if r.w.calls["commit"] != 0 {
		t.Fatal("書いている最中に commit した")
	}
	r.svc.Tick(time.Now().Add(2 * time.Minute))
	if st := r.states(t); st != "superseded,committed" {
		t.Fatalf("待ち行: %s", st)
	}
	if got := git(t, r.work, "show", "HEAD:"+log); got+"\n" != body2 {
		t.Fatalf("commit の中身: %q", got)
	}
	if git(t, r.origin, "rev-parse", "master") != git(t, r.work, "rev-parse", "HEAD") {
		t.Fatal("push していない")
	}
	st, _ := r.svc.Status()
	if st.Pending != 0 || st.Push.Kind != notes.PushDone {
		t.Fatalf("%+v", st)
	}
}

func TestConflictingSavesMergeWhenTheyDoNotOverlap(t *testing.T) {
	r := newRig(t)
	src, _ := r.svc.Open(r.ids[log])
	// 本人は朝の行を、エージェントは夜の節を直した。
	agent := strings.Replace(src.Body, "- 22:00 寝る\n", "- 22:00 寝る\n- 23:00 エージェントが足した\n", 1)
	os.WriteFile(filepath.Join(r.work, log), []byte(agent), 0o644)
	mine := strings.Replace(src.Body, "- 09:00 起きた\n", "- 09:00 起きた\n- 09:30 本人が足した\n", 1)
	res, err := r.svc.Save(r.ids[log], src.SHA, mine, false)
	if err != nil || res.Status != "merged" {
		t.Fatalf("%+v %v", res, err)
	}
	got := r.disk(t, log)
	if !strings.Contains(got, "本人が足した") || !strings.Contains(got, "エージェントが足した") || res.Body != got {
		t.Fatalf("合わせていない:\n%s", got)
	}

	// 同じ行を直していたら書かない。
	src, _ = r.svc.Open(r.ids[log])
	os.WriteFile(filepath.Join(r.work, log), []byte(strings.Replace(src.Body, "起きた", "エージェントの起きた", 1)), 0o644)
	before := r.disk(t, log)
	res, err = r.svc.Save(r.ids[log], src.SHA, strings.Replace(src.Body, "起きた", "本人の起きた", 1), false)
	if err != nil || res.Status != "conflict" || res.Disk != before {
		t.Fatalf("%+v %v", res, err)
	}
	if r.disk(t, log) != before {
		t.Fatal("ぶつかったのに書いた")
	}
	// 本人の版は blobs に残っている。
	if b, _ := vault.BlobBody(r.db, notes.Sum([]byte(strings.Replace(src.Body, "起きた", "本人の起きた", 1)))); b == nil {
		t.Fatal("本人の版を控えていない")
	}
}

func TestOverwrittenBeforeCommitIsNotCommittedAndIsKept(t *testing.T) {
	r := newRig(t)
	src, _ := r.svc.Open(r.ids[log])
	mine := src.Body + "- 本人\n"
	if _, err := r.svc.Save(r.ids[log], src.SHA, mine, false); err != nil {
		t.Fatal(err)
	}
	// commit を待つ間にエージェントが上書きした。
	os.WriteFile(filepath.Join(r.work, log), []byte("エージェントが丸ごと書き換えた\n"), 0o644)
	r.svc.Tick(time.Now().Add(2 * time.Minute))
	if st := r.states(t); st != "overwritten" {
		t.Fatalf("待ち行: %s", st)
	}
	if n := git(t, r.work, "rev-list", "--count", "HEAD"); n != "1" {
		t.Fatalf("commit した: %s", n)
	}
	st, _ := r.svc.Status()
	if len(st.Overwritten) != 1 || st.Overwritten[0].SHA != notes.Sum([]byte(mine)) {
		t.Fatalf("%+v", st)
	}
	if b, _ := vault.BlobBody(r.db, st.Overwritten[0].SHA); string(b) != mine {
		t.Fatal("Camp の版を戻せない")
	}
}

func TestRefusedPlaces(t *testing.T) {
	r := newRig(t)
	if _, err := r.svc.Save(r.ids["Data/Health/2026-09-13.md"], "", "x\n", false); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("%v", err)
	}
	src, _ := r.svc.Open(r.ids["AGENTS.md"])
	if _, err := r.svc.Save(r.ids["AGENTS.md"], src.SHA, "乗っ取り\n", false); !errors.Is(err, ErrNeedReauth) {
		t.Fatalf("%v", err)
	}
	if r.w.calls["write"] != 0 || r.disk(t, "AGENTS.md") != "指示\n" {
		t.Fatal("実行面へ頼んだ")
	}
	if res, err := r.svc.Save(r.ids["AGENTS.md"], src.SHA, "新しい指示\n", true); err != nil || res.Status != "saved" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestReconcilePlannedRowsAgainstTheDisk(t *testing.T) {
	r := newRig(t)
	src, _ := r.svc.Open(r.ids[log])
	vid := int64(1)
	r.db.QueryRow(`select vault_id from notes where id = ?`, r.ids[log]).Scan(&vid)
	put := func(body string) {
		sha, _ := vault.PutBlob(r.db, []byte(body))
		r.db.Exec(`insert into note_writes(vault_id, path, sha256, state, created_at, updated_at)
			values(?, ?, ?, 'planned', '2026-09-13T00:00:00Z', '2026-09-13T00:00:00Z')`, vid, log, sha)
	}
	put("書けなかった版\n") // ディスクと違う
	put(src.Body)    // ディスクと同じ（書けてから落ちた）
	p, d, err := r.svc.Reconcile()
	// 古い「書く予定」は、同じパスに新しい行があるので superseded（新しい行の commit のあとで浮かばない）。
	if err != nil || p != 1 || d != 0 || r.states(t) != "superseded,pending" {
		t.Fatalf("%d %d %v %s", p, d, err, r.states(t))
	}
}

func TestPushWaitsForAgentCommits(t *testing.T) {
	r := newRig(t)
	os.WriteFile(filepath.Join(r.work, "Human/Logs/other.md"), []byte("エージェント\n"), 0o644)
	git(t, r.work, "add", "Human/Logs/other.md")
	git(t, r.work, "commit", "-qm", "エージェントの確認待ち")
	src, _ := r.svc.Open(r.ids[log])
	r.svc.Save(r.ids[log], src.SHA, src.Body+"本人\n", false)
	r.svc.Tick(time.Now().Add(2 * time.Minute))
	st, _ := r.svc.Status()
	if st.Push.Kind != notes.PushAgentPending {
		t.Fatalf("%+v", st.Push)
	}
	if git(t, r.origin, "rev-list", "--count", "master") != "1" {
		t.Fatal("出た")
	}
}

// merged は送った版の sha を返す。画面はそれを次の base にする（Fable の M54 設計レビュー 1）。
// 合わせた版の sha を base にして送ると、相手の編集を消すことも縛る。
func TestMergedReturnsTheSentSHAAndConflictKeepsTheDiskVersion(t *testing.T) {
	r := newRig(t)
	src, _ := r.svc.Open(r.ids[log])
	agent := strings.Replace(src.Body, "- 22:00 寝る\n", "- 22:00 寝る\n- 23:00 エージェント\n", 1)
	os.WriteFile(filepath.Join(r.work, log), []byte(agent), 0o644)
	mine := strings.Replace(src.Body, "- 09:00 起きた\n", "- 09:00 起きた\n- 09:30 本人\n", 1)
	res, err := r.svc.Save(r.ids[log], src.SHA, mine, false)
	if err != nil || res.Status != "merged" || res.SentSHA != notes.Sum([]byte(mine)) || res.SHA == res.SentSHA {
		t.Fatalf("%+v %v", res, err)
	}
	// 画面が送ったあとに書き足していた: base は SentSHA、本文は「送った版 + 続き」。
	more := strings.Replace(mine, "- 09:30 本人\n", "- 09:30 本人\n- 続き\n", 1)
	res2, err := r.svc.Save(r.ids[log], res.SentSHA, more, false)
	if err != nil || res2.Status != "merged" {
		t.Fatalf("%+v %v", res2, err)
	}
	if got := r.disk(t, log); !strings.Contains(got, "エージェント") || !strings.Contains(got, "続き") {
		t.Fatalf("相手の編集が消えた:\n%s", got)
	}

	// 同じ行でぶつかったら、ディスクの版を blobs に控える。
	src, _ = r.svc.Open(r.ids[log])
	theirs := strings.Replace(src.Body, "起きた", "エージェントの起きた", 1)
	os.WriteFile(filepath.Join(r.work, log), []byte(theirs), 0o644)
	res, err = r.svc.Save(r.ids[log], src.SHA, strings.Replace(src.Body, "起きた", "本人の起きた", 1), false)
	if err != nil || res.Status != "conflict" {
		t.Fatalf("%+v %v", res, err)
	}
	if b, _ := vault.BlobBody(r.db, res.DiskSHA); string(b) != theirs {
		t.Fatal("ディスクの版を控えていない")
	}
}

func TestResolveUsesTheServerRules(t *testing.T) {
	r := newRig(t)
	// 同じベース名を2つ作って索引し直す。
	for _, p := range []string{"Human/Projects/同名.md", "AI/同名.md"} {
		os.MkdirAll(filepath.Dir(filepath.Join(r.work, p)), 0o755)
		os.WriteFile(filepath.Join(r.work, p), []byte("x\n"), 0o644)
	}
	if _, err := vault.Index(r.db, "test", r.work, "Vault"); err != nil {
		t.Fatal(err)
	}
	var agents int64
	r.db.QueryRow(`select id from notes where path = 'AGENTS.md'`).Scan(&agents)
	got, err := r.svc.Resolve(r.ids[log], []string{"AGENTS", "", "無い", "同名"})
	if err != nil {
		t.Fatal(err)
	}
	if got["AGENTS"].ToID != agents || got[""].ToID != r.ids[log] || got["無い"].ToID != 0 ||
		!got["同名"].Ambiguous || len(got["同名"].Candidates) != 2 {
		t.Fatalf("%+v", got)
	}
	many := make([]string, MaxTargets+1)
	if _, err := r.svc.Resolve(r.ids[log], many); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("%v", err)
	}
}

func (d *direct) NoteTrash(v, path, base string, reauthed bool) (notes.TrashResult, error) {
	d.calls["trash"]++
	if err := d.check(v); err != nil {
		return notes.TrashResult{}, err
	}
	return d.g.Disk.Trash(path, base, reauthed)
}
