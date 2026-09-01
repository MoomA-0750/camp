package ingest

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoomA-0750/camp/internal/store"
)

// assistant 行1本。usage の中身を指定できる。
func assistantLine(sessionID, uuid, msgID, model string, minute, out, cacheRead int) string {
	return fmt.Sprintf(
		`{"type":"assistant","uuid":"%[2]s","sessionId":"%[1]s","session_id":"r-%[1]s",`+
			`"timestamp":"2026-09-02T00:%02[5]d:00.000Z","cwd":"/nonexistent/proj","effort":"high",`+
			`"requestId":"req_%[3]s",`+
			`"message":{"id":"%[3]s","role":"assistant","model":"%[4]s","content":[{"type":"text","text":"x"}],`+
			`"usage":{"input_tokens":2,"output_tokens":%[6]d,"cache_creation_input_tokens":10,`+
			`"cache_read_input_tokens":%[7]d,"cache_creation":{"ephemeral_1h_input_tokens":10,"ephemeral_5m_input_tokens":0},`+
			`"output_tokens_details":{"thinking_tokens":1},"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0},`+
			`"service_tier":"standard","speed":"standard",`+
			`"iterations":[{"type":"message","output_tokens":%[6]d}]}}}`+"\n",
		sessionID, uuid, msgID, model, minute, out, cacheRead)
}

func writeSession(t *testing.T, root, sid, body string) string {
	t.Helper()
	dir := filepath.Join(root, "-nonexistent-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func oneInt(t *testing.T, db *store.DB, q string, args ...any) int64 {
	t.Helper()
	var v int64
	if err := db.QueryRow(q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

// ストリーミングの途中経過。同じ api_message_id が出力を伸ばしながら
// 何行も現れる。最初の1行を採る実装だと過少計上になる（実コーパスで48件）。
// しかも途中経過と確定値が別々の取り込みパスに分かれても結果は同じでなければならない。
func TestStreamingUsageTakesFinalValue(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	const sid = "aaaaaaaa-2222-4333-8444-555555555555"

	// 1回目: 途中経過だけが書かれている状態
	partial := convoLines(sid, 0, 1) + assistantLine(sid, "a1", "msg_stream", "claude-opus-5", 1, 8, 5000)
	path := writeSession(t, root, sid, partial)
	if _, err := Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	if got := oneInt(t, db, `select output_tokens from usage where api_message_id = 'msg_stream'`); got != 8 {
		t.Fatalf("途中経過の時点で output_tokens=%d、8 であるべき", got)
	}

	// 2回目: 確定値の行が追記される
	final := assistantLine(sid, "a2", "msg_stream", "claude-opus-5", 1, 261, 5000)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(final)
	f.Close()

	if _, err := Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `select count(*) from usage`); n != 1 {
		t.Fatalf("usage %d 行。api_message_id ごとに1行であるべき", n)
	}
	if got := oneInt(t, db, `select output_tokens from usage where api_message_id = 'msg_stream'`); got != 261 {
		t.Fatalf("output_tokens=%d、確定値の 261 であるべき", got)
	}
	// input 側は不変。伸びるのは output だけ。
	if got := oneInt(t, db, `select cache_read_input_tokens from usage`); got != 5000 {
		t.Fatalf("cache_read=%d、5000 であるべき", got)
	}
	// messages 側には両方残る。捨てるのは計上だけ。
	if n := count(t, db, `select count(*) from messages where api_message_id = 'msg_stream'`); n != 2 {
		t.Fatalf("messages %d 行。2行とも残るべき", n)
	}
}

// 複製が先に来ても順序に依らない。max なので冪等。
func TestUsageIsOrderIndependent(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	const sid = "bbbbbbbb-2222-4333-8444-555555555555"
	// 確定値を先、途中経過を後に置く（fork の複製が先に読まれる状況）
	body := convoLines(sid, 0, 1) +
		assistantLine(sid, "a1", "msg_x", "claude-opus-5", 1, 261, 5000) +
		assistantLine(sid, "a2", "msg_x", "claude-opus-5", 1, 8, 5000)
	writeSession(t, root, sid, body)
	if _, err := Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	if got := oneInt(t, db, `select output_tokens from usage where api_message_id = 'msg_x'`); got != 261 {
		t.Fatalf("output_tokens=%d、大きいほうの 261 であるべき", got)
	}
}

// <synthetic> は CLI がローカルで作った行。課金は発生していない。
// usage には入れず、messages には残す。
func TestSyntheticStaysOutOfUsage(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	const sid = "cccccccc-2222-4333-8444-555555555555"
	body := convoLines(sid, 0, 1) +
		assistantLine(sid, "a1", "msg_real", "claude-opus-5", 1, 100, 0) +
		assistantLine(sid, "a2", "msg_synth", "<synthetic>", 2, 0, 0)
	writeSession(t, root, sid, body)
	if _, err := Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `select count(*) from usage`); n != 1 {
		t.Fatalf("usage %d 行。<synthetic> は計上しない", n)
	}
	if n := count(t, db, `select count(*) from messages where model = '<synthetic>'`); n != 1 {
		t.Fatalf("messages に <synthetic> が %d 行。残すべき", n)
	}
}

// cost-state はタイムスタンプを持たない。累計値なので最大を採る。
func TestCostStateFillsSessionCost(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	const sid = "dddddddd-2222-4333-8444-555555555555"
	body := convoLines(sid, 0, 1) +
		`{"type":"cost-state","uuid":"c1","sessionId":"` + sid + `","totalCostUSD":1.5,"totalLinesAdded":3,"totalLinesRemoved":0}` + "\n" +
		`{"type":"cost-state","uuid":"c2","sessionId":"` + sid + `","totalCostUSD":0.25,"totalLinesAdded":1,"totalLinesRemoved":0}` + "\n"
	writeSession(t, root, sid, body)
	if _, err := Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	var cost float64
	if err := db.QueryRow(`select total_cost_usd from sessions where id = ?`, sid).Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if cost != 1.5 {
		t.Fatalf("total_cost_usd=%v、累計の最大 1.5 であるべき", cost)
	}
}

// iterations は整数ではなくオブジェクトの配列だった。本数だけ持つ。
// キーが無い行を 0 と混同しない。
func TestIterationsStoresArrayLength(t *testing.T) {
	if n, ok := iterationCount(nil); ok || n != 0 {
		t.Fatalf("キー無しは (0,false) であるべき: (%d,%v)", n, ok)
	}
	if n, ok := iterationCount([]byte(`null`)); ok || n != 0 {
		t.Fatalf("null は (0,false) であるべき: (%d,%v)", n, ok)
	}
	if n, ok := iterationCount([]byte(`[]`)); !ok || n != 0 {
		t.Fatalf("空配列は (0,true) であるべき: (%d,%v)", n, ok)
	}
	if n, ok := iterationCount([]byte(`[{"type":"message"},{"type":"message"}]`)); !ok || n != 2 {
		t.Fatalf("2要素は (2,true) であるべき: (%d,%v)", n, ok)
	}
}

// day はUTCではなくローカル日付。JSTだと 15:00Z 以降は翌日になる。
// UTCで切ると「今日どれだけ使ったか」が毎日9時間ぶんずれる。
func TestLocalDayFollowsHostTimezone(t *testing.T) {
	jst := time.FixedZone("JST", 9*3600)
	orig := time.Local
	time.Local = jst
	defer func() { time.Local = orig }()

	if got := localDay("2026-09-01T15:30:00.000Z"); got != "2026-09-02" {
		t.Fatalf("15:30Z は JST では翌日。got %s", got)
	}
	if got := localDay("2026-09-01T14:30:00.000Z"); got != "2026-09-01" {
		t.Fatalf("14:30Z は JST では同日。got %s", got)
	}
	// 壊れていても落とさない
	if got := localDay("2026-09-01"); got != "2026-09-01" {
		t.Fatalf("解釈できない値は頭10文字。got %s", got)
	}
}

// backfill は messages だけから usage を作る。ディスクを読み直さない。
// 取り込み経路で作った結果と一致し、何度回しても変わらない。
func TestBackfillMatchesIngestAndIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	const sid = "eeeeeeee-2222-4333-8444-555555555555"
	body := convoLines(sid, 0, 1) +
		assistantLine(sid, "a1", "msg_1", "claude-opus-5", 1, 8, 5000) +
		assistantLine(sid, "a2", "msg_1", "claude-opus-5", 1, 261, 5000) +
		assistantLine(sid, "a3", "msg_2", "claude-sonnet-5", 2, 40, 7000) +
		assistantLine(sid, "a4", "msg_s", "<synthetic>", 3, 0, 0) +
		`{"type":"cost-state","uuid":"c1","sessionId":"` + sid + `","totalCostUSD":2.75}` + "\n"
	writeSession(t, root, sid, body)
	if _, err := Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	want := oneInt(t, db, `select sum(output_tokens) from usage`)
	if want != 301 {
		t.Fatalf("取り込み時点で %d、261+40=301 であるべき", want)
	}

	// 元ファイルを消しても結果は変わらない（これが backfill の存在理由）
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		rows, sess, err := BackfillUsage(db)
		if err != nil {
			t.Fatal(err)
		}
		if sess != 1 {
			t.Fatalf("%d 回目: total_cost_usd を入れたセッションが %d 件", i+1, sess)
		}
		if rows != 3 {
			t.Fatalf("%d 回目: 走査した usage 行が %d、<synthetic> を除く3行であるべき", i+1, rows)
		}
		if got := oneInt(t, db, `select sum(output_tokens) from usage`); got != want {
			t.Fatalf("%d 回目: sum(output_tokens)=%d、%d であるべき", i+1, got, want)
		}
		if n := count(t, db, `select count(*) from usage`); n != 2 {
			t.Fatalf("%d 回目: usage %d 行、2行であるべき", i+1, n)
		}
	}
}
