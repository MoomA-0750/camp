package session

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
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

// fakeClaude は stream-json を最小限だけ喋る子。本物を呼ばずに状態機械を回す。
func fakeClaude(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-claude")
	body := `#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"fake-1"}'
while IFS= read -r line; do
  case "$line" in
    *interrupt*) echo '{"type":"result","subtype":"aborted","session_id":"fake-1"}' ;;
    *'"type":"user"'*) echo '{"type":"result","subtype":"success","session_id":"fake-1"}' ;;
  esac
done
exit 0
`
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// wire は supervisor と実行面を本物の socket で繋ぐ。
func wire(t *testing.T, db *store.DB) (*Supervisor, *Agent) {
	t.Helper()
	s := New(db)
	return s, attach(t, s, fakeClaude(t))
}

// attach は既にある supervisor に実行面を繋ぐ。campd の再起動を模すのに使う。
func attach(t *testing.T, s *Supervisor, claude string) *Agent {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "a.sock")
	c, err := s.Listen(sock, "", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	go c.Serve()
	t.Cleanup(func() { c.Close() })

	a := NewAgent(sock, claude)
	a.Scope = false // テストで systemd に触らない
	if err := a.Dial("test"); err != nil {
		t.Fatal(err)
	}
	go a.Run()
	t.Cleanup(func() { a.conn.Close() })

	waitFor(t, 3*time.Second, func() bool { return s.AgentConnected() })
	return a
}

func waitFor(t *testing.T, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%v 待っても起きなかった", d)
}

func state(t *testing.T, db *store.DB, id string) string {
	t.Helper()
	r, err := get(db, id)
	if err != nil {
		t.Fatal(err)
	}
	return r.State
}

// ---------------------------------------------------------------- 所有権

// **pid の使い回しで他人のプロセスを掴まない。**
//
// 起動時刻を1つずらすだけで「別のもの」と判定できなければ、pid が回ってきた
// ときに無関係なプロセスを自分の子だと思い込む——止めれば他人を殺す。
func TestPIDReuseCannotStealAnotherProcess(t *testing.T) {
	self := os.Getpid()
	st, err := Starttime(self)
	if err != nil {
		t.Fatal(err)
	}
	boot := BootID()

	if alive, known := (Owner{PID: self, Started: st, BootID: boot}).Alive(); !alive || !known {
		t.Fatal("自分自身を生きていると判定できていない")
	}
	// 同じ pid、違う起動時刻＝同じ番号を取った別のプロセス。
	alive, known := (Owner{PID: self, Started: st + 1, BootID: boot}).Alive()
	if alive {
		t.Fatal("pid だけで判定している。使い回された pid を掴む")
	}
	if !known {
		t.Fatal("判定できたはずなのに「分からない」と言っている")
	}
}

// 再起動を跨いだら、起動時刻は比べる意味を失う。
func TestARebootMakesEveryOldPIDStale(t *testing.T) {
	self := os.Getpid()
	st, _ := Starttime(self)
	alive, known := (Owner{PID: self, Started: st, BootID: "00000000-0000-0000-0000-000000000000"}).Alive()
	if alive {
		t.Fatal("boot_id を見ていない。再起動前の pid を生きていると答える")
	}
	if !known {
		t.Fatal("再起動を跨いだことは分かるはず")
	}
}

// **「見ていないから居ない」を「居ないから居ない」と読ませない。**
func TestUnknownLivenessIsNotReportedAsAbsent(t *testing.T) {
	old := procRoot
	procRoot = filepath.Join(t.TempDir(), "no-proc")
	defer func() { procRoot = old }()

	// stat が読めない（ディレクトリごと無い）。ENOENT なので「居ない」と答えるのが正しい。
	if alive, known := (Owner{PID: 1, Started: 1, BootID: ""}).Alive(); alive || !known {
		t.Fatalf("居ない pid の扱いがおかしい: alive=%v known=%v", alive, known)
	}
	// 起動時刻を持っていないものは、比べようがない＝分からない。
	procRoot = old
	if _, known := (Owner{PID: os.Getpid(), Started: 0, BootID: ""}).Alive(); known {
		t.Fatal("比べる相手が無いのに「分かった」と答えている")
	}
}

// ---------------------------------------------------------------- 再起動後の照合

// campd を落として再起動したとき、幽霊と孤児を**両方**見つける。
func TestRestartFindsBothGhostsAndOrphans(t *testing.T) {
	db := newDB(t)
	s := New(db)

	// 孤児: 生きているプロセス（このテスト自身）を指す行。
	self := os.Getpid()
	st, _ := Starttime(self)
	mustInsert(t, db, "orphan", StateRunning, self, st, BootID())

	// 幽霊: もう居ないプロセスを指す行。
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	mustInsert(t, db, "ghost", StateRunning, dead.Process.Pid, 1, BootID())

	ghosts, orphans, unknown, err := s.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	if ghosts != 1 || orphans != 1 || unknown != 0 {
		t.Fatalf("幽霊=%d 孤児=%d 不明=%d（1/1/0 のはず）", ghosts, orphans, unknown)
	}
	if got := state(t, db, "orphan"); got != StateOrphaned {
		t.Fatalf("孤児が %s になっている。**まだ動いているものを「終わった」と書かない**", got)
	}
	if got := state(t, db, "ghost"); got != StateExited {
		t.Fatalf("幽霊が %s のまま", got)
	}
}

func mustInsert(t *testing.T, db *store.DB, id, st string, pid int, started uint64, boot string) {
	t.Helper()
	if err := insert(db, Record{
		ID: id, Cwd: "/tmp", State: st, RequestedBy: "test",
		CreatedAt: now(), UpdatedAt: now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`update runtime_sessions set state=?, pid=?, proc_started=?, boot_id=? where id=?`,
		st, pid, started, boot, id); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------- 実行面

// 1本のセッションが starting→idle→running→idle→exited を通る。
func TestOneSessionRunsThroughTheStateMachine(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)

	rec, err := s.Start("test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != StateStarting {
		t.Fatalf("最初は starting のはず: %s", rec.State)
	}
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })

	if err := s.Input(rec.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	// result が返れば idle に戻る。
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })

	// 子が名乗った id が結びついている。**これが無いと記録と繋がらない。**
	r, _ := get(db, rec.ID)
	if r.ClaudeID != "fake-1" {
		t.Fatalf("claude_id が結びついていない: %q", r.ClaudeID)
	}
	if r.PID == 0 || r.Started == 0 {
		t.Fatalf("所有権が書かれていない: pid=%d started=%d", r.PID, r.Started)
	}

	if err := s.Stop(rec.ID, StopTerminate); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateExited })
}

// **合鍵が無ければ、他のセッションのフレームを流し込めない。**
func TestTheTokenStopsCrossSessionMixups(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)

	rec, err := s.Start("test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })

	if _, ok := s.check(Msg{Session: rec.ID, Token: "でたらめ"}); ok {
		t.Fatal("違う合鍵で通っている")
	}
	if _, ok := s.check(Msg{Session: rec.ID}); ok {
		t.Fatal("合鍵なしで通っている")
	}
	if _, ok := s.check(Msg{Session: "知らない id", Token: "でたらめ"}); ok {
		t.Fatal("知らないセッションが通っている")
	}
}

// 待っていない承認には答えられない。二度答えることもできない。
func TestAnApprovalCanOnlyBeAnsweredOnce(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	rec, _ := s.Start("test", t.TempDir())
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })

	if err := s.Approve(rec.ID, "req-1", "allow", ""); err == nil {
		t.Fatal("待っていない承認に答えられてしまった")
	}
	if err := ask(db, rec.ID, "req-1", "Write", `{"tool_name":"Write"}`, time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := s.Approve(rec.ID, "req-1", "allow", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(rec.ID, "req-1", "deny", ""); err == nil {
		t.Fatal("同じ承認に二度答えられてしまった")
	}
}

// 実行面は同時に1つだけ。**2つ繋げると、どちらが本物か campd が決められない。**
func TestASecondExecutionSideIsRefused(t *testing.T) {
	db := newDB(t)
	s, a := wire(t, db)

	b := NewAgent(a.Sock, a.Claude)
	b.Scope = false
	if err := b.Dial("test-2"); err != nil {
		t.Fatal(err)
	}
	defer b.conn.Close()
	err := b.Run()
	if err == nil || !strings.Contains(err.Error(), "既に繋がっている") {
		t.Fatalf("2つ目が断られていない: %v", err)
	}
	if !s.AgentConnected() {
		t.Fatal("1つ目まで落ちている")
	}
}

// 実行面が落ちたら、走っていたものは**孤児**になる。「終わった」ではない。
func TestLosingTheExecutionSideOrphansInsteadOfBuries(t *testing.T) {
	db := newDB(t)
	s, a := wire(t, db)
	rec, _ := s.Start("test", t.TempDir())
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })

	a.conn.Close()
	waitFor(t, 5*time.Second, func() bool { return !s.AgentConnected() })
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateOrphaned })

	if err := s.Input(rec.ID, "x"); err == nil {
		t.Fatal("実行面が居ないのに入力を受けている")
	}
	// 後始末（子は生きているので明示的に止める）
	a.stopAll("テストの後始末")
}

func TestStartingWithoutAnExecutionSideIsRefused(t *testing.T) {
	db := newDB(t)
	s := New(db)
	if _, err := s.Start("test", t.TempDir()); err == nil {
		t.Fatal("実行面が無いのに起こせてしまった")
	}
}

func TestTooManyAtOnceIsRefused(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	s.SetMaxConcurrent(1)
	if _, err := s.Start("test", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start("test", t.TempDir()); err == nil {
		t.Fatal("上限を越えて起こせてしまった")
	}
}

// cwd は実パスで見る。symlink 越しでも同じ場所を指す。
func TestCwdIsResolvedThroughSymlinks(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	got, err := resolveCwd(link)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(real)
	if got != want {
		t.Fatalf("symlink を解いていない: %s（%s のはず）", got, want)
	}
	if _, err := resolveCwd("relative/path"); err == nil {
		t.Fatal("相対パスが通っている")
	}
	f := filepath.Join(real, "file")
	os.WriteFile(f, nil, 0o600)
	if _, err := resolveCwd(f); err == nil {
		t.Fatal("ファイルを cwd にできている")
	}
}

// ---------------------------------------------------------------- 時間切れ

func TestIdleAndLongTurnsAreCollected(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	rec, _ := s.Start("test", t.TempDir())
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })

	// 時計を進める代わりに、待つ長さをゼロにする。
	s.IdleAfter = 0
	s.Tick()
	waitFor(t, 5*time.Second, func() bool {
		st := state(t, db, rec.ID)
		return st == StateStopping || st == StateExited
	})
}

func TestAStartThatNeverReportsIsGivenUp(t *testing.T) {
	db := newDB(t)
	s := New(db)
	// 実行面が居ることにするが、何も返さない。
	s.agent = &agentConn{c: nopConn{}, who: "test"}
	s.StartAfter = 0
	rec, err := s.Start("test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.Tick()
	if got := state(t, db, rec.ID); got != StateExited {
		t.Fatalf("起動が確認できないまま放置されている: %s", got)
	}
}

// nopConn は書き込みを捨てるだけの接続。
type nopConn struct{}

func (nopConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (nopConn) Write(b []byte) (int, error)      { return len(b), nil }
func (nopConn) Close() error                     { return nil }
func (nopConn) SetDeadline(time.Time) error      { return nil }
func (nopConn) SetReadDeadline(time.Time) error  { return nil }
func (nopConn) SetWriteDeadline(time.Time) error { return nil }
func (nopConn) LocalAddr() net.Addr              { return nopAddr{} }
func (nopConn) RemoteAddr() net.Addr             { return nopAddr{} }

type nopAddr struct{}

func (nopAddr) Network() string { return "nop" }
func (nopAddr) String() string  { return "nop" }

// ---------------------------------------------------------------- 分割そのもの

// **実行面は DB へ触る口を持たない。**
//
// 分割の全部がこれ1つに掛かっているので、コードの形として確かめる。
// agent.go が store を import した時点で、この分割は意味を失う。
func TestTheExecutionSideHasNoWayToTouchTheDatabase(t *testing.T) {
	b, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"internal/store", "database/sql", "modernc.org/sqlite"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("agent.go が %s を参照している。**実行面に DB を持たせない**", bad)
		}
	}
}

func TestFrameKindsAreFoldedTheSameWayTheProbeDid(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"type":"system","subtype":"init"}`, "system/init"},
		{`{"type":"control_request","request":{"subtype":"can_use_tool"}}`, "control_request/can_use_tool"},
		{`{"type":"result"}`, "result"},
		{`{"type":"rate_limit_event"}`, "rate_limit_event"},
		{`{}`, "?"},
	}
	for _, c := range cases {
		var f map[string]any
		if err := jsonUnmarshal(c.in, &f); err != nil {
			t.Fatal(err)
		}
		if got := FrameKind(f); got != c.want {
			t.Errorf("%s → %s（%s のはず）", c.in, got, c.want)
		}
	}
}

func jsonUnmarshal(s string, v any) error {
	return jsonDecode([]byte(s), v)
}

func TestStateNamesAreClosed(t *testing.T) {
	for _, s := range []string{StateStarting, StateIdle, StateRunning, StateStopping, StateExited, StateOrphaned} {
		if !validState(s) {
			t.Errorf("%s が知らない状態になっている", s)
		}
	}
	if validState("走ってる") {
		t.Error("知らない状態が通っている")
	}
	if err := insert(newDB(t), Record{ID: "x", Cwd: "/", State: "でたらめ", RequestedBy: "t"}); err == nil {
		t.Error("知らない状態のまま書けてしまった")
	}
	_ = fmt.Sprint()
}

// ---------------------------------------------------------------- 本物で通す

// 本物の `claude` を1本起こして、承認まで含めて通す。
//
// 既定では走らない（トークンを使うので）。走らせるときは
//
//	CAMP_E2E_CLAUDE=1 go test ./internal/session/ -run RealClaude -v
//
// **偽物だけで済ませない理由**: 承認要求は本物の CLI が出すもので、
// 偽物に出させると「自分で書いた形が自分で読める」ことしか確かめられない。
func TestARealClaudeSessionRunsEndToEnd(t *testing.T) {
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

	db := newDB(t)
	s := New(db)
	sock := filepath.Join(t.TempDir(), "a.sock")
	c, err := s.Listen(sock, "", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	go c.Serve()
	defer c.Close()

	a := NewAgent(sock, bin)
	a.Scope = true // **孫まで包む。本物でこそ確かめる意味がある**
	if err := a.Dial("e2e"); err != nil {
		t.Fatal(err)
	}
	go a.Run()
	defer a.conn.Close()
	waitFor(t, 3*time.Second, func() bool { return s.AgentConnected() })

	work := t.TempDir()
	rec, err := s.Start("e2e", work)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop(rec.ID, StopTerminate)
	waitFor(t, 30*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })

	if err := s.Input(rec.ID, "Write a file named hello.txt containing exactly `hi` in the current directory, then say done."); err != nil {
		t.Fatal(err)
	}

	// 承認は**1ターンに何度でも来る。**（2026-09-04 実測。最初この test は
	// 1回だけ答えて止まった。1つ答えて終わりにすると、2つ目で子が待ち続ける。）
	// 画面も列として扱う必要がある——M28 の受け入れ条件に効く。
	answered := 0
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			for _, req := range s.Pending(rec.ID) {
				if err := s.Approve(rec.ID, req, "allow", ""); err == nil {
					answered++
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	waitFor(t, 180*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(work, "hello.txt"))
		return err == nil
	})
	waitFor(t, 180*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	if answered == 0 {
		t.Fatal("承認が一度も来ていない。**来なければ、この設計は成り立っていない**")
	}
	t.Logf("承認を %d 回答えた", answered)

	r, _ := get(db, rec.ID)
	if r.ClaudeID == "" {
		t.Fatal("子が名乗った session_id を結びつけていない")
	}
	if r.Scope == "" {
		t.Fatal("scope で包んでいない。孫まで止められない")
	}
	// scope が本当に居たか。**「包んだつもり」を確かめる。**
	cg := "/sys/fs/cgroup/user.slice/user-" + fmt.Sprint(os.Getuid()) +
		".slice/user@" + fmt.Sprint(os.Getuid()) + ".service/app.slice/" + r.Scope
	if _, err := os.Stat(cg); err != nil {
		t.Errorf("scope の cgroup が見つからない（%s）: %v", cg, err)
	}
	t.Logf("claude_id=%s pid=%d scope=%s", r.ClaudeID, r.PID, r.Scope)

	if err := s.Stop(rec.ID, StopTerminate); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, func() bool { return state(t, db, rec.ID) == StateExited })
	if _, err := os.Stat(cg); err == nil {
		t.Error("止めたのに cgroup が残っている")
	}
}

// campd を落として起こし直したとき、**生きている子を「終わった」と書かない。**
//
// 単体の Reconcile とは別に、本当に走っている子で確かめる。ここが逆になると、
// 画面には「終わった」と出ているのに、プロセスは動き続けることになる。
func TestARestartWhileAChildIsAliveMarksItOrphanAndThenReapsIt(t *testing.T) {
	db := newDB(t)
	s1, a1 := wire(t, db)

	rec, err := s1.Start("test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	r, _ := get(db, rec.ID)
	if alive, known := r.Owner().Alive(); !alive || !known {
		t.Fatal("そもそも子が生きていない")
	}

	// campd が落ちた（実行面と子はそのまま）。
	s2 := New(db)
	ghosts, orphans, unknown, err := s2.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	if orphans != 1 || ghosts != 0 || unknown != 0 {
		t.Fatalf("幽霊=%d 孤児=%d 不明=%d（0/1/0 のはず）", ghosts, orphans, unknown)
	}
	if got := state(t, db, rec.ID); got != StateOrphaned {
		t.Fatalf("%s になっている。**生きているものを埋めてはいけない**", got)
	}

	// 古い実行面を落として、新しいのを繋ぐ。孤児は始末される。
	a1.conn.Close()
	attach(t, s2, a1.Claude)
	waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateExited })

	r, _ = get(db, rec.ID)
	if alive, _ := r.Owner().Alive(); alive {
		t.Fatal("exited と書いたのにプロセスが生きている")
	}
	if !strings.Contains(r.ExitReason, "孤児") {
		t.Fatalf("何があったか分からない終わり方: %q", r.ExitReason)
	}
}

// **始末したという申告を、そのまま信じない。**
//
// 実行面は本人のユーザーで動くので、嘘を言える。生きているものを「止めた」と
// 言われたら、campd は /proc を見て食い違いを記録する。
func TestAFalseReapIsCaughtByLookingAtProc(t *testing.T) {
	db := newDB(t)
	s := New(db)
	self := os.Getpid()
	st, _ := Starttime(self)
	mustInsert(t, db, "liar", StateOrphaned, self, st, BootID())

	s.dispatchForTest(Msg{T: MsgReaped, Session: "liar", Reason: "止めた（嘘）"})

	if got := state(t, db, "liar"); got == StateExited {
		t.Fatal("生きているのに「終わった」と書いた。申告を鵜呑みにしている")
	}
	rows, err := db.Query(`select detail_json from audit where action='session.reap'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var found bool
	for rows.Next() {
		var d string
		rows.Scan(&d)
		if strings.Contains(d, "まだ生きている") {
			found = true
		}
	}
	if !found {
		t.Fatal("食い違いが監査ログに残っていない")
	}
}

// ---------------------------------------------------------------- 落とし先（M27）

// noisyClaude は1ターンごとに大量に吐く子。**パイプが詰まるかを見るため。**
func noisyClaude(t *testing.T, perTurn int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "noisy-claude")
	body := fmt.Sprintf(`#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"noisy-1"}'
pad=$(head -c 900 /dev/zero | tr '\0' 'x')
while IFS= read -r line; do
  case "$line" in
    *'"type":"user"'*)
      i=0
      while [ $i -lt %d ]; do
        echo "{\"type\":\"assistant\",\"session_id\":\"noisy-1\",\"pad\":\"$pad\"}"
        i=$((i+1))
      done
      echo '{"type":"result","subtype":"success","session_id":"noisy-1"}'
      ;;
  esac
done
exit 0
`, perTurn)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// **誰も読んでいなくても、子は最後まで走り切る。**
//
// これが M27 の受け入れ条件そのもの。読み手が居ないとき stdout が詰まって
// 子が止まる、という壊れ方は、画面を閉じただけで起きる。
func TestTheChildRunsToTheEndWithNobodyReading(t *testing.T) {
	db := newDB(t)
	s := New(db)
	a := attach(t, s, noisyClaude(t, 4000)) // 約 4MB。パイプ(64KB)よりずっと大きい
	a.LogDir = t.TempDir()

	rec, err := s.Start("test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	if err := s.Input(rec.ID, "go"); err != nil {
		t.Fatal(err)
	}
	// 誰も Tail を呼ばない。それでも result まで届く。
	waitFor(t, 60*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })

	res, err := s.Tail(rec.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Newest < 4000 {
		t.Fatalf("落ちているのが %d 件しかない（4000 件以上のはず）", res.Newest)
	}
}

// 遅い読み手が居ても、子は止まらない。
func TestASlowReaderNeverStallsTheChild(t *testing.T) {
	db := newDB(t)
	s := New(db)
	a := attach(t, s, noisyClaude(t, 3000))
	a.LogDir = t.TempDir()

	rec, _ := s.Start("test", t.TempDir())
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })

	stop := make(chan struct{})
	defer close(stop)
	go func() { // モバイル回線を模した読み手。1回読んでは寝る
		var since int64
		for {
			select {
			case <-stop:
				return
			default:
			}
			r, err := s.Tail(rec.ID, since, 5)
			if err == nil && len(r.Lines) > 0 {
				since = r.Lines[len(r.Lines)-1].Seq
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()

	if err := s.Input(rec.ID, "go"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
}

// 溢れたら古いほうから落とす。**落としたことを言う。**
func TestTheLogDropsTheOldestAndSaysSo(t *testing.T) {
	old := maxLogBytes
	maxLogBytes = 4096
	defer func() { maxLogBytes = old }()

	lg, err := OpenLog(t.TempDir(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	for i := 0; i < 300; i++ {
		if _, err := lg.Append("assistant", []byte(`{"pad":"`+strings.Repeat("x", 100)+`"}`)); err != nil {
			t.Fatal(err)
		}
	}
	oldest, newest, dropped := lg.Stats()
	if dropped == 0 {
		t.Fatal("溢れたのに落としていない（上限が効いていない）")
	}
	if oldest <= 1 {
		t.Fatalf("古いほうが残ったまま: oldest=%d", oldest)
	}
	if newest != 300 {
		t.Fatalf("newest=%d（300 のはず）", newest)
	}
	// カーソルが落ちた範囲を指していたら、飛んだことを言う。
	_, gap, err := lg.Tail(0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !gap {
		t.Fatal("飛んだことを言っていない。**黙って飛ばしてはいけない**")
	}
	// 残っている範囲を指していれば、飛んでいない。
	_, gap, _ = lg.Tail(newest-1, 10)
	if gap {
		t.Fatal("飛んでいないのに飛んだと言っている")
	}
}

// カーソルは通し番号。**実行面を起こし直しても振り直さない。**
func TestTheCursorSurvivesTheExecutionSideRestarting(t *testing.T) {
	dir := t.TempDir()
	lg, err := OpenLog(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		lg.Append("assistant", []byte(`{}`))
	}
	lg.Close()

	again, err := OpenLog(dir, "s1")
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	seq, err := again.Append("assistant", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if seq != 6 {
		t.Fatalf("番号が %d に戻った（6 のはず）。読み手のカーソルが黙ってずれる", seq)
	}
	lines, _, _ := again.Tail(3, 100)
	if len(lines) != 3 {
		t.Fatalf("カーソルの続きが %d 件（3 件のはず）", len(lines))
	}
}

// tail は要求された範囲だけを返す。**境界を越える量に天井がある。**
func TestTailNeverReturnsMoreThanAsked(t *testing.T) {
	lg, err := OpenLog(t.TempDir(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	for i := 0; i < 1200; i++ {
		lg.Append("assistant", []byte(`{}`))
	}
	lines, _, _ := lg.Tail(0, 10)
	if len(lines) != 10 {
		t.Fatalf("%d 件返した（10 件のはず）", len(lines))
	}
	lines, _, _ = lg.Tail(0, 99999) // 天井を越えて要求する
	if len(lines) > maxTailLines {
		t.Fatalf("%d 件返した（上限 %d）", len(lines), maxTailLines)
	}
}

// ---------------------------------------------------------------- 承認（M28）

// **待っている承認は campd を入れ替えても消えない。**
//
// 承認要求が来ると子は答えるまで止まる。待ちが campd のメモリにしか無いと、
// 入れ替えた瞬間に「誰が何を訊かれていたか」が消えて、子だけが待ち続ける。
func TestAWaitingApprovalSurvivesCampdRestarting(t *testing.T) {
	db := newDB(t)
	s1, a := wire(t, db)

	rec, err := s1.Start("test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	if err := ask(db, rec.ID, "req-9", "Write", `{"tool_name":"Write"}`, time.Now()); err != nil {
		t.Fatal(err)
	}

	// campd が入れ替わる。実行面と子はそのまま。
	a.conn.Close()
	waitFor(t, 5*time.Second, func() bool { return !s1.AgentConnected() })

	s2 := New(db)
	if _, _, _, err := s2.Reconcile(); err != nil {
		t.Fatal(err)
	}
	// 実行面が繋ぎ直す（抱えている子を名乗る）。
	sock := filepath.Join(t.TempDir(), "b.sock")
	c2, err := s2.Listen(sock, "", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	go c2.Serve()
	t.Cleanup(func() { c2.Close() })
	a.Sock = sock
	if err := a.Dial("test-again"); err != nil {
		t.Fatal(err)
	}
	go a.Run()
	waitFor(t, 5*time.Second, func() bool { return s2.AgentConnected() })

	// **子が殺されていない。** 引き取り直されて、また入力を受けられる。
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	if got := s2.Pending(rec.ID); len(got) != 1 || got[0] != "req-9" {
		t.Fatalf("待っている承認が見えない: %v", got)
	}
	w, err := s2.Waiting(rec.ID)
	if err != nil || len(w) != 1 || w[0].Tool != "Write" {
		t.Fatalf("何を訊かれているかが分からない: %+v (%v)", w, err)
	}
	if err := s2.Approve(rec.ID, "req-9", "allow", ""); err != nil {
		t.Fatalf("引き取り直したのに答えられない: %v", err)
	}
	if err := s2.Input(rec.ID, "まだ話せる"); err != nil {
		t.Fatalf("引き取り直したのに入力できない: %v", err)
	}
}

// 期限切れは**拒否として**扱い、そう記録する。
func TestAnExpiredApprovalIsDeniedAndSaidSo(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	rec, _ := s.Start("test", t.TempDir())
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })

	// 期限を過去にして置く。
	past := time.Now().Add(-2 * parkLimit)
	if err := ask(db, rec.ID, "req-old", "Bash", `{"tool_name":"Bash"}`, past); err != nil {
		t.Fatal(err)
	}
	s.Tick()

	if got := s.Pending(rec.ID); len(got) != 0 {
		t.Fatalf("期限切れがまだ待っている: %v", got)
	}
	hist, err := ApprovalHistory(db, rec.ID, 10)
	if err != nil || len(hist) != 1 {
		t.Fatalf("履歴が読めない: %+v (%v)", hist, err)
	}
	if hist[0].Behavior != "deny" || hist[0].Reason != ByTimeout {
		t.Fatalf("期限切れの扱いが違う: behavior=%s reason=%s", hist[0].Behavior, hist[0].Reason)
	}
	if !auditHas(t, db, "tool.approve", "期限切れ") {
		t.Fatal("期限切れが監査ログに残っていない")
	}
}

// セッションが終わったら、宙に浮いた承認を閉じる。
func TestApprovalsDoNotStayWaitingAfterTheSessionEnds(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	rec, _ := s.Start("test", t.TempDir())
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	if err := ask(db, rec.ID, "req-x", "Write", "{}", time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := s.Stop(rec.ID, StopTerminate); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool { return state(t, db, rec.ID) == StateExited })
	waitFor(t, 5*time.Second, func() bool { return len(s.Pending(rec.ID)) == 0 })

	hist, _ := ApprovalHistory(db, rec.ID, 10)
	if len(hist) != 1 || hist[0].Reason != BySessionEnd {
		t.Fatalf("閉じ方が違う: %+v", hist)
	}
}

// 承認は**列**。1つだけ持つ形にすると、2つ目で子が待ち続ける
// （2026-09-04 に本物で実際に踏んだ）。
func TestApprovalsAreAQueueNotASingleSlot(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	rec, _ := s.Start("test", t.TempDir())
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })

	for _, id := range []string{"r1", "r2", "r3"} {
		if err := ask(db, rec.ID, id, "Write", "{}", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.Pending(rec.ID); len(got) != 3 {
		t.Fatalf("待っているのが %d 件（3 件のはず）", len(got))
	}
	for _, id := range []string{"r1", "r2", "r3"} {
		if err := s.Approve(rec.ID, id, "allow", ""); err != nil {
			t.Fatalf("%s に答えられない: %v", id, err)
		}
	}
	if got := s.Pending(rec.ID); len(got) != 0 {
		t.Fatalf("答えたのに残っている: %v", got)
	}
}

func auditHas(t *testing.T, db *store.DB, action, needle string) bool {
	t.Helper()
	rows, err := db.Query(`select coalesce(detail_json,'') from audit where action=?`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		rows.Scan(&d)
		if strings.Contains(d, needle) {
			return true
		}
	}
	return false
}

// **抱えているという名乗りを、そのまま信じない。**
//
// 実行面は本人のユーザーで動くので、何とでも言える。居ないプロセスを
// 「まだ抱えている」と名乗られて引き取ってしまうと、台帳には走っていると
// 出たまま、実体が無いセッションが残る。
func TestAFalseClaimOfHoldingASessionIsNotReadopted(t *testing.T) {
	db := newDB(t)
	s := New(db)
	sock := filepath.Join(t.TempDir(), "a.sock")
	c, err := s.Listen(sock, "", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	go c.Serve()
	t.Cleanup(func() { c.Close() })

	// もう居ないプロセスを指す行を置く。
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	mustInsert(t, db, "claimed", StateRunning, dead.Process.Pid, 1, BootID())

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	hello, _ := json.Marshal(Msg{T: MsgHello, Version: "liar", Held: []Held{{
		ID: "claimed", Token: "でたらめ", PID: dead.Process.Pid,
		Started: 1, BootID: BootID(),
	}}})
	conn.Write(append(hello, '\n'))

	waitFor(t, 3*time.Second, func() bool { return s.AgentConnected() })
	waitFor(t, 3*time.Second, func() bool {
		return auditHasSilent(db, "session.readopt", "そのプロセスは居ない")
	})
	if len(s.Live()) != 0 {
		t.Fatal("居ないプロセスを引き取ってしまった")
	}
	if got := state(t, db, "claimed"); got == StateIdle {
		t.Fatal("台帳が idle に戻っている。実体が無いのに走っていることになる")
	}
}

// 起動時刻が食い違う名乗りも引き取らない（pid が回ってきただけ）。
func TestAReadoptWithTheWrongStartTimeIsRefused(t *testing.T) {
	db := newDB(t)
	s := New(db)
	self := os.Getpid()
	st, _ := Starttime(self)
	mustInsert(t, db, "reused", StateRunning, self, st, BootID())

	c := &Control{s: s}
	c.readopt([]Held{{ID: "reused", Token: "t", PID: self, Started: st + 1, BootID: BootID()}})
	if len(s.Live()) != 0 {
		t.Fatal("起動時刻が違うのに引き取った。pid の使い回しを掴む")
	}
	// 正しい起動時刻なら引き取る。
	c.readopt([]Held{{ID: "reused", Token: "t", PID: self, Started: st, BootID: BootID()}})
	if len(s.Live()) != 1 {
		t.Fatal("正しい名乗りを引き取れていない")
	}
}

func auditHasSilent(db *store.DB, action, needle string) bool {
	rows, err := db.Query(`select coalesce(detail_json,'') from audit where action=?`, action)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		rows.Scan(&d)
		if strings.Contains(d, needle) {
			return true
		}
	}
	return false
}
