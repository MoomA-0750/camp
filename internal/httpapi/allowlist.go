package httpapi

import (
	"net/http"

	"github.com/MoomA-0750/camp/internal/audit"
	"github.com/MoomA-0750/camp/internal/session"
)

// 許可リストの口。
//
// **実行専用OSユーザーもVM分離も採らないので、ここが唯一の境界になる。**
// だから変更にはパスワードの再入力を要求する——Cookie を盗られただけで
// 境界を広げられてはいけない。読むだけならログイン済みで足りる。
//
// なお、この許可リストが止めるのは **Camp の API を経由した誤用・暴走**であって、
// VM に入られたあとの何かではない。**両者を混同しない。**
func (s *Server) allowlistRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/allowlist", s.handleAllowlist)
	m.HandleFunc("POST /api/allowlist", s.handleAllowlistAdd)
	m.HandleFunc("POST /api/allowlist/remove", s.handleAllowlistRemove)
}

// handleAllowlist はこのマシンの場所と、向こうの場所（host 付き）をまとめて返す。
func (s *Server) handleAllowlist(w http.ResponseWriter, r *http.Request) {
	rows, err := session.ListAllAllowed(s.db)
	s.respond(w, r, rows, err)
}

func where(host, path string) string {
	if host == "" {
		return path
	}
	return host + ":" + path
}

// reauth はパスワードの再入力を確かめる。**失敗も記録に残す。**
func (s *Server) reauth(w http.ResponseWriter, r *http.Request, password, what string) bool {
	if s.throttle.blocked() {
		s.fail(w, r, http.StatusTooManyRequests, "試行が多すぎる。しばらく待つこと")
		return false
	}
	if err := checkPassword(s.db, password); err != nil {
		s.throttle.fail()
		audit.Append(s.db, audit.Entry{Actor: "user", Action: "allowlist.reauth",
			Target: what, Detail: "パスワードが違う", Outcome: audit.Denied})
		s.fail(w, r, http.StatusUnauthorized, "パスワードが違う")
		return false
	}
	s.throttle.reset()
	return true
}

func (s *Server) handleAllowlistAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path     string `json:"path"`
		Host     string `json:"host"` // 空ならこのマシン
		Note     string `json:"note"`
		Password string `json:"password"`
	}
	if !s.readBody(w, r, &body) {
		return
	}
	if !s.reauth(w, r, body.Password, where(body.Host, body.Path)) {
		return
	}
	var a session.Allowed
	var err error
	if body.Host == "" {
		a, err = session.AddAllowed(s.db, body.Path, body.Note, "user")
	} else {
		a, err = session.AddRemoteAllowed(s.db, body.Host, body.Path, body.Note, "user")
	}
	if err != nil {
		audit.Append(s.db, audit.Entry{Actor: "user", Action: "allowlist.add",
			Target: where(body.Host, body.Path), Detail: err.Error(), Outcome: audit.Error})
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	audit.Append(s.db, audit.Entry{Actor: "user", Action: "allowlist.add",
		Target: where(a.Host, a.Path), Detail: body.Note, Outcome: audit.OK})
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handleAllowlistRemove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path     string `json:"path"`
		Host     string `json:"host"`
		Password string `json:"password"`
	}
	if !s.readBody(w, r, &body) {
		return
	}
	if !s.reauth(w, r, body.Password, where(body.Host, body.Path)) {
		return
	}
	var ok bool
	var err error
	if body.Host == "" {
		ok, err = session.RemoveAllowed(s.db, body.Path)
	} else {
		ok, err = session.RemoveRemoteAllowed(s.db, body.Host, body.Path)
	}
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	out := audit.OK
	if !ok {
		out = audit.Error
	}
	audit.Append(s.db, audit.Entry{Actor: "user", Action: "allowlist.remove",
		Target: where(body.Host, body.Path), Outcome: out})
	writeJSON(w, http.StatusOK, map[string]any{"removed": ok})
}
