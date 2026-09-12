package ingest

import (
	"path/filepath"
	"testing"
)

// 「消えた」印は**今回見た置き場の下だけ**に付ける（2026-09-12、codex のフェーズレビュー 3）。
//
// 取り込み器は1回に1つなので、Corpus には片方のエージェントのファイルしか入らない。
// ホストだけで絞ると、もう片方の記録が全部「消えた」ことになる。**実データで起きていた**:
// `rp` を claude → codex の順に読んだあと、Claude の 38 本すべてに印が付いていた。
func TestOneAgentsIngestDoesNotMarkTheOthersFilesMissing(t *testing.T) {
	db := newTestDB(t)
	base := t.TempDir()
	claudeRoot := filepath.Join(base, ".claude", "projects")
	codexRoot := filepath.Join(base, ".codex", "sessions")

	write(t, filepath.Join(claudeRoot, "p", "11111111-2222-4333-8444-555555555555.jsonl"),
		`{"type":"user","uuid":"u1","sessionId":"11111111-2222-4333-8444-555555555555",`+
			`"timestamp":"2026-09-12T00:00:00Z","cwd":"/w","message":{"role":"user","content":"x"}}`)
	write(t, filepath.Join(codexRoot, "2026", "09", "12", "rollout-2026-09-12T00-00-00-aaaaaaaa.jsonl"),
		`{"timestamp":"2026-09-12T00:00:00Z","type":"session_meta",`+
			`"payload":{"id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","cwd":"/w","cli_version":"0.1"}}`,
		`{"timestamp":"2026-09-12T00:00:01Z","type":"response_item",`+
			`"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"x"}]}}`)

	// 同じホストに、両方のエージェントの記録を順に取り込む。
	if _, err := IngestWith(db, claudeCollector{}, "h", claudeRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := IngestWith(db, codexCollector{}, "h", codexRoot); err != nil {
		t.Fatal(err)
	}

	var missing int
	if err := db.QueryRow(
		`select count(*) from source_files where missing_at is not null`).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Fatalf("消えた印が %d 件（0 のはず。片方の取り込みが、もう片方を消えた扱いにしている）", missing)
	}

	// 逆順でも同じ（後から読んだほうが、先のものを消さない）。
	if _, err := IngestWith(db, claudeCollector{}, "h", claudeRoot); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(
		`select count(*) from source_files where missing_at is not null`).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Fatalf("2巡目で消えた印が %d 件（0 のはず）", missing)
	}
}

// **在るが今回は運ばなかったファイルに、消えた印を付けない**（同レビュー 2）。
//
// 向こうのホストから少しずつ運ぶとき、1回の蓋に達したファイルは一覧に載せない。
// そのとき「消えた」と書くと嘘になるし、次回そこから読み直せなくなる。
func TestDeferredFilesAreNotMarkedMissing(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	a := filepath.Join(root, "11111111-2222-4333-8444-555555555555.jsonl")
	b := filepath.Join(root, "22222222-3333-4444-8555-666666666666.jsonl")
	line := func(id, u string) string {
		return `{"type":"user","uuid":"` + u + `","sessionId":"` + id + `",` +
			`"timestamp":"2026-09-12T00:00:00Z","cwd":"/w","message":{"role":"user","content":"x"}}`
	}
	write(t, a, line("11111111-2222-4333-8444-555555555555", "u1"))
	write(t, b, line("22222222-3333-4444-8555-666666666666", "u2"))

	// 1回目は両方入れる。
	if _, err := Ingest(db, "h", root); err != nil {
		t.Fatal(err)
	}

	// 2回目は b を「今回は運ばなかった」ことにする（走査からは外れるが、在ることは分かっている）。
	col := claudeCollector{}
	fset, err := LocalFiles{Root: root}.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer fset.Close()
	res, err := IngestFrom(db, col, "h", deferring{FileSet: fset, skip: b}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = res

	var missing int
	if err := db.QueryRow(
		`select count(*) from source_files where missing_at is not null`).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Fatalf("見送ったファイルに消えた印が %d 件付いた（0 のはず）", missing)
	}
}

// deferring は1本だけ「今回は運ばなかった」ことにする読み手。
type deferring struct {
	FileSet
	skip string
}

func (d deferring) List(at map[string]int64) (*Listing, error) {
	lst, err := d.FileSet.List(at)
	if err != nil {
		return nil, err
	}
	var keep []FileInfo
	for _, fi := range lst.Files {
		if fi.Path == d.skip {
			lst.Deferred = append(lst.Deferred, fi.Path)
			continue
		}
		keep = append(keep, fi)
	}
	lst.Files = keep
	return lst, nil
}
