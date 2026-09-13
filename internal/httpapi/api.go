package httpapi

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/MoomA-0750/camp/internal/audit"
	"github.com/MoomA-0750/camp/internal/files"
	"github.com/MoomA-0750/camp/internal/limits"
	"github.com/MoomA-0750/camp/internal/query"
	"github.com/MoomA-0750/camp/internal/search"
	"github.com/MoomA-0750/camp/internal/secrets"
	"github.com/MoomA-0750/camp/internal/vault"
)

// routes は認証を通ったあとの振り分け。
// ここに登録しないパスは spa（未マッチの受け皿）に落ちる。
func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("/", s.spa) // catch-all。深いURLの直接オープンとリロードのため

	m.HandleFunc("POST /api/logout", s.handleLogout)
	m.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true})
	})

	m.HandleFunc("GET /api/hosts", s.handleHosts)
	m.HandleFunc("GET /api/projects", s.handleProjects)
	m.HandleFunc("GET /api/sessions", s.handleSessions)
	m.HandleFunc("GET /api/sessions/{id}", s.handleSession)
	m.HandleFunc("GET /api/sessions/{id}/messages", s.handleMessages)
	m.HandleFunc("GET /api/search", s.handleSearch)
	m.HandleFunc("GET /api/usage/summary", s.handleUsage)
	m.HandleFunc("GET /api/usage/windows", s.handleWindows)
	m.HandleFunc("GET /api/files", s.handleFiles)
	m.HandleFunc("GET /api/backups", s.handleBackups)
	m.HandleFunc("GET /api/backups/{id}/content", s.handleBackupContent)
	m.HandleFunc("GET /api/findings", s.handleFindings)
	m.HandleFunc("GET /api/vaults", s.handleVaults)
	m.HandleFunc("GET /api/notes", s.handleNotes)
	m.HandleFunc("GET /api/notes/{id}", s.handleNote)
	m.HandleFunc("GET /api/notes/{id}/body", s.handleNoteBody)
	m.HandleFunc("GET /api/notes/{id}/links", s.handleNoteLinks)
	m.HandleFunc("GET /api/notes/{id}/sessions", s.handleNoteSessions)
	m.HandleFunc("GET /api/vault/ghosts", s.handleGhosts)
	m.HandleFunc("GET /api/vault/issues", s.handleIssues)
	m.HandleFunc("GET /api/audit", s.handleAudit)
	m.HandleFunc("GET /api/views", s.handleViews)
	m.HandleFunc("GET /api/graph", s.handleGraph)
	// 定義の読み書きと履歴（M50、2026-09-13）。**書きは POST**——ほかの書き口と揃える
	// （この API に PUT は1つも無い）。`{id...}` より literal を含むこちらが優先される。
	// そのぶん `def`・`history` という名前のビューは開けない（実在しないので許す）。
	m.HandleFunc("GET /api/views/{base}/def", s.handleViewDef)
	m.HandleFunc("POST /api/views/{base}/def", s.handleViewDefSave)
	m.HandleFunc("GET /api/views/{base}/history", s.handleViewHistory)
	m.HandleFunc("GET /api/views/{id...}", s.handleView)
}

// handleAudit は監査ログを新しい順に返す。
//
// **読むだけ。書く口はここに置かない。** 監査ログは Camp 自身が
// 何をしたかの記録で、外から足せるようにすると記録の意味が薄れる。
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := audit.List(s.db, audit.Opts{
		Session: q.Get("session"),
		Action:  q.Get("action"),
		Limit:   atoi(q.Get("limit")),
		Before:  int64(atoi(q.Get("before"))),
	})
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	next := int64(0)
	if len(rows) > 0 {
		next = rows[len(rows)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"audit": rows, "next_before": next})
}

func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	rows, err := query.Hosts(s.db)
	s.respond(w, r, rows, err)
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	rows, err := query.Projects(s.db, r.URL.Query().Get("host"))
	s.respond(w, r, rows, err)
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := query.Sessions(s.db, query.SessionOpts{
		Host:    q.Get("host"),
		Project: q.Get("project"),
		Agent:   q.Get("agent"),
		Q:       q.Get("q"),
		From:    q.Get("from"),
		To:      q.Get("to"),
		Empty:   q.Get("empty") == "1",
		Limit:   atoi(q.Get("limit")),
		Cursor:  q.Get("cursor"),
	})
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	// 次ページの起点は最後の updated_at。件数ではなく時刻で送るのは、
	// 取り込みが走って行が増えても位置がずれないため。
	next := ""
	if len(rows) > 0 {
		next = rows[len(rows)-1].UpdatedAt
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": rows, "next_cursor": next})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	one, err := query.One(s.db, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	if one == nil {
		s.fail(w, r, http.StatusNotFound, "そのセッションは持っていない")
		return
	}
	writeJSON(w, http.StatusOK, one)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := query.Messages(s.db, r.PathValue("id"),
		int64(atoi(q.Get("after"))), atoi(q.Get("limit")), q.Get("all") == "1")
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	next := int64(0)
	if len(rows) > 0 {
		next = rows[len(rows)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": rows, "next_after": next})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("q") == "" {
		s.fail(w, r, http.StatusBadRequest, "q が要る")
		return
	}
	rows, err := search.Query(s.db, q.Get("q"), search.Opts{
		Kind:    q.Get("kind"),
		Session: q.Get("session"),
		Limit:   atoi(q.Get("limit")),
	})
	s.respond(w, r, rows, err)
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := query.UsageSummary(s.db, q.Get("by"), q.Get("from"), q.Get("to"), atoi(q.Get("limit")))
	if err == query.ErrBadBy {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	s.respond(w, r, rows, err)
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	o := files.Opts{Path: q.Get("path"), Session: q.Get("session"),
		Op: q.Get("op"), Limit: atoi(q.Get("limit"))}
	if q.Get("summary") == "1" {
		rows, err := files.Summarize(s.db, o)
		s.respond(w, r, rows, err)
		return
	}
	rows, err := files.Touches(s.db, o)
	s.respond(w, r, rows, err)
}

func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := files.Backups(s.db, files.Opts{
		Path: q.Get("path"), Session: q.Get("session"), Limit: atoi(q.Get("limit"))})
	s.respond(w, r, rows, err)
}

// handleBackupContent は捕獲した中身そのものを返す。
// 元が消えていても読める。ここが M9 の値打ちの出口。
func (s *Server) handleBackupContent(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "id が数でない")
		return
	}
	b, body, err := files.BackupContent(s.db, id)
	if err != nil {
		s.fail(w, r, http.StatusNotFound, err.Error())
		return
	}
	// text/plain で返す。中身はノートやソースなので、HTML として解釈させない。
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Camp-Path", b.AbsPath)
	w.Write(body)
}

func (s *Server) handleFindings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// reveal は既定で false。当たりそのものは伏せる（D-019）。
	rows, err := secrets.List(s.db, q.Get("open") == "1", q.Get("reveal") == "1")
	s.respond(w, r, rows, err)
}

func (s *Server) respond(w http.ResponseWriter, r *http.Request, v any, err error) {
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// handleWindows はプラン上限の窓を返す。?current=1 で進行中のぶんだけ。
func (s *Server) handleWindows(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("current") == "1" {
		rows, err := limits.Current(s.db)
		s.respond(w, r, rows, err)
		return
	}
	rows, err := limits.Windows(s.db, limits.Opts{
		Kind:  q.Get("kind"),
		Limit: atoi(q.Get("n")),
	})
	s.respond(w, r, rows, err)
}

func (s *Server) handleVaults(w http.ResponseWriter, r *http.Request) {
	rows, err := vault.Vaults(s.db)
	s.respond(w, r, rows, err)
}

func (s *Server) handleNotes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := vault.Notes(s.db, vault.NoteOpts{
		VaultID: int64(atoi(q.Get("vault"))),
		Folder:  q.Get("folder"),
		Kind:    q.Get("kind"),
		Q:       q.Get("q"),
		Missing: q.Get("missing"),
		Limit:   atoi(q.Get("n")),
	})
	s.respond(w, r, rows, err)
}

func (s *Server) handleNote(w http.ResponseWriter, r *http.Request) {
	n, err := vault.OneNote(s.db, int64(atoi(r.PathValue("id"))))
	if err == nil && n == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "そのノートは無い"})
		return
	}
	s.respond(w, r, n, err)
}

// handleNoteBody は blobs から本文を返す。Vault のファイルは読まない。
// ノートが消えていても中身が返る。
func (s *Server) handleNoteBody(w http.ResponseWriter, r *http.Request) {
	id := int64(atoi(r.PathValue("id")))
	n, err := vault.OneNote(s.db, id)
	if err != nil {
		s.respond(w, r, nil, err)
		return
	}
	if n == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "そのノートは無い"})
		return
	}
	body, err := vault.NoteBody(s.db, id)
	if err != nil {
		s.respond(w, r, nil, err)
		return
	}
	if body == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "本文を保存していない種別"})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Camp-Path", url.PathEscape(n.Path))
	w.Write(body)
}

func (s *Server) handleNoteLinks(w http.ResponseWriter, r *http.Request) {
	id := int64(atoi(r.PathValue("id")))
	out, err := vault.OutLinks(s.db, id)
	if err != nil {
		s.respond(w, r, nil, err)
		return
	}
	back, err := vault.Backlinks(s.db, id)
	s.respond(w, r, map[string]any{"out": out, "back": back}, err)
}

func (s *Server) handleNoteSessions(w http.ResponseWriter, r *http.Request) {
	rows, err := vault.NoteTouches(s.db, int64(atoi(r.PathValue("id"))), atoi(r.URL.Query().Get("n")))
	s.respond(w, r, rows, err)
}

func (s *Server) handleGhosts(w http.ResponseWriter, r *http.Request) {
	id := int64(atoi(r.URL.Query().Get("vault")))
	if id == 0 {
		id = 1
	}
	rows, err := vault.Ghosts(s.db, id)
	s.respond(w, r, rows, err)
}

// handleIssues は「Obsidian と食い違うかもしれない場所」をまとめて返す。
func (s *Server) handleIssues(w http.ResponseWriter, r *http.Request) {
	id := int64(atoi(r.URL.Query().Get("vault")))
	amb, err := vault.Ambiguous(s.db, id)
	if err != nil {
		s.respond(w, r, nil, err)
		return
	}
	dang, err := vault.Dangling(s.db, id)
	s.respond(w, r, map[string]any{"ambiguous": amb, "dangling": dang}, err)
}
