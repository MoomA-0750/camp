package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoomA-0750/camp/internal/store"
)

// テストは KDF の回数を下げる。ここで測りたいのは経路であって
// パスワードの強度ではない。本番の値は auth.go の既定のまま。
func TestMain(m *testing.M) {
	kdfIterations = 1000
	os.Exit(m.Run())
}

func newServer(t *testing.T) (*httptest.Server, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "camp.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		insert into hosts(id, name) values(1, 'h');
		insert into projects(id, host_id, repo_path, name) values(1, 1, '/p', 'p');
		insert into sessions(id, host_id, project_id, agent, started_at, updated_at,
		                     ai_title, conversation_count)
		values('11111111-2222-4333-8444-555555555555', 1, 1, 'claude',
		       '2026-09-02T00:00:00Z', '2026-09-02T01:00:00Z', 'テストの会話', 2)`); err != nil {
		t.Fatal(err)
	}
	if err := SetPassword(db, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, db
}

// クッキーを追わないクライアント。未認証を試すため。
func bare() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func loggedIn(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()
	jar, _ := newJar()
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err := c.Post(ts.URL+"/api/login", "application/json",
		strings.NewReader(`{"password":"correct horse battery"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("ログインできない: %d", res.StatusCode)
	}
	return c
}

// 受け入れ1: /healthz 以外は未認証で通らない。
//
// 画面の殻もバンドルも通さない。中身が入っていなくても、
// 何が置いてあるかを見せる必要が無い。
func TestEverythingButHealthzNeedsAuth(t *testing.T) {
	ts, _ := newServer(t)
	c := bare()

	res, err := c.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("/healthz が %d", res.StatusCode)
	}

	for _, p := range []string{
		"/", "/index.html", "/sessions/11111111-2222-4333-8444-555555555555",
		"/api/me", "/api/hosts", "/api/projects", "/api/sessions",
		"/api/sessions/11111111-2222-4333-8444-555555555555",
		"/api/sessions/11111111-2222-4333-8444-555555555555/messages",
		"/api/search?q=x", "/api/usage/summary", "/api/files", "/api/backups",
		"/api/backups/1/content", "/api/findings", "/api/nonexistent",
	} {
		res, err := c.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		switch {
		case strings.HasPrefix(p, "/api/"):
			if res.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s が %d（401 のはず）: %s", p, res.StatusCode, body)
			}
		default:
			if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/login" {
				t.Fatalf("%s が %d → %q（/login への 302 のはず）",
					p, res.StatusCode, res.Header.Get("Location"))
			}
		}
	}

	// ログイン画面だけは未認証で開ける。開けないとログインできない。
	res, err = c.Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("/login が %d", res.StatusCode)
	}
}

// 受け入れ2: オリジン検証がある。
// Cookie は自動で付くので、どこから叩かれたかを見ないと CSRF になる。
func TestOriginIsChecked(t *testing.T) {
	ts, _ := newServer(t)
	c := loggedIn(t, ts)

	req, _ := http.NewRequest("POST", ts.URL+"/api/logout", nil)
	req.Header.Set("Origin", "https://evil.example")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("他所からの POST が %d（403 のはず）", res.StatusCode)
	}

	// 自分自身からなら通る。
	req, _ = http.NewRequest("POST", ts.URL+"/api/logout", nil)
	req.Header.Set("Origin", ts.URL)
	res, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("自分自身からの POST が %d", res.StatusCode)
	}

	// CORS ヘッダは返さない。返さないことがそのまま防御になる。
	res, _ = c.Get(ts.URL + "/api/hosts")
	if v := res.Header.Get("Access-Control-Allow-Origin"); v != "" {
		t.Fatalf("CORS を許してしまっている: %q", v)
	}
	res.Body.Close()
}

// 受け入れ3: catch-all がある。深いURLを直接開いてもリロードしても404にならない。
func TestDeepURLsFallBackToTheShell(t *testing.T) {
	ts, _ := newServer(t)
	c := loggedIn(t, ts)

	for _, p := range []string{
		"/", "/sessions/11111111-2222-4333-8444-555555555555",
		"/sessions/11111111-2222-4333-8444-555555555555/runs/0",
		"/notes/Human/Projects/Camp.md", "/search?q=%E8%AA%8D%E8%A8%BC",
		"/views/anything/deeper/still",
	} {
		res, err := c.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("%s が %d", p, res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("%s の Content-Type が %q", p, ct)
		}
		if !strings.Contains(string(body), "<title>Camp</title>") {
			t.Fatalf("%s に殻が返っていない", p)
		}
	}

	// ただし /api/ の未マッチは殻ではなく 404。JSON を待っている相手に
	// HTML を返すと、原因の分からない壊れ方をする。
	res, err := c.Get(ts.URL + "/api/nope")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 404 || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("/api/nope が %d %q %s", res.StatusCode, res.Header.Get("Content-Type"), body)
	}
}

// パスワードは平文でも可逆でも持たない。Cookie の値そのものも持たない。
// DB を読めた者がそのままログインできてはいけない。
func TestNoPlaintextSecretsInTheDatabase(t *testing.T) {
	ts, db := newServer(t)
	loggedIn(t, ts)

	var algo string
	var hash []byte
	if err := db.QueryRow(`select algo, hash from auth_credential`).Scan(&algo, &hash); err != nil {
		t.Fatal(err)
	}
	if algo != "pbkdf2-sha256" {
		t.Fatalf("algo=%q", algo)
	}
	if strings.Contains(string(hash), "correct horse") {
		t.Fatal("パスワードがそのまま入っている")
	}

	var n int
	if err := db.QueryRow(`select count(*) from auth_sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("ログインセッションが %d 件", n)
	}
	// Cookie の値は保存していない（保存しているのはその SHA-256）。
	var th []byte
	if err := db.QueryRow(`select token_hash from auth_sessions`).Scan(&th); err != nil {
		t.Fatal(err)
	}
	if len(th) != 32 {
		t.Fatalf("token_hash が %d バイト", len(th))
	}
}

// パスワードを変えたら、いま開いている口は全部閉じる。
func TestChangingPasswordClosesOpenSessions(t *testing.T) {
	ts, db := newServer(t)
	c := loggedIn(t, ts)

	res, _ := c.Get(ts.URL + "/api/me")
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("ログインできていない: %d", res.StatusCode)
	}

	if err := SetPassword(db, "べつのぱすわーど"); err != nil {
		t.Fatal(err)
	}
	res, _ = c.Get(ts.URL + "/api/me")
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("古い Cookie がまだ通る: %d", res.StatusCode)
	}
}

// 総当たりに何も抵抗しないのはまずい。パスワード1本が唯一の壁なので。
func TestLoginIsThrottled(t *testing.T) {
	ts, _ := newServer(t)
	c := bare()
	got429 := false
	for i := 0; i < maxFails+2; i++ {
		res, err := c.Post(ts.URL+"/api/login", "application/json",
			strings.NewReader(`{"password":"ちがう"}`))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatalf("何回間違えても 429 にならない")
	}
}

// API が実際に答えること。認証と振り分けが繋がっているかの通し確認。
func TestAuthenticatedAPIAnswers(t *testing.T) {
	ts, _ := newServer(t)
	c := loggedIn(t, ts)

	res, err := c.Get(ts.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body struct {
		Sessions []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"sessions"`
		Next string `json:"next_cursor"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sessions) != 1 || body.Sessions[0].Title != "テストの会話" {
		t.Fatalf("%+v", body)
	}
	if body.Next == "" {
		t.Fatal("next_cursor が空")
	}
}

func newJar() (http.CookieJar, error) { return &jar{m: map[string][]*http.Cookie{}}, nil }

// jar は net/http/cookiejar を使わない最小の入れ物。
// cookiejar は httptest の 127.0.0.1 を公開サフィックス無しで扱えるが、
// 依存を増やさずに済むならそのほうがよい。
type jar struct{ m map[string][]*http.Cookie }

func (j *jar) SetCookies(u *url.URL, cs []*http.Cookie) { j.m[u.Host] = cs }
func (j *jar) Cookies(u *url.URL) []*http.Cookie        { return j.m[u.Host] }

// 使い捨てトークンは1回だけ通る。開発中の入口だが、二度使えたら
// 「URL を知っている者が何度でも入れる」ことになり、パスワードを
// 置いた意味が消える。
func TestLoginTokenIsSingleUse(t *testing.T) {
	ts, db := newServer(t)
	tok, _, err := MintLoginToken(db, LoginTokenTTL)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Jar: mustJar(), CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	res, err := c.Get(ts.URL + "/login?t=" + url.QueryEscape(tok))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/" {
		t.Fatalf("1回目が %d → %q", res.StatusCode, res.Header.Get("Location"))
	}
	// Cookie が入って、認証を通る。
	res, _ = c.Get(ts.URL + "/api/me")
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("トークンで入れていない: %d", res.StatusCode)
	}

	// 2回目は通らない。別のクライアントで試す。
	c2 := &http.Client{Jar: mustJar(), CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, _ = c2.Get(ts.URL + "/login?t=" + url.QueryEscape(tok))
	res.Body.Close()
	if res.Header.Get("Location") != "/login" {
		t.Fatalf("2回目が通ってしまった: %d → %q", res.StatusCode, res.Header.Get("Location"))
	}
	res, _ = c2.Get(ts.URL + "/api/me")
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("2回目のクライアントが入れてしまった: %d", res.StatusCode)
	}
}

// 期限切れは通らない。
func TestLoginTokenExpires(t *testing.T) {
	ts, db := newServer(t)
	tok, _, err := MintLoginToken(db, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	// 期限は秒精度（RFC3339）なので、確実に過去にしてから試す。
	if _, err := db.Exec(`update auth_login_tokens set expires_at = '2020-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	c := bare()
	res, _ := c.Get(ts.URL + "/login?t=" + url.QueryEscape(tok))
	res.Body.Close()
	if res.Header.Get("Location") != "/login" {
		t.Fatalf("期限切れが通った: %d → %q", res.StatusCode, res.Header.Get("Location"))
	}
}

// でたらめなトークンは通らない。
func TestBogusLoginTokenIsRejected(t *testing.T) {
	ts, _ := newServer(t)
	c := bare()
	res, _ := c.Get(ts.URL + "/login?t=" + url.QueryEscape("でたらめ"))
	res.Body.Close()
	if res.Header.Get("Location") != "/login" {
		t.Fatalf("通ってしまった: %d → %q", res.StatusCode, res.Header.Get("Location"))
	}
}

func mustJar() http.CookieJar {
	j, _ := newJar()
	return j
}
