package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/MoomA-0750/camp/internal/session"
	"github.com/MoomA-0750/camp/internal/store"
)

// 向こうのホストの記録を読むかどうかの口（M47）。
//
// **既定で読みに行かない。** 起こしてよい接続先と、記録を読んでよいかは別物なので、
// 許可とは別に選ぶ（本人の決定 2026-09-12）。
//
// **読むようにするときは再認証が要る**——Cookie を盗られただけで、新しい場所を
// 読みに行かせない。**止めるときは要らない**（安全側へ倒すのは軽くてよい）。
func TestTurningRecordReadingOnNeedsThePasswordAgain(t *testing.T) {
	ts, db := newServer(t)
	c := loggedIn(t, ts)
	mustHost(t, db, "rp")

	// 既定では行が無い＝読みに行かない。
	if on := recordOn(t, db, "rp", "claude"); on {
		t.Fatal("何もしていないのに記録を読む設定になっている")
	}

	// パスワードが違えば断られ、**行は増えない**。
	res := post(t, c, ts.URL+"/api/ssh/rp/record",
		`{"agent":"claude","enabled":true,"password":"ちがう"}`)
	if res != http.StatusUnauthorized && res != http.StatusForbidden {
		t.Fatalf("違うパスワードで読む設定にできた: %d", res)
	}
	if on := recordOn(t, db, "rp", "claude"); on {
		t.Fatal("断られたのに台帳が変わっている")
	}

	// 正しいパスワードなら通る。
	if res := post(t, c, ts.URL+"/api/ssh/rp/record",
		`{"agent":"claude","enabled":true,"password":"correct horse battery"}`); res != http.StatusOK {
		t.Fatalf("正しいパスワードなのに読む設定にできない: %d", res)
	}
	if on := recordOn(t, db, "rp", "claude"); !on {
		t.Fatal("許したのに台帳が変わっていない")
	}

	// **止めるのはパスワード無しで通る。**
	if res := post(t, c, ts.URL+"/api/ssh/rp/record",
		`{"agent":"claude","enabled":false}`); res != http.StatusOK {
		t.Fatalf("パスワード無しで止められない: %d", res)
	}
	if on := recordOn(t, db, "rp", "claude"); on {
		t.Fatal("止めたのに台帳が変わっていない")
	}
}

// 接続先の一覧に、記録を読むかどうかが出る（画面が状態を見られるように）。
func TestTheDestinationListShowsWhetherRecordsAreRead(t *testing.T) {
	ts, db := newServer(t)
	c := loggedIn(t, ts)
	mustHost(t, db, "rp")
	if err := session.SetRecordRoot(db, "rp", "codex", true); err != nil {
		t.Fatal(err)
	}

	res, err := c.Get(ts.URL + "/api/ssh")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var rows []session.Destination
	if err := json.NewDecoder(res.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	var got *session.Destination
	for i := range rows {
		if rows[i].Alias == "rp" {
			got = &rows[i]
		}
	}
	if got == nil {
		t.Fatal("一覧に rp が無い")
	}
	if !got.Records["codex"].Enabled {
		t.Fatalf("codex の記録を読む設定が一覧に出ていない: %+v", got.Records)
	}
	if _, ok := got.Records["claude"]; ok {
		t.Fatalf("触っていない claude の行が出ている: %+v", got.Records)
	}
}

// 実行面が居なければ「いま読む」は断る。**黙って成功と言わない。**
func TestReadingNowFailsWithoutTheExecutionSide(t *testing.T) {
	ts, db := newServer(t)
	c := loggedIn(t, ts)
	mustHost(t, db, "rp")
	if res := post(t, c, ts.URL+"/api/ssh/rp/record/read", `{"agent":"claude"}`); res != http.StatusServiceUnavailable {
		t.Fatalf("実行面が居ないのに「読み始めた」と答えた: %d", res)
	}
}

func post(t *testing.T, c *http.Client, url, body string) int {
	t.Helper()
	res, err := c.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func recordOn(t *testing.T, db *store.DB, host, agent string) bool {
	t.Helper()
	rows, err := session.ListRecordRoots(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Host == host && r.Agent == agent {
			return r.Enabled
		}
	}
	return false
}

// mustHost は台帳に接続先を1つ置く（`~/.ssh/config` を読んだ体）。
func mustHost(t *testing.T, db *store.DB, alias string) {
	t.Helper()
	if _, err := db.Exec(`insert into ssh_hosts(alias, hostname, allowed, source, seen_at, updated_at)
		values(?, 'h', 0, 'config', '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')`, alias); err != nil {
		t.Fatal(err)
	}
}
