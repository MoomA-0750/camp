package httpapi

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoomA-0750/camp/internal/session"
	"github.com/MoomA-0750/camp/internal/store"
)

// runtimeServer は supervisor と実行面まで繋いだサーバーを立てる。
var dbFor = map[*httptest.Server]*store.DB{}

// dbOf はそのサーバーが使っている DB。テストから直に触るため。
func dbOf(t *testing.T, ts *httptest.Server) *store.DB {
	t.Helper()
	db := dbFor[ts]
	if db == nil {
		t.Fatal("この httptest.Server の DB を知らない")
	}
	return db
}

func runtimeServer(t *testing.T) (*httptest.Server, *http.Client, *session.Supervisor) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "camp.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := SetPassword(db, "correct horse battery"); err != nil {
		t.Fatal(err)
	}

	sup := session.New(db)
	sock := filepath.Join(t.TempDir(), "a.sock")
	cl, err := sup.Listen(sock, "", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	go cl.Serve()
	t.Cleanup(func() { cl.Close() })

	// 1ターンで少しだけ吐く子。
	fake := filepath.Join(t.TempDir(), "fake-claude")
	body := `#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"fake-1"}'
while IFS= read -r line; do
  case "$line" in
    *'"type":"user"'*)
      i=0
      while [ $i -lt 5 ]; do echo '{"type":"assistant","session_id":"fake-1"}'; i=$((i+1)); done
      echo '{"type":"result","subtype":"success","session_id":"fake-1"}' ;;
  esac
done
`
	if err := os.WriteFile(fake, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	ag := session.NewAgent(sock, fake)
	ag.Scope = false
	ag.LogDir = t.TempDir()
	// **本物の ~/.ssh/config を読まない。** 許すときの `ssh -G` は偽物に答えさせる。
	// 鍵は本物を作り、使い捨ての known_hosts に置く（許すときに指紋を固定するので）。
	khDir := t.TempDir()
	hk := filepath.Join(khDir, "hostkey")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "", "-f", hk).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v %s", err, out)
	}
	pub, err := os.ReadFile(hk + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	pf := strings.Fields(string(pub))
	kh := filepath.Join(khDir, "known_hosts")
	if err := os.WriteFile(kh, []byte("tower.example "+pf[0]+" "+pf[1]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeSSH := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(fakeSSH, []byte(`#!/bin/sh
for a; do last=$a; done
printf 'user tester\nhostname %s.example\nport 22\nuserknownhostsfile `+kh+`\n' "$last"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	ag.SSH = fakeSSH
	ag.SSHConfig = filepath.Join(t.TempDir(), "no-config")
	if err := ag.Dial("test"); err != nil {
		t.Fatal(err)
	}
	go ag.Run()

	s, err := New(db, Options{Sessions: sup})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	dbFor[ts] = db
	t.Cleanup(func() { delete(dbFor, ts); ts.Close() })

	deadline := time.Now().Add(3 * time.Second)
	for !sup.AgentConnected() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !sup.AgentConnected() {
		t.Fatal("実行面が繋がらない")
	}
	return ts, loggedIn(t, ts), sup
}

// 画面から起こして、SSE で追いつける。
//
// **WebSocket ではなく SSE**（2026-09-04 決定。runtime.go の頭に理由）。
func TestTheScreenCanStartASessionAndFollowItWithSSE(t *testing.T) {
	ts, c, _ := runtimeServer(t)

	work := allowDir(t, c, ts)
	res, err := c.Post(ts.URL+"/api/runtime", "application/json",
		strings.NewReader(`{"cwd":`+jsonString(work)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("起こせない: %s", res.Status)
	}
	var rec struct {
		ID string `json:"id"`
	}
	json.NewDecoder(res.Body).Decode(&rec)
	if rec.ID == "" {
		t.Fatal("id が返らない")
	}

	// 走り始めるまで待つ。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := c.Get(ts.URL + "/api/runtime")
		if err == nil {
			var body struct {
				Sessions []struct {
					ID, State string
				}
			}
			json.NewDecoder(r.Body).Decode(&body)
			r.Body.Close()
			for _, s := range body.Sessions {
				if s.ID == rec.ID && s.State == "idle" {
					deadline = time.Time{}
				}
			}
		}
		if deadline.IsZero() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 流し始めてから入力する。**繋いでいなくても子は走るが、繋げば届く。**
	req, _ := http.NewRequest("GET", ts.URL+"/api/runtime/"+rec.ID+"/stream", nil)
	// **自分で付ける。** Go の transport が勝手に付けた場合は展開まで
	// してしまい、畳まれていたことが見えなくなる。
	req.Header.Set("Accept-Encoding", "gzip")
	sr, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer sr.Body.Close()
	if ct := sr.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("SSE ではない: %s", ct)
	}
	if enc := sr.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("流す応答が畳まれている（%s）。最初のイベントが届かなくなる", enc)
	}

	if r, err := c.Post(ts.URL+"/api/runtime/"+rec.ID+"/input", "application/json",
		strings.NewReader(`{"text":"go"}`)); err != nil {
		t.Fatal(err)
	} else {
		r.Body.Close()
	}

	// id: と data: が来る。
	done := make(chan int, 1)
	go func() {
		n := 0
		sc := bufio.NewScanner(sr.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "id: ") {
				n++
				if n >= 5 {
					done <- n
					return
				}
			}
		}
		done <- n
	}()
	select {
	case n := <-done:
		if n < 5 {
			t.Fatalf("届いたイベントが %d 件しかない", n)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("SSE でイベントが届かない")
	}
}

// 落としてあるぶんはカーソルで引ける。
func TestTheLogIsReadableByCursor(t *testing.T) {
	ts, c, sup := runtimeServer(t)
	work := allowDir(t, c, ts)
	rec, err := sup.Start("test", work)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		idle := false
		for _, r := range sup.Live() {
			if r.ID == rec.ID && r.State == session.StateIdle {
				idle = true
			}
		}
		if idle {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := sup.Input(rec.ID, "go"); err != nil {
		t.Fatal(err)
	}
	var first, second struct {
		Lines []struct {
			Seq  int64  `json:"seq"`
			Kind string `json:"kind"`
		} `json:"lines"`
		Newest int64 `json:"newest"`
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r, err := c.Get(ts.URL + "/api/runtime/" + rec.ID + "/log?since=0&limit=100")
		if err != nil {
			t.Fatal(err)
		}
		json.NewDecoder(r.Body).Decode(&first)
		r.Body.Close()
		if len(first.Lines) >= 6 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(first.Lines) < 6 {
		t.Fatalf("落ちているのが %d 件しかない", len(first.Lines))
	}
	cursor := first.Lines[2].Seq
	r, err := c.Get(ts.URL + "/api/runtime/" + rec.ID + "/log?since=" +
		jsonInt(cursor) + "&limit=100")
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(r.Body).Decode(&second)
	r.Body.Close()
	if len(second.Lines) == 0 || second.Lines[0].Seq != cursor+1 {
		t.Fatalf("カーソルの続きから返っていない: %+v", second.Lines)
	}
}

// allowDir は使い捨てのディレクトリを1つ、API 越しに許可リストへ入れる。
func allowDir(t *testing.T, c *http.Client, ts *httptest.Server) string {
	t.Helper()
	dir := t.TempDir()
	r, err := c.Post(ts.URL+"/api/allowlist", "application/json",
		strings.NewReader(`{"path":`+jsonString(dir)+`,"password":"correct horse battery"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("許可リストに入れられない: %s", r.Status)
	}
	return dir
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// **許可リストの変更にはパスワードが要る。** Cookie だけでは広げられない。
func TestChangingTheAllowlistNeedsThePasswordAgain(t *testing.T) {
	ts, c, _ := runtimeServer(t)
	dir := t.TempDir()

	// Cookie はある。パスワードが無い／違う。
	for _, body := range []string{
		`{"path":` + jsonString(dir) + `}`,
		`{"path":` + jsonString(dir) + `,"password":"ちがう"}`,
	} {
		r, err := c.Post(ts.URL+"/api/allowlist", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("パスワード無しで通った（%s）: %s", body, r.Status)
		}
	}

	// 正しいパスワードなら通る。
	r, err := c.Post(ts.URL+"/api/allowlist", "application/json",
		strings.NewReader(`{"path":`+jsonString(dir)+`,"password":"correct horse battery"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("正しいパスワードで通らない: %s", r.Status)
	}

	// 読むだけならログイン済みで足りる。
	g, err := c.Get(ts.URL + "/api/allowlist")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Body.Close()
	var list []struct {
		Path string `json:"path"`
	}
	json.NewDecoder(g.Body).Decode(&list)
	if len(list) != 1 {
		t.Fatalf("足したはずのものが見えない: %+v", list)
	}
}

// 許可リストの変更は監査ログに残る。**失敗も残る。**
func TestAllowlistChangesAreRecorded(t *testing.T) {
	ts, c, _ := runtimeServer(t)
	dir := t.TempDir()
	post := func(body string) {
		r, err := c.Post(ts.URL+"/api/allowlist", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
	}
	post(`{"path":` + jsonString(dir) + `,"password":"ちがう"}`)
	post(`{"path":` + jsonString(dir) + `,"password":"correct horse battery"}`)

	g, err := c.Get(ts.URL + "/api/audit?limit=50")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Body.Close()
	b, _ := io.ReadAll(g.Body)
	for _, want := range []string{"allowlist.reauth", "allowlist.add"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("%s が監査ログに無い", want)
		}
	}
}

// 接続先の許可フラグにもパスワードが要る。
func TestAllowingAnSSHDestinationNeedsThePasswordAgain(t *testing.T) {
	ts, c, _ := runtimeServer(t)
	if _, _, err := session.ImportSSH(dbOf(t, ts), []session.SSHHost{{Alias: "tower"}}); err != nil {
		t.Fatal(err)
	}

	r, err := c.Post(ts.URL+"/api/ssh/tower/allow", "application/json",
		strings.NewReader(`{"allowed":true}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("パスワード無しで許可できた: %s", r.Status)
	}

	r, err = c.Post(ts.URL+"/api/ssh/tower/allow", "application/json",
		strings.NewReader(`{"allowed":true,"password":"correct horse battery"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("正しいパスワードで通らない: %s", r.Status)
	}

	g, err := c.Get(ts.URL + "/api/ssh")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Body.Close()
	var list []struct {
		Alias   string `json:"alias"`
		Allowed bool   `json:"allowed"`
		Pinned  *struct {
			HostName string   `json:"hostname"`
			HostKeys []string `json:"hostkeys"`
		} `json:"pinned"`
	}
	json.NewDecoder(g.Body).Decode(&list)
	if len(list) != 1 || !list[0].Allowed {
		t.Fatalf("許可が反映されていない: %+v", list)
	}
	// **許したときの行き先と、信じるホスト鍵が固定される。**
	if list[0].Pinned == nil || list[0].Pinned.HostName != "tower.example" || len(list[0].Pinned.HostKeys) != 1 {
		t.Fatalf("行き先が固定されていない: %+v", list[0].Pinned)
	}
}

// 向こうへ起こす口も、許可リストの外は断る。**文字の上で決められることは campd が決める。**
func TestStartingOnAnotherHostIsCheckedByCampd(t *testing.T) {
	ts, c, _ := runtimeServer(t)
	db := dbOf(t, ts)
	if _, _, err := session.ImportSSH(db, []session.SSHHost{{Alias: "far"}}); err != nil {
		t.Fatal(err)
	}
	post := func(body string) (int, string) {
		t.Helper()
		r, err := c.Post(ts.URL+"/api/runtime", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	if code, msg := post(`{"host":"far","cwd":"/srv/work"}`); code != 400 || !strings.Contains(msg, "許していない") {
		t.Fatalf("許していない先に起こせた: %d %s", code, msg)
	}
	if err := session.AllowDestination(db, "far", session.Resolved{HostName: "far.example",
		HostKeys: []string{"SHA256:x"}}); err != nil {
		t.Fatal(err)
	}
	if code, msg := post(`{"host":"far","cwd":"/srv/work"}`); code != 400 || !strings.Contains(msg, "許した場所が無い") {
		t.Fatalf("場所を許していないのに起こせた: %d %s", code, msg)
	}
	if code, msg := post(`{"host":"-oProxyCommand=x","cwd":"/srv/work"}`); code != 400 {
		t.Fatalf("名前でないものを通した: %d %s", code, msg)
	}

	// 場所を足すのにも再認証が要る。
	r, err := c.Post(ts.URL+"/api/allowlist", "application/json",
		strings.NewReader(`{"host":"far","path":"/srv/work"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("パスワード無しで向こうの場所を足せた: %s", r.Status)
	}
	r, err = c.Post(ts.URL+"/api/allowlist", "application/json",
		strings.NewReader(`{"host":"far","path":"/srv/work","password":"correct horse battery"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("向こうの場所を足せない: %s", r.Status)
	}
	g, err := c.Get(ts.URL + "/api/allowlist")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Body.Close()
	var rows []struct {
		Host string `json:"host"`
		Path string `json:"path"`
	}
	json.NewDecoder(g.Body).Decode(&rows)
	found := false
	for _, a := range rows {
		if a.Host == "far" && a.Path == "/srv/work" {
			found = true
		}
	}
	if !found {
		t.Fatalf("足した向こうの場所が一覧に出ない: %+v", rows)
	}
}

// 開きっぱなしの流れに上限がある。**タブを開いたまま忘れても積み上がらない。**
func TestTooManyOpenStreamsAreRefused(t *testing.T) {
	ts, c, sup := runtimeServer(t)
	work := allowDir(t, c, ts)
	rec, err := sup.Start("test", work)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ok := false
		for _, r := range sup.Live() {
			if r.ID == rec.ID && r.State == session.StateIdle {
				ok = true
			}
		}
		if ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	var bodies []io.Closer
	defer func() {
		for _, b := range bodies {
			b.Close()
		}
	}()
	refused := 0
	for i := 0; i < 24; i++ {
		req, _ := http.NewRequest("GET", ts.URL+"/api/runtime/"+rec.ID+"/stream", nil)
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode == http.StatusServiceUnavailable {
			refused++
			res.Body.Close()
			continue
		}
		bodies = append(bodies, res.Body)
	}
	if refused == 0 {
		t.Fatal("24本開いても1本も断られない。**上限が効いていない**")
	}
	t.Logf("24本のうち %d 本を断った", refused)
}

// **空の一覧は `[]` で返す。`null` では返さない。**
//
// Go の nil スライスは JSON で `null` になる。2026-09-04、実ブラウザで開いて
// 初めて分かった——画面が `sessions.length` で落ちて真っ白になった。
// テストは JSON の中身しか見ていなかったので出なかった。
func TestEmptyListsComeBackAsArraysNotNull(t *testing.T) {
	ts, c, _ := runtimeServer(t)
	for _, path := range []string{
		"/api/runtime", "/api/runtime/ended", "/api/allowlist", "/api/ssh",
	} {
		r, err := c.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if strings.Contains(string(b), "null") {
			t.Errorf("%s が null を含む: %s", path, b)
		}
	}
}

// 終わったものは走っているものの一覧から外れ、別の口から終わり方つきで引ける。
// 1本は、どちらの一覧に載っていなくても直に開ける（2026-09-11）。
func TestEndedSessionsHaveTheirOwnListAndCanBeOpened(t *testing.T) {
	ts, c, sup := runtimeServer(t)
	db := dbOf(t, ts)
	work := allowDir(t, c, ts)
	rec, err := sup.Start("test", work)
	if err != nil {
		t.Fatal(err)
	}
	until := func(ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatal("待っても起きなかった")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	stateOf := func() string {
		r, err := session.Get(db, rec.ID)
		if err != nil {
			t.Fatal(err)
		}
		return r.State
	}
	until(func() bool { return stateOf() == session.StateIdle })
	if err := sup.Stop(rec.ID, session.StopTerminate); err != nil {
		t.Fatal(err)
	}
	until(func() bool { return stateOf() == session.StateExited })

	get := func(path string, want int, v any) {
		t.Helper()
		r, err := c.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		if r.StatusCode != want {
			t.Fatalf("%s が %d（%d のはず）", path, r.StatusCode, want)
		}
		if v != nil {
			json.NewDecoder(r.Body).Decode(v)
		}
	}

	var live struct {
		Sessions []struct{ ID string } `json:"sessions"`
	}
	get("/api/runtime", 200, &live)
	for _, s := range live.Sessions {
		if s.ID == rec.ID {
			t.Fatal("終わったものが「走っているもの」に残っている")
		}
	}

	var ended struct {
		Sessions []struct {
			ID       string `json:"id"`
			EndCause string `json:"end_cause"`
			EndState string `json:"end_state"`
		} `json:"sessions"`
		Counts map[string]int `json:"counts"`
	}
	get("/api/runtime/ended", 200, &ended)
	if len(ended.Sessions) != 1 || ended.Sessions[0].ID != rec.ID ||
		ended.Sessions[0].EndCause != session.EndUserStop || ended.Sessions[0].EndState != session.StateIdle {
		t.Fatalf("終わったものの一覧が違う: %+v", ended.Sessions)
	}
	if ended.Counts["all"] != 1 || ended.Counts[session.EndUserStop] != 1 {
		t.Fatalf("件数が違う: %v", ended.Counts)
	}

	var one struct {
		ID       string `json:"id"`
		EndCause string `json:"end_cause"`
	}
	get("/api/runtime/"+rec.ID, 200, &one)
	if one.ID != rec.ID || one.EndCause != session.EndUserStop {
		t.Fatalf("1本を開けない: %+v", one)
	}

	get("/api/runtime/無い", 404, nil)
	get("/api/runtime/ended?kind=bogus", 400, nil)
}
