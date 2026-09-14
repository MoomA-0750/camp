package noteedit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoomA-0750/camp/internal/notes"
	"github.com/MoomA-0750/camp/internal/vault"
)

// M55: 名前の一覧・新しいノート・日次ログ・.trash へ移す。

// more は rig にファイルを足して commit・push し、索引し直す。
func (r *rig) more(t *testing.T, files map[string]string) {
	t.Helper()
	for p, b := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(r.work, p)), 0o755)
		os.WriteFile(filepath.Join(r.work, p), []byte(b), 0o644)
	}
	git(t, r.work, "add", ".")
	git(t, r.work, "commit", "-qm", "more")
	git(t, r.work, "push", "-q", "origin", "master")
	if _, err := vault.Index(r.db, "test", r.work, "Vault"); err != nil {
		t.Fatal(err)
	}
	rows, _ := r.db.Query(`select id, path from notes where missing_at is null`)
	for rows.Next() {
		var id int64
		var p string
		rows.Scan(&id, &p)
		r.ids[p] = id
	}
	rows.Close()
}

func (r *rig) vaultID(t *testing.T) int64 {
	var id int64
	if err := r.db.QueryRow(`select id from vaults`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestNamesGiveLinksThatResolveToThemselves(t *testing.T) {
	r := newRig(t)
	r.more(t, map[string]string{
		"Inbox/Todo.md":          "a\n",
		"Human/Projects/todo.md": "b\n", // 大文字小文字だけ違うベース名
		"Human/Projects/Camp.md": "c\n",
		"Human/Projects/a#b.md":  "リンクに書けない名前\n",
		"Human/Attach/pic.png":   "png",
	})
	vid := r.vaultID(t)
	got, err := r.svc.Names(vid, "")
	if err != nil || got.Gen == "" || got.Same {
		t.Fatalf("%+v %v", got, err)
	}
	link := map[string]string{}
	for _, n := range got.Names {
		link[n.Path] = n.Link
	}
	want := map[string]string{
		"Human/Logs/2026-09-13.md":  "Human/Logs/2026-09-13", // Data/Health と衝突
		"Data/Health/2026-09-13.md": "Data/Health/2026-09-13",
		"Inbox/Todo.md":             "Inbox/Todo", // 畳むと衝突
		"Human/Projects/todo.md":    "Human/Projects/todo",
		"Human/Projects/Camp.md":    "Camp",
		"AGENTS.md":                 "AGENTS",
		"Human/Projects/a#b.md":     "",
		"Human/Attach/pic.png":      "pic.png",
	}
	for p, w := range want {
		if link[p] != w {
			t.Errorf("%s: link %q、欲しいのは %q", p, link[p], w)
		}
	}
	// どのノートから書いても、そのノートへ曖昧でなく届く。
	ix, _, _, _ := r.svc.linkIndexKey(vid)
	for _, n := range got.Names {
		if n.Link == "" {
			continue
		}
		for from := range link {
			if res := ix.Resolve(from, n.Link); res.To != n.Path || res.Ambiguous {
				t.Errorf("%s から [[%s]] が %+v", from, n.Link, res)
			}
		}
	}
	if again, _ := r.svc.Names(vid, got.Gen); !again.Same || len(again.Names) != 0 {
		t.Fatalf("同じ世代なのに中身を返した: %+v", again)
	}
}

func TestCreateNewNote(t *testing.T) {
	r := newRig(t)
	r.more(t, map[string]string{"Inbox/Todo.md": "a\n"})
	vid := r.vaultID(t)

	out, err := r.svc.Create(vid, "Inbox/思いつき.md", "書き留めた\n")
	if err != nil || !out.Created || out.NoteID == 0 || out.Path != "Inbox/思いつき.md" {
		t.Fatalf("%+v %v", out, err)
	}
	if r.disk(t, "Inbox/思いつき.md") != "書き留めた\n" {
		t.Fatal("書いていない")
	}
	// 作ったノートをそのまま開いて書ける。
	src, err := r.svc.Open(out.NoteID)
	if err != nil || !src.Editable {
		t.Fatalf("%+v %v", src, err)
	}
	// 同じ名前・大文字小文字違い → 既にある（既にあるノートの id を返す）。
	for _, p := range []string{"Inbox/思いつき.md", "Inbox/todo.md", "Inbox/TODO.md"} {
		got, err := r.svc.Create(vid, p, "x\n")
		if !errors.Is(err, ErrExists) || got == nil || got.NoteID == 0 {
			t.Errorf("%s: %+v %v", p, got, err)
		}
	}
	// 作れない場所・名前。
	for p, want := range map[string]error{
		"Data/Health/x.md":       ErrReadOnly,
		"Meta/Agent-Skills/x.md": ErrReadOnly,
		"AGENTS2.md":             ErrReadOnly,
		"Inbox/a#b.md":           ErrBadName,
		"Inbox/.x.md":            ErrBadName,
		"Inbox/x.txt":            ErrBadName,
	} {
		if _, err := r.svc.Create(vid, p, "x\n"); !errors.Is(err, want) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if _, err := r.svc.Create(vid, "Inbox/無い/x.md", "x\n"); !errors.Is(err, ErrBadName) {
		t.Errorf("無いフォルダ: %v", err)
	}
	if r.w.calls["write"] != 1 {
		t.Errorf("断るべき要求を実行面へ頼んだ: %v", r.w.calls) // 名前・場所・フォルダ・索引で分かるぶつかりは campd で断る
	}
	// commit に入り、push される。
	r.svc.Tick(time.Now().Add(2 * time.Minute))
	if got := git(t, r.work, "show", "HEAD:Inbox/思いつき.md"); got != "書き留めた" {
		t.Fatalf("commit の中身: %q", got)
	}
	if git(t, r.origin, "rev-parse", "master") != git(t, r.work, "rev-parse", "HEAD") {
		t.Fatal("push していない")
	}
}

func TestDailyLogFromTemplate(t *testing.T) {
	r := newRig(t)
	r.more(t, map[string]string{DailyTemplate: "# <% tp.file.title %>\n\n## 今日のこと\n\n-\n"})
	vid := r.vaultID(t)

	out, err := r.svc.Daily(vid, "2026-09-14")
	if err != nil || !out.Created || out.Path != "Human/Logs/2026-09-14.md" || out.Warn != "" {
		t.Fatalf("%+v %v", out, err)
	}
	if got := r.disk(t, out.Path); got != "# 2026-09-14\n\n## 今日のこと\n\n-\n" {
		t.Fatalf("テンプレートの形でない:\n%s", got)
	}
	// 二度目は開くだけ。
	again, err := r.svc.Daily(vid, "2026-09-14")
	if err != nil || again.Created || again.NoteID != out.NoteID {
		t.Fatalf("%+v %v", again, err)
	}
	// 既にある日（索引済み）はそのまま。
	if got, err := r.svc.Daily(vid, "2026-09-13"); err != nil || got.Created || got.NoteID != r.ids[log] {
		t.Fatalf("%+v %v", got, err)
	}
	for _, bad := range []string{"2026-9-14", "2026-02-30", "../x", ""} {
		if _, err := r.svc.Daily(vid, bad); !errors.Is(err, ErrBadName) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestDailyLogDoesNotRunTemplaterCode(t *testing.T) {
	r := newRig(t)
	r.more(t, map[string]string{DailyTemplate: "# {{title}}\n<% tp.system.prompt(\"x\") %>\n<%* require('child_process') %>\n"})
	out, err := r.svc.Daily(r.vaultID(t), "2026-09-15")
	if err != nil || !strings.Contains(out.Warn, "tp.system.prompt") {
		t.Fatalf("%+v %v", out, err)
	}
	if got := r.disk(t, out.Path); !strings.HasPrefix(got, "# 2026-09-15\n<% tp.system.prompt") {
		t.Fatalf("%q", got)
	}
}

func TestTrashThenCommitDeletionAndPush(t *testing.T) {
	r := newRig(t)
	r.more(t, map[string]string{"Inbox/消す.md": "消すノート\n", "Inbox/残す.md": "残す\n"})
	id := r.ids["Inbox/消す.md"]
	src, _ := r.svc.Open(id)
	// 書いて commit を待っている間は移さない（その版を git の履歴に残す。本人の決定）。
	if _, err := r.svc.Save(id, src.SHA, "直してから消す\n", false); err != nil {
		t.Fatal(err)
	}
	src, _ = r.svc.Open(id)
	if out, err := r.svc.Trash(id, src.SHA, false); err != nil || out.Status != TrashWaiting {
		t.Fatalf("%+v %v", out, err)
	}
	if r.w.calls["trash"] != 0 {
		t.Fatal("commit を待っているのに実行面へ頼んだ")
	}
	r.svc.Tick(time.Now().Add(2 * time.Minute))
	// 見ている版が古いと移さない。
	if out, err := r.svc.Trash(id, notes.Sum([]byte("消すノート\n")), false); err != nil || out.Status != notes.TrashChanged {
		t.Fatalf("%+v %v", out, err)
	}
	out, err := r.svc.Trash(id, src.SHA, false)
	if err != nil || out.Status != notes.TrashDone || out.To != ".trash/消す.md" {
		t.Fatalf("%+v %v", out, err)
	}
	if st := r.states(t); st != "committed,dropped,pending" {
		t.Fatalf("待ち行: %s", st)
	}
	n, _ := vault.OneNote(r.db, id)
	if n.Missing == "" {
		t.Fatal("索引で消えた印が無い")
	}
	r.svc.Tick(time.Now().Add(2 * time.Minute))
	if st := r.states(t); st != "committed,dropped,committed" {
		t.Fatalf("待ち行: %s", st)
	}
	// 移す前の版は git の履歴にある。
	if got := git(t, r.work, "-c", "core.quotepath=false", "show", "HEAD~1:Inbox/消す.md"); got != "直してから消す" {
		t.Fatalf("履歴に最後の版が無い: %q", got)
	}
	if got := git(t, r.work, "-c", "core.quotepath=false", "show", "--name-status", "--format=", "HEAD"); got != "D\tInbox/消す.md" {
		t.Fatalf("commit: %q", got)
	}
	if git(t, r.origin, "rev-parse", "master") != git(t, r.work, "rev-parse", "HEAD") {
		t.Fatal("push していない")
	}
	if b, _ := os.ReadFile(filepath.Join(r.work, ".trash/消す.md")); string(b) != "直してから消す\n" {
		t.Fatalf(".trash の中身: %q", b)
	}
	if again, _ := r.svc.Trash(id, src.SHA, false); again.Status != notes.TrashGone {
		t.Fatalf("%+v", again)
	}
}

func TestTrashRefusals(t *testing.T) {
	r := newRig(t)
	src, _ := r.svc.Open(r.ids["AGENTS.md"])
	if _, err := r.svc.Trash(r.ids["AGENTS.md"], src.SHA, false); !errors.Is(err, ErrNeedReauth) {
		t.Fatalf("%v", err)
	}
	h, _ := r.svc.Open(r.ids["Data/Health/2026-09-13.md"])
	if _, err := r.svc.Trash(r.ids["Data/Health/2026-09-13.md"], h.SHA, true); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("%v", err)
	}
	// 見ていた版の控えが無い（開いていない）なら頼まない。
	if _, err := r.svc.Trash(r.ids[log], notes.Sum([]byte("知らない版")), false); err == nil {
		t.Fatal("控えの無い版で移した")
	}
	if r.w.calls["trash"] != 0 {
		t.Fatalf("実行面に頼んだ: %v", r.w.calls)
	}
}

func TestReconcilePlannedTrash(t *testing.T) {
	r := newRig(t)
	r.more(t, map[string]string{"Inbox/a.md": "a\n", "Inbox/b.md": "b\n"})
	vid := r.vaultID(t)
	sha := notes.Sum([]byte("a\n"))
	vault.PutBlob(r.db, []byte("a\n"))
	vault.PutBlob(r.db, []byte("b\n"))
	// a は移せてから落ちた、b は移す前に落ちた。
	os.Rename(filepath.Join(r.work, "Inbox/a.md"), filepath.Join(r.work, "a-moved"))
	for _, p := range []string{"Inbox/a.md", "Inbox/b.md"} {
		r.db.Exec(`insert into note_writes(vault_id, path, sha256, state, op, created_at, updated_at)
			values(?, ?, ?, 'planned', 'trash', '2026-09-13T00:00:00Z', '2026-09-13T00:00:00Z')`, vid, p,
			map[string]string{"Inbox/a.md": sha, "Inbox/b.md": notes.Sum([]byte("b\n"))}[p])
	}
	pending, dropped, err := r.svc.Reconcile()
	if err != nil || pending != 1 || dropped != 1 {
		t.Fatalf("%d %d %v", pending, dropped, err)
	}
	if st := r.states(t); st != "pending,dropped" {
		t.Fatalf("%s", st)
	}
}

// lossy は実行面の仕事はするが、返事が届かなかったことにする。mismatch は commit の結果に食い違いを足す。
type lossy struct {
	*direct
	loseTrash, loseWrite bool
	mismatch             bool
}

func (l *lossy) NoteTrash(v, path, base string, reauthed bool) (notes.TrashResult, error) {
	res, err := l.direct.NoteTrash(v, path, base, reauthed)
	if l.loseTrash && err == nil {
		return notes.TrashResult{}, errors.New("実行面が返事をしない")
	}
	return res, err
}

func (l *lossy) NoteWrite(v, path, base, body string, create, reauthed bool) (notes.Result, error) {
	res, err := l.direct.NoteWrite(v, path, base, body, create, reauthed)
	if l.loseWrite && err == nil {
		return notes.Result{}, errors.New("実行面が返事をしない")
	}
	return res, err
}

func (l *lossy) NoteCommit(v string, e []notes.Entry) (notes.CommitResult, error) {
	res, err := l.direct.NoteCommit(v, e)
	if l.mismatch && err == nil && res.Commit != "" {
		res.Mismatch = []string{e[0].Path}
	}
	return res, err
}

// 移せたのに返事が届かなくても、定期の照合で「移せていた」になり、削除が commit・push される（outer gate の Fable 3）。
func TestTrashWhoseReplyWasLostIsStillCommitted(t *testing.T) {
	r := newRig(t)
	r.more(t, map[string]string{"Inbox/消す.md": "消す\n"})
	l := &lossy{direct: r.w, loseTrash: true}
	r.svc.W = l
	id := r.ids["Inbox/消す.md"]
	src, _ := r.svc.Open(id)
	if _, err := r.svc.Trash(id, src.SHA, false); err == nil {
		t.Fatal("返事が届かないのに成功した")
	}
	if st := r.states(t); st != "planned" {
		t.Fatalf("待ち行: %s", st)
	}
	// 返事の待ちより前には照らさない。過ぎたら照らして commit する。
	r.svc.Tick(time.Now())
	if st := r.states(t); st != "planned" {
		t.Fatalf("すぐに照らした: %s", st)
	}
	r.db.Exec(`update note_writes set updated_at = '2026-09-13T00:00:00Z'`)
	r.svc.Tick(time.Now().Add(10 * time.Minute))
	if st := r.states(t); st != "committed" {
		t.Fatalf("待ち行: %s", st)
	}
	if got := git(t, r.work, "-c", "core.quotepath=false", "show", "--name-status", "--format=", "HEAD"); got != "D\tInbox/消す.md" {
		t.Fatalf("commit: %q", got)
	}
	if n, _ := vault.OneNote(r.db, id); n.Missing == "" {
		t.Fatal("索引で消えた印が無い")
	}
}

// 古い pending のあとの新しい行が「書く予定」で残って落ちても、古い行は上書きされたとして浮かばない（Fable 4）。
func TestReconcileSupersedesOlderPending(t *testing.T) {
	r := newRig(t)
	id := r.ids[log]
	src, _ := r.svc.Open(id)
	a, err := r.svc.Save(id, src.SHA, src.Body+"A\n", false)
	if err != nil {
		t.Fatal(err)
	}
	r.svc.W = &lossy{direct: r.w, loseWrite: true}
	if _, err := r.svc.Save(id, a.SHA, src.Body+"A\nB\n", false); err == nil {
		t.Fatal("返事が届かないのに成功した")
	}
	r.svc.W = r.w
	if p, d, err := r.svc.Reconcile(); err != nil || p != 1 || d != 0 {
		t.Fatalf("%d %d %v", p, d, err)
	}
	if st := r.states(t); st != "superseded,pending" {
		t.Fatalf("待ち行: %s", st)
	}
	r.svc.Tick(time.Now().Add(2 * time.Minute))
	st, _ := r.svc.Status()
	if len(st.Overwritten) != 0 || r.states(t) != "superseded,committed" {
		t.Fatalf("%+v %s", st, r.states(t))
	}
	// Camp が書いた版は、merge で消えた版も含めて画面から辿れる。
	ws, err := r.svc.Writes(id)
	if err != nil || len(ws) != 2 || ws[0].State != "committed" || ws[1].State != "superseded" {
		t.Fatalf("%+v %v", ws, err)
	}
}

// commit に照らした中身と違うものが入ったら、その commit は Camp の commit として覚えず、push は本人の確認待ち（codex）。
func TestMismatchedCommitIsNotPushed(t *testing.T) {
	r := newRig(t)
	r.svc.W = &lossy{direct: r.w, mismatch: true}
	id := r.ids[log]
	src, _ := r.svc.Open(id)
	if _, err := r.svc.Save(id, src.SHA, src.Body+"本人\n", false); err != nil {
		t.Fatal(err)
	}
	before := git(t, r.origin, "rev-parse", "master")
	r.svc.Tick(time.Now().Add(2 * time.Minute))
	var n int
	r.db.QueryRow(`select count(*) from note_commits`).Scan(&n)
	st, _ := r.svc.Status()
	if n != 0 || r.states(t) != "overwritten" || st.Push.Kind != notes.PushAgentPending {
		t.Fatalf("覚えた commit %d・待ち行 %s・push %+v", n, r.states(t), st.Push)
	}
	if git(t, r.origin, "rev-parse", "master") != before {
		t.Fatal("GitHub 役に出た")
	}
}
