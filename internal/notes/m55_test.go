package notes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M55: 新しいノートの名前・.trash へ移す・削除の commit。

func TestNameRel(t *testing.T) {
	nfd := "Inbox/か\u3099っこう.md" // 「が」を NFD で
	got, err := NameRel(nfd)
	if err != nil || got != "Inbox/がっこう.md" {
		t.Fatalf("NFD を NFC に揃えない: %q %v", got, err)
	}
	for _, ok := range []string{"Inbox/メモ 2026-09-13 2215.md", "Human/Logs/2026-09-13.md", "Inbox/a (1).md"} {
		if _, err := NameRel(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"Inbox/a#b.md", "Inbox/a|b.md", "Inbox/a[b].md", "Inbox/a^b.md", "Inbox/a:b.md", "Inbox/a?.md",
		"Inbox/.hidden.md", "Inbox/a .md", "Inbox/a..md", "Inbox/ .md", "Inbox/a.txt", "Inbox/a\tb.md",
		"Inbox/" + strings.Repeat("あ", 90) + ".md", "../x.md", "Inbox//a.md",
	} {
		if _, err := NameRel(bad); err == nil {
			t.Errorf("%q を通した", bad)
		}
	}
}

func TestCreateRefusesTheSameNameFoldedAndNFD(t *testing.T) {
	dir, d := vaultDir(t)
	put(t, dir, "Inbox/Todo.md", "既にある\n", 0o644)
	res, err := d.Write("Inbox/todo.md", "", []byte("新しい\n"), true, false)
	if err != nil || res.Status != StatusExists || res.Other != "Inbox/Todo.md" {
		t.Fatalf("大文字小文字違いを作った: %+v %v", res, err)
	}
	put(t, dir, "Inbox/か\u3099.md", "NFD の名前\n", 0o644)
	if res, err := d.Write("Inbox/が.md", "", []byte("x\n"), true, false); err != nil || res.Status != StatusExists || res.Other != "Inbox/か\u3099.md" {
		t.Fatalf("NFD 違いを作った: %+v %v", res, err)
	}
	if _, err := d.Write("Inbox/か\u3099っ.md", "", []byte("x\n"), true, false); err == nil {
		t.Fatal("NFD の名前で作った")
	}
	for _, p := range []string{"AGENTS2.md", "Meta/Agent-Skills/x/new.md", "Data/Health/x.md"} {
		if _, err := d.Write(p, "", []byte("x\n"), true, true); err == nil {
			t.Errorf("%s を新しく作った", p)
		}
	}
	if _, err := d.Write("Inbox/無いフォルダ/a.md", "", []byte("x\n"), true, false); err == nil {
		t.Fatal("無いフォルダに作った")
	}
	if names, _ := os.ReadDir(filepath.Join(dir, "Inbox")); len(names) != 2 {
		t.Fatalf("何か作られた: %v", names)
	}
}

func TestTrashMovesOnlyTheVersionSeen(t *testing.T) {
	dir, d := vaultDir(t)
	put(t, dir, "Inbox/a.md", "一つ目\n", 0o644)
	res, err := d.Trash("Inbox/a.md", Sum([]byte("一つ目\n")), false)
	if err != nil || res.Status != TrashDone || res.To != ".trash/a.md" {
		t.Fatalf("%+v %v", res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".trash/a.md")); string(b) != "一つ目\n" {
		t.Fatalf(".trash の中身: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "Inbox/a.md")); !os.IsNotExist(err) {
		t.Fatal("元の場所に残った")
	}
	// 同じ名前をもう一度移すと ` 1` を付けて並べる（上書きしない）。
	put(t, dir, "Inbox/a.md", "二つ目\n", 0o644)
	if res, err := d.Trash("Inbox/a.md", Sum([]byte("二つ目\n")), false); err != nil || res.To != ".trash/a 1.md" {
		t.Fatalf("%+v %v", res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".trash/a.md")); string(b) != "一つ目\n" {
		t.Fatal("先に移したものを上書きした")
	}
	// 見ていた版と違えば移さない。
	put(t, dir, "Inbox/b.md", "エージェントが書いた\n", 0o644)
	if res, err := d.Trash("Inbox/b.md", Sum([]byte("見ていた版\n")), false); err != nil || res.Status != TrashChanged {
		t.Fatalf("%+v %v", res, err)
	}
	if res, _ := d.Trash("Inbox/none.md", "x", false); res.Status != TrashGone {
		t.Fatalf("%+v", res)
	}
	// 場所と指示の紙。
	put(t, dir, "AGENTS.md", "指示\n", 0o644)
	if _, err := d.Trash("AGENTS.md", Sum([]byte("指示\n")), false); err == nil {
		t.Fatal("再認証なしで指示の紙を移した")
	}
	put(t, dir, "Data/Health/h.md", "h\n", 0o644)
	if _, err := d.Trash("Data/Health/h.md", Sum([]byte("h\n")), true); err == nil {
		t.Fatal("一覧の外を移した")
	}
	for _, p := range []string{"AGENTS.md", "Data/Health/h.md", "Inbox/b.md"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("%s が消えた", p)
		}
	}
}

func TestTrashRefusesASymlinkedTrash(t *testing.T) {
	dir, d := vaultDir(t)
	put(t, dir, "Inbox/a.md", "a\n", 0o644)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, ".trash")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Trash("Inbox/a.md", Sum([]byte("a\n")), false); err == nil {
		t.Fatal("symlink の .trash へ移した")
	}
	if _, err := os.Stat(filepath.Join(dir, "Inbox/a.md")); err != nil {
		t.Fatal("元の場所から消えた")
	}
}

// 照合と rename の間に書かれたら、移したものを元へ戻す。戻す場所に誰かが作っていたら両方残す。
func TestTrashRaceKeepsEverything(t *testing.T) {
	dir, d := vaultDir(t)
	put(t, dir, "Inbox/a.md", "見ていた版\n", 0o644)
	base := Sum([]byte("見ていた版\n"))

	// 1) rename の直前に中身が変わった（rename は変わった中身を運ぶ）→ 元へ戻す。
	swapped := false
	afterTrashRename = func() {
		if swapped {
			return
		}
		swapped = true
		// .trash に運ばれた乱数の名前のファイルを「割り込んだ書き手の版」に差し替える。
		ents, _ := os.ReadDir(filepath.Join(dir, ".trash"))
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".camp-trash-") {
				os.WriteFile(filepath.Join(dir, ".trash", e.Name()), []byte("割り込んだ版\n"), 0o644)
			}
		}
	}
	t.Cleanup(func() { afterTrashRename = nil })
	res, err := d.Trash("Inbox/a.md", base, false)
	if err != nil || res.Status != TrashChanged {
		t.Fatalf("%+v %v", res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "Inbox/a.md")); string(b) != "割り込んだ版\n" {
		t.Fatalf("元へ戻していない: %q", b)
	}

	// 2) 戻す場所にも誰かが作っていた → 移したものは .trash に残し、元の場所のものも残す。
	put(t, dir, "Inbox/c.md", "見ていた版\n", 0o644)
	afterTrashRename = func() {
		ents, _ := os.ReadDir(filepath.Join(dir, ".trash"))
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".camp-trash-") {
				os.WriteFile(filepath.Join(dir, ".trash", e.Name()), []byte("割り込んだ版\n"), 0o644)
			}
		}
		os.WriteFile(filepath.Join(dir, "Inbox/c.md"), []byte("作り直した版\n"), 0o644)
	}
	res, err = d.Trash("Inbox/c.md", base, false)
	if err != nil || res.Status != TrashKept || !strings.HasPrefix(res.To, ".trash/") {
		t.Fatalf("%+v %v", res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, res.To)); string(b) != "割り込んだ版\n" {
		t.Fatalf(".trash に残っていない: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "Inbox/c.md")); string(b) != "作り直した版\n" {
		t.Fatalf("作り直した版を消した: %q", b)
	}
}

func TestCommitDeletesOnlyTheTrashedPath(t *testing.T) {
	r := newRepo(t)
	// エージェントの書きかけ（stage）と、Camp が移すノート。
	put(t, r.work, "Human/Logs/b.md", "エージェントが stage した\n", 0o644)
	sh(t, r.work, "add", "Human/Logs/b.md")
	res, err := r.disk.Trash("Human/Logs/a.md", Sum([]byte("a\n")), false)
	if err != nil || res.Status != TrashDone {
		t.Fatalf("%+v %v", res, err)
	}
	cr, err := r.git.Commit([]Entry{{ID: 5, Path: "Human/Logs/a.md", SHA: Sum([]byte("a\n")), Delete: true}})
	if err != nil || cr.Commit == "" || len(cr.Done) != 1 || len(cr.Mismatch) != 0 {
		t.Fatalf("%+v %v", cr, err)
	}
	if got := sh(t, r.work, "show", "--name-status", "--format=%s", "HEAD"); got != "Camp: Human/Logs/a.md を .trash へ\n\nD\tHuman/Logs/a.md" {
		t.Fatalf("commit:\n%s", got)
	}
	if st := sh(t, r.work, "status", "--porcelain"); !strings.Contains(st, "M  Human/Logs/b.md") {
		t.Fatalf("エージェントの stage が変わった:\n%s", st)
	}
}

func TestCommitDoesNotDeleteWhatWasRecreated(t *testing.T) {
	r := newRepo(t)
	r.disk.Trash("Human/Logs/a.md", Sum([]byte("a\n")), false)
	put(t, r.work, "Human/Logs/a.md", "作り直した\n", 0o644)
	head := sh(t, r.work, "rev-parse", "HEAD")
	cr, err := r.git.Commit([]Entry{{ID: 5, Path: "Human/Logs/a.md", SHA: Sum([]byte("a\n")), Delete: true}})
	if err != nil || cr.Commit != "" || len(cr.Overwritten) != 1 || !cr.Overwritten[0].Delete {
		t.Fatalf("%+v %v", cr, err)
	}
	if sh(t, r.work, "rev-parse", "HEAD") != head {
		t.Fatal("commit した")
	}
}

func TestCommitOfATrashedUncommittedNoteIsJustDone(t *testing.T) {
	r := newRepo(t)
	e := r.write(t, 3, "Inbox/new.md", "作ってすぐ消す\n")
	if res, err := r.disk.Trash(e.Path, e.SHA, false); err != nil || res.Status != TrashDone {
		t.Fatalf("%+v %v", res, err)
	}
	head := sh(t, r.work, "rev-parse", "HEAD")
	cr, err := r.git.Commit([]Entry{{ID: 4, Path: e.Path, SHA: e.SHA, Delete: true}})
	if err != nil || cr.Commit != "" || len(cr.Done) != 1 {
		t.Fatalf("%+v %v", cr, err)
	}
	if sh(t, r.work, "rev-parse", "HEAD") != head {
		t.Fatal("commit した")
	}
}
