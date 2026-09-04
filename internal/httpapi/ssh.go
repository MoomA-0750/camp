package httpapi

import (
	"net/http"

	"github.com/MoomA-0750/camp/internal/audit"
	"github.com/MoomA-0750/camp/internal/session"
)

// SSH接続先の台帳。
//
// **リモート起動はまだしない**（Phase 3 のスコープ外）。ここで作るのは
// 台帳と許可フラグだけ。`~/.ssh/config` は読むだけで、書き戻さない。
func (s *Server) sshRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/ssh", s.handleSSHList)
	m.HandleFunc("POST /api/ssh/scan", s.handleSSHScan)
	m.HandleFunc("POST /api/ssh/{alias}/edit", s.handleSSHEdit)
	m.HandleFunc("POST /api/ssh/{alias}/allow", s.handleSSHAllow)
}

func (s *Server) handleSSHList(w http.ResponseWriter, r *http.Request) {
	rows, err := session.ListDestinations(s.db)
	s.respond(w, r, rows, err)
}

// handleSSHScan は取り込み直す。**allowed は変わらない。**
func (s *Server) handleSSHScan(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "実行面が居ないので読めない")
		return
	}
	added, updated, err := s.sessions.ScanSSH()
	if err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, err.Error())
		return
	}
	audit.Append(s.db, audit.Entry{Actor: "user", Action: "ssh.scan",
		Detail: "読み取りのみ。allowed は変えていない", Outcome: audit.OK})
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "updated": updated})
}

func (s *Server) handleSSHEdit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Note        string `json:"note"`
		TailscaleIP string `json:"tailscale_ip"`
	}
	if !s.readBody(w, r, &body) {
		return
	}
	alias := r.PathValue("alias")
	if err := session.EditDestination(s.db, alias, body.Note, body.TailscaleIP); err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	audit.Append(s.db, audit.Entry{Actor: "user", Action: "ssh.edit",
		Target: alias, Outcome: audit.OK})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSSHAllow は許可フラグを切り替える。**再認証が要る。**
//
// cwd の許可リストと同じ理由——Cookie を盗られただけで、繋いでよい先を
// 増やされてはいけない。
func (s *Server) handleSSHAllow(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Allowed  bool   `json:"allowed"`
		Password string `json:"password"`
	}
	if !s.readBody(w, r, &body) {
		return
	}
	alias := r.PathValue("alias")
	if !s.reauth(w, r, body.Password, "ssh:"+alias) {
		return
	}
	if err := session.SetDestinationAllowed(s.db, alias, body.Allowed); err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	out, detail := audit.OK, "許した"
	if !body.Allowed {
		out, detail = audit.Denied, "許可を外した"
	}
	audit.Append(s.db, audit.Entry{Actor: "user", Action: "ssh.allow",
		Target: alias, Detail: detail, Outcome: out})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
