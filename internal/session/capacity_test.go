package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Phase 3 の outer gate「上限まで大きくする」。
//
// **`claude` のプロセス数を数えても意味がない。** セッションはテスト・LSP・
// `ssh` の孫を産む。cgroup で包んであるので、孫まで含めて数えられる。
//
// 本物を呼ぶので既定では走らない:
//
//	CAMP_E2E_CLAUDE=1 go test ./internal/session/ -run Capacity -v -timeout 20m
func TestCapacityWithRealClaude(t *testing.T) {
	if os.Getenv("CAMP_E2E_CLAUDE") == "" {
		t.Skip("CAMP_E2E_CLAUDE=1 のときだけ走らせる（本物を呼ぶ）")
	}
	bin := os.Getenv("CAMP_CLAUDE_BIN")
	if bin == "" {
		bin = os.Getenv("HOME") + "/.local/bin/claude"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("claude が無い: %v", err)
	}
	n := 4 // 4コアに合わせる
	db := newDB(t)
	s := New(db)
	s.SetMaxConcurrent(n)

	sock := filepath.Join(t.TempDir(), "a.sock")
	c, err := s.Listen(sock, "", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	go c.Serve()
	defer c.Close()

	a := NewAgent(sock, bin)
	a.Scope = true // **孫まで包む**
	a.LogDir = t.TempDir()
	if err := a.Dial("capacity"); err != nil {
		t.Fatal(err)
	}
	go a.Run()
	defer a.conn.Close()
	waitFor(t, 5*time.Second, func() bool { return s.AgentConnected() })

	ids := make([]string, n)
	for i := range ids {
		r, err := s.Start("capacity", allowHere(t, db))
		if err != nil {
			t.Fatalf("%d 本目が起こせない: %v", i, err)
		}
		ids[i] = r.ID
	}
	defer func() {
		for _, id := range ids {
			s.Stop(id, StopTerminate)
		}
	}()
	for _, id := range ids {
		id := id
		waitFor(t, 60*time.Second, func() bool { return state(t, db, id) == StateIdle })
	}

	// 何でも許可する係。承認は1ターンに複数来る。
	stopApprover := make(chan struct{})
	defer close(stopApprover)
	go func() {
		for {
			select {
			case <-stopApprover:
				return
			default:
			}
			for _, id := range ids {
				for _, req := range s.Pending(id) {
					s.Approve(id, req, "allow", "")
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	// 孫を産む仕事を同時に投げる。
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if err := s.Input(id, "Use Bash to run: bash -c 'for i in 1 2 3; do (sleep 3 &) ; done; sleep 8; echo done'. Then say finished."); err != nil {
				t.Errorf("入力できない: %v", err)
			}
		}(id)
	}
	wg.Wait()

	// 走っている間に、孫まで数える。
	var peakProcs, peakMem int
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		total, mem := 0, 0
		for _, id := range ids {
			r, _ := get(db, id)
			if r.Scope == "" {
				continue
			}
			p, m := cgroupStats(r.Scope)
			total += p
			mem += m
		}
		if total > peakProcs {
			peakProcs = total
		}
		if mem > peakMem {
			peakMem = mem
		}
		done := true
		for _, id := range ids {
			if state(t, db, id) != StateIdle {
				done = false
			}
		}
		if done && total > 0 {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	t.Logf("%d 本同時: プロセスのピーク %d（**孫まで数えた**）/ メモリのピーク %s / 1本あたり %s",
		n, peakProcs, human(peakMem), human(peakMem/max1(n)))
	if peakProcs <= n {
		t.Errorf("孫を数えられていない（%d プロセス。%d 本より多いはず）", peakProcs, n)
	}
	for _, id := range ids {
		r, _ := get(db, id)
		if r.State == StateExited {
			t.Errorf("途中で落ちた: %s（%s）", firstN(id, 8), r.ExitReason)
		}
	}
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// cgroupStats は scope の中のプロセス数と memory.current。
func cgroupStats(scope string) (procs, mem int) {
	base := fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/app.slice/%s",
		os.Getuid(), os.Getuid(), scope)
	if b, err := os.ReadFile(filepath.Join(base, "cgroup.procs")); err == nil {
		procs = len(strings.Fields(string(b)))
	}
	if b, err := os.ReadFile(filepath.Join(base, "memory.current")); err == nil {
		fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &mem)
	}
	return procs, mem
}

func human(n int) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%dMB", n/(1<<20))
	}
	return fmt.Sprintf("%dB", n)
}
