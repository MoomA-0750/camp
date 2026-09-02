package httpapi

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MoomA-0750/camp/internal/store"
)

const (
	cookieName = "camp_session"
	kdfKeyLen  = 32
	sessionTTL = 30 * 24 * time.Hour
)

// kdfIterations は pbkdf2 の回数。単一ユーザーのログインは1回きりなので、
// 体感できない範囲で高くしておく（OWASP の PBKDF2-HMAC-SHA256 の推奨値）。
//
// var なのはテストが下げるため。本番の経路からは書き換えない。
var kdfIterations = 600_000

// ErrNoCredential はパスワードがまだ設定されていない状態。
var ErrNoCredential = errors.New("パスワードが設定されていない（campd passwd）")

// SetPassword はパスワードを設定する（1行を置き換える）。
func SetPassword(db *store.DB, password string) error {
	if len(password) < 8 {
		return fmt.Errorf("パスワードは8文字以上にすること")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	sum, err := pbkdf2.Key(sha256.New, password, salt, kdfIterations, kdfKeyLen)
	if err != nil {
		return err
	}
	_, err = db.Exec(`
		insert into auth_credential(id, algo, iterations, salt, hash, updated_at)
		values(1, 'pbkdf2-sha256', ?, ?, ?, ?)
		on conflict(id) do update set
			algo = excluded.algo, iterations = excluded.iterations,
			salt = excluded.salt, hash = excluded.hash, updated_at = excluded.updated_at`,
		kdfIterations, salt, sum, nowRFC3339())
	if err != nil {
		return err
	}
	// パスワードを変えたら、いま開いている口は全部閉じる。
	_, err = db.Exec(`delete from auth_sessions`)
	return err
}

// checkPassword は照合する。合っていれば nil。
func checkPassword(db *store.DB, password string) error {
	var algo string
	var iter int
	var salt, want []byte
	err := db.QueryRow(`select algo, iterations, salt, hash from auth_credential where id = 1`).
		Scan(&algo, &iter, &salt, &want)
	if err == sql.ErrNoRows {
		return ErrNoCredential
	}
	if err != nil {
		return err
	}
	if algo != "pbkdf2-sha256" {
		return fmt.Errorf("知らないアルゴリズム %q", algo)
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return err
	}
	// 長さでも中身でも早期に返さない。
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return errors.New("パスワードが違う")
	}
	return nil
}

// hashToken は Cookie の値から DB に置く形を作る。
// 乱数そのものは保存しない（DB を読めただけでログインできてしまうため）。
func hashToken(tok string) []byte {
	sum := sha256.Sum256([]byte(tok))
	return sum[:]
}

// newSession はログインセッションを1つ作り、Cookie に載せる値を返す。
func newSession(db *store.DB, r *http.Request) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now().UTC()
	exp := now.Add(sessionTTL)
	_, err := db.Exec(`
		insert into auth_sessions(token_hash, created_at, expires_at, last_seen_at, user_agent, remote_addr)
		values(?,?,?,?,?,?)`,
		hashToken(tok), now.Format(time.RFC3339), exp.Format(time.RFC3339),
		now.Format(time.RFC3339), r.UserAgent(), r.RemoteAddr)
	if err != nil {
		return "", time.Time{}, err
	}
	return tok, exp, nil
}

// validSession は Cookie が生きているかを見る。
func validSession(db *store.DB, tok string) bool {
	if tok == "" {
		return false
	}
	var exp string
	err := db.QueryRow(`select expires_at from auth_sessions where token_hash = ?`,
		hashToken(tok)).Scan(&exp)
	if err != nil {
		return false
	}
	t, err := time.Parse(time.RFC3339, exp)
	if err != nil || time.Now().UTC().After(t) {
		return false
	}
	// 最終利用だけ更新する。失敗しても認証の可否は変えない。
	db.Exec(`update auth_sessions set last_seen_at = ? where token_hash = ?`,
		nowRFC3339(), hashToken(tok))
	return true
}

func dropSession(db *store.DB, tok string) {
	if tok != "" {
		db.Exec(`delete from auth_sessions where token_hash = ?`, hashToken(tok))
	}
}

// throttle はログイン失敗の回数を数える。
//
// パスワード1本が唯一の壁なので、総当たりに何も抵抗しないのはまずい。
// プロセス内で足りる（単一ユーザー・単一プロセス）。
type throttle struct {
	mu     sync.Mutex
	fails  int
	window time.Time
}

const (
	maxFails   = 10
	failWindow = 15 * time.Minute
)

func (t *throttle) blocked() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if time.Now().After(t.window) {
		t.fails = 0
	}
	return t.fails >= maxFails
}

func (t *throttle) fail() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if time.Now().After(t.window) {
		t.fails = 0
		t.window = time.Now().Add(failWindow)
	}
	t.fails++
}

func (t *throttle) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fails = 0
}

func cookieValue(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *Server) setCookie(w http.ResponseWriter, tok string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    tok,
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		Secure:   s.secureCookie,
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secureCookie, SameSite: http.SameSiteStrictMode,
	})
}

// PurgeExpiredSessions は期限切れの口を掃除する。
func PurgeExpiredSessions(db *store.DB) (int64, error) {
	r, err := db.Exec(`delete from auth_sessions where expires_at < ?`, nowRFC3339())
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// sameOrigin は Origin ヘッダが自分自身を指しているかを見る。
//
// 状態を変える要求（POST など）は、Cookie が自動で付く以上、
// どこから叩かれたかを確かめないと CSRF になる。CORS ヘッダは一切
// 返さないので、他所からの読み取りはブラウザ側で止まる。書き込みは
// フォーム投稿で飛ぶのでここで止める。
func sameOrigin(r *http.Request, allowed []string) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		// Origin を付けない古い経路は、ブラウザからのフォーム投稿とも
		// 区別が付かない。Sec-Fetch-Site があればそれを見る。
		switch r.Header.Get("Sec-Fetch-Site") {
		case "same-origin", "same-site", "none":
			return true
		case "":
			return true // 非ブラウザ（curl 等）。Cookie を持っていれば通す
		}
		return false
	}
	for _, a := range allowed {
		if strings.EqualFold(a, o) {
			return true
		}
	}
	// スキームを剥がしてホストだけ比べる。プロキシの後ろでは
	// スキームが当てにならない（X-Forwarded-Proto は信用しない）。
	host := o
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	return host == r.Host
}

// HasPassword はパスワードが設定済みかを返す。
func HasPassword(db *store.DB) bool {
	var n int
	if err := db.QueryRow(`select count(*) from auth_credential where id = 1`).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// LoginTokenTTL は使い捨てトークンの既定の寿命。
// URL に載る以上ブラウザの履歴に残るので、短くして1回で殺す。
const LoginTokenTTL = 2 * time.Minute

// MintLoginToken は使い捨てのログイン用トークンを1つ作る。
//
// 開発中に、パスワードを打たずにブラウザを認証済みにするための入口。
// 発行できるのは DB に書ける者だけで、その者はもう全部読めるので、
// これで新しく手に入るものは無い。
func MintLoginToken(db *store.DB, ttl time.Duration) (string, time.Time, error) {
	if ttl <= 0 {
		ttl = LoginTokenTTL
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now().UTC()
	exp := now.Add(ttl)
	if _, err := db.Exec(`
		insert into auth_login_tokens(token_hash, created_at, expires_at)
		values(?,?,?)`, hashToken(tok), now.Format(time.RFC3339), exp.Format(time.RFC3339)); err != nil {
		return "", time.Time{}, err
	}
	// 期限切れは溜めない。
	db.Exec(`delete from auth_login_tokens where expires_at < ?`, nowRFC3339())
	return tok, exp, nil
}

// redeemLoginToken は使い捨てトークンを1回だけ通す。
//
// 「未使用かつ期限内」の行に印を付ける UPDATE 1本で判定する。
// 読んでから書くと、同じトークンで2回入れる隙間ができる。
func redeemLoginToken(db *store.DB, tok string) bool {
	if tok == "" {
		return false
	}
	r, err := db.Exec(`
		update auth_login_tokens set used_at = ?
		 where token_hash = ? and used_at is null and expires_at >= ?`,
		nowRFC3339(), hashToken(tok), nowRFC3339())
	if err != nil {
		return false
	}
	n, _ := r.RowsAffected()
	return n > 0
}
