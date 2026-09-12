package limits

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// statusLine は実物と同じ形の入力を組む。resets_at は epoch 秒。
func statusLine(fiveHour float64, fiveEnds int64, sevenDay float64, sevenEnds int64) string {
	return fmt.Sprintf(`{
	  "model": {"display_name": "Opus 5"},
	  "context_window": {"used_percentage": 40.0},
	  "rate_limits": {
	    "five_hour": {"used_percentage": %f, "resets_at": %d},
	    "seven_day": {"used_percentage": %f, "resets_at": %d}
	  }
	}`, fiveHour, fiveEnds, sevenDay, sevenEnds)
}

func record(t *testing.T, db *store.DB, body string) []Reading {
	t.Helper()
	got, err := Record(db, strings.NewReader(body), AgentClaude, SourceStatusLine)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// 同じ窓を何度観測しても行は増えない。statusLine は毎描画走るので、
// ここが崩れると1日で数千行に膨れる。
func TestSameWindowStaysOneRow(t *testing.T) {
	db := newDB(t)
	ends := time.Now().Add(2 * time.Hour).Unix()
	week := time.Now().Add(72 * time.Hour).Unix()

	record(t, db, statusLine(10, ends, 30, week))
	record(t, db, statusLine(20, ends, 31, week))
	record(t, db, statusLine(35, ends, 32, week))

	rows, err := Windows(db, Opts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("窓は2つのはずが %d 行: %+v", len(rows), rows)
	}
	for _, w := range rows {
		if w.Samples != 3 {
			t.Errorf("%s: samples=%d, 3回観測したはず", w.Kind, w.Samples)
		}
	}
}

// 窓が変わったら（リセットされたら）別の行になる。過去の窓は残る。
func TestNewWindowMakesNewRow(t *testing.T) {
	db := newDB(t)
	old := time.Now().Add(-1 * time.Hour).Unix()
	next := time.Now().Add(4 * time.Hour).Unix()
	week := time.Now().Add(72 * time.Hour).Unix()

	record(t, db, statusLine(95, old, 40, week))
	record(t, db, statusLine(3, next, 41, week))

	rows, err := Windows(db, Opts{Kind: "five_hour"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("5時間枠は2窓ぶん残るはずが %d 行", len(rows))
	}
	// 新しい窓が先頭。古い窓の 95% は消えない。
	if rows[0].UsedPct != 3 || rows[1].UsedPct != 95 {
		t.Fatalf("並びか値がおかしい: %+v", rows)
	}
	if rows[0].Current == false {
		t.Error("先頭は進行中の窓のはず")
	}
	if rows[1].Current {
		t.Error("期限切れの窓を進行中と言っている")
	}
}

// 観測が飛んで値が下がって見えても、その窓のピークは失われない。
// 「あの窓で壁に当たったか」を後から言えることがこの列の存在理由。
func TestPeakSurvivesADip(t *testing.T) {
	db := newDB(t)
	ends := time.Now().Add(2 * time.Hour).Unix()
	week := time.Now().Add(72 * time.Hour).Unix()

	record(t, db, statusLine(10, ends, 99.5, week))
	record(t, db, statusLine(20, ends, 12.0, week)) // 7日枠が落ちた

	rows, err := Windows(db, Opts{Kind: "seven_day"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("1行のはずが %d", len(rows))
	}
	if rows[0].UsedPct != 12.0 {
		t.Errorf("最新は12.0のはず: %v", rows[0].UsedPct)
	}
	if rows[0].PeakPct != 99.5 {
		t.Errorf("ピークは99.5のはず: %v", rows[0].PeakPct)
	}
}

// rate_limits がまだ無い（起動直後など）入力で落ちない。
// フックから毎描画呼ばれるので、ここでエラーを返すとプロンプトが荒れる。
func TestMissingRateLimitsIsNotAFailure(t *testing.T) {
	db := newDB(t)
	_, err := Record(db, strings.NewReader(`{"model":{"display_name":"Opus 5"}}`),
		AgentClaude, SourceStatusLine)
	if !errors.Is(err, ErrNoWindows) {
		t.Fatalf("ErrNoWindows のはず: %v", err)
	}
}

// 値の片方だけ欠けている窓（spend_limit など）は黙って飛ばす。
func TestPartialWindowIsSkipped(t *testing.T) {
	db := newDB(t)
	ends := time.Now().Add(2 * time.Hour).Unix()
	body := fmt.Sprintf(`{"rate_limits":{
	  "five_hour":   {"used_percentage": 10, "resets_at": %d},
	  "spend_limit": {"used_percentage": null, "resets_at": null}}}`, ends)
	got := record(t, db, body)
	if len(got) != 1 || got[0].Kind != "five_hour" {
		t.Fatalf("five_hour だけ入るはず: %+v", got)
	}
}

// started_at は ends_at から窓の長さぶん引いた値。
func TestStartedAtIsDerived(t *testing.T) {
	db := newDB(t)
	ends := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	record(t, db, statusLine(10, ends.Unix(), 30, ends.Add(48*time.Hour).Unix()))

	rows, _ := Windows(db, Opts{Kind: "five_hour"})
	want := ends.UTC().Add(-5 * time.Hour).Format(time.RFC3339)
	if rows[0].StartedAt != want {
		t.Fatalf("started_at=%q, want %q", rows[0].StartedAt, want)
	}
}

// Current は種類ごとに1つ、進行中のものだけ返す。
func TestCurrentPicksLiveWindowPerKind(t *testing.T) {
	db := newDB(t)
	past := time.Now().Add(-2 * time.Hour).Unix()
	live := time.Now().Add(3 * time.Hour).Unix()
	week := time.Now().Add(72 * time.Hour).Unix()

	record(t, db, statusLine(99, past, 50, week))
	record(t, db, statusLine(7, live, 51, week))
	// 期限切れの窓しか無い種類。残量として出してはいけない。
	if _, err := Record(db, strings.NewReader(fmt.Sprintf(
		`{"rate_limits":{"spend_limit":{"used_percentage":88,"resets_at":%d}}}`, past)),
		AgentClaude, SourceStatusLine); err != nil {
		t.Fatal(err)
	}

	cur, err := Current(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(cur) != 2 {
		t.Fatalf("進行中の窓を持つ種類は2つのはずが %d: %+v", len(cur), cur)
	}
	for _, w := range cur {
		if w.Kind == "spend_limit" {
			t.Error("期限切れの窓しか無い種類を残量として返している")
		}
	}
	for _, w := range cur {
		if !w.Current {
			t.Errorf("%s: 期限切れの窓を返している", w.Kind)
		}
		if w.Kind == "five_hour" && w.UsedPct != 7 {
			t.Errorf("進行中の窓を取れていない: %v", w.UsedPct)
		}
	}
}

// 別の観測元は同じ窓でも別行になる。のちに制御プロトコル（D-007）由来の
// 行が並んでも、statusLine 由来の記録を上書きしない。
func TestSourcesDoNotCollide(t *testing.T) {
	db := newDB(t)
	ends := time.Now().Add(2 * time.Hour).Unix()
	week := time.Now().Add(72 * time.Hour).Unix()

	record(t, db, statusLine(10, ends, 30, week))
	if _, err := Record(db, strings.NewReader(statusLine(11, ends, 31, week)),
		AgentClaude, "control"); err != nil {
		t.Fatal(err)
	}

	rows, _ := Windows(db, Opts{Kind: "five_hour"})
	if len(rows) != 2 {
		t.Fatalf("観測元ごとに1行で2行のはずが %d", len(rows))
	}
}

// 期限だけ先にある古い行に引っぱられず、最後に観測できた窓を現在とみなす。
func TestCurrentPrefersTheLatestObservation(t *testing.T) {
	db := newDB(t)
	week := time.Now().Add(72 * time.Hour).Unix()
	far := time.Now().Add(9 * time.Hour).Unix()  // 期限はいちばん先
	near := time.Now().Add(1 * time.Hour).Unix() // でも観測はこちらが新しい

	record(t, db, statusLine(90, far, 50, week))
	time.Sleep(1100 * time.Millisecond) // fetched_at は秒精度
	record(t, db, statusLine(4, near, 51, week))

	cur, err := Current(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range cur {
		if w.Kind == "five_hour" && w.UsedPct != 4 {
			t.Fatalf("最後に観測した窓を現在にすべき: used=%v", w.UsedPct)
		}
	}
}
