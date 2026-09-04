package retain_test

import (
	"path/filepath"
	"testing"

	"github.com/MoomA-0750/camp/internal/retain"
	"github.com/MoomA-0750/camp/internal/store"
)

// 数えられなかった列を「0件」にしない。
//
// 2026-09-04 の outer gate の指摘。走査の失敗を 0 に潰すと、総なめは
// 「秘密が残っていないこと」の検知器ではなくなる。
func TestSweepDoesNotTurnFailuresIntoZero(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "camp.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(); err != nil {
		t.Fatal(err)
	}

	// 走査できない表を作る。存在しないモジュールの仮想表は開けない。
	if _, err := db.Exec(
		`create virtual table broken using fts5(x, content='nowhere', content_rowid='id')`,
	); err != nil {
		t.Skipf("この環境では壊れた表を作れない: %v", err)
	}

	hits, err := retain.Sweep(db, []byte("なにか"))
	if err != nil {
		t.Fatalf("Sweep 自体が落ちた: %v", err)
	}
	if len(retain.Unknowns(hits)) == 0 {
		t.Error("数えられない列があるのに、そう言っていない")
	}
	if retain.Total(hits) == 0 {
		t.Error("数えられない列があるのに合計 0 件と言っている")
	}
}
