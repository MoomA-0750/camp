package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/MoomA-0750/camp/internal/session"
)

// Phase 3 / M26。**Camp が起こしたセッション**を操る口。
//
// 起こす当人はここには居ない。campd は camp ユーザーで動いていて `claude` を
// 起こせないので（dev/active/phase3-baseline.md 4節）、実行面は別プロセス。
// ここが決めて、実行面が動く。
//
// **決めるのがこちら側であることが、この分割で唯一守れているもの。**
// 実行面は人間と同じユーザーで動くので、内容を偽ることは防げない。
func (s *Server) runtimeRoutes() {
	if s.sessions == nil {
		return
	}
	m := s.mux
	m.HandleFunc("GET /api/runtime", s.handleRuntimeList)
	m.HandleFunc("POST /api/runtime", s.handleRuntimeStart)
	m.HandleFunc("POST /api/runtime/{id}/input", s.handleRuntimeInput)
	m.HandleFunc("POST /api/runtime/{id}/stop", s.handleRuntimeStop)
	m.HandleFunc("POST /api/runtime/{id}/approve", s.handleRuntimeApprove)
}

func (s *Server) handleRuntimeList(w http.ResponseWriter, r *http.Request) {
	rows, err := session.List(s.db, atoi(r.URL.Query().Get("limit")))
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_connected": s.sessions.AgentConnected(),
		"sessions":        rows,
	})
}

func (s *Server) handleRuntimeStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cwd string `json:"cwd"`
	}
	if !s.readBody(w, r, &body) {
		return
	}
	rec, err := s.sessions.Start("user", body.Cwd)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, session.ErrNoAgent) {
			code = http.StatusServiceUnavailable
		}
		s.fail(w, r, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) handleRuntimeInput(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if !s.readBody(w, r, &body) {
		return
	}
	if err := s.sessions.Input(r.PathValue("id"), body.Text); err != nil {
		s.fail(w, r, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRuntimeStop(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode string `json:"mode"`
	}
	if !s.readBody(w, r, &body) {
		return
	}
	if body.Mode == "" {
		body.Mode = session.StopInterrupt
	}
	if err := s.sessions.Stop(r.PathValue("id"), body.Mode); err != nil {
		s.fail(w, r, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRuntimeApprove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequestID string `json:"request_id"`
		Behavior  string `json:"behavior"`
		Message   string `json:"message"`
	}
	if !s.readBody(w, r, &body) {
		return
	}
	if err := s.sessions.Approve(r.PathValue("id"), body.RequestID, body.Behavior, body.Message); err != nil {
		s.fail(w, r, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// readBody は上限つきで JSON を読む。**上限のない読み取りを置かない。**
func (s *Server) readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "本文が読めない")
		return false
	}
	if err := json.Unmarshal(b, v); err != nil {
		s.fail(w, r, http.StatusBadRequest, "本文が JSON ではない")
		return false
	}
	return true
}
