package notes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func vaultDir(t *testing.T) (string, *Disk) {
	t.Helper()
	dir := t.TempDir()
	for _, p := range []string{"Human/Logs", "Inbox", "Meta/Agent-Skills/x", "Data/Health"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d, err := OpenDisk(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return dir, d
}

func put(t *testing.T, dir, rel, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestClassify(t *testing.T) {
	for rel, want := range map[string]Class{
		"Human/Logs/2026-09-13.md":        Editable,
		"Data/Notes/調査（メモ）・下書き.md":  Editable,
		"Inbox/Untitled.md":               Editable,
		"AGENTS.md":                       Instruction,
		"CLAUDE.md":                       Instruction,
		"Meta/Agent-Skills/todo/SKILL.md": Instruction,
		"Home.md":                         ReadOnly,
		"Human/Dashboards/Dashboard.md":   ReadOnly,
		"Data/Health/2026-09-13.md":       ReadOnly,
		"Human/Dashboards/Payments.base":  ReadOnly,
		".obsidian/app.md":                ReadOnly,
		"Human/.trash/x.md":               ReadOnly,
		"Meta/Scripts/x.md":               ReadOnly,
		"README.md":                       ReadOnly,
		"../x.md":                         ReadOnly,
		"Human/../AGENTS.md":              ReadOnly,
		"/Human/x.md":                     ReadOnly,
		"Human//x.md":                     ReadOnly,
		"Human/x\x00.md":                  ReadOnly,
	} {
		if got, why := Classify(rel); got != want {
			t.Errorf("Classify(%q) = %v（%s）want %v", rel, got, why, want)
		}
	}
}

func TestWriteChecksTheBaseAndIsIdempotent(t *testing.T) {
	dir, d := vaultDir(t)
	rel := "Human/Logs/2026-09-13.md"
	put(t, dir, rel, "- 09:00 起きた\n", 0o640)
	base := Sum([]byte("- 09:00 起きた\n"))
	body := []byte("- 09:00 起きた\n- 10:00 書いた\n")

	r, err := d.Write(rel, base, body, false, false)
	if err != nil || r.Status != StatusWritten || r.DiskSHA != Sum(body) {
		t.Fatalf("%+v %v", r, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, rel)); string(got) != string(body) {
		t.Fatalf("中身が違う: %q", got)
	}
	if fi, _ := os.Stat(filepath.Join(dir, rel)); fi.Mode().Perm() != 0o640 {
		t.Fatalf("権限を写していない: %v", fi.Mode())
	}
	// 再送（同じ base・同じ本文）は成功として返す。
	if r, err := d.Write(rel, base, body, false, false); err != nil || r.Status != StatusSame {
		t.Fatalf("再送を断った: %+v %v", r, err)
	}
	// 別の書き手が変えたあと、古い base では書かない。
	put(t, dir, rel, "エージェントの版\n", 0o640)
	r, err = d.Write(rel, base, []byte("本人の版\n"), false, false)
	if err != nil || r.Status != StatusChanged || r.DiskSHA != Sum([]byte("エージェントの版\n")) {
		t.Fatalf("変わっていたのに書いた: %+v %v", r, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, rel)); string(got) != "エージェントの版\n" {
		t.Fatalf("上書きした: %q", got)
	}
	// 一時ファイルを残さない。
	ents, _ := os.ReadDir(filepath.Join(dir, "Human/Logs"))
	if len(ents) != 1 {
		t.Fatalf("一時ファイルが残った: %v", ents)
	}
}

func TestCreateNeverOverwrites(t *testing.T) {
	dir, d := vaultDir(t)
	rel := "Inbox/メモ.md"
	if r, err := d.Write(rel, "", []byte("a\n"), true, false); err != nil || r.Status != StatusWritten {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err := d.Write(rel, "", []byte("b\n"), true, false); err != nil || r.Status != StatusExists {
		t.Fatalf("既にあるのに作った: %+v %v", r, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, rel)); string(got) != "a\n" {
		t.Fatalf("%q", got)
	}
	// 無いノートを「直す」ことはしない。
	if _, err := d.Write("Inbox/無い.md", Sum([]byte("x")), []byte("y\n"), false, false); err == nil {
		t.Fatal("消えたノートを作り直した")
	}
}

func TestWriteRefusesSymlinksPlacesAndText(t *testing.T) {
	dir, d := vaultDir(t)
	outside := t.TempDir()
	put(t, outside, "secret.md", "外\n", 0o644)
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(dir, "Inbox/link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "Human/out")); err != nil {
		t.Fatal(err)
	}
	put(t, dir, "Inbox/real.md", "x\n", 0o644)
	if err := os.Symlink("real.md", filepath.Join(dir, "Inbox/inner.md")); err != nil {
		t.Fatal(err)
	}
	put(t, dir, "AGENTS.md", "指示\n", 0o644)

	cases := []struct {
		rel    string
		create bool
		reauth bool
		body   string
		want   string
	}{
		{"Inbox/link.md", false, false, "y\n", "symlink"},
		{"Human/out/new.md", true, false, "y\n", "symlink"},
		{"Inbox/inner.md", false, false, "y\n", "symlink"},
		{"Data/Health/2026-09-13.md", true, false, "y\n", "直せない"},
		{"AGENTS.md", false, false, "乗っ取り\n", "パスワード"},
		{"Inbox/crlf.md", true, false, "a\r\nb\r\n", "CR"},
		{"Inbox/bom.md", true, false, "\xEF\xBB\xBFa\n", "BOM"},
		{"Inbox/bad.md", true, false, "\xff\xfe\n", "UTF-8"},
		{"Inbox/big.md", true, false, strings.Repeat("あ", MaxBody/3+1), "大きすぎる"},
	}
	for _, c := range cases {
		base := ""
		if !c.create {
			if b, err := os.ReadFile(filepath.Join(dir, c.rel)); err == nil {
				base = Sum(b)
			}
		}
		_, err := d.Write(c.rel, base, []byte(c.body), c.create, c.reauth)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: 狙いの理由で断っていない: %v（want %q）", c.rel, err, c.want)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(outside, "secret.md")); string(got) != "外\n" {
		t.Fatalf("Vault の外を書いた: %q", got)
	}
	// 指示の紙は、入れ直したときだけ書ける。
	if r, err := d.Write("AGENTS.md", Sum([]byte("指示\n")), []byte("新しい指示\n"), false, true); err != nil || r.Status != StatusWritten {
		t.Fatalf("入れ直しても書けない: %+v %v", r, err)
	}
}

// 実行面が使う部品なので、DB へ届く道を持たせない（session の同名の試験と同じ考え）。
func TestNotesHasNoWayToTouchTheDatabase(t *testing.T) {
	ents, _ := os.ReadDir(".")
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, _ := os.ReadFile(e.Name())
		for _, bad := range []string{"internal/store", "database/sql", "modernc.org/sqlite"} {
			if strings.Contains(string(b), bad) {
				t.Fatalf("%s が %s を参照している。**実行面に DB を持たせない**", e.Name(), bad)
			}
		}
	}
}
