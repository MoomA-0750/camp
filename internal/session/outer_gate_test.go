package session

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoomA-0750/camp/internal/store"
)

// Phase 3 の outer gate。**「動くこと」だけを見ない。**
//
// Phase 2 の反省が計画に書いてある——受け入れ条件が「動くこと」に寄ると、
// 同時実行と壊れた入力で出る欠陥が丸ごと抜ける（実際に抜けた）。
// ここは壊しに行く側。

// ---------------------------------------------------------------- 同時に叩く

func TestManySessionsAtOnce(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	s.SetMaxConcurrent(8)

	const n = 8
	dirs := make([]string, n)
	for i := range dirs {
		dirs[i] = allowHere(t, db)
	}

	var wg sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := s.Start("test", dirs[i])
			ids[i], errs[i] = r.ID, err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("%d 本目が起こせない: %v", i, err)
		}
	}
	for _, id := range ids {
		id := id
		waitFor(t, 15*time.Second, func() bool { return state(t, db, id) == StateIdle })
	}
	// 全部に同時に話しかける。
	wg = sync.WaitGroup{}
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if err := s.Input(id, "go"); err != nil {
				t.Errorf("%s に入力できない: %v", firstN(id, 8), err)
			}
		}(id)
	}
	wg.Wait()
	for _, id := range ids {
		id := id
		waitFor(t, 20*time.Second, func() bool { return state(t, db, id) == StateIdle })
	}
	// 全部同時に止める。
	wg = sync.WaitGroup{}
	for _, id := range ids {
		wg.Add(1)
		go func(id string) { defer wg.Done(); s.Stop(id, StopTerminate) }(id)
	}
	wg.Wait()
	for _, id := range ids {
		id := id
		waitFor(t, 20*time.Second, func() bool { return state(t, db, id) == StateExited })
	}
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// **起動中に投げた停止が、捨てられない。**
//
// 2026-09-04 の outer gate で見つけた欠陥。起動は非同期なので、実行面が子を
// 登録する前に「止めろ」が着くことがある。そのとき黙って捨てていた——
// campd は stopping のまま、子は走り続け、**止めたつもりで止まっていない**。
// Stop はエラーも返さないので、画面には成功に見えていた。
func TestStopWhileStillStartingIsNotSwallowed(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	for i := 0; i < 10; i++ {
		rec, err := s.Start("test", allowHere(t, db))
		if err != nil {
			t.Fatal(err)
		}
		// **間を置かずに投げる。** 子はまだ生まれていない。
		if err := s.Stop(rec.ID, StopTerminate); err != nil {
			t.Fatalf("#%d 止められない: %v", i, err)
		}
		waitFor(t, 20*time.Second, func() bool { return state(t, db, rec.ID) == StateExited })
		r, _ := get(db, rec.ID)
		if alive, known := r.Owner().Alive(); alive && known {
			t.Fatalf("#%d exited と書いたのに pid %d が生きている", i, r.PID)
		}
	}
}

// 止まらないまま放置しない。**stopping で永久に残らない。**
func TestASessionThatWillNotStopIsGivenUpOn(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	rec, err := s.Start("test", allowHere(t, db))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })

	// 実行面が受け取り損ねた体にする（stopping に落として放置）。
	s.mu.Lock()
	s.live[rec.ID].rec.State = StateStopping
	s.mu.Unlock()
	_ = setState(s.db, rec.ID, StateStopping)

	s.StopAfter = 0
	s.Tick()
	if got := state(t, db, rec.ID); got != StateExited {
		t.Fatalf("stopping のまま残っている: %s", got)
	}
	r, _ := get(db, rec.ID)
	if !strings.Contains(r.ExitReason, "止まらない") {
		t.Fatalf("何があったか分からない終わり方: %q", r.ExitReason)
	}
}

// 同じ承認に、同時に何本も答える。**通るのは1本だけ。**
func TestAnswerTheSameApprovalConcurrently(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	rec, _ := s.Start("test", allowHere(t, db))
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	if err := ask(db, rec.ID, "r", "Write", "{}", time.Now()); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var ok int64
	var mu sync.Mutex
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b := "allow"
			if i%2 == 0 {
				b = "deny"
			}
			if err := s.Approve(rec.ID, "r", b, ""); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d 本が通った。**同じ承認に答えられるのは1回だけ**", ok)
	}
}

// 同じセッションを同時に何度も tail する。
func TestManyReadersOnOneSession(t *testing.T) {
	db := newDB(t)
	s := New(db)
	a := attach(t, s, noisyClaude(t, 500))
	a.LogDir = t.TempDir()
	rec, _ := s.Start("test", allowHere(t, db))
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	if err := s.Input(rec.ID, "go"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var since int64
			for {
				select {
				case <-stop:
					return
				default:
				}
				r, err := s.Tail(rec.ID, since, 50)
				if err != nil {
					continue
				}
				for _, ln := range r.Lines {
					if ln.Seq <= since {
						t.Errorf("カーソルより前が返った: %d <= %d", ln.Seq, since)
						return
					}
					since = ln.Seq
				}
			}
		}()
	}
	waitFor(t, 60*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	close(stop)
	wg.Wait()
}

// ---------------------------------------------------------------- 壊れた入力

// 制御口にでたらめを流し込む。**campd が落ちない。**
func TestGarbageOnTheControlSocket(t *testing.T) {
	db := newDB(t)
	s := New(db)
	sock := filepath.Join(t.TempDir(), "a.sock")
	c, err := s.Listen(sock, "", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	go c.Serve()
	t.Cleanup(func() { c.Close() })

	junk := []string{
		"", "{", "null", "[]", `{"t":`, `{"t":"知らない"}`,
		`{"t":"started"}`, `{"t":"frame","session":"x","token":"y"}`,
		`{"t":"exited","session":"","token":""}`,
		`{"t":"hello"}`, `{"t":"tail_result"}`,
		`{"t":"reaped","session":"居ない"}`,
		`{"t":"dropped","session":"x","token":"y","dropped":-1}`,
		strings.Repeat("x", 4096),
	}
	// 1本目は hello を送って実行面になる。
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte(`{"t":"hello"}` + "\n"))
	waitFor(t, 3*time.Second, func() bool { return s.AgentConnected() })
	for _, j := range junk {
		if _, err := conn.Write([]byte(j + "\n")); err != nil {
			t.Fatalf("書けなくなった（campd が落ちた？）: %v", err)
		}
	}
	// まだ生きているか。
	if _, err := conn.Write([]byte(`{"t":"ping"}` + "\n")); err != nil {
		t.Fatalf("でたらめのあとで死んでいる: %v", err)
	}
	if !s.AgentConnected() {
		t.Fatal("でたらめで実行面の登録が外れた")
	}
}

// hello を送る前に指示を出す。**名乗る前に受け付けない。**
func TestNoInstructionsBeforeHello(t *testing.T) {
	db := newDB(t)
	s := New(db)
	sock := filepath.Join(t.TempDir(), "a.sock")
	c, err := s.Listen(sock, "", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	go c.Serve()
	t.Cleanup(func() { c.Close() })

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte(`{"t":"started","session":"x","pid":1}` + "\n"))
	buf := make([]byte, 512)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("何も返らない: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "hello") {
		t.Fatalf("hello を求めていない: %s", buf[:n])
	}
	if s.AgentConnected() {
		t.Fatal("名乗らずに実行面になれた")
	}
}

// 子がターンの途中で死ぬ。
func TestTheChildDyingMidTurnIsRecorded(t *testing.T) {
	db := newDB(t)
	s := New(db)
	p := filepath.Join(t.TempDir(), "dying-claude")
	body := `#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"d-1"}'
while IFS= read -r line; do
  case "$line" in
    *'"type":"user"'*) echo '{"type":"assistant"}'; exit 3 ;;
  esac
done
`
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	attach(t, s, p)
	rec, _ := s.Start("test", allowHere(t, db))
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	if err := ask(db, rec.ID, "pending", "Write", "{}", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Input(rec.ID, "go"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, func() bool { return state(t, db, rec.ID) == StateExited })
	r, _ := get(db, rec.ID)
	if r.ExitCode == nil || *r.ExitCode != 3 {
		t.Fatalf("終了コードが残っていない: %+v", r.ExitCode)
	}
	// **待っていた承認が宙に浮かない。**
	if got := pending(t, s, rec.ID); len(got) != 0 {
		t.Fatalf("子が死んだのに承認が待ったまま: %v", got)
	}
}

// 存在しない cwd・ファイル・空文字。
func TestBadCwdIsRefusedWithoutCrashing(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	allowHere(t, db)
	for _, bad := range []string{"", "relative", "/no/such/place/at/all", "/etc/hostname", "/"} {
		if _, err := s.Start("test", bad); err == nil {
			t.Errorf("%q で起こせてしまった", bad)
		}
	}
}

// でたらめなセッションIDへの操作。
func TestOperationsOnUnknownSessionsAreRefused(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	id := "居ないセッション"
	if err := s.Input(id, "x"); err == nil {
		t.Error("知らないセッションに入力できた")
	}
	if err := s.Stop(id, StopTerminate); err == nil {
		t.Error("知らないセッションを止められた")
	}
	if err := s.Approve(id, "r", "allow", ""); err == nil {
		t.Error("知らないセッションの承認に答えられた")
	}
	if _, err := s.Control(id, "get_usage"); err == nil {
		t.Error("知らないセッションへ制御を出せた")
	}
	// **「まだ何も流れていない」と「そんなセッションは無い」を混ぜない。**
	if _, err := s.Tail(id, 0, 10); err == nil {
		t.Error("知らないセッションの tail が空で返った（無いことが伝わらない）")
	}
	if err := s.Stop("", "でたらめな止め方"); err == nil {
		t.Error("知らない止め方が通った")
	}
}

// 長すぎる入力。
func TestOversizedInputIsRefused(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	rec, _ := s.Start("test", allowHere(t, db))
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	if err := s.Input(rec.ID, strings.Repeat("あ", maxText)); err == nil {
		t.Fatal("上限を越えた入力が通った")
	}
}

// ---------------------------------------------------------------- 境界を攻める

// **他人のセッションの承認に答えられない。**
func TestOneSessionCannotAnswerAnothersApproval(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	a, _ := s.Start("test", allowHere(t, db))
	b, _ := s.Start("test", allowHere(t, db))
	waitFor(t, 5*time.Second, func() bool { return state(t, db, a.ID) == StateIdle })
	waitFor(t, 5*time.Second, func() bool { return state(t, db, b.ID) == StateIdle })
	if err := ask(db, a.ID, "r-a", "Write", "{}", time.Now()); err != nil {
		t.Fatal(err)
	}
	// b の側から a の承認に答えようとする。
	if err := s.Approve(b.ID, "r-a", "allow", ""); err == nil {
		t.Fatal("別のセッションの承認に答えられた")
	}
	if got := pending(t, s, a.ID); len(got) != 1 {
		t.Fatalf("a の待ちが壊れた: %v", got)
	}
}

// **起こしてもいないセッションのフレームを流し込めない。**
func TestFramesForASessionYouDoNotHoldAreDropped(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	rec, _ := s.Start("test", allowHere(t, db))
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })

	before := state(t, db, rec.ID)
	// 合鍵の無いフレームを直接流し込む。
	s.dispatchForTest(Msg{T: MsgFrame, Session: rec.ID, Token: "でたらめ", Kind: "result"})
	s.dispatchForTest(Msg{T: MsgExited, Session: rec.ID, Token: "でたらめ", Code: 0})
	time.Sleep(200 * time.Millisecond)
	if got := state(t, db, rec.ID); got != before {
		t.Fatalf("合鍵なしで状態を動かせた: %s → %s", before, got)
	}
}

// 2つ目の実行面は断られ、**1つ目のセッションは無事**。
func TestASecondExecutionSideCannotStealSessions(t *testing.T) {
	db := newDB(t)
	s, a := wire(t, db)
	rec, _ := s.Start("test", allowHere(t, db))
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })

	// なりすまし: 抱えていると名乗って割り込む。
	conn, err := net.Dial("unix", a.Sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r, _ := get(db, rec.ID)
	hello, _ := json.Marshal(Msg{T: MsgHello, Version: "泥棒", Held: []Held{{
		ID: rec.ID, Token: "盗んだ鍵", PID: r.PID, Started: r.Started, BootID: r.BootID,
	}}})
	conn.Write(append(hello, '\n'))
	time.Sleep(500 * time.Millisecond)

	// 元の実行面はそのまま。盗んだ鍵では触れない。
	if err := s.Input(rec.ID, "まだ話せる"); err != nil {
		t.Fatalf("正規の経路が壊れた: %v", err)
	}
	if _, ok := s.check(Msg{Session: rec.ID, Token: "盗んだ鍵"}); ok {
		t.Fatal("盗んだ鍵が通った")
	}
}

// 許した uid 以外は実行面になれない。
func TestAnotherUIDCannotBecomeTheExecutionSide(t *testing.T) {
	db := newDB(t)
	s := New(db)
	sock := filepath.Join(t.TempDir(), "a.sock")
	// **自分ではない uid だけを許す。**
	c, err := s.Listen(sock, "", os.Getuid()+1)
	if err != nil {
		t.Fatal(err)
	}
	go c.Serve()
	t.Cleanup(func() { c.Close() })

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte(`{"t":"hello"}` + "\n"))
	buf := make([]byte, 512)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _ := conn.Read(buf)
	if !strings.Contains(string(buf[:n]), "uid") {
		t.Fatalf("uid で断っていない: %s", buf[:n])
	}
	if s.AgentConnected() {
		t.Fatal("許していない uid が実行面になれた")
	}
}

// ---------------------------------------------------------------- 落として起こす

// campd を何度も入れ替える。**そのたびに子が飛ばない。**
func TestRestartingCampdRepeatedlyKeepsTheChild(t *testing.T) {
	db := newDB(t)
	s, a := wire(t, db)
	rec, err := s.Start("test", allowHere(t, db))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	r0, _ := get(db, rec.ID)

	cur := s
	for i := 0; i < 3; i++ {
		a.conn.Close()
		waitFor(t, 5*time.Second, func() bool { return !cur.AgentConnected() })

		next := New(db)
		if _, _, _, err := next.Reconcile(); err != nil {
			t.Fatal(err)
		}
		sock := filepath.Join(t.TempDir(), fmt.Sprintf("s%d.sock", i))
		c, err := next.Listen(sock, "", os.Getuid())
		if err != nil {
			t.Fatal(err)
		}
		go c.Serve()
		t.Cleanup(func() { c.Close() })
		a.Sock = sock
		if err := a.Dial("again"); err != nil {
			t.Fatal(err)
		}
		go a.Run()
		waitFor(t, 5*time.Second, func() bool { return next.AgentConnected() })
		waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
		cur = next
	}

	r1, _ := get(db, rec.ID)
	if r1.PID != r0.PID || r1.Started != r0.Started {
		t.Fatalf("3回入れ替えたら別のプロセスになった: %d/%d → %d/%d",
			r0.PID, r0.Started, r1.PID, r1.Started)
	}
	if err := cur.Input(rec.ID, "まだ話せる"); err != nil {
		t.Fatalf("3回入れ替えたら話せなくなった: %v", err)
	}
	cur.Stop(rec.ID, StopTerminate)
}

// **孤児を、孤児のまま置き去りにしない。**
//
// 2026-09-04 の outer gate で見つけた欠陥。実行面を kill -9 すると、その子は
// stdin が閉じて自分で終わる（CLI の仕様。実測）。ところが campd は落ちた
// 瞬間に orphaned と書いたきり誰も見に行かず、**台帳は永久に「孤児」のまま**
// 残っていた。プロセスはもう居ないのに。
func TestOrphansAreNotLeftLyingAround(t *testing.T) {
	db := newDB(t)
	s, a := wire(t, db)
	rec, err := s.Start("test", allowHere(t, db))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	if err := ask(db, rec.ID, "r", "Write", "{}", time.Now()); err != nil {
		t.Fatal(err)
	}

	// 実行面が落ちて、子も（stdin が閉じて）終わる。
	a.stopAll("テスト: 実行面が落ちた体")
	a.conn.Close()
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateOrphaned })
	waitFor(t, 10*time.Second, func() bool {
		r, _ := get(db, rec.ID)
		alive, known := r.Owner().Alive()
		return known && !alive
	})

	// **ここで誰も見に行かないと、孤児のまま残る。**
	s.Tick()
	if got := state(t, db, rec.ID); got != StateExited {
		t.Fatalf("孤児のまま残っている: %s", got)
	}
	r, _ := get(db, rec.ID)
	if !strings.Contains(r.ExitReason, "見張る者が居ないうちに") {
		t.Fatalf("何があったか分からない終わり方: %q", r.ExitReason)
	}
	if got := pending(t, s, rec.ID); len(got) != 0 {
		t.Fatalf("承認が宙に浮いたまま: %v", got)
	}
}

// 生きている孤児は触らない。**動いているものを「終わった」と書かない。**
func TestALiveOrphanIsLeftAlone(t *testing.T) {
	db := newDB(t)
	s := New(db)
	self := os.Getpid()
	st, _ := Starttime(self)
	mustInsert(t, db, "alive-orphan", StateOrphaned, self, st, BootID())

	s.Tick()
	if got := state(t, db, "alive-orphan"); got != StateOrphaned {
		t.Fatalf("生きている孤児を %s にした", got)
	}
}

// ---------------------------------------------------------------- 「見ていないから0」

// **読めなかったことを「空だった」と答えない。**
//
// このプロジェクトが繰り返し踏んできた形。落とし先が読めないときに 0 件と
// 答えると、番号の付け直しが黙って起きて、画面は同じ seq の別のフレームを
// 受け取る（カーソルが嘘になる）。
func TestAnUnreadableLogIsAnErrorNotAnEmptyOne(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root では権限で弾けない")
	}
	old := maxLogBytes
	maxLogBytes = 512
	defer func() { maxLogBytes = old }()

	dir := t.TempDir()
	lg, err := OpenLog(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		lg.Append("assistant", []byte(`{"pad":"`+strings.Repeat("x", 40)+`"}`))
	}
	_, newest, _ := lg.Stats()
	lg.Close()

	// **1つ前の世代だけ読めなくする。** いま書くファイルは開けるので、
	// 「開けたから大丈夫」では通れない——数え直しが失敗したことに
	// 気づかなければ、そのまま続きを書いてしまう。
	prev := filepath.Join(dir, "s1.1.jsonl")
	if _, err := os.Stat(prev); err != nil {
		t.Skipf("まだ世代が回っていない: %v", err)
	}
	if err := os.Chmod(prev, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(prev, 0o600)

	again, err := OpenLog(dir, "s1")
	if err == nil {
		_, n, _ := again.Stats()
		again.Close()
		t.Fatalf("読めない世代を「空」として開いた（newest %d → %d）", newest, n)
	}
}

// tail も同じ。世代が読めないなら、そう言う。
func TestATailThatCannotReadSaysSo(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root では権限で弾けない")
	}
	old := maxLogBytes
	maxLogBytes = 512
	defer func() { maxLogBytes = old }()

	dir := t.TempDir()
	lg, err := OpenLog(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	for i := 0; i < 40; i++ {
		lg.Append("assistant", []byte(`{"pad":"`+strings.Repeat("x", 40)+`"}`))
	}
	prev := filepath.Join(dir, "s1.1.jsonl")
	if _, err := os.Stat(prev); err != nil {
		t.Skipf("まだ世代が回っていない: %v", err)
	}
	if err := os.Chmod(prev, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(prev, 0o600)

	if _, _, err := lg.Tail(0, 100); err == nil {
		t.Fatal("読めない世代を黙って飛ばした")
	}
}

// 承認の読み取りが失敗したら、そう言う。**「待っている承認は無い」にしない。**
func TestPendingApprovalsFailLoudly(t *testing.T) {
	db := newDB(t)
	s := New(db)
	if _, err := db.Exec(`drop table approvals`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pending("x"); err == nil {
		t.Fatal("読めないのに「待っている承認は無い」と答えた")
	}
}

// 目印まで飛ぶようにしたので、**飛びすぎて行を落としていないか**を全カーソルで見る。
//
// 速くするために足した仕組みが、静かに行を落とすのが一番悪い。
func TestSeekingToMarksNeverSkipsALine(t *testing.T) {
	old := maxLogBytes
	maxLogBytes = 8192 // 世代を何度も回す
	defer func() { maxLogBytes = old }()

	dir := t.TempDir()
	lg, err := OpenLog(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	const n = 600
	for i := 1; i <= n; i++ {
		if _, err := lg.Append("assistant", []byte(fmt.Sprintf(`{"i":%d}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	oldest, newest, _ := lg.Stats()
	if newest != n {
		t.Fatalf("newest=%d（%d のはず）", newest, n)
	}

	for since := int64(0); since <= newest; since++ {
		lines, gap, err := lg.Tail(since, maxTailLines)
		if err != nil {
			t.Fatalf("since=%d: %v", since, err)
		}
		if since+1 < oldest {
			if !gap {
				t.Fatalf("since=%d: 落ちた範囲なのに gap を言わない", since)
			}
			continue
		}
		want := newest - since
		if want > maxTailLines {
			want = maxTailLines
		}
		if int64(len(lines)) != want {
			t.Fatalf("since=%d: %d 行（%d 行のはず）", since, len(lines), want)
		}
		// **連番であること。** 飛ばしていたらここで落ちる。
		for i, ln := range lines {
			if ln.Seq != since+int64(i)+1 {
				t.Fatalf("since=%d: %d 番目が seq=%d（%d のはず）",
					since, i, ln.Seq, since+int64(i)+1)
			}
		}
	}
}

// 何も無いときはファイルを開かない（SSE は 300ms ごとに聞きに来る）。
func TestAnIdlePollDoesNotTouchTheFiles(t *testing.T) {
	dir := t.TempDir()
	lg, err := OpenLog(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	for i := 0; i < 10; i++ {
		lg.Append("assistant", []byte(`{}`))
	}
	_, newest, _ := lg.Stats()

	// 読めなくしても、末尾に居る読み手は困らない（開かないので）。
	p := filepath.Join(dir, "s1.jsonl")
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(p, 0o600)
	if os.Getuid() == 0 {
		t.Skip("root では権限で弾けない")
	}
	lines, gap, err := lg.Tail(newest, 100)
	if err != nil || gap || len(lines) != 0 {
		t.Fatalf("空振りの問い合わせでファイルを開いている: %d 行 gap=%v err=%v",
			len(lines), gap, err)
	}
}

// **同時に叩いても、監査ログの連鎖が壊れない。**
//
// 監査ログは1行前のハッシュを取り込む。書き手が同時に走ると、連鎖が
// 途切れたり枝分かれしたりしうる——そうなると「改竄に気づく」仕組みが
// そこで死ぬ。SetMaxOpenConns(1) に頼っているので、実際に確かめる。
func TestTheAuditChainSurvivesConcurrency(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	s.SetMaxConcurrent(6)

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec, err := s.Start("test", allowHere(t, db))
			if err != nil {
				return
			}
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				r, _ := get(db, rec.ID)
				if r.State == StateIdle {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			for j := 0; j < 5; j++ {
				s.Input(rec.ID, "go")
				ask(db, rec.ID, fmt.Sprintf("r%d", j), "Write", "{}", time.Now())
				s.Approve(rec.ID, fmt.Sprintf("r%d", j), "allow", "")
			}
			s.Stop(rec.ID, StopTerminate)
		}()
	}
	wg.Wait()
	time.Sleep(500 * time.Millisecond)

	if ok, n, err := auditChainOK(db); err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatalf("%d 行目で連鎖が切れている", n)
	} else {
		t.Logf("%d 行、連鎖は繋がっている", n)
	}
}

// auditChainOK は prev_hash が1行前の hash と繋がっているかを見る。
func auditChainOK(db *store.DB) (bool, int, error) {
	rows, err := db.Query(`select prev_hash, hash from audit order by id`)
	if err != nil {
		return false, 0, err
	}
	defer rows.Close()
	prev, n := "", 0
	for rows.Next() {
		var p, h string
		if err := rows.Scan(&p, &h); err != nil {
			return false, n, err
		}
		n++
		if n == 1 {
			prev = h
			continue
		}
		if p != prev {
			return false, n, nil
		}
		prev = h
	}
	return true, n, rows.Err()
}

// **実行面から DB を太らせられない。**
//
// 2026-09-04 の outer gate で見つけた欠陥。上限が無いと 5,756 フレーム/秒で
// 承認要求を流し込め、3.5秒で approvals 2万行・audit 2万行・DB 13MB になった。
// 監査ログは保持ポリシーの対象外なので、そのまま溜まり続ける。
// M25.5 で報告口に対して見つけたのと同じ形が、新しい口で再発していた。
//
// **最初はフレームそのものを絞ろうとして、間違えた。** よく喋る子（実測
// 18,000 フレーム/秒）のセッションで制御口が切れる——子が饒舌だからという
// 理由で見張りを切るのは、防いでいるものより悪い。絞るのは DB に行が
// 増える経路だけにした。
func TestTheExecutionSideCannotInflateTheDatabase(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	rec, err := s.Start("test", allowHere(t, db))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	s.mu.Lock()
	tok := s.live[rec.ID].token
	s.mu.Unlock()

	var before int
	db.QueryRow(`select count(*) from audit`).Scan(&before)

	const n = 20000
	frame, _ := json.Marshal(map[string]any{"tool_name": "Write"})
	for i := 0; i < n; i++ {
		s.dispatchForTest(Msg{T: MsgFrame, Session: rec.ID, Token: tok,
			Kind:  "control_request/can_use_tool",
			ReqID: fmt.Sprintf("flood-%d", i), Text: "Write", Frame: frame})
	}

	var approvals, after int
	db.QueryRow(`select count(*) from approvals`).Scan(&approvals)
	db.QueryRow(`select count(*) from audit`).Scan(&after)
	grew := after - before
	t.Logf("%d 件を流し込んで approvals %d 行 / audit +%d 行", n, approvals, grew)
	if approvals > maxOpenApprovals {
		t.Fatalf("approvals が %d 行（上限 %d）", approvals, maxOpenApprovals)
	}
	if grew > maxAuditPerMin+8 {
		t.Fatalf("監査ログが %d 行増えた（1分あたりの上限 %d）", grew, maxAuditPerMin)
	}
}

// **よく喋る子で見張りが切れない。**（上の直しで一度壊した形）
func TestAChattyChildDoesNotLoseItsSupervisor(t *testing.T) {
	db := newDB(t)
	s := New(db)
	a := attach(t, s, noisyClaude(t, 4000))
	a.LogDir = t.TempDir()
	rec, err := s.Start("test", allowHere(t, db))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	if err := s.Input(rec.ID, "go"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	if !s.AgentConnected() {
		t.Fatal("よく喋ったせいで制御口が切れた")
	}
}

// 待っている承認の数にも上限がある（ゆっくり流し込まれても溜まらない）。
func TestOpenApprovalsAreCapped(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	rec, err := s.Start("test", allowHere(t, db))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	s.mu.Lock()
	tok := s.live[rec.ID].token
	s.mu.Unlock()

	for i := 0; i < maxOpenApprovals*3; i++ {
		s.dispatchForTest(Msg{T: MsgFrame, Session: rec.ID, Token: tok,
			Kind:  "control_request/can_use_tool",
			ReqID: fmt.Sprintf("a-%d", i), Text: "Write"})
	}
	got := pending(t, s, rec.ID)
	if len(got) > maxOpenApprovals {
		t.Fatalf("待っている承認が %d 件（上限 %d）", len(got), maxOpenApprovals)
	}
	if !auditHasSilent(db, "tool.ask", "越えたので記録しない") {
		t.Fatal("上限に達したことが記録に残っていない")
	}
	t.Logf("%d 件で頭打ち（上限 %d）", len(got), maxOpenApprovals)
}

// 承認以外の経路でも、実行面から監査ログを太らせられない。
//
// 承認には別の上限（同時に待てる数）が掛かっているので、そちらだけ塞いでも
// **「溢れた」の報告のような、何度でも送れるもの**が残る。
func TestOtherAgentPathsCannotInflateTheAuditLog(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	rec, err := s.Start("test", allowHere(t, db))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	s.mu.Lock()
	tok := s.live[rec.ID].token
	s.mu.Unlock()

	var before int
	db.QueryRow(`select count(*) from audit`).Scan(&before)
	for i := 0; i < 5000; i++ {
		s.dispatchForTest(Msg{T: MsgDropped, Session: rec.ID, Token: tok, Dropped: int64(i)})
	}
	var after int
	db.QueryRow(`select count(*) from audit`).Scan(&after)
	grew := after - before
	t.Logf("5000 件の「溢れた」報告で監査ログが %d 行増えた", grew)
	if grew > maxAuditPerMin+4 {
		t.Fatalf("監査ログが %d 行増えた（1分あたりの上限 %d）", grew, maxAuditPerMin)
	}
	if grew == 0 {
		t.Fatal("1行も残っていない。**黙って捨ててはいけない**")
	}
	// **抑えたこと自体が記録に残る。** 残さないと、あとから
	// 「その分は起きなかった」と読める。
	if !auditHasSilent(db, "session.log_dropped", "しばらく記録しない") {
		t.Fatal("記録を抑えたことが、どこにも残っていない")
	}
}

// ---------------------------------------------------------------- codex の指摘

// **台帳が知っている pid と違うものを名乗ったら引き取らない。**
//
// 見ないと、偽の実行面が自分の持つ生きた pid を名乗って他人のセッションの行を
// 乗っ取れる。そのあと偽の exited や承認要求を campd 自身に書かせられる。
func TestReadoptRefusesAPIDTheLedgerDoesNotKnow(t *testing.T) {
	db := newDB(t)
	s := New(db)
	self := os.Getpid()
	st, _ := Starttime(self)
	// 台帳は別の pid を覚えている。
	mustInsert(t, db, "sess", StateRunning, 999999, 12345, BootID())

	c := &Control{s: s}
	c.readopt([]Held{{ID: "sess", Token: "t", PID: self, Started: st, BootID: BootID()}})
	if len(s.Live()) != 0 {
		t.Fatal("台帳と違う pid を名乗って引き取られた")
	}
	if !auditHasSilent(db, "session.readopt", "台帳の pid") {
		t.Fatal("食い違いが記録に残っていない")
	}
}

// **scope 名を名乗らせない。** これは systemctl --user stop に渡る。
func TestReadoptRefusesAForeignScopeName(t *testing.T) {
	db := newDB(t)
	s := New(db)
	self := os.Getpid()
	st, _ := Starttime(self)
	mustInsert(t, db, "sess", StateRunning, self, st, BootID())

	c := &Control{s: s}
	c.readopt([]Held{{ID: "sess", Token: "t", PID: self, Started: st,
		BootID: BootID(), Scope: "dbus.service"}})

	r, _ := get(db, "sess")
	if r.Scope == "dbus.service" {
		t.Fatal("知らない unit 名を台帳に入れた。**止めろと言われたら本当に止める**")
	}
	if !auditHasSilent(db, "session.readopt", "知らない scope") {
		t.Fatal("名乗りを断ったことが記録に残っていない")
	}
	// 正しい名前なら通る。
	s2 := New(db)
	c2 := &Control{s: s2}
	c2.readopt([]Held{{ID: "sess", Token: "t", PID: self, Started: st,
		BootID: BootID(), Scope: scopeName("sess")}})
	r, _ = get(db, "sess")
	if r.Scope != scopeName("sess") {
		t.Fatalf("正しい scope 名が入っていない: %q", r.Scope)
	}
}

// **ターンの途中で引き取ったら、idle と言わない。**
//
// idle にすると、応答生成中の子に入力を重ねて送れる。
func TestReadoptKeepsARunningTurnRunning(t *testing.T) {
	db := newDB(t)
	s := New(db)
	self := os.Getpid()
	st, _ := Starttime(self)
	mustInsert(t, db, "sess", StateRunning, self, st, BootID())

	c := &Control{s: s}
	c.readopt([]Held{{ID: "sess", Token: "t", PID: self, Started: st,
		BootID: BootID(), State: StateRunning}})
	if got := state(t, db, "sess"); got != StateRunning {
		t.Fatalf("%s で引き取った。ターン中の子に入力を重ねられる", got)
	}
	if err := s.Input("sess", "x"); err == nil {
		t.Fatal("ターン中なのに入力を受けた")
	}
	// 何も言われなければ running 側に倒す（分からないときに idle にしない）。
	s2 := New(db)
	(&Control{s: s2}).readopt([]Held{{ID: "sess", Token: "t", PID: self,
		Started: st, BootID: BootID()}})
	if got := state(t, db, "sess"); got != StateRunning {
		t.Fatalf("状態を名乗られなかったのに %s にした", got)
	}
}

// **「届かなかった」を「起きなかった」と確定しない。**
//
// 途中まで書けていれば子は起きている。exited と書くと、あとから来る started も
// 繋ぎ直しの名乗りも弾いてしまい、生きた子が台帳から外れる。
func TestAFailedStartSendDoesNotBuryAPossiblyLiveChild(t *testing.T) {
	db := newDB(t)
	s := New(db)
	s.agent = &agentConn{c: brokenConn{}, who: "test"}
	rec, err := s.Start("test", allowHere(t, db))
	if err == nil {
		t.Fatal("送れないのに成功した")
	}
	if rec.ID == "" {
		t.Fatal("id を返していない。あとから来た started と結びつけられない")
	}
	if got := state(t, db, rec.ID); got != StateStarting {
		t.Fatalf("%s にした。**生きているかもしれない子を埋めた**", got)
	}
	if len(s.Live()) != 1 {
		t.Fatal("live から外した。あとから来る started を弾いてしまう")
	}
}

// 書けない接続。
type brokenConn struct{ nopConn }

func (brokenConn) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// **届かなかった承認を「答えた」ことにしない。**
//
// DB だけ回答済みにすると、子は待ったまま、画面は答えたと出て、
// 同じ承認へ答え直すこともできなくなる。
func TestAnUndeliverableApprovalGoesBackToWaiting(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	rec, err := s.Start("test", allowHere(t, db))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	if err := ask(db, rec.ID, "r", "Write", "{}", time.Now()); err != nil {
		t.Fatal(err)
	}
	// 実行面を「書けない」ものに差し替える。
	s.mu.Lock()
	s.agent = &agentConn{c: brokenConn{}, who: "test"}
	s.mu.Unlock()

	if err := s.Approve(rec.ID, "r", "allow", ""); err == nil {
		t.Fatal("届いていないのに成功した")
	}
	if got := pending(t, s, rec.ID); len(got) != 1 {
		t.Fatalf("待ちに戻っていない: %v", got)
	}
	if !auditHasSilent(db, "tool.approve", "待ちに戻した") {
		t.Fatal("戻したことが記録に残っていない")
	}
}

// **書けなかったのに数えを進めない。**
//
// 進めると、途中まで書かれた行が次の行と繋がって壊れ、Tail はそれを黙って
// 飛ばす——欠番なのに gap が立たない。
func TestAFailedWriteDoesNotAdvanceTheCursor(t *testing.T) {
	dir := t.TempDir()
	lg, err := OpenLog(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	if _, err := lg.Append("assistant", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	_, before, _ := lg.Stats()

	// 書けなくする。
	lg.mu.Lock()
	lg.f.Close()
	lg.mu.Unlock()

	if _, err := lg.Append("assistant", []byte(`{}`)); err == nil {
		t.Fatal("閉じたファイルに書けたことになっている")
	}
	if _, after, _ := lg.Stats(); after != before {
		t.Fatalf("番号が %d → %d に進んだ", before, after)
	}
	if !lg.Broken() {
		t.Fatal("壊れたことを覚えていない")
	}
	// 以後「飛んでいない」とは言えない。
	if _, gap, _ := lg.Tail(before, 10); !gap {
		t.Fatal("壊れたあとに gap を立てていない")
	}
}

// 落とした件数は、実行面を起こし直しても 0 に戻らない。
func TestTheDroppedCountSurvivesAReopen(t *testing.T) {
	old := maxLogBytes
	maxLogBytes = 1024
	defer func() { maxLogBytes = old }()

	dir := t.TempDir()
	lg, err := OpenLog(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		lg.Append("assistant", []byte(`{"pad":"`+strings.Repeat("x", 60)+`"}`))
	}
	_, _, dropped := lg.Stats()
	if dropped == 0 {
		t.Skip("まだ2回転していない")
	}
	lg.Close()

	again, err := OpenLog(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if _, _, got := again.Stats(); got != dropped {
		t.Fatalf("落とした件数が %d → %d に戻った", dropped, got)
	}
}

// **上限は競合で越えられない。**
func TestTheConcurrencyLimitHoldsUnderARace(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	s.SetMaxConcurrent(2)
	dirs := make([]string, 12)
	for i := range dirs {
		dirs[i] = allowHere(t, db)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	started := 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Start("test", dirs[i]); err == nil {
				mu.Lock()
				started++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if started > 2 {
		t.Fatalf("上限 2 に対して %d 本起こした", started)
	}
}
