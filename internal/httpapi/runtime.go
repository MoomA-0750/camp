package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

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
	m.HandleFunc("GET /api/runtime/{id}/log", s.handleRuntimeLog)
	m.HandleFunc("GET /api/runtime/{id}/stream", s.handleRuntimeStream)
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

// handleRuntimeLog は落としてあるフレームを、カーソルより後ろだけ返す。
func (s *Server) handleRuntimeLog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	res, err := s.sessions.Tail(r.PathValue("id"), int64(atoi(q.Get("since"))), atoi(q.Get("limit")))
	if err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleRuntimeStream は追いつきながら流し続ける。
//
// **WebSocket ではなく SSE にした**（2026-09-04）。理由は3つ:
//   - 要るのは片方向。入力・承認・停止は POST で足りる
//   - WebSocket は net/http だけでは書けず、依存が1つ増える。この
//     リポジトリの依存は全部 indirect で、直接依存を増やす価値が無い
//   - 再接続の作法（Last-Event-ID）がカーソルの設計とそのまま噛み合う
//
// **子は読み手を待たない。** ここが読んでいるのは実行面が落としたファイルで、
// 誰も繋いでいなくても子は走り切る。それが M27 の受け入れ条件そのもの。
func (s *Server) handleRuntimeStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		s.fail(w, r, http.StatusInternalServerError, "流せない")
		return
	}
	id := r.PathValue("id")
	since := int64(atoi(r.URL.Query().Get("since")))
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			since = n // 切れたところから続ける
		}
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	ctx := r.Context()
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	idle := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		res, err := s.sessions.Tail(id, since, 200)
		if err != nil {
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", jsonString(err.Error()))
			fl.Flush()
			return
		}
		if res.Gap {
			// **黙って飛ばさない。**
			fmt.Fprintf(w, "event: gap\ndata: {\"dropped\":%d}\n\n", res.Dropped)
		}
		for _, ln := range res.Lines {
			b, err := json.Marshal(ln)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ln.Seq, b)
			since = ln.Seq
		}
		if len(res.Lines) > 0 || res.Gap {
			idle = 0
			fl.Flush()
			continue
		}
		// 何も無い間も、経路が生きていることを示す。
		idle++
		if idle%50 == 0 {
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
