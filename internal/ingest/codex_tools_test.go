package ingest

import (
	"path/filepath"
	"testing"
)

// Codex の工具の引数と結果を、Claude と同じように検索へ入れる
// （2026-09-12、codex のフェーズレビュー 5）。
//
// **中身は `content` ではない。** 実データ（`rp`）では `custom_tool_call` が `input`、
// `custom_tool_call_output` が `output` を持ち、`content` は無い。`content` だけを見ていたので、
// Codex の工具は空のブロックになって索引から落ちていた（本物の記録 62 行のうち
// `tool_use`・`tool_result` が1件も作られていなかった）。
func TestCodexToolCallsAndOutputsAreIndexed(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	path := filepath.Join(root, "2026", "09", "12",
		"rollout-2026-09-12T00-00-00-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee.jsonl")

	write(t, path,
		`{"timestamp":"2026-09-12T00:00:00Z","type":"session_meta",`+
			`"payload":{"id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","cwd":"/w","cli_version":"0.1"}}`,
		`{"timestamp":"2026-09-12T00:00:01Z","type":"turn_context",`+
			`"payload":{"model":"gpt-5-codex","cwd":"/w","turn_id":"t1"}}`,
		// 実データと同じ形: call は input、output は output。
		`{"timestamp":"2026-09-12T00:00:02Z","type":"response_item",`+
			`"payload":{"type":"custom_tool_call","id":"c1","call_id":"call-1","name":"shell",`+
			`"input":"{\"command\":\"grep トウモロコシ /w/notes.md\"}"}}`,
		`{"timestamp":"2026-09-12T00:00:03Z","type":"response_item",`+
			`"payload":{"type":"custom_tool_call_output","id":"o1","call_id":"call-1",`+
			`"output":"ホットドッグの屋台で買った"}}`)

	if _, err := IngestWith(db, codexCollector{}, "h", root); err != nil {
		t.Fatal(err)
	}

	var use, result int
	if err := db.QueryRow(`select
			sum(case when kind = 'tool_use' then 1 else 0 end),
			sum(case when kind = 'tool_result' then 1 else 0 end)
		from message_blocks`).Scan(&use, &result); err != nil {
		t.Fatal(err)
	}
	if use != 1 || result != 1 {
		t.Fatalf("工具のブロックが tool_use %d / tool_result %d（1 と 1 のはず。"+
			"input と output を見ていないと空になって索引から落ちる）", use, result)
	}

	// **中身が入っていること。** 空でも行はできるが、空なら検索には入らない。
	for _, c := range []struct{ kind, want string }{
		{"tool_use", "トウモロコシ"},
		{"tool_result", "ホットドッグ"},
	} {
		var n int
		if err := db.QueryRow(
			`select count(*) from message_blocks where kind = ? and text like '%' || ? || '%'`,
			c.kind, c.want).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("%s の中身が索引に入っていない（%q で %d 件）", c.kind, c.want, n)
		}
	}
}
