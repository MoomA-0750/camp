package ingest

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/MoomA-0750/camp/internal/store"
)

// 会話1往復ぶんの行を作る。中身は最小限で、必要なのは形だけ。
func convoLines(sessionID string, from, to int) string {
	out := ""
	for i := from; i < to; i++ {
		out += fmt.Sprintf(
			`{"type":"user","uuid":"u%[1]s-%[2]d","sessionId":"%[1]s","session_id":"r-%[1]s",`+
				`"timestamp":"2026-09-02T00:%02[2]d:00.000Z","cwd":"/nonexistent/proj",`+
				`"message":{"role":"user","content":"行 %[2]d"}}`+"\n", sessionID, i)
	}
	return out
}

func newTestDB(t *testing.T) *store.DB {
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

func count(t *testing.T, db *store.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// 差分追尾。書き込み途中のファイルを読んでも、完了した行までしか
// コミットしない。追記しながら回しても重複も欠落もしない。
func TestTailPartialAndAppend(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	dir := filepath.Join(root, "-nonexistent-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const sid = "11111111-2222-4333-8444-555555555555"
	path := filepath.Join(dir, sid+".jsonl")

	// 3行＋改行で終わっていない4行目。フラッシュ途中を再現する。
	complete := convoLines(sid, 0, 3)
	partial := `{"type":"user","uuid":"u-partial","sessionId":"` + sid + `","messag`
	if err := os.WriteFile(path, []byte(complete+partial), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}

	if n := count(t, db, `select count(*) from messages`); n != 3 {
		t.Fatalf("messages %d。完了した3行だけであるべき", n)
	}
	var offset int64
	var tail []byte
	if err := db.QueryRow(
		`select ingested_offset, pending_tail from source_files where path = ?`, path,
	).Scan(&offset, &tail); err != nil {
		t.Fatal(err)
	}
	if offset != int64(len(complete)) {
		t.Fatalf("ingested_offset %d、完了分 %d であるべき", offset, len(complete))
	}
	if string(tail) != partial {
		t.Fatalf("pending_tail が %q。未完了の末尾が退避されていない", tail)
	}

	// 途中だった行が完了し、さらに追記された。
	rest := convoLines(sid, 3, 6)
	if err := os.WriteFile(path, []byte(complete+rest), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}

	if n := count(t, db, `select count(*) from messages`); n != 6 {
		t.Fatalf("messages %d。6行であるべき（重複も欠落も無し）", n)
	}
	if n := count(t, db, `select count(distinct byte_offset) from messages`); n != 6 {
		t.Fatalf("byte_offset が重複している")
	}
}

// 中身が入れ替わったファイルは、同じ行のオフセットを0に戻すのではなく
// 世代を進めた別の行にする。戻すと新しい行が
// UNIQUE(source_file_id, byte_offset) に当たって黙って捨てられる。
func TestRotationCreatesNewIncarnation(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	dir := filepath.Join(root, "-nonexistent-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const sid = "22222222-2222-4333-8444-555555555555"
	path := filepath.Join(dir, sid+".jsonl")

	if err := os.WriteFile(path, []byte(convoLines(sid, 0, 5)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	before := count(t, db, `select count(*) from messages`)
	if before != 5 {
		t.Fatalf("messages %d、5であるべき", before)
	}

	// 切り詰めて別の内容を書く。size < ingested_offset になる。
	if err := os.WriteFile(path, []byte(convoLines(sid, 90, 92)), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Ingest(db, "testhost", root)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reread != 1 {
		t.Fatalf("世代を進めたファイルが %d。1であるべき", res.Reread)
	}

	if n := count(t, db, `select count(*) from source_files where path = ?`, path); n != 2 {
		t.Fatalf("source_files が %d 行。世代ごとに1行で2行であるべき", n)
	}
	if n := count(t, db,
		`select count(*) from source_files where path = ? and superseded_at is not null`, path); n != 1 {
		t.Fatalf("古い世代が閉じられていない")
	}
	// 新しい2行が捨てられていないこと。古い5行も消えていないこと。
	if n := count(t, db, `select count(*) from messages`); n != 7 {
		t.Fatalf("messages %d。古い5行＋新しい2行で7であるべき", n)
	}
}

// 元ファイルが消えても行は削除しない。消えた事実だけ記録する（D-001）。
func TestMissingFileKeepsRows(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	dir := filepath.Join(root, "-nonexistent-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const sid = "33333333-2222-4333-8444-555555555555"
	path := filepath.Join(dir, sid+".jsonl")

	if err := os.WriteFile(path, []byte(convoLines(sid, 0, 4)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	res, err := Ingest(db, "testhost", root)
	if err != nil {
		t.Fatal(err)
	}
	if res.Missing != 1 {
		t.Fatalf("消えていたファイルが %d。1であるべき", res.Missing)
	}
	if n := count(t, db, `select count(*) from messages`); n != 4 {
		t.Fatalf("messages %d。元が消えても4行残るべき", n)
	}
	if n := count(t, db,
		`select count(*) from source_files where path = ? and missing_at is not null`, path); n != 1 {
		t.Fatalf("missing_at が立っていない")
	}
	if n := count(t, db, `select count(*) from sessions where id = ?`, sid); n != 1 {
		t.Fatalf("セッションが消えている")
	}

	// 2回目の走査で missing_at を上書きしない（消えた時刻を保つ）。
	var first string
	if err := db.QueryRow(`select missing_at from source_files where path = ?`, path).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if _, err := Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	var second string
	if err := db.QueryRow(`select missing_at from source_files where path = ?`, path).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("missing_at が上書きされた: %s -> %s", first, second)
	}
}

// (dev, inode) も size も食い違わない書き直し。前より長くなっているので
// 「切り詰め」でも「別の実体」でもない。再開点のハッシュだけが唯一の手がかり。
// これを見逃すと、新しい中身の途中から読み始めて黙って壊れる。
func TestRewriteLongerSameInodeIsDetected(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	dir := filepath.Join(root, "-nonexistent-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const sid = "44444444-2222-4333-8444-555555555555"
	path := filepath.Join(dir, sid+".jsonl")

	if err := os.WriteFile(path, []byte(convoLines(sid, 0, 3)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}

	// 同じ inode のまま、前より長い別の内容で上書きする。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(convoLines(sid, 50, 60)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	res, err := Ingest(db, "testhost", root)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reread != 1 {
		t.Fatalf("書き直しを検出できていない（世代を進めたファイル %d）", res.Reread)
	}
	if n := count(t, db, `select count(*) from messages`); n != 13 {
		t.Fatalf("messages %d。古い3行＋新しい10行で13であるべき", n)
	}
	if n := count(t, db, `select count(*) from source_files where path = ?`, path); n != 2 {
		t.Fatalf("source_files が %d 行。2世代であるべき", n)
	}
}
