package session

import (
	"testing"
	"time"
)

// 向こうのホストの記録を読んでよいかの台帳（M47。移行 0028）。
//
// **行が無ければ読まない。** 起こしてよい接続先でも、記録を読むかは別に選ぶ
// （本人の決定 2026-09-12）。既定で読みに行かないことを、ここで縛る。
func TestRecordsAreNotReadWithoutALedgerRow(t *testing.T) {
	db := newDB(t)

	on, err := recordRootEnabled(db, "rp", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if on {
		t.Fatal("台帳に行が無いのに読みに行こうとしている")
	}

	if err := SetRecordRoot(db, "rp", "claude", true); err != nil {
		t.Fatal(err)
	}
	if on, err = recordRootEnabled(db, "rp", "claude"); err != nil || !on {
		t.Fatalf("許したのに読まない（on=%v err=%v）", on, err)
	}

	// **別のエージェントは巻き込まない。** 行はホストとエージェントごと。
	if on, err = recordRootEnabled(db, "rp", "codex"); err != nil || on {
		t.Fatalf("claude を許しただけで codex まで読もうとしている（on=%v err=%v）", on, err)
	}

	// 止めるのは軽くてよい（安全側）。
	if err := SetRecordRoot(db, "rp", "claude", false); err != nil {
		t.Fatal(err)
	}
	if on, err = recordRootEnabled(db, "rp", "claude"); err != nil || on {
		t.Fatalf("止めたのに読もうとしている（on=%v err=%v）", on, err)
	}
}

// 続けて失敗した接続先は、見に行く間隔を伸ばす。**寝ている携帯を叩き続けない。**
// 成功したら数が 0 に戻るので、間隔も戻る。
func TestAFailingHostIsVisitedLessOften(t *testing.T) {
	db := newDB(t)
	if err := SetRecordRoot(db, "rp", "claude", true); err != nil {
		t.Fatal(err)
	}
	const every, capEvery = time.Hour, 4 * time.Hour
	t0 := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)

	// 一度も読めていないうちは、いつでも番が来る。
	due, err := DueRecordRoots(db, t0, every, capEvery)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("一度も読めていないのに番が来ない（%d 件）", len(due))
	}

	if err := MarkRecordOK(db, "rp", "claude", t0); err != nil {
		t.Fatal(err)
	}
	if due, err = DueRecordRoots(db, t0.Add(30*time.Minute), every, capEvery); err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatal("読んだ直後なのに、また見に行こうとしている")
	}
	if due, err = DueRecordRoots(db, t0.Add(61*time.Minute), every, capEvery); err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatal("間隔が過ぎても番が来ない")
	}

	// 2回続けて失敗 → 間隔は 4 倍。
	for i := 0; i < 2; i++ {
		if err := MarkRecordFail(db, "rp", "claude", "繋がらない"); err != nil {
			t.Fatal(err)
		}
	}
	if due, err = DueRecordRoots(db, t0.Add(3*time.Hour), every, capEvery); err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatal("続けて失敗しているのに、間隔が伸びていない")
	}
	if due, err = DueRecordRoots(db, t0.Add(5*time.Hour), every, capEvery); err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatal("上限（4時間）を超えても番が来ない")
	}

	// 読めたら戻る。**失敗の跡も消える。**
	if err := MarkRecordOK(db, "rp", "claude", t0.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, err := ListRecordRoots(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Fails != 0 || rows[0].LastError != "" {
		t.Fatalf("読めたのに失敗の跡が残っている: %+v", rows)
	}
	if due, err = DueRecordRoots(db, t0.Add(5*time.Hour+30*time.Minute), every, capEvery); err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatal("読めたのに間隔が戻っていない")
	}
}
