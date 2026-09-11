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
	m.HandleFunc("GET /api/runtime/ended", s.handleRuntimeEnded)
	m.HandleFunc("GET /api/runtime/{id}", s.handleRuntimeOne)
	m.HandleFunc("POST /api/runtime", s.handleRuntimeStart)
	m.HandleFunc("POST /api/runtime/{id}/input", s.handleRuntimeInput)
	m.HandleFunc("POST /api/runtime/{id}/stop", s.handleRuntimeStop)
	m.HandleFunc("POST /api/runtime/{id}/approve", s.handleRuntimeApprove)
	m.HandleFunc("GET /api/runtime/{id}/log", s.handleRuntimeLog)
	m.HandleFunc("GET /api/runtime/{id}/approvals", s.handleRuntimeApprovals)
	m.HandleFunc("GET /api/runtime/{id}/usage", s.handleRuntimeUsage)
	m.HandleFunc("GET /api/runtime/{id}/stream", s.handleRuntimeStream)
}

// handleRuntimeList は終わっていないものを全部。**終わったものは /ended に分けた。**
// 混ぜると、走っているものが終わったものの山に埋もれる。
func (s *Server) handleRuntimeList(w http.ResponseWriter, r *http.Request) {
	rows, err := session.ListLive(s.db)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_connected": s.sessions.AgentConnected(),
		"sessions":        rows,
	})
}

// handleRuntimeEnded は終わったセッションを1頁ぶん。
// `?kind=` で絞り（mid / waiting / ignored / unknown / 終わり方の語）、
// `?before=` で続きを引く。件数は頁に関係なく全体を返す。
func (s *Server) handleRuntimeEnded(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, err := session.ListEnded(s.db, session.EndedQuery{
		Kind: q.Get("kind"), Before: q.Get("before"), Limit: atoi(q.Get("limit")),
	})
	switch {
	case errors.Is(err, session.ErrBadQuery):
		s.fail(w, r, http.StatusBadRequest, err.Error())
	case err != nil:
		s.fail(w, r, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, page)
	}
}

// handleRuntimeOne は1本。**一覧に載っていない古いものも開ける。**
func (s *Server) handleRuntimeOne(w http.ResponseWriter, r *http.Request) {
	rec, err := session.Get(s.db, r.PathValue("id"))
	switch {
	case errors.Is(err, session.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, err.Error())
	case err != nil:
		s.fail(w, r, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, rec)
	}
}

func (s *Server) handleRuntimeStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cwd string `json:"cwd"`
		// Host は ssh の Host 名。空ならこのマシン。
		Host string `json:"host"`
	}
	if !s.readBody(w, r, &body) {
		return
	}
	rec, err := s.sessions.StartOn("user", body.Host, body.Cwd)
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
	// **開きっぱなしの接続に上限を置く。**
	// 1本ごとに goroutine が1つと、300ms ごとの問い合わせが1つ増える。
	// タブを開いたまま忘れるだけで積み上がるので、数を絞る。
	select {
	case s.streams <- struct{}{}:
		defer func() { <-s.streams }()
	default:
		s.fail(w, r, http.StatusServiceUnavailable,
			"開いている流れが多すぎる。使っていないタブを閉じる")
		return
	}
	id := r.PathValue("id")
	since := int64(atoi(r.URL.Query().Get("since")))
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			since = n // 切れたところから続ける
		}
	}

	// **読まない相手に、いつまでも書こうとしない。**
	// 期限が無いと、受け取らないクライアントで Write が詰まり、
	// goroutine と枠が返らない（切っても気づけない）。
	rc := http.NewResponseController(w)
	deadline := func() {
		rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	}
	deadline()

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
		deadline()
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

// handleRuntimeApprovals は待っている承認を出す。
//
// **画面を閉じて開き直しても見える。** 待ちは DB にあり、campd のメモリには無い。
// `?all=1` で答え済みのぶんも含めた履歴。
func (s *Server) handleRuntimeApprovals(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.URL.Query().Get("all") != "" {
		rows, err := session.ApprovalHistory(s.db, id, atoi(r.URL.Query().Get("limit")))
		s.respond(w, r, rows, err)
		return
	}
	rows, err := s.sessions.Waiting(id)
	s.respond(w, r, rows, err)
}

// handleRuntimeUsage は残量の4種のうち、制御プロトコルからしか取れない2つを返す。
//
//   - get_usage        … モデル別の入出力・キャッシュ・費用、プラン枠の残り
//   - get_context_usage… コンテキストの内訳
//
// **モデル呼び出しは起きない。** 押すたびにトークンを使うことはない
// （2026-09-04 に実測。docs/30-session-protocol.md）。
//
// 残り2つ（プラン残量の履歴・同時実行）は DB と supervisor から取る。
func (s *Server) handleRuntimeUsage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	out := map[string]any{}

	if b, err := s.sessions.Control(id, "get_usage"); err != nil {
		out["usage_error"] = err.Error()
	} else {
		out["usage"] = json.RawMessage(b)
	}
	if b, err := s.sessions.Control(id, "get_context_usage"); err != nil {
		out["context_error"] = err.Error()
	} else {
		out["context"] = json.RawMessage(b)
	}
	running, max := s.sessions.Capacity()
	out["running"] = running
	out["max"] = max
	// **4コアしかない。** 上限に達していなくても、並べれば遅くなる。
	if running >= max {
		out["warning"] = "同時実行の上限に達している"
	} else if running > 1 {
		out["warning"] = fmt.Sprintf("%d 本が同時に走っている（4コア）", running)
	}
	writeJSON(w, http.StatusOK, out)
}
