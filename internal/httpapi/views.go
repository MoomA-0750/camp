package httpapi

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/MoomA-0750/camp/internal/audit"
	"github.com/MoomA-0750/camp/internal/vault"
	"github.com/MoomA-0750/camp/internal/views"
)

// viewCache はビューの材料を持ち回す。
//
// LoadRecords は Vault 全ノート＋87,401件のプロパティを読むので、
// 画面を1枚開くたびに走らせると遅い。索引が変わるまで使い回す。
type viewCache struct {
	mu      sync.Mutex
	at      time.Time
	scanned string
	bases   []*views.Base
	recs    []*views.Record
	links   []views.Link // グラフ（M52）。行と同じ時に読み、同じ時に捨てる
	// rev は定義の版（`views.Revision`）。**定義を直したら、どこから直しても控えを捨てる**
	// （実装後レビュー、codex の指摘8。画面・MCP・`-convert` がそれぞれ別の控えを持っていて、
	// 画面で直しても MCP は最大 30 秒古い定義で答えていた）。
	rev int64
}

const viewCacheTTL = 30 * time.Second

func (s *Server) viewData() ([]*views.Base, []*views.Record, error) {
	b, r, _, err := s.viewMaterial()
	return b, r, err
}

func (s *Server) viewMaterial() ([]*views.Base, []*views.Record, []views.Link, error) {
	s.views.mu.Lock()
	defer s.views.mu.Unlock()

	var root, scanned string
	if err := s.db.QueryRow(
		`select root, coalesce(scanned_at,'') from vaults order by id limit 1`).
		Scan(&root, &scanned); err != nil {
		return nil, nil, nil, err
	}
	// 索引し直されたら作り直す。時間でも切る（Vault は外から書き換わる）。
	rev, err := views.Revision(s.db, 1)
	if err != nil {
		return nil, nil, nil, err
	}
	if s.views.recs != nil && s.views.scanned == scanned && s.views.rev == rev &&
		time.Since(s.views.at) < viewCacheTTL {
		return s.views.bases, s.views.recs, s.views.links, nil
	}

	// **DB の独自定義を優先し、定義の無い台紙だけ `.base` に落ちる**（M50、2026-09-13）。
	// 落ちるのは台紙単位（`views.Load`）。
	bases, err := views.Load(s.db, 1, func(rel string) ([]byte, error) {
		return vault.Read(root, rel)
	})
	if err != nil {
		return nil, nil, nil, err
	}
	recs, err := views.LoadRecords(s.db, 1)
	if err != nil {
		return nil, nil, nil, err
	}
	links, err := views.LoadLinks(s.db, 1)
	if err != nil {
		return nil, nil, nil, err
	}
	s.views.bases, s.views.recs, s.views.links = bases, recs, links
	s.views.at, s.views.scanned, s.views.rev = time.Now(), scanned, rev
	return bases, recs, links, nil
}

// ViewInfo は一覧に出す1ビュー。
type ViewInfo struct {
	Base    string `json:"base"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Rows    int    `json:"rows"`
	Columns int    `json:"columns"`
	Pinned  int    `json:"pinned"`
	Error   string `json:"error,omitempty"`
	// FromDB は Camp の独自定義から来たか（M50、2026-09-13）。false なら `.base` を読んでいる。
	// **併読の間はどちらを描いたかを画面に出す**——出さないと、変換したつもりで `.base` を
	// 見続けていても気づけない。
	FromDB bool `json:"from_db,omitempty"`
}

func viewID(base, name string) string { return base + "/" + name }

func (s *Server) handleViews(w http.ResponseWriter, r *http.Request) {
	bases, recs, err := s.viewData()
	if err != nil {
		s.respond(w, r, nil, err)
		return
	}
	out := []ViewInfo{}
	for _, b := range bases {
		// 読めなかった `.base` も1行として出す。黙って消えると
		// 「ビューが減った」ことに気付けない。
		if b.ParseError != "" {
			out = append(out, ViewInfo{Base: b.Name, Name: "(読めない)",
				ID: viewID(b.Name, ""), Error: b.ParseError, FromDB: views.FromDB(b)})
			continue
		}
		for i := range b.Views {
			v := &b.Views[i]
			info := ViewInfo{Base: b.Name, Name: v.Name, Kind: v.Kind,
				ID: viewID(b.Name, v.Name), FromDB: views.FromDB(b)}
			res, err := views.Run(b, v, recs)
			if err != nil {
				info.Error = err.Error()
			} else {
				info.Rows, info.Columns = res.Total, len(res.Columns)
				for _, c := range res.Columns {
					if c.Pinned {
						info.Pinned++
					}
				}
			}
			out = append(out, info)
		}
	}
	s.respond(w, r, out, nil)
}

func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	bases, recs, links, err := s.viewMaterial()
	if err != nil {
		s.respond(w, r, nil, err)
		return
	}
	for _, b := range bases {
		for i := range b.Views {
			if viewID(b.Name, b.Views[i].Name) != id {
				continue
			}
			res, err := views.Run(b, &b.Views[i], recs)
			if err == nil {
				views.AttachGraph(res, &b.Views[i], links)
			}
			s.respond(w, r, res, err)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "そのビューは無い"})
}

// ---- 定義の編集（M50 の決定6、2026-09-13）

// handleViewDef は台紙1枚ぶんの定義（YAML）を返す。画面で直すため。
func (s *Server) handleViewDef(w http.ResponseWriter, r *http.Request) {
	base := r.PathValue("base")
	def, err := views.LoadDef(s.db, 1, base)
	if views.IsNoDef(err) {
		// 作り方は画面が添える（ここで言うと画面で二重に出る。2026-09-13、実ブラウザで見た）。
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "その台紙の定義はまだ無い"})
		return
	}
	if err != nil {
		s.respond(w, r, nil, err)
		return
	}
	// **手編集かどうかはサーバーが決めて渡す。** 画面で時刻を比べると、`-convert` が実際に
	// 使う決め方（履歴の最後の書き手）と食い違う。
	edited, err := views.HandEditedDef(s.db, 1, base)
	s.respond(w, r, struct {
		*views.Def
		HandEdited bool `json:"hand_edited"`
		// BodySHA256 は保存のときにそのまま返してもらう（読んだ版の上にしか書かない）。
		BodySHA256 string `json:"body_sha256"`
	}{def, edited, views.SHA256([]byte(def.Body))}, err)
}

// handleViewDefSave は定義を書き換える。
//
// **合言葉の再入力は求めない。** 許可リストの変更に求めるのは「**エージェントが走れる場所が
// 広がる**」から。ビューの定義は**何を表示するか**しか変えないので、同じ重さにしない。
// 代わりに監査へ残し、履歴（`view_history`）にも1行積む。
//
// **壊れた定義は保存されない**（`views.SaveDef` が読めるかを確かめる）。入ってしまうと
// 画面から直せなくなる。
func (s *Server) handleViewDefSave(w http.ResponseWriter, r *http.Request) {
	base := r.PathValue("base")
	var body struct {
		Def string `json:"def"`
		// BaseSHA256 は画面が読み込んだ本文の指紋（GET def の body_sha256）。**読んだ版の上にしか
		// 書かない**——2つのタブ、画面と `-convert` が同時に書くと、後から書いたほうが先を黙って
		// 消していた（実装後レビュー、codex の指摘2）。新しく作るときは空。
		BaseSHA256 string `json:"base_sha256"`
	}
	if !s.readBody(w, r, &body) {
		return
	}
	if err := views.SaveDef(s.db, 1, &views.Def{Base: base, Body: body.Def, BaseSHA256: body.BaseSHA256},
		views.ByUser, false); err != nil {
		audit.Append(s.db, audit.Entry{Actor: "user", Action: "views.def",
			Target: base, Detail: err.Error(), Outcome: audit.Error})
		code := http.StatusBadRequest
		if views.IsConflict(err) {
			code = http.StatusConflict
		}
		s.fail(w, r, code, err.Error())
		return
	}
	audit.Append(s.db, audit.Entry{Actor: "user", Action: "views.def",
		Target: base, Detail: "ビューの定義を書き換えた", Outcome: audit.OK})
	// **控えを捨てる。** 捨てないと、直したのに最大 30 秒は古い定義で描かれる。
	s.views.mu.Lock()
	s.views.recs = nil
	s.views.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleViewHistory は書き換えの履歴（新しい順）。`.base` にあった git 履歴の代わり。
func (s *Server) handleViewHistory(w http.ResponseWriter, r *http.Request) {
	h, err := views.History(s.db, 1, r.PathValue("base"), atoi(r.URL.Query().Get("n")))
	s.respond(w, r, h, err)
}

// handleGraph はノートから辿るグラフ（M52、2026-09-13）。`note` が無ければリンクの一番多いノートが中心。
// `depth` は 1〜4、0 はリンクを持つノート全部。**既定は 1**（ハブから 2 歩で 176 節になり、
// 最初に開く1枚としては多すぎる）。
func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	_, recs, links, err := s.viewMaterial()
	if err != nil {
		s.respond(w, r, nil, err)
		return
	}
	q := r.URL.Query()
	depth := 1
	if d := q.Get("depth"); d != "" {
		// `atoi` は読めないと 0（＝全部）を返すので、ここは読めたかを見る。
		n, err := strconv.Atoi(d)
		if err != nil {
			n = -1
		}
		depth = n
	}
	if depth < 0 || depth > 4 {
		s.fail(w, r, http.StatusBadRequest, "depth は 0〜4")
		return
	}
	g, err := views.Neighborhood(recs, links, int64(atoi(q.Get("note"))), depth)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	s.respond(w, r, g, nil)
}
