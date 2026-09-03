package snapshot_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MoomA-0750/camp/internal/snapshot"
	"github.com/MoomA-0750/camp/internal/store"
)

func seedDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "camp.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`insert into hosts(name) values('h')`); err != nil {
		t.Fatal(err)
	}
	return db
}

// 取って、消して、戻して、doctor が通る。
//
// **取れているつもりのバックアップは取れていない。** 戻して開いて点検まで
// 通らなければ、退避先として成立していない。
func TestTakeItDeleteItPutItBackAndTheDoctorPasses(t *testing.T) {
	db := seedDB(t)
	dir := t.TempDir()
	enc := filepath.Join(dir, "camp.snapshot")
	key := []byte("1Password から渡ってくる長い鍵")

	made, err := snapshot.Create(db, enc, key)
	if err != nil {
		t.Fatal(err)
	}
	if made.PlainBytes == 0 || made.SHA256 == "" {
		t.Fatalf("素性が空: %+v", made)
	}
	if made.CipherBytes <= 0 {
		t.Error("何も書き出していない")
	}

	// 元を消す。ここから先は退避先しか残っていない。
	orig := db.Path
	db.Close()
	if err := os.Remove(orig); err != nil {
		t.Fatal(err)
	}

	back := filepath.Join(dir, "restored.sqlite")
	got, err := snapshot.Restore(enc, back, key)
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA256 != made.SHA256 {
		t.Errorf("戻したものの sha256 が違う:\n取った %s\n戻した %s", made.SHA256, got.SHA256)
	}
	if got.PlainBytes != made.PlainBytes {
		t.Errorf("大きさが違う: %d → %d", made.PlainBytes, got.PlainBytes)
	}

	restored, err := store.Open(back)
	if err != nil {
		t.Fatalf("戻したが開けない: %v", err)
	}
	defer restored.Close()
	checks, err := restored.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range checks {
		// 元ファイルの有無は退避先の話ではないので、そこだけは見ない。
		if !c.OK && c.Name != "source_files 実体" {
			t.Errorf("戻したDBが点検に落ちた: %s — %s", c.Name, c.Detail)
		}
	}
	var n int
	if err := restored.QueryRow(`select count(*) from hosts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("中身が戻っていない: hosts %d 行", n)
	}
}

// 既にあるファイルを黙って上書きしない。
func TestItRefusesToClobber(t *testing.T) {
	db := seedDB(t)
	dir := t.TempDir()
	enc := filepath.Join(dir, "camp.snapshot")
	key := []byte("じゅうぶんに長い鍵")

	if _, err := snapshot.Create(db, enc, key); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.Create(db, enc, key); err == nil {
		t.Error("同じ名前で2回取れてしまった。前のバックアップが消える")
	}

	back := filepath.Join(dir, "restored.sqlite")
	if _, err := snapshot.Restore(enc, back, key); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.Restore(enc, back, key); err == nil {
		t.Error("既にあるDBの上へ戻してしまった")
	}
}

// 鍵が違うとき、中途半端なファイルを残さない。
func TestAFailedRestoreLeavesNothingBehind(t *testing.T) {
	db := seedDB(t)
	dir := t.TempDir()
	enc := filepath.Join(dir, "camp.snapshot")
	if _, err := snapshot.Create(db, enc, []byte("ただしい鍵ただしい鍵")); err != nil {
		t.Fatal(err)
	}
	back := filepath.Join(dir, "restored.sqlite")
	if _, err := snapshot.Restore(enc, back, []byte("ちがう鍵ちがう鍵")); err == nil {
		t.Fatal("違う鍵で戻せた")
	}
	if _, err := os.Stat(back); err == nil {
		t.Error("失敗したのにファイルが残っている。壊れたDBを掴まされる")
	}
}

// 平文の一時ファイルを残さない。
func TestNoPlaintextIsLeftOnDisk(t *testing.T) {
	db := seedDB(t)
	dir := t.TempDir()
	enc := filepath.Join(dir, "camp.snapshot")
	if _, err := snapshot.Create(db, enc, []byte("じゅうぶんに長い鍵")); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() != filepath.Base(enc) {
			t.Errorf("余計なファイルが残っている: %s", e.Name())
		}
	}
}
