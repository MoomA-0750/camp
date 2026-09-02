package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkVault は最小の Vault を作る。
func mkVault(t *testing.T, files map[string]string) string {
	t.Helper()
	// 末尾に文字を含む名前にする。t.TempDir() の末尾は "002" のような数字で、
	// 小文字化しても同じパスになってしまい、表記違いの検査が成り立たない。
	root := filepath.Join(t.TempDir(), "Obsidian-Vault")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func rels(r *Result) map[string]File {
	m := map[string]File{}
	for _, f := range r.Files {
		m[f.Rel] = f
	}
	return m
}

// ドット始まりのディレクトリは**降りる前に**切る。実測で .claude/worktrees には
// この Vault の11倍のファイルがある。降りてから捨てる形にすると索引が実用にならない。
func TestDotDirsArePrunedNotFiltered(t *testing.T) {
	root := mkVault(t, map[string]string{
		"Human/Logs/2026-09-02.md":      "本文",
		".claude/worktrees/a/big.md":    "ノイズ",
		".git/objects/x/y.md":           "ノイズ",
		".trash/消したもの.md":               "ノイズ",
		".obsidian/plugins/p/README.md": "ノイズ",
	})
	res, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("実ノート1件のはずが %d: %v", len(res.Files), rels(res))
	}
	// 切ったのが「ディレクトリの入口」であること。中のファイル名が
	// Pruned に出てきたら、降りてしまっている。
	for _, p := range res.Pruned {
		if filepath.Dir(p) != "." {
			t.Errorf("入口より下まで降りている: %s", p)
		}
	}
	if len(res.Pruned) != 4 {
		t.Errorf("切ったディレクトリは4つのはず: %v", res.Pruned)
	}
}

// リンク先は .md に限らない。.base や .pdf も行として持つ。
func TestNonMarkdownIsIndexed(t *testing.T) {
	root := mkVault(t, map[string]string{
		"Human/Dashboards/To-Do.base": "views: []",
		"attachments/資料.pdf":          "%PDF",
		"Human/Logs/x.md":             "本文",
	})
	res, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	got := rels(res)
	if got["Human/Dashboards/To-Do.base"].Kind != KindBase {
		t.Errorf(".base を base として持てていない: %+v", got["Human/Dashboards/To-Do.base"])
	}
	if got["attachments/資料.pdf"].Kind != KindAsset {
		t.Errorf(".pdf を asset として持てていない")
	}
	if got["Human/Logs/x.md"].Kind != KindMarkdown {
		t.Errorf(".md を markdown として持てていない")
	}
}

// 大文字小文字を畳まない。畳むのは「触っていない側を触ったことにする」こと。
func TestCaseIsPreserved(t *testing.T) {
	root := mkVault(t, map[string]string{
		"Human/Projects/Camp.md": "A",
		"human/projects/camp.md": "B",
	})
	res, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	got := rels(res)
	if len(got) != 2 {
		t.Fatalf("別物として2件残るはずが %d: %v", len(got), got)
	}
}

// シンボリックリンクは辿らない。Vault の外へ出るし輪を作れる。
func TestSymlinksAreNotFollowed(t *testing.T) {
	root := mkVault(t, map[string]string{"a.md": "本文"})
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "外.md"), []byte("外"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip("symlink を作れない環境")
	}
	res, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Files {
		if f.Rel != "a.md" {
			t.Errorf("リンクの先まで拾っている: %s", f.Rel)
		}
	}
}

// lowerLast は最後のパス要素だけ小文字にする（Obsidian-Vault → Obsidian-vault 相当）。
func lowerLast(p string) string {
	d, b := filepath.Split(p)
	return filepath.Join(d, strings.ToLower(b))
}

func removeFile(root, rel string) error {
	return os.Remove(filepath.Join(root, filepath.FromSlash(rel)))
}
