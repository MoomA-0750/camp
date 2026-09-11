package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MoomA-0750/camp/internal/store"
)

// 2026-09-11。SSH で他のホストにセッションを起こす。
//
// 本物の ssh の代わりに、**sshd がすることだけを真似る偽物**を使う:
//
//   - 遠隔コマンドの文字列を新しいセッション（setsid）の sh に渡す
//   - 向こうは fifo だけに繋がり、手元の管は偽の ssh（とその cat）だけが握る
//     ——sshd と同じく、**向こうの誰かが出力を握っている間はチャネルを閉じない**
//   - 接続が切れたふりは、中継を切って 255 で抜ける。向こうの stdin は閉じるが、
//     向こうのプロセスは殺さない（実測どおり、読んでいない者は生き残る）
//
// 本物の sshd での確かめは outer gate で ssh localhost を使う（phase3.5-plan.md）。

const fakeSSHBody = `#!/bin/sh
dir=@DIR@
mode=run
while [ $# -gt 0 ]; do
  case "$1" in
    -G) mode=resolve; shift ;;
    -F|-o|-i|-p|-l) shift 2 ;;
    --) shift; break ;;
    -*) shift ;;
    *) break ;;
  esac
done
alias=$1; shift
echo "$mode $alias" >> "$dir/ssh.log"
if [ "$mode" = resolve ]; then cat "$dir/resolved"; exit 0; fi
if [ -e "$dir/unreachable" ]; then echo "ssh: connect to host $alias port 22: Connection timed out" >&2; exit 255; fi
if [ -e "$dir/hostkey" ]; then echo "Host key verification failed." >&2; exit 255; fi
if [ -d "$dir/bin" ]; then PATH="$dir/bin:$PATH"; export PATH; fi
# ログインシェルが名乗りに似た行を吐いたふり（合言葉は知らない）
if [ -e "$dir/forge" ]; then printf 'CAMP-REMOTE\t2\tforged\t1\t1\t-\t-\t/\t/\n'; fi
t=$(mktemp -d "$dir/ch.XXXXXX")
mkfifo "$t/in" "$t/out" "$t/err"
exec 3<&0
setsid sh -c "$1" <"$t/in" >"$t/out" 2>"$t/err" &
kid=$!
cat "$t/out" &
o=$!
cat "$t/err" >&2 &
e=$!
cat <&3 >"$t/in" &
i=$!
exec 3<&-
while kill -0 $kid 2>/dev/null; do
  if [ -e "$dir/drop" ]; then
    rm -f "$dir/drop"
    kill $o $e $i 2>/dev/null
    echo "Connection to $alias closed by remote host." >&2
    exit 255
  fi
  sleep 0.05
done
wait $kid
st=$?
wait $o
wait $e
kill $i 2>/dev/null
[ $st -gt 128 ] && exit 255
exit $st
`

// remoteClaudeBody は向こうの claude。名乗り、孫を1つ残し、話しかけられたら答える。
const remoteClaudeBody = `#!/bin/sh
touch @DIR@/claude-ran
echo $$ > @DIR@/claude.pid
sleep 300 &
echo $! > @DIR@/grandchild.pid
echo '{"type":"system","subtype":"init","session_id":"far-1"}'
while IFS= read -r line; do
  case "$line" in
    *'"type":"user"'*) echo '{"type":"result","subtype":"success","session_id":"far-1"}' ;;
  esac
done
`

// busyClaudeBody は話しかけられると工具を走らせているふりをする（stdin を読まない）。
// **接続が切れても、読んでいない子は生き残る**（実測）——それを再現する。
const busyClaudeBody = `#!/bin/sh
touch @DIR@/claude-ran
echo $$ > @DIR@/claude.pid
sleep 300 &
echo $! > @DIR@/grandchild.pid
echo '{"type":"system","subtype":"init","session_id":"far-1"}'
while IFS= read -r line; do
  case "$line" in
    *'"type":"user"'*) sleep 300 ;;
  esac
done
`

// sessionOf は pid のセッション番号。
func sessionOf(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	s := string(b)
	i := strings.LastIndex(s, ") ")
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+2:])
	if len(f) < 4 {
		return 0
	}
	n, _ := strconv.Atoi(f[3])
	return n
}

type remoteRig struct {
	s    *Supervisor
	db   *store.DB
	a    *Agent
	dir  string // 偽の ssh の置き場と印
	root string // 向こうで許した場所（実パス）
	kh   string // 偽の known_hosts（本物の ssh-keygen で引く）
}

// hostKeyLine は本物の鍵を1つ作り、known_hosts の1行にする。
func hostKeyLine(t *testing.T, dir, name string) string {
	t.Helper()
	k := filepath.Join(dir, "hostkey-"+newID()[:8])
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "", "-f", k).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v %s", err, out)
	}
	pub, err := os.ReadFile(k + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(pub))
	return name + " " + f[0] + " " + f[1] + "\n"
}

func newRemoteRig(t *testing.T, claudeBody string) *remoteRig {
	t.Helper()
	return newRemoteRigWith(t, claudeBody)
}

// newRemoteRigWith は、実行面に opts を掛けてから繋ぐ（向こうで Codex を起こすときなど）。
func newRemoteRigWith(t *testing.T, claudeBody string, opts ...func(*Agent)) *remoteRig {
	t.Helper()
	dir := t.TempDir()
	dir, _ = filepath.EvalSymlinks(dir)
	ssh := filepath.Join(dir, "ssh")
	if err := os.WriteFile(ssh, []byte(strings.ReplaceAll(fakeSSHBody, "@DIR@", dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	kh := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(kh, []byte(hostKeyLine(t, dir, "far.example")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "resolved"),
		[]byte("user tester\nhostname far.example\nport 22\nproxyjump none\n"+
			"userknownhostsfile "+kh+" "+dir+"/no-such-known-hosts\n"+
			"globalknownhostsfile /nonexistent/ssh_known_hosts\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(dir, "claude")
	if err := os.WriteFile(claude, []byte(strings.ReplaceAll(claudeBody, "@DIR@", dir)), 0o755); err != nil {
		t.Fatal(err)
	}

	db := newDB(t)
	s := New(db)
	a := attach(t, s, "/nonexistent/local-claude", append([]func(*Agent){func(a *Agent) {
		a.SSH = ssh
		a.HeaderWait = 5 * time.Second
		a.ReapWait = 10 * time.Second
		a.ReapRetry = 10 * time.Millisecond
	}}, opts...)...)

	if _, _, err := ImportSSH(db, []SSHHost{{Alias: "far"}}); err != nil {
		t.Fatal(err)
	}
	// 許すときと同じ道で固定する（ssh -G ＋ 本物の ssh-keygen）。
	pin, err := a.resolve("far")
	if err != nil {
		t.Fatal(err)
	}
	if len(pin.HostKeys) != 1 {
		t.Fatalf("known_hosts の鍵を読めていない: %+v", pin)
	}
	if err := AllowDestination(db, "far", pin); err != nil {
		t.Fatal(err)
	}
	if err := SetAgentPath(db, "far", AgentClaude, claude); err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	if _, err := AddRemoteAllowed(db, "far", root, "テスト", "test"); err != nil {
		t.Fatal(err)
	}
	return &remoteRig{s: s, db: db, a: a, dir: dir, root: root, kh: kh}
}

func (r *remoteRig) pidFrom(t *testing.T, name string) int {
	t.Helper()
	var pid int
	waitFor(t, 5*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(r.dir, name))
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && pid > 0
	})
	return pid
}

func (r *remoteRig) ran() bool {
	_, err := os.Stat(filepath.Join(r.dir, "claude-ran"))
	return err == nil
}

func (r *remoteRig) sshLog() string {
	b, _ := os.ReadFile(filepath.Join(r.dir, "ssh.log"))
	return string(b)
}

// procAlive は pid が生きているか（ゾンビは死んでいるとみなす）。
func procAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndex(s, ") ")
	return i < 0 || !strings.HasPrefix(s[i+2:], "Z")
}

func killIfAlive(pids ...int) {
	for _, p := range pids {
		if p > 0 && procAlive(p) {
			syscall.Kill(p, syscall.SIGKILL)
		}
	}
}

func (r *remoteRig) start(t *testing.T, cwd string) Record {
	t.Helper()
	rec, err := r.s.StartOn("test", "far", cwd)
	if err != nil {
		t.Fatalf("起こせない: %v", err)
	}
	return rec
}

func (r *remoteRig) waitState(t *testing.T, id, want string) Record {
	t.Helper()
	var got Record
	waitFor(t, 15*time.Second, func() bool {
		var err error
		got, err = get(r.db, id)
		return err == nil && got.State == want
	})
	return got
}

// ---------------------------------------------------------------- 起こす・話す・止める

// 起こして、話して、止める。止めたら**向こうの孫まで**居なくなる。
func TestARemoteSessionStartsTalksAndStopsEverythingOverThere(t *testing.T) {
	r := newRemoteRig(t, remoteClaudeBody)
	rec := r.start(t, r.root)
	got := r.waitState(t, rec.ID, StateIdle)
	kid, grand := r.pidFrom(t, "claude.pid"), r.pidFrom(t, "grandchild.pid")
	defer killIfAlive(kid, grand)

	// 向こうの pid は、claude の親として残る sh（sshd が setsid したセッションの頭）。
	if got.Host != "far" || got.RemotePID == 0 || sessionOf(kid) != got.RemotePID {
		t.Fatalf("向こうの身元が残っていない: host=%q remote_pid=%d（claude %d のセッションは %d）",
			got.Host, got.RemotePID, kid, sessionOf(kid))
	}
	if got.RemoteStarted == 0 || got.RemoteBootID == "" {
		t.Fatalf("向こうの起動時刻・boot_id が無い（止めるときに照らせない）: %+v", got)
	}
	if got.PID == 0 || got.PID == kid {
		t.Fatalf("手元の pid が ssh を指していない: %d", got.PID)
	}

	if err := r.s.Input(rec.ID, "こんにちは"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return liveState(r.s, rec.ID) == StateIdle })

	if err := r.s.Stop(rec.ID, StopTerminate); err != nil {
		t.Fatal(err)
	}
	end := r.waitState(t, rec.ID, StateExited)
	if end.EndCause != EndUserStop {
		t.Errorf("本人が止めたのに %q", end.EndCause)
	}
	if procAlive(kid) || procAlive(grand) {
		t.Fatalf("止めたのに向こうに残っている: 子=%v 孫=%v", procAlive(kid), procAlive(grand))
	}
}

// 子が自分で終わっても、**孫は残る**（実測）。向こうの sh が始末してから抜ける
// ——始末しないと、孫が出力を握ったままチャネルが閉じず、手元の ssh も終わらない。
func TestLeftoversOverThereAreCleanedUpWhenTheChildQuits(t *testing.T) {
	body := strings.Replace(remoteClaudeBody, "while IFS=", "exit 3\nwhile IFS=", 1)
	r := newRemoteRig(t, body)
	rec := r.start(t, r.root)
	grand := r.pidFrom(t, "grandchild.pid")
	defer killIfAlive(grand)

	end := r.waitState(t, rec.ID, StateExited)
	if end.EndCause != EndSelf || end.ExitCode == nil || *end.ExitCode != 3 {
		t.Fatalf("自分で終わった子の記録が違う: cause=%q code=%v", end.EndCause, end.ExitCode)
	}
	if procAlive(grand) {
		t.Fatal("子が終わったあと、向こうの孫が残ったまま")
	}
	if !strings.Contains(end.ExitReason, "向こう:") {
		t.Errorf("向こうを見に行ったことが理由に書かれていない: %q", end.ExitReason)
	}
}

// ---------------------------------------------------------------- 境界

// **~/.ssh/config を書き換えただけで、同じ名前のまま別の先へ繋がらない。**
func TestAConfigThatNowPointsElsewhereIsNotConnected(t *testing.T) {
	r := newRemoteRig(t, remoteClaudeBody)
	os.WriteFile(filepath.Join(r.dir, "resolved"),
		[]byte("user tester\nhostname evil.example\nport 22\n"), 0o600)

	rec := r.start(t, r.root)
	end := r.waitState(t, rec.ID, StateExited)
	if end.EndCause != EndStartFailed || !strings.Contains(end.ExitReason, "許したときと違う") {
		t.Fatalf("行き先が変わったのに止まらない: %q / %q", end.EndCause, end.ExitReason)
	}
	if strings.Contains(r.sshLog(), "run far") {
		t.Fatalf("照らす前に繋いでいる:\n%s", r.sshLog())
	}
}

// symlink で許した場所の外へ出る道は、**向こうで実パスに直してから**塞ぐ。
// campd は向こうのパスを解けないので、文字の上では通ってしまう。
func TestASymlinkOutOfTheAllowedPlaceIsRefusedOverThere(t *testing.T) {
	r := newRemoteRig(t, remoteClaudeBody)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(r.root, "escape")); err != nil {
		t.Fatal(err)
	}
	rec := r.start(t, filepath.Join(r.root, "escape"))
	end := r.waitState(t, rec.ID, StateExited)
	if end.EndCause != EndStartFailed || !strings.Contains(end.ExitReason, "許した場所の外") {
		t.Fatalf("外へ出たのに起きた: %q / %q", end.EndCause, end.ExitReason)
	}
	if r.ran() {
		t.Fatal("許した場所の外で claude が走った")
	}
}

// **パスに何が入っていても、向こうのシェルに解釈されない。**
func TestHostilePathsAreNotInterpretedOverThere(t *testing.T) {
	r := newRemoteRig(t, remoteClaudeBody)
	name := `a'b "c" $(touch PWNED) ; touch PWNED2 ` + "`touch PWNED3`"
	cwd := filepath.Join(r.root, name)
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := r.start(t, cwd)
	got := r.waitState(t, rec.ID, StateIdle)
	defer killIfAlive(r.pidFrom(t, "claude.pid"), r.pidFrom(t, "grandchild.pid"))
	if got.Cwd != cwd {
		t.Errorf("降りた先が違う: %q", got.Cwd)
	}
	for _, p := range []string{"PWNED", "PWNED2", "PWNED3"} {
		for _, where := range []string{r.root, cwd, r.dir, "."} {
			if _, err := os.Stat(filepath.Join(where, p)); err == nil {
				t.Fatalf("パスの中身が実行された: %s/%s", where, p)
			}
		}
	}
	_ = r.s.Stop(rec.ID, StopTerminate)
	r.waitState(t, rec.ID, StateExited)
}

// 知らないホスト鍵では繋がない。**理由を人の言葉で出す。**
func TestAnUnknownHostKeyIsExplained(t *testing.T) {
	r := newRemoteRig(t, remoteClaudeBody)
	os.WriteFile(filepath.Join(r.dir, "hostkey"), nil, 0o600)
	rec := r.start(t, r.root)
	end := r.waitState(t, rec.ID, StateExited)
	if end.EndCause != EndStartFailed || !strings.Contains(end.ExitReason, "ホスト鍵") ||
		!strings.Contains(end.ExitReason, "ssh far") {
		t.Fatalf("ホスト鍵のことが言えていない: %q", end.ExitReason)
	}
}

// ---------------------------------------------------------------- 接続が切れる

// busyTurn は1ターン始め、向こうの子が stdin を読まなくなるまで待つ。
func (r *remoteRig) busyTurn(t *testing.T, id string) (kid, grand int) {
	t.Helper()
	r.waitState(t, id, StateIdle)
	kid, grand = r.pidFrom(t, "claude.pid"), r.pidFrom(t, "grandchild.pid")
	if err := r.s.Input(id, "時間のかかる仕事"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // 子が sleep に入る
	return kid, grand
}

// **接続が切れても、向こうの子は終わるとは限らない。** 工具を走らせている最中の子は
// stdin を読んでいないので生き残る（実測）。実行面が見に行って始末する。
func TestLosingTheConnectionStopsWhatIsLeftOverThere(t *testing.T) {
	r := newRemoteRig(t, busyClaudeBody)
	rec := r.start(t, r.root)
	kid, grand := r.busyTurn(t, rec.ID)
	defer killIfAlive(kid, grand)

	os.WriteFile(filepath.Join(r.dir, "drop"), nil, 0o600)
	end := r.waitState(t, rec.ID, StateExited)
	if end.EndCause != EndConnLost {
		t.Fatalf("SSH が切れたのに %q", end.EndCause)
	}
	if end.EndState != StateRunning {
		t.Errorf("切れる直前の状態が %q（動いている途中だった）", end.EndState)
	}
	if procAlive(kid) || procAlive(grand) {
		t.Fatalf("切れたあと向こうに残ったまま: 子=%v 孫=%v", procAlive(kid), procAlive(grand))
	}
}

// 見に行けないうちは「終わった」と書かない。**孤児にして、繋がり次第始末する。**
func TestAnUnreachableHostLeavesAnOrphanUntilItCanBeChecked(t *testing.T) {
	r := newRemoteRig(t, busyClaudeBody)
	rec := r.start(t, r.root)
	kid, grand := r.busyTurn(t, rec.ID)
	defer killIfAlive(kid, grand)

	os.WriteFile(filepath.Join(r.dir, "unreachable"), nil, 0o600)
	os.WriteFile(filepath.Join(r.dir, "drop"), nil, 0o600)
	orphan := r.waitState(t, rec.ID, StateOrphaned)
	if orphan.EndCause != EndConnLost {
		t.Errorf("孤児にした理由の控えが %q", orphan.EndCause)
	}
	if !procAlive(kid) {
		t.Fatal("このテストの前提が崩れた（向こうの子がもう居ない）")
	}

	// 繋がるようになった。Tick が見に行かせる。
	os.Remove(filepath.Join(r.dir, "unreachable"))
	r.s.Tick()
	end := r.waitState(t, rec.ID, StateExited)
	if end.EndCause != EndConnLost {
		t.Errorf("始末したあと、切れたことが残っていない: %q", end.EndCause)
	}
	if procAlive(kid) || procAlive(grand) {
		t.Fatal("繋がったのに始末していない")
	}
}

// campd が落ちている間に手元の ssh が消えても、**向こうはまだ分からない**。
func TestAfterARestartARemoteRowWithoutItsSSHIsAnOrphanNotAGhost(t *testing.T) {
	db := newDB(t)
	mustInsert(t, db, "r1", StateIdle, 999999, 1, BootID())
	if _, err := db.Exec(`update runtime_sessions set host='far', remote_pid=4242 where id='r1'`); err != nil {
		t.Fatal(err)
	}
	s := New(db)
	ghosts, orphans, _, err := s.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	if ghosts != 0 || orphans != 1 || state(t, db, "r1") != StateOrphaned {
		t.Fatalf("向こうを確かめずに閉じた: ghosts=%d orphans=%d state=%s", ghosts, orphans, state(t, db, "r1"))
	}
}

// ---------------------------------------------------------------- 許可

// 向こうの場所は**ホストごと**。あるホストで許した場所は、他のホストでも手元でも通らない。
func TestRemotePlacesAreAllowedPerHost(t *testing.T) {
	db := newDB(t)
	ImportSSH(db, []SSHHost{{Alias: "a"}, {Alias: "b"}})
	pin := Resolved{HostName: "h", User: "u", Port: "22", HostKeys: []string{"SHA256:x"}}
	if err := AllowDestination(db, "a", pin); err != nil {
		t.Fatal(err)
	}
	AllowDestination(db, "b", pin)
	if _, err := AddRemoteAllowed(db, "a", "/srv/work", "", "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := checkRemote(db, AgentClaude, "a", "/srv/work/x"); err != nil {
		t.Fatalf("許した場所の下が通らない: %v", err)
	}
	if _, _, _, err := checkRemote(db, AgentClaude, "a", "/srv/workshop"); err == nil {
		t.Fatal("/srv/work を許しただけで /srv/workshop が通った")
	}
	if _, _, _, err := checkRemote(db, AgentClaude, "b", "/srv/work"); err == nil {
		t.Fatal("a で許した場所が b でも通った")
	}
	if list, _ := ListAllowed(db); len(list) != 0 {
		t.Fatalf("向こうの場所が手元の許可リストに入った: %+v", list)
	}
}

// 許していない先・行き先を固定していない先には起こせない。
func TestOnlyAllowedAndPinnedDestinationsCanStart(t *testing.T) {
	db := newDB(t)
	ImportSSH(db, []SSHHost{{Alias: "a"}})
	AddRemoteAllowed(db, "a", "/srv/work", "", "test")
	if _, _, _, err := checkRemote(db, AgentClaude, "a", "/srv/work"); err == nil || !strings.Contains(err.Error(), "許していない") {
		t.Fatalf("許していない先に起こせる: %v", err)
	}
	// 9/11 より前の許し方（固定なし）
	SetDestinationAllowed(db, "a", true)
	if _, _, _, err := checkRemote(db, AgentClaude, "a", "/srv/work"); err == nil || !strings.Contains(err.Error(), "許し直す") {
		t.Fatalf("固定していない先に起こせる: %v", err)
	}
	// 鍵の無い固定は固定ではない。許すこともできないし、台帳にあっても起こさない。
	if err := AllowDestination(db, "a", Resolved{HostName: "h"}); err == nil || !strings.Contains(err.Error(), "known_hosts") {
		t.Fatalf("ホスト鍵の無い先を許せた: %v", err)
	}
	db.Exec(`update ssh_hosts set allowed=1, pinned='{"hostname":"h","user":"u","port":"22"}' where alias='a'`)
	if _, _, _, err := checkRemote(db, AgentClaude, "a", "/srv/work"); err == nil || !strings.Contains(err.Error(), "許し直す") {
		t.Fatalf("鍵の無い固定で起こせる: %v", err)
	}
	if err := AllowDestination(db, "a", Resolved{HostName: "h", HostKeys: []string{"SHA256:x"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := checkRemote(db, AgentClaude, "a", "/srv/work"); err != nil {
		t.Fatal(err)
	}
	// 外すと固定も消える。許し直すときは、そのときの行き先で固定し直す。
	SetDestinationAllowed(db, "a", false)
	d, _ := getDestination(db, "a")
	if d.Allowed || d.Pinned != nil {
		t.Fatalf("外したのに固定が残っている: %+v", d)
	}
	if _, _, _, err := checkRemote(db, AgentClaude, "nope", "/srv/work"); err == nil {
		t.Fatal("台帳に無い先に起こせる")
	}
}

// ---------------------------------------------------------------- 文字の上で決めること

func TestAliasesThatLookLikeOptionsAreRefused(t *testing.T) {
	for _, bad := range []string{"-oProxyCommand=touch x", "", "a b", "a;b", "-", "a\nb"} {
		if validAlias(bad) == nil {
			t.Errorf("%q を通した", bad)
		}
	}
	for _, ok := range []string{"far", "github.com", "192.168.1.101", "alpine-vm", "user@host"} {
		if err := validAlias(ok); err != nil {
			t.Errorf("%q を通さない: %v", ok, err)
		}
	}
}

func TestRemotePathsAreCheckedByTheirLetters(t *testing.T) {
	for _, bad := range []string{"", "relative", "/", "/a/../b", "/a/..", "/a\nb", "/a\tb", "//"} {
		if _, err := cleanRemotePath(bad); err == nil {
			t.Errorf("%q を通した", bad)
		}
	}
	if c, err := cleanRemotePath("/srv//work/./x/"); err != nil || c != "/srv/work/x" {
		t.Errorf("正規化が違う: %q %v", c, err)
	}
	if validAgentPath("/opt/$HOME/claude") == nil {
		t.Error("$ の入った claude の場所を通した（systemd-run が展開する）")
	}
}

func TestSSHGIsReadAndDifferencesAreSpelledOut(t *testing.T) {
	r, files := parseSSHG([]byte("user me\nhostname 10.0.0.1\nport 2222\nproxyjump none\nproxycommand none\n" +
		"forwardagent no\nuserknownhostsfile /a /b\nglobalknownhostsfile /c\n"))
	want := Resolved{HostName: "10.0.0.1", User: "me", Port: "2222"}
	if !reflect.DeepEqual(r, want) {
		t.Fatalf("読み違えた: %+v", r)
	}
	if strings.Join(files, " ") != "/a /b /c" {
		t.Fatalf("known_hosts の在り処を読み違えた: %v", files)
	}
	k1, k2 := want, want
	k1.HostKeys, k2.HostKeys = []string{"SHA256:a"}, []string{"SHA256:b"}
	if d := k1.Diff(k2); !strings.Contains(d, "ホスト鍵") {
		t.Errorf("鍵の違いが書かれていない: %q", d)
	}
	if d := want.Diff(r); d != "" {
		t.Errorf("同じなのに違いがある: %s", d)
	}
	moved := r
	moved.HostName, moved.ProxyJump = "10.0.0.2", "bastion"
	d := want.Diff(moved)
	if !strings.Contains(d, "10.0.0.2") || !strings.Contains(d, "bastion") {
		t.Errorf("違いが書かれていない: %s", d)
	}
}

// **起動時刻の違う pid は撃たない。** 向こうで pid が使い回されていれば、それは別人。
func TestAReusedPIDOverThereIsNotKilled(t *testing.T) {
	r := newRemoteRig(t, remoteClaudeBody)
	cmd := exec.Command("setsid", "sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	go cmd.Wait()
	defer killIfAlive(pid)
	var st uint64
	waitFor(t, 3*time.Second, func() bool {
		v, err := Starttime(pid)
		st = v
		return err == nil && sessionOf(pid) == pid
	})

	res, why := r.a.reapRemote(&RemoteOwner{Host: "far", PID: pid, Started: st + 1, BootID: BootID()})
	if res != RemoteGone || !procAlive(pid) {
		t.Fatalf("起動時刻の違う pid を止めた: %s（%s） alive=%v", res, why, procAlive(pid))
	}
	res, why = r.a.reapRemote(&RemoteOwner{Host: "far", PID: pid, Started: st, BootID: BootID()})
	if res != RemoteKilled {
		t.Fatalf("起動時刻の合う pid を止めない: %s（%s）", res, why)
	}
	waitFor(t, 5*time.Second, func() bool { return !procAlive(pid) })
}

// Camp の ssh は**鍵を受け入れず、相乗りせず、手元でコマンドを走らせない**。config より先に効く。
// **転送は config のまま**にする（D-030、本人の決定 2026-09-12）——`ForwardAgent` を有効にしている
// 接続先では、向こうから手元の鍵を使える（`ssh <host>` してから CLI を起こすのと同じ）。
func TestCampsSSHPinsHostKeysButLeavesForwardingToTheConfig(t *testing.T) {
	a := NewAgent("/nonexistent", "/nonexistent")
	a.SSHFile = ""
	args := a.sshArgs("--", "far", "cmd")
	joined := strings.Join(args, " ")
	for _, must := range []string{
		"BatchMode=yes", "StrictHostKeyChecking=yes", "UpdateHostKeys=no", "KnownHostsCommand=none",
		"ControlMaster=no", "ControlPath=none", "PermitLocalCommand=no",
		"-- far cmd",
	} {
		if !strings.Contains(joined, must) {
			t.Errorf("%s が無い: %s", must, joined)
		}
	}
	// **転送を Camp が決め打ちしない。** 足し直すと、本人の config が効かなくなる。
	for _, never := range []string{"ForwardAgent=", "ForwardX11=", "ClearAllForwardings=", " -a", " -x"} {
		if strings.Contains(joined, never) {
			t.Errorf("転送を決め打ちしている（%s）: %s", never, joined)
		}
	}
	for _, a := range args {
		if a == "-F" {
			t.Error("既定の config なのに -F を付けた（/etc/ssh/ssh_config が読まれなくなる）")
		}
	}
}

// ---------------------------------------------------------------- outer gate（codex）で足したもの

// **信じる鍵が変わったら繋がない。** hostname・user・port が同じでも、
// UserKnownHostsFile や HostKeyAlias を書き換えれば別の鍵を信じさせられる。
func TestAChangedHostKeyIsNotConnected(t *testing.T) {
	r := newRemoteRig(t, remoteClaudeBody)
	os.WriteFile(r.kh, []byte(hostKeyLine(t, r.dir, "far.example")), 0o600)
	rec := r.start(t, r.root)
	end := r.waitState(t, rec.ID, StateExited)
	if end.EndCause != EndStartFailed || !strings.Contains(end.ExitReason, "ホスト鍵") {
		t.Fatalf("鍵が変わったのに止まらない: %q / %q", end.EndCause, end.ExitReason)
	}
	if strings.Contains(r.sshLog(), "run far") {
		t.Fatalf("照らす前に繋いでいる:\n%s", r.sshLog())
	}
}

// known_hosts に鍵の無い先は、許すときに断る（どうせ繋がらない）。
func TestADestinationWithoutAKnownHostKeyCannotBeAllowed(t *testing.T) {
	r := newRemoteRig(t, remoteClaudeBody)
	os.WriteFile(r.kh, nil, 0o600)
	pin, err := r.a.resolve("far")
	if err != nil {
		t.Fatal(err)
	}
	if len(pin.HostKeys) != 0 {
		t.Fatalf("空の known_hosts から鍵が出た: %v", pin.HostKeys)
	}
	if err := AllowDestination(r.db, "far", pin); err == nil || !strings.Contains(err.Error(), "known_hosts") {
		t.Fatalf("鍵の無い先を許せた: %v", err)
	}
}

// 名乗りに似た行を先に吐かれても、**合言葉の合わないものは採らない**。
// 採ると、あとで始末するときに名乗られた別の pid を撃つ。
func TestALineThatLooksLikeTheHeaderIsNotBelieved(t *testing.T) {
	r := newRemoteRig(t, remoteClaudeBody)
	os.WriteFile(filepath.Join(r.dir, "forge"), nil, 0o600)
	rec := r.start(t, r.root)
	got := r.waitState(t, rec.ID, StateIdle)
	kid, grand := r.pidFrom(t, "claude.pid"), r.pidFrom(t, "grandchild.pid")
	defer killIfAlive(kid, grand)
	if got.RemotePID == 1 || sessionOf(kid) != got.RemotePID {
		t.Fatalf("偽の名乗りを信じた: remote_pid=%d", got.RemotePID)
	}
	_ = r.s.Stop(rec.ID, StopTerminate)
	r.waitState(t, rec.ID, StateExited)
}

// **照らし終えるまで scope に触らない。** 起動時刻が違う・再起動を跨いだなら、
// systemctl を呼ばずに「もう居ない」と返す。
func TestTheReaperChecksWhoItIsBeforeStoppingAUnit(t *testing.T) {
	r := newRemoteRig(t, remoteClaudeBody)
	bin := filepath.Join(r.dir, "bin")
	os.MkdirAll(bin, 0o755)
	log := filepath.Join(r.dir, "systemctl.log")
	os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\necho \"$@\" >> "+log+"\n"), 0o755)
	called := func() bool { _, err := os.Stat(log); return err == nil }

	cmd := exec.Command("setsid", "sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	go cmd.Wait()
	defer killIfAlive(pid)
	var st uint64
	waitFor(t, 3*time.Second, func() bool {
		v, err := Starttime(pid)
		st = v
		return err == nil && sessionOf(pid) == pid
	})
	unit := scopeName(newID())

	res, why := r.a.reapRemote(&RemoteOwner{Host: "far", PID: pid, Started: st + 1, BootID: BootID(), Scope: unit})
	if res != RemoteGone || called() {
		t.Fatalf("起動時刻が違うのに scope を止めた: %s（%s） systemctl=%v", res, why, called())
	}
	res, why = r.a.reapRemote(&RemoteOwner{Host: "far", PID: pid, Started: st,
		BootID: "00000000-0000-0000-0000-000000000000", Scope: unit})
	if res != RemoteGone || called() {
		t.Fatalf("再起動を跨いだのに scope を止めた: %s（%s） systemctl=%v", res, why, called())
	}
	res, why = r.a.reapRemote(&RemoteOwner{Host: "far", PID: pid, Started: st, BootID: BootID(), Scope: unit})
	if res != RemoteKilled || !called() {
		t.Fatalf("本人なのに止めない: %s（%s） systemctl=%v", res, why, called())
	}
}

// **「確かめようがない」を「終わった」と書かない。** 孤児のまま残す。
func TestAnUnconfirmableRemoteEndStaysOrphaned(t *testing.T) {
	db := newDB(t)
	now := time.Now().UTC().Format(time.RFC3339)
	if err := insert(db, Record{ID: "r1", Cwd: "/srv/x", State: StateOrphaned, RequestedBy: "test",
		CreatedAt: now, UpdatedAt: now, Host: "far"}); err != nil {
		t.Fatal(err)
	}
	db.Exec(`update runtime_sessions set pid=999999, proc_started=1, boot_id=?,
		remote_pid=4242, remote_started=7 where id='r1'`, BootID())
	s := New(db)
	s.dispatchForTest(Msg{T: MsgReaped, Session: "r1", RemoteEnd: RemoteUnsupported, Reason: "確かめようがない"})
	if got := state(t, db, "r1"); got != StateOrphaned {
		t.Fatalf("確かめようがないのに %s にした", got)
	}
	s.dispatchForTest(Msg{T: MsgReaped, Session: "r1", RemoteEnd: RemoteGone, Reason: "もう居なかった"})
	if got := state(t, db, "r1"); got != StateExited {
		t.Fatalf("居なかったのに %s のまま", got)
	}
}

// started が届く前に campd が落ちても、**実行面の名乗りから向こうの身元を取り戻す**。
// 台帳が既に知っているなら、違うものは採らない。
func TestAReadoptedRemoteSessionKeepsWhoItIsOverThere(t *testing.T) {
	db := newDB(t)
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	go cmd.Wait()
	defer killIfAlive(pid)
	var st uint64
	waitFor(t, 3*time.Second, func() bool { v, err := Starttime(pid); st = v; return err == nil })

	now := time.Now().UTC().Format(time.RFC3339)
	for _, id := range []string{"r1", "r2"} {
		if err := insert(db, Record{ID: id, Cwd: "/srv/x", State: StateIdle, RequestedBy: "test",
			CreatedAt: now, UpdatedAt: now, Host: "far"}); err != nil {
			t.Fatal(err)
		}
	}
	// r2 は向こうの pid を台帳が知っている（started は届いていた）。
	db.Exec(`update runtime_sessions set remote_pid=1111, remote_started=9 where id='r2'`)

	s := New(db)
	c := &Control{s: s, allowUID: -1}
	ro := &RemoteOwner{Host: "far", PID: 4242, Started: 7, BootID: "b", Root: "/srv", Cwd: "/srv/x"}
	held := func(id string) Held {
		return Held{ID: id, Token: "t-" + id, PID: pid, Started: st, BootID: BootID(), State: StateIdle, RemoteOwner: ro}
	}
	c.readopt([]Held{held("r1"), held("r2")})

	got, _ := get(db, "r1")
	if got.RemotePID != 4242 || got.RemoteStarted != 7 {
		t.Fatalf("向こうの身元を取り戻していない: %+v", got)
	}
	if liveState(s, "r2") != "" {
		t.Fatal("台帳の向こうの pid と違うものを名乗ったのに引き取った")
	}
	if got, _ := get(db, "r2"); got.RemotePID != 1111 {
		t.Fatalf("台帳の向こうの pid を書き換えた: %d", got.RemotePID)
	}
}

func TestTheWrapperIsPlainSh(t *testing.T) {
	// どのログインシェルから呼ばれても sh で読む。構文だけ確かめる。
	for name, script := range map[string]string{"wrapper": wrapperScript, "reap": reapScript} {
		out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput()
		if err != nil {
			t.Errorf("%s が sh で読めない: %v\n%s", name, err, out)
		}
	}
}
