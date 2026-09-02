// Package httpapi は Camp の HTTP 面。認証・オリジン検証・SPA フォールバックを持つ。
package httpapi

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/MoomA-0750/camp/internal/store"
	"github.com/MoomA-0750/camp/web"
)

//go:embed assets
var builtinFS embed.FS

// Options はサーバーの設定。
type Options struct {
	Addr string
	// WebDir が空でなければ、そこの実ビルドを配る。空なら組み込みの仮の殻。
	WebDir string
	// Origins は Origin ヘッダの明示的な許可一覧。空なら「自分自身」だけ。
	Origins []string
	// SecureCookie は Cookie に Secure を付けるか。TLS 終端の後ろに置くときだけ true。
	SecureCookie bool
	Log          *slog.Logger
}

// Server は Camp の HTTP サーバー。
type Server struct {
	db           *store.DB
	opts         Options
	mux          *http.ServeMux
	static       http.Handler
	assets       fs.FS  // 画面の実体。実在するファイルの判定にも使う
	source       string // どこから画面を配っているか（起動時に出す）
	shell        []byte // 未マッチのパスに返す殻
	login        []byte
	secureCookie bool
	throttle     throttle
	log          *slog.Logger
	views        viewCache
}

// New はハンドラを組み立てる。
func New(db *store.DB, o Options) (*Server, error) {
	s := &Server{db: db, opts: o, mux: http.NewServeMux(),
		secureCookie: o.SecureCookie, log: o.Log}
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}

	var err error
	if s.login, err = builtinFS.ReadFile("assets/login.html"); err != nil {
		return nil, err
	}

	// 画面の出どころは3つ。上から順に使う。
	//
	//  1. -web <dir>  ディスクの実ビルド（開発中に差し替える用）
	//  2. 埋め込みの web/dist（npm run build を通してあれば入っている）
	//  3. 組み込みの仮の殻（node を持たないところでもサーバーは動く）
	switch {
	case o.WebDir != "":
		idx := filepath.Join(o.WebDir, "index.html")
		if s.shell, err = os.ReadFile(idx); err != nil {
			return nil, fmt.Errorf("%s が読めない: %w", idx, err)
		}
		s.assets = os.DirFS(o.WebDir)
		s.source = o.WebDir
	default:
		sub, ok := web.Dist()
		if !ok {
			if sub, err = fs.Sub(builtinFS, "assets"); err != nil {
				return nil, err
			}
			s.source = "組み込みの仮の殻（web/dist が未ビルド）"
		} else {
			s.source = "埋め込みの web/dist"
		}
		if s.shell, err = fs.ReadFile(sub, "index.html"); err != nil {
			return nil, err
		}
		s.assets = sub
	}
	s.static = http.FileServer(http.FS(s.assets))

	s.routes()
	return s, nil
}

// Handler は全体のハンドラ。ここを通らない経路を作らないこと。
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.serve)
}

// serve は1リクエストの入口。順番に意味がある。
//
//  1. セキュリティヘッダ（何を返すにせよ必ず付ける）
//  2. /healthz だけは素通し（受け入れ条件の唯一の例外）
//  3. オリジン検証（状態を変える要求のみ）
//  4. ログイン経路（未認証でも通す2つ）
//  5. 認証。ここを通らないものは1つも無い
//  6. 振り分け。未マッチの GET は殻を返す（SPA の深いURL）
func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.securityHeaders(w)

	if r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
		return
	}

	if !isSafeMethod(r.Method) && !sameOrigin(r, s.opts.Origins) {
		s.fail(w, r, http.StatusForbidden, "オリジンが違う")
		return
	}

	switch {
	case r.URL.Path == "/api/login" && r.Method == http.MethodPost:
		s.handleLogin(w, r)
		return
	case r.URL.Path == "/login" && isSafeMethod(r.Method):
		// 使い捨てトークン付きなら、そのまま認証済みにして中へ入れる。
		// campd login-url が発行する開発中の入口（パスワードは通らない）。
		if t := r.URL.Query().Get("t"); t != "" {
			if !redeemLoginToken(s.db, t) {
				s.log.Warn("使い捨てトークンが通らない", "remote", r.RemoteAddr)
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			tok, exp, err := newSession(s.db, r)
			if err != nil {
				s.fail(w, r, http.StatusInternalServerError, err.Error())
				return
			}
			s.setCookie(w, tok, exp)
			s.log.Info("使い捨てトークンでログイン", "remote", r.RemoteAddr)
			// トークンを URL から落とすため、素の / へ送り直す。
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		s.writeHTML(w, s.login)
		return
	}

	if !validSession(s.db, cookieValue(r)) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			s.fail(w, r, http.StatusUnauthorized, "ログインが要る")
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	withGzip(s.mux.ServeHTTP)(w, r)
}

// securityHeaders は返すもの全部に付ける。
//
// CORS ヘッダは一切返さない。他所のページから読める必要が無く、
// 返さないことがそのまま防御になる。
func (s *Server) securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	// トランスクリプトには任意のテキストが入る。外部への発信路を塞ぐ。
	h.Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
			"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.throttle.blocked() {
		s.fail(w, r, http.StatusTooManyRequests, "試行が多すぎる。しばらく待つこと")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		// フォーム投稿も受ける（組み込みのログイン画面が JS 無しでも動くように）。
		if err := r.ParseForm(); err == nil {
			body.Password = r.PostFormValue("password")
		}
	}
	if err := checkPassword(s.db, body.Password); err != nil {
		s.throttle.fail()
		s.log.Warn("ログイン失敗", "remote", r.RemoteAddr, "err", err)
		if err == ErrNoCredential {
			s.fail(w, r, http.StatusServiceUnavailable, ErrNoCredential.Error())
			return
		}
		s.fail(w, r, http.StatusUnauthorized, "パスワードが違う")
		return
	}
	s.throttle.reset()

	tok, exp, err := newSession(s.db, r)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	s.setCookie(w, tok, exp)
	s.log.Info("ログイン", "remote", r.RemoteAddr, "expires", exp.Format(time.RFC3339))

	if strings.Contains(r.Header.Get("Content-Type"), "form") {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "expires_at": exp})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	dropSession(s.db, cookieValue(r))
	s.clearCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// fail は API には JSON、画面には素のテキストを返す。
func (s *Server) fail(w http.ResponseWriter, r *http.Request, code int, msg string) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeJSON(w, code, map[string]any{"error": msg})
		return
	}
	http.Error(w, msg, code)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func (s *Server) writeHTML(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

// spa は未マッチのパスの受け皿。
//
// **これが無いと `/sessions/<id>` の直接オープンとリロードが404になる。**
// クライアント側ルーティングで本物のURLを持つ以上、サーバーは知らない
// パスにも殻を返さなければならない。実在するファイルはそのまま配る。
func (s *Server) spa(w http.ResponseWriter, r *http.Request) {
	if !isSafeMethod(r.Method) {
		s.fail(w, r, http.StatusMethodNotAllowed, "その方法は受けない")
		return
	}
	// /api/ の未マッチは殻ではなく 404 を返す。JSON を期待している相手に
	// HTML を返すと、原因が分からない壊れ方をする。
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.fail(w, r, http.StatusNotFound, "そのAPIは無い")
		return
	}
	if f := s.openStatic(r.URL.Path); f {
		s.static.ServeHTTP(w, r)
		return
	}
	s.writeHTML(w, s.shell)
}

// openStatic はそのパスに実ファイルがあるかを見る。
// あれば静的配信、無ければ殻。どちらから配っていても同じ判定でよい。
func (s *Server) openStatic(p string) bool {
	name := strings.TrimPrefix(path.Clean("/"+p), "/")
	if name == "" || name == "." {
		return false
	}
	st, err := fs.Stat(s.assets, name)
	return err == nil && !st.IsDir()
}

// Source は画面をどこから配っているかを返す。起動時に出す用。
func (s *Server) Source() string { return s.source }
