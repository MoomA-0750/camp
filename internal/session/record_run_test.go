package session

import (
	"sync"
	"testing"
	"time"
)

// 定期の読み（M47。本人の決定 2026-09-12: 押したときだけでなく定期にも読む）。
//
// **番が来た行だけを読みに行く。** 台帳に行が無い・止めてある・まだ間隔が来ていない
// 組み合わせは呼ばない。

type recCalls struct {
	mu   sync.Mutex
	seen []string
}

func (c *recCalls) add(host, agent string) {
	c.mu.Lock()
	c.seen = append(c.seen, host+"/"+agent)
	c.mu.Unlock()
}

func (c *recCalls) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen...)
}

func TestOnlyDueRecordRootsAreRead(t *testing.T) {
	db := newDB(t)
	s := New(db)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }

	var calls recCalls
	s.SetReadRecords(calls.add)

	// 読む（rp/claude）、止めてある（rp/codex）、行が無い（far/claude）。
	if err := SetRecordRoot(db, "rp", "claude", true); err != nil {
		t.Fatal(err)
	}
	if err := SetRecordRoot(db, "rp", "codex", false); err != nil {
		t.Fatal(err)
	}

	s.readDue(time.Hour)
	if got := calls.list(); len(got) != 1 || got[0] != "rp/claude" {
		t.Fatalf("読みに行った先が %v（rp/claude だけのはず。止めた行と、行の無い先は読まない）", got)
	}

	// 読めたことにすると、間隔が来るまで番が来ない。
	if err := MarkRecordOK(db, "rp", "claude", now); err != nil {
		t.Fatal(err)
	}
	calls = recCalls{}
	s.SetReadRecords(calls.add)
	s.Now = func() time.Time { return now.Add(30 * time.Minute) }
	s.readDue(time.Hour)
	if got := calls.list(); len(got) != 0 {
		t.Fatalf("読んだ直後なのに、また見に行った: %v", got)
	}
	s.Now = func() time.Time { return now.Add(61 * time.Minute) }
	s.readDue(time.Hour)
	if got := calls.list(); len(got) != 1 {
		t.Fatalf("間隔が過ぎても見に行かない: %v", got)
	}
}

// **同じ周が重ならない。** 携帯の回線では1周に何分もかかりうる。
func TestRecordSweepsDoNotOverlap(t *testing.T) {
	db := newDB(t)
	s := New(db)
	s.Now = func() time.Time { return time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC) }
	if err := SetRecordRoot(db, "rp", "claude", true); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var n int
	var mu sync.Mutex
	s.SetReadRecords(func(host, agent string) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if first {
			close(started)
			<-release // 1周目を止めたまま、2周目を走らせてみる
		}
	})

	go s.readDue(time.Hour)
	<-started
	s.readDue(time.Hour) // 走っている最中の2周目。**何もしないで戻る。**
	mu.Lock()
	got := n
	mu.Unlock()
	if got != 1 {
		t.Fatalf("前の周が終わっていないのに %d 回目が走った", got)
	}
	close(release)
}

// **0 なら定期をやめる**（本人の決定: 間隔は設定で変えられ、やめられる）。
func TestRecordsAreNotReadPeriodicallyWhenTurnedOff(t *testing.T) {
	db := newDB(t)
	s := New(db)
	if err := SetRecordRoot(db, "rp", "claude", true); err != nil {
		t.Fatal(err)
	}
	var calls recCalls
	s.SetReadRecords(calls.add)

	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() { s.RunRecords(done, 0); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("0 を渡したのに定期が回り続けている")
	}
	close(done)
	if got := calls.list(); len(got) != 0 {
		t.Fatalf("定期をやめたのに読みに行った: %v", got)
	}
}
