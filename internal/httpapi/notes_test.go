package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoomA-0750/camp/internal/noteedit"
	"github.com/MoomA-0750/camp/internal/notes"
	"github.com/MoomA-0750/camp/internal/session"
	"github.com/MoomA-0750/camp/internal/store"
	"github.com/MoomA-0750/camp/internal/vault"
)

// 画面 → campd → 実行面 → ディスク を本物の制御口で通す（Phase 5 / M53）。
func notesServer(t *testing.T, withAgent bool) (*httptest.Server, *http.Client, string, map[string]int64) {
	t.Helper()
	for k, v := range map[string]string{"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_AUTHOR_NAME": "t",
		"GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"} {
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "Human/Logs"), 0o755)
	os.WriteFile(filepath.Join(dir, "Human/Logs/a.md"), []byte("a\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("指示\n"), 0o644)
	if out, err := exec.Command("git", "-C", dir, "init", "-q", "-b", "master").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	real, _ := filepath.EvalSymlinks(dir)

	db, err := store.Open(filepath.Join(t.TempDir(), "camp.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := SetPassword(db, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if _, err := vault.Index(db, "h", real, "Vault"); err != nil {
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

	sup := session.New(db)
	if withAgent {
		sock := filepath.Join(t.TempDir(), "a.sock")
		cl, err := sup.Listen(sock, "", os.Getuid())
		if err != nil {
			t.Fatal(err)
		}
		go cl.Serve()
		t.Cleanup(func() { cl.Close() })
		d, err := notes.OpenDisk(real)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Close() })
		ag := session.NewAgent(sock, "/nonexistent/claude")
		ag.Scope, ag.LogDir = false, t.TempDir()
		ag.Notes = &notes.Git{Disk: d}
		if err := ag.Dial("test"); err != nil {
			t.Fatal(err)
		}
		go ag.Run()
		deadline := time.Now().Add(3 * time.Second)
		for sup.NoteVault() == "" && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
	}
	edit := &noteedit.Service{DB: db, W: sup}
	s, err := New(db, Options{Sessions: sup, Notes: edit})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, loggedIn(t, ts), real, ids
}

func saveNote(t *testing.T, c *http.Client, url string, v any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(v)
	res, err := c.Post(url, "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

func TestNoteSaveThroughTheAPI(t *testing.T) {
	ts, c, dir, ids := notesServer(t, true)
	var src noteedit.Source
	if code := getJSONAs(t, c, ts.URL+"/api/notes/"+itoa(int(ids["Human/Logs/a.md"]))+"/source", &src); code != 200 || src.Body != "a\n" {
		t.Fatalf("%d %+v", code, src)
	}
	code, out := saveNote(t, c, ts.URL+"/api/notes/"+itoa(int(ids["Human/Logs/a.md"]))+"/source",
		map[string]string{"base_sha256": src.SHA, "body": "a\n本人\n"})
	if code != 200 || out["status"] != "saved" {
		t.Fatalf("%d %v", code, out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "Human/Logs/a.md")); string(b) != "a\n本人\n" {
		t.Fatalf("%q", b)
	}

	url := ts.URL + "/api/notes/" + itoa(int(ids["AGENTS.md"])) + "/source"
	base := notes.Sum([]byte("指示\n"))
	if code, _ := saveNote(t, c, url, map[string]string{"base_sha256": base, "body": "乗っ取り\n"}); code != 403 {
		t.Fatalf("再認証なしで指示の紙: %d", code)
	}
	if code, _ := saveNote(t, c, url, map[string]string{"base_sha256": base, "body": "乗っ取り\n", "password": "違う"}); code != 401 {
		t.Fatalf("違うパスワード: %d", code)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md")); string(b) != "指示\n" {
		t.Fatal("書かれた")
	}
	if code, out := saveNote(t, c, url, map[string]string{"base_sha256": base, "body": "新しい指示\n",
		"password": "correct horse battery"}); code != 200 || out["status"] != "saved" {
		t.Fatalf("%d %v", code, out)
	}
	// ログインしていなければ通らない。
	if code, _ := saveNote(t, bare(), ts.URL+"/api/notes/"+itoa(int(ids["Human/Logs/a.md"]))+"/source",
		map[string]string{"body": "x\n"}); code == 200 {
		t.Fatal("ログインなしで保存できた")
	}
}

func TestNoteSaveWithoutAnExecutionSideIsUnavailable(t *testing.T) {
	ts, c, dir, ids := notesServer(t, false)
	code, out := saveNote(t, c, ts.URL+"/api/notes/"+itoa(int(ids["Human/Logs/a.md"]))+"/source",
		map[string]string{"base_sha256": notes.Sum([]byte("a\n")), "body": "b\n"})
	if code != 503 {
		t.Fatalf("%d %v", code, out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "Human/Logs/a.md")); string(b) != "a\n" {
		t.Fatal("書かれた")
	}
}

// M55: 名前の一覧・新しいノート・日次ログ・.trash へ移す を本物の制御口で通す。
func TestNoteNavigationThroughTheAPI(t *testing.T) {
	ts, c, dir, ids := notesServer(t, true)
	os.MkdirAll(filepath.Join(dir, "Inbox"), 0o755)
	os.MkdirAll(filepath.Join(dir, "Meta/Templates"), 0o755)
	os.WriteFile(filepath.Join(dir, noteedit.DailyTemplate), []byte("# <% tp.file.title %>\n"), 0o644)

	var sync map[string]any
	getJSONAs(t, c, ts.URL+"/api/notes/sync", &sync)
	vid, _ := sync["vault_id"].(float64)
	if vid == 0 {
		t.Fatalf("vault_id が無い: %v", sync)
	}
	vurl := ts.URL + "/api/vaults/" + itoa(int(vid))

	var names noteedit.Names
	if code := getJSONAs(t, c, vurl+"/names", &names); code != 200 || len(names.Names) == 0 {
		t.Fatalf("%d %+v", code, names)
	}
	var same noteedit.Names
	getJSONAs(t, c, vurl+"/names?gen="+names.Gen, &same)
	if !same.Same {
		t.Fatalf("%+v", same)
	}

	code, out := saveNote(t, c, vurl+"/notes", map[string]string{"path": "Inbox/メモ.md", "body": "書き留めた\n"})
	if code != 201 || out["path"] != "Inbox/メモ.md" || out["note_id"] == nil {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := saveNote(t, c, vurl+"/notes", map[string]string{"path": "Inbox/めも.md"}); code != 201 {
		t.Fatalf("別の名前: %d %v", code, out)
	}
	for body, want := range map[string]int{
		`{"path":"Inbox/メモ.md"}`: 409, `{"path":"Inbox/a#b.md"}`: 400, `{"path":"AGENTS2.md"}`: 403,
		`{"path":"Inbox/無い/a.md"}`: 400,
	} {
		res, err := c.Post(vurl+"/notes", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("%s: %d", body, res.StatusCode)
		}
	}

	code, out = saveNote(t, c, vurl+"/daily", map[string]string{"date": "2026-09-14"})
	if code != 200 || out["created"] != true {
		t.Fatalf("%d %v", code, out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "Human/Logs/2026-09-14.md")); string(b) != "# 2026-09-14\n" {
		t.Fatalf("%q", b)
	}
	// GET では作らない。
	if res, _ := c.Get(vurl + "/daily"); res != nil && res.StatusCode == 200 {
		t.Fatal("GET で日次ログの口が開いている")
	}

	// 移す: 指示の紙は再認証、それ以外は見ている版で。
	aid := itoa(int(ids["Human/Logs/a.md"]))
	var src noteedit.Source
	getJSONAs(t, c, ts.URL+"/api/notes/"+aid+"/source", &src)
	if code, out := saveNote(t, c, ts.URL+"/api/notes/"+aid+"/trash", map[string]string{"base_sha256": src.SHA}); code != 200 || out["status"] != "trashed" {
		t.Fatalf("%d %v", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".trash/a.md")); err != nil {
		t.Fatal(".trash に無い")
	}
	gid := itoa(int(ids["AGENTS.md"]))
	getJSONAs(t, c, ts.URL+"/api/notes/"+gid+"/source", &src)
	if code, _ := saveNote(t, c, ts.URL+"/api/notes/"+gid+"/trash", map[string]string{"base_sha256": src.SHA}); code != 403 {
		t.Fatalf("再認証なしで指示の紙を移せた: %d", code)
	}
	if code, _ := saveNote(t, bare(), ts.URL+"/api/notes/"+gid+"/trash", map[string]string{"base_sha256": src.SHA}); code == 200 {
		t.Fatal("ログインなしで移せた")
	}
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Fatal("指示の紙が消えた")
	}
}
