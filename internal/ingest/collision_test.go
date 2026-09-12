package ingest

import (
	"path/filepath"
	"testing"

	"github.com/MoomA-0750/camp/internal/store"
)

// **別のホストが既に使っている session id は書かない**（2026-09-12、codex のフェーズレビュー 1）。
//
// `sessions.id` はホストを含まない主キーなので、向こうのホストに同じ id があると、
// upsert が手元のセッションの題名・作業場所・モデルを向こうの値で上書きし、向こうの
// messages が手元のセッションにぶら下がる（`host_id` は更新されないので所属は手元のまま）。
//
// 設計では「書かずに数える」と決めていたが、実装されていなかった。
const collideID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

func TestASessionIDOwnedByAnotherHostIsNotOverwritten(t *testing.T) {
	db := newTestDB(t)

	local := t.TempDir()
	remote := t.TempDir()
	write(t, filepath.Join(local, collideID+".jsonl"),
		collideLine("u1", "/手元の場所", "手元の話"))
	write(t, filepath.Join(remote, collideID+".jsonl"),
		collideLine("u9", "/向こうの場所", "向こうの話"))

	if _, err := Ingest(db, "手元", local); err != nil {
		t.Fatal(err)
	}
	before := sessionRow(t, db)

	// 同じ id を、別のホストとして取り込む。
	res, err := Ingest(db, "向こう", remote)
	if err != nil {
		t.Fatal(err)
	}
	if res.Collisions != 1 {
		t.Fatalf("衝突の件数が %d（1 のはず。書かなかったことを数えて出す）", res.Collisions)
	}
	after := sessionRow(t, db)
	if before != after {
		t.Fatalf("手元のセッションが上書きされた\n前: %+v\n後: %+v", before, after)
	}

	// **向こうの行も、手元のセッションにぶら下げない。**
	var n int
	if err := db.QueryRow(`select count(*) from messages where session_id = ?`, collideID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("messages が %d 件（1 のはず。向こうの行まで同じセッションに入っている）", n)
	}

	// **消えた印も付けない。** 向こうのファイルは在って、読めてもいる（書かなかっただけ）。
	var missing int
	if err := db.QueryRow(`select count(*) from source_files where missing_at is not null`).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Fatalf("消えた印が %d 件（0 のはず）", missing)
	}
}

// sessSnap は上書きされたかを見るための写し。
type sessSnap struct {
	host  int64
	title string
	cwd   string
}

func sessionRow(t *testing.T, db *store.DB) sessSnap {
	t.Helper()
	var s sessSnap
	if err := db.QueryRow(
		`select host_id, coalesce(ai_title,''), coalesce(last_cwd,'') from sessions where id = ?`,
		collideID).Scan(&s.host, &s.title, &s.cwd); err != nil {
		t.Fatal(err)
	}
	return s
}

func collideLine(uuid, cwd, text string) string {
	return `{"type":"user","uuid":"` + uuid + `","sessionId":"` + collideID + `",` +
		`"timestamp":"2026-09-12T00:00:00Z","cwd":"` + cwd + `",` +
		`"message":{"role":"user","content":"` + text + `"}}`
}
