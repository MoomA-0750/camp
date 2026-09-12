package ingest

import (
	"path/filepath"
	"testing"
)

// 差分だけ読むときも、Codex の model と cwd を落とさない
// （2026-09-12、codex のフェーズレビュー 4）。
//
// Codex は `turn_context` の model・cwd を後の行へ持ち回る。前回のうちに `turn_context` を
// 読み終えていると、次に追記された行だけを空の解釈器で読むことになり、model も cwd も空になる。
// usage は空の model で計上される。**Claude は状態を持たないので、これは Codex にだけ起きる差。**
//
// 向こうのホストの記録は1回に運ぶ量に蓋があるので、なおさら起きやすい。
func TestCodexKeepsItsModelAcrossIncrementalReads(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	path := filepath.Join(root, "2026", "09", "12",
		"rollout-2026-09-12T00-00-00-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee.jsonl")

	// 1回目: session_meta と turn_context まで（model と cwd はここにしか出てこない）。
	write(t, path,
		`{"timestamp":"2026-09-12T00:00:00Z","type":"session_meta",`+
			`"payload":{"id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","cwd":"/w","cli_version":"0.1"}}`,
		`{"timestamp":"2026-09-12T00:00:01Z","type":"turn_context",`+
			`"payload":{"model":"gpt-5-codex","cwd":"/w","turn_id":"t1"}}`,
		`{"timestamp":"2026-09-12T00:00:02Z","type":"response_item",`+
			`"payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ひとつめ"}]}}`)
	if _, err := IngestWith(db, codexCollector{}, "h", root); err != nil {
		t.Fatal(err)
	}

	// 2回目: **turn_context を挟まずに**本文だけ追記する（実際の続きの会話と同じ）。
	appendTo(t, path,
		`{"timestamp":"2026-09-12T00:00:03Z","type":"response_item",`+
			`"payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ふたつめ"}]}}`)
	res, err := IngestWith(db, codexCollector{}, "h", root)
	if err != nil {
		t.Fatal(err)
	}
	if res.Messages != 1 {
		t.Fatalf("追記分が %d 行（1 のはず）", res.Messages)
	}

	// 追記された行にも model と cwd が乗っていること。
	var model, cwd string
	if err := db.QueryRow(`select coalesce(model,''), coalesce(cwd,'')
		from messages order by byte_offset desc limit 1`).Scan(&model, &cwd); err != nil {
		t.Fatal(err)
	}
	if model != "gpt-5-codex" {
		t.Fatalf("追記分の model が %q（gpt-5-codex のはず。差分読みで持ち回りが切れている）", model)
	}
	if cwd != "/w" {
		t.Fatalf("追記分の cwd が %q（/w のはず）", cwd)
	}

	// **セッションの last_model も空にならない。**
	var last string
	if err := db.QueryRow(`select coalesce(last_model,'') from sessions`).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if last != "gpt-5-codex" {
		t.Fatalf("sessions.last_model が %q（gpt-5-codex のはず）", last)
	}
}
