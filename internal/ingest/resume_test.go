package ingest

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/MoomA-0750/camp/internal/store"
)

// 取り込みは同じ範囲を2回読む（要約する `summarize` と、行を入れる `writeMessages`）。
// **両者の終点が揃っていないと、台帳には「2周目の終点」と「1周目のハッシュ」が並ぶ。**
// 次回それを rotatedFrom が「同じ位置に違う中身」と読み、世代が進んで
// ファイル1本ぶんの行がもう一度入る（2026-09-03 に実際に起きた事故と同じ経路）。
//
// 実装の中身（2周読むこと）には触れず、**台帳に残った値だけ**で縛る。
// 2周目に終点を渡し忘れても、1周目の蓋を外しても、ここが落ちる。

const resumeSID = "11111111-2222-4333-8444-666666666666"

func TestTheResumeHashMatchesTheRecordedOffset(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	path := filepath.Join(root, resumeSID+".jsonl")
	write(t, path, resumeLine(1), resumeLine(2), resumeLine(3))

	if _, err := Ingest(db, "h", root); err != nil {
		t.Fatal(err)
	}
	assertResumeMatches(t, db, path, "初回")

	// 追記してもう一度（差分読みの経路でも揃っていること）。
	appendTo(t, path, resumeLine(4))
	if _, err := Ingest(db, "h", root); err != nil {
		t.Fatal(err)
	}
	assertResumeMatches(t, db, path, "追記後")

	// 世代が進んでいない＝行が二重に入っていない。
	var n, distinct int
	if err := db.QueryRow(`select count(*), count(distinct byte_offset) from messages`).
		Scan(&n, &distinct); err != nil {
		t.Fatal(err)
	}
	if n != 4 || distinct != 4 {
		t.Fatalf("messages %d 件（うち別の位置 %d 件）。4 と 4 のはず（二重に入っている）", n, distinct)
	}
}

func assertResumeMatches(t *testing.T, db *store.DB, path, when string) {
	t.Helper()
	var off int64
	var sha sql.NullString
	if err := db.QueryRow(
		`select ingested_offset, resume_sha from source_files where path = ?`, path,
	).Scan(&off, &sha); err != nil {
		t.Fatal(err)
	}
	if !sha.Valid || sha.String == "" {
		t.Fatalf("%s: resume_sha が空。再開点が残っていない", when)
	}
	if want := resumeSHA(path, off); sha.String != want {
		t.Fatalf("%s: resume_sha が %q。ingested_offset %d の位置は %q。"+
			"要約と書き込みの終点が食い違っている", when, sha.String, off, want)
	}
}

// 蓋（1回に読む上限）を掛けると、要約は末尾より手前で止まる。**そのとき台帳に残る
// 位置と再開点が揃っていること**を縛る。
//
// 上の TestTheResumeHashMatchesTheRecordedOffset だけでは足りない——蓋が無いと
// 要約と書き込みの終点が必ず一致してしまい、「書き込む側に終点を渡し忘れる」変異を
// 素通しする（2026-09-12 に測って確かめた）。蓋があると、書き込む側が最後まで走った
// 瞬間に位置と再開点が食い違い、ここが落ちる。
func TestACappedIngestLeavesAMatchingResumePoint(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	path := filepath.Join(root, resumeSID+".jsonl")
	l1 := resumeLine(1)
	write(t, path, l1, resumeLine(2), resumeLine(3), resumeLine(4))

	// 1行ぶんの蓋。**行の途中では切らない**ので、1回目は1行だけ入る。
	res, err := IngestLimited(db, claudeCollector{}, "h", root, int64(len(l1)))
	if err != nil {
		t.Fatal(err)
	}
	if res.Capped != 1 {
		t.Fatalf("蓋で止めたファイルが %d 件（1 のはず）", res.Capped)
	}
	if n := count(t, db, `select count(*) from messages`); n != 1 {
		t.Fatalf("messages %d 件（1 のはず。蓋を超えて読んでいる）", n)
	}
	assertResumeMatches(t, db, path, "蓋あり1回目")

	// 続きを蓋なしで読む。**二重に入らない。**
	res2, err := IngestLimited(db, claudeCollector{}, "h", root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Capped != 0 {
		t.Fatalf("2回目も蓋で止まった（%d 件）。続きを読み切れていない", res2.Capped)
	}
	var n, distinct int
	if err := db.QueryRow(`select count(*), count(distinct byte_offset) from messages`).
		Scan(&n, &distinct); err != nil {
		t.Fatal(err)
	}
	if n != 4 || distinct != 4 {
		t.Fatalf("messages %d 件（うち別の位置 %d 件）。4 と 4 のはず（二重に入っている）", n, distinct)
	}
	assertResumeMatches(t, db, path, "続きを読んだ後")
}

// WalkFileRange は渡された終点で止まる。**0 は「最後まで」**。
func TestWalkStopsAtTheGivenEnd(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "x.jsonl")
	l1, l2, l3 := resumeLine(1), resumeLine(2), resumeLine(3)
	write(t, path, l1, l2, l3)

	stop := int64(len(l1) + 1 + len(l2) + 1) // 2行目の直後（改行込み）
	var got int
	res, err := WalkFileRange(path, 0, stop, ParseLine, func(*Line) error { got++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 || res.EndOffset != stop {
		t.Fatalf("%d 行・終点 %d（2 行・終点 %d のはず）", got, res.EndOffset, stop)
	}

	got = 0
	res, err = WalkFileRange(path, 0, 0, ParseLine, func(*Line) error { got++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	want := int64(len(l1) + len(l2) + len(l3) + 3)
	if got != 3 || res.EndOffset != want {
		t.Fatalf("終点 0 で %d 行・終点 %d（3 行・終点 %d のはず。0 は最後まで）", got, res.EndOffset, want)
	}
}

func resumeLine(n int) string {
	return fmt.Sprintf(
		`{"type":"user","uuid":"u%d","sessionId":"%s","timestamp":"2026-09-12T00:00:0%dZ",`+
			`"cwd":"/w","message":{"role":"user","content":"x"}}`, n, resumeSID, n)
}
