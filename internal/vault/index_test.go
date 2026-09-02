package vault

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MoomA-0750/camp/internal/store"
)

func newDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "camp.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	return db
}

func index(t *testing.T, db *store.DB, root string) *IndexResult {
	t.Helper()
	res, err := Index(db, "h", root, "v")
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// 2回目は何も変わらない。索引は毎回走るので、ここが崩れると
// 変更検知（＝バックリンクの張り直し）が常に全件になる。
func TestIndexIsIdempotent(t *testing.T) {
	db := newDB(t)
	root := mkVault(t, map[string]string{"a.md": "本文", "b/c.md": "別"})

	first := index(t, db, root)
	if first.Added != 2 || first.Changed != 0 {
		t.Fatalf("初回は新規2のはず: %+v", first)
	}
	second := index(t, db, root)
	if second.Added != 0 || second.Changed != 0 || second.Same != 2 {
		t.Fatalf("2回目は同じ2のはず: %+v", second)
	}
	if second.Stored != 0 {
		t.Errorf("同じ中身を blobs に入れ直している: %d", second.Stored)
	}
}

// 中身が変われば変化として拾い、変わらなければ触らない。
func TestChangeIsDetectedByContent(t *testing.T) {
	db := newDB(t)
	root := mkVault(t, map[string]string{"a.md": "本文"})
	index(t, db, root)

	// mtime だけ変えて中身は同じ
	p := filepath.Join(root, "a.md")
	if err := os.WriteFile(p, []byte("本文"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := index(t, db, root); r.Changed != 0 {
		t.Errorf("中身が同じなら変化ではない: %+v", r)
	}

	if err := os.WriteFile(p, []byte("書き換えた"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := index(t, db, root); r.Changed != 1 {
		t.Errorf("中身が変わったら変化: %+v", r)
	}
}

// 消えたノートは行を消さない。中身も blobs に残る。
// 「Campにしか残っていない」を作るのがこの製品の存在理由。
func TestDeletedNoteKeepsItsRowAndBody(t *testing.T) {
	db := newDB(t)
	root := mkVault(t, map[string]string{"消える.md": "残ってほしい中身"})
	index(t, db, root)

	if err := os.Remove(filepath.Join(root, "消える.md")); err != nil {
		t.Fatal(err)
	}
	r := index(t, db, root)
	if r.Missing != 1 {
		t.Fatalf("消えたことを拾えていない: %+v", r)
	}

	var missing string
	var hash string
	if err := db.QueryRow(
		`select coalesce(missing_at,''), coalesce(sha256,'') from notes where path = ?`,
		"消える.md").Scan(&missing, &hash); err != nil {
		t.Fatalf("行が消えている: %v", err)
	}
	if missing == "" {
		t.Error("missing_at が付いていない")
	}
	var n int
	db.QueryRow(`select count(*) from blobs where sha256 = ?`, hash).Scan(&n)
	if n != 1 {
		t.Error("中身が blobs から消えている")
	}

	// 戻ってきたら印を外す
	if err := os.WriteFile(filepath.Join(root, "消える.md"), []byte("残ってほしい中身"), 0o644); err != nil {
		t.Fatal(err)
	}
	back := index(t, db, root)
	if back.Restored != 1 {
		t.Fatalf("戻ってきたことを拾えていない: %+v", back)
	}
	db.QueryRow(`select count(*) from notes where path = ? and missing_at is null`, "消える.md").Scan(&n)
	if n != 1 {
		t.Error("missing_at が外れていない")
	}
}

// 同じ中身のノートが複数あっても blobs は1つ。
func TestIdenticalNotesShareOneBlob(t *testing.T) {
	db := newDB(t)
	root := mkVault(t, map[string]string{
		"a.md": "同じ中身", "b.md": "同じ中身", "c.md": "違う",
	})
	r := index(t, db, root)
	if r.Stored != 2 {
		t.Fatalf("相異なる中身は2つのはず: %d", r.Stored)
	}
}

// 画像やPDFは行だけ持ち、中身は入れない。
func TestAssetsGetARowButNoBody(t *testing.T) {
	db := newDB(t)
	root := mkVault(t, map[string]string{"a.md": "本文", "img.png": "PNGのバイト列"})
	index(t, db, root)

	var kind, hash string
	if err := db.QueryRow(
		`select kind, coalesce(sha256,'') from notes where path = ?`, "img.png").Scan(&kind, &hash); err != nil {
		t.Fatal(err)
	}
	if kind != KindAsset {
		t.Errorf("kind=%q", kind)
	}
	if hash != "" {
		t.Error("画像の中身まで blobs に入れている")
	}
}
