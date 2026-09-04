package httpapi

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoomA-0750/camp/internal/session"
	"github.com/MoomA-0750/camp/internal/store"
)

// runtimeServer は supervisor と実行面まで繋いだサーバーを立てる。
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
	if err := ag.Dial("test"); err != nil {
		t.Fatal(err)
	}
	go ag.Run()

	s, err := New(db, Options{Sessions: sup})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

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

	work := t.TempDir()
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
	work := t.TempDir()
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

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
