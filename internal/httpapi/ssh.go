package httpapi

import (
	"net/http"

	"github.com/MoomA-0750/camp/internal/audit"
	"github.com/MoomA-0750/camp/internal/session"
)

// SSH接続先の台帳。`~/.ssh/config` は読むだけで、書き戻さない。
//
// 2026-09-11 から、ここで許した先にセッションを起こせる（POST /api/runtime の host）。
// **許すときに行き先を固定する**——許したあとで config を書き換えても、
// 同じ名前のまま別の先へは繋がらない。
func (s *Server) sshRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/ssh", s.handleSSHList)
	m.HandleFunc("POST /api/ssh/scan", s.handleSSHScan)
	m.HandleFunc("POST /api/ssh/{alias}/edit", s.handleSSHEdit)
	m.HandleFunc("POST /api/ssh/{alias}/allow", s.handleSSHAllow)
	m.HandleFunc("POST /api/ssh/{alias}/claude", s.handleSSHClaude)
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
	if !body.Allowed {
		if err := session.SetDestinationAllowed(s.db, alias, false); err != nil {
			s.fail(w, r, http.StatusBadRequest, err.Error())
			return
		}
		audit.Append(s.db, audit.Entry{Actor: "user", Action: "ssh.allow",
			Target: alias, Detail: "許可を外した", Outcome: audit.Denied})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	// **許すときに行き先を固定する。** `~/.ssh/config` を読めるのは実行面だけ
	// （camp ユーザーには開けていない）なので、実行面に `ssh -G` を読ませる。
	if s.sessions == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "実行面が居ないので行き先を固定できない")
		return
	}
	pin, err := s.sessions.ResolveSSH(alias)
	if err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "行き先を読めない: "+err.Error())
		return
	}
	if err := session.AllowDestination(s.db, alias, pin); err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	audit.Append(s.db, audit.Entry{Actor: "user", Action: "ssh.allow",
		Target: alias, Detail: "許した（行き先を " + pin.String() + " に固定）", Outcome: audit.OK})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pinned": pin})
}

// handleSSHClaude は向こうの `claude` の場所を書く。**再認証が要る**
// ——向こうで何を走らせるかを変えるので、許可と同じ重さで扱う。
func (s *Server) handleSSHClaude(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClaudePath string `json:"claude_path"`
		Password   string `json:"password"`
	}
	if !s.readBody(w, r, &body) {
		return
	}
	alias := r.PathValue("alias")
	if !s.reauth(w, r, body.Password, "ssh:"+alias) {
		return
	}
	if err := session.SetClaudePath(s.db, alias, body.ClaudePath); err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	detail := body.ClaudePath
	if detail == "" {
		detail = "（向こうで探す）"
	}
	audit.Append(s.db, audit.Entry{Actor: "user", Action: "ssh.claude",
		Target: alias, Detail: detail, Outcome: audit.OK})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
