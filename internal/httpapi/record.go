package httpapi

import (
	"net/http"

	"github.com/MoomA-0750/camp/internal/audit"
	"github.com/MoomA-0750/camp/internal/session"
)

// 向こうのホストの記録を読むかどうかの口（M47）。
//
// **台帳の行が無ければ読まない。** 起こしてよい接続先と、記録を読んでよいかは別物なので、
// 許可とは別に選ぶ（本人の決定 2026-09-12）。
//
// **足すときは再認証が要る**（Cookie を盗られただけで、新しい場所を読みに行かせない）。
// **止めるときは要らない**——安全側へ倒すのは軽くてよい。
func (s *Server) recordRoutes() {
	m := s.mux
	m.HandleFunc("POST /api/ssh/{alias}/record", s.handleRecordSet)
	m.HandleFunc("POST /api/ssh/{alias}/record/read", s.handleRecordRead)
}

func (s *Server) handleRecordSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		// Agent は取り込み器の名前（claude / codex）。
		Agent    string `json:"agent"`
		Enabled  bool   `json:"enabled"`
		Password string `json:"password"`
	}
	if !s.readBody(w, r, &body) {
		return
	}
	alias := r.PathValue("alias")
	// 読むようにするときだけ再認証。止めるのは軽く。
	if body.Enabled && !s.reauth(w, r, body.Password, "ssh:"+alias) {
		return
	}
	if err := session.SetRecordRoot(s.db, alias, body.Agent, body.Enabled); err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	detail, outcome := body.Agent+": 記録を読む", audit.OK
	if !body.Enabled {
		detail, outcome = body.Agent+": 記録を読まない", audit.Denied
	}
	audit.Append(s.db, audit.Entry{Actor: "user", Action: "ssh.record",
		Target: alias, Detail: detail, Outcome: outcome})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleRecordRead は「いま読む」。**待たない**——携帯の回線では1周に何分もかかる。
// 結果は台帳の跡（最後に読めた時刻・最後の失敗）と、取り込んだ行に出る。
func (s *Server) handleRecordRead(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Agent string `json:"agent"`
	}
	if !s.readBody(w, r, &body) {
		return
	}
	if s.sessions == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "実行面と話す口が無いので読めない")
		return
	}
	alias := r.PathValue("alias")
	if !s.sessions.ReadRecordsNow(alias, body.Agent) {
		s.fail(w, r, http.StatusServiceUnavailable, "取り込みが組み立てられていない")
		return
	}
	audit.Append(s.db, audit.Entry{Actor: "user", Action: "ssh.record_read",
		Target: alias, Detail: body.Agent, Outcome: audit.OK})
	writeJSON(w, http.StatusOK, map[string]any{"started": true})
}
