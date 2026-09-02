package httpapi

import (
	"net/http"
	"sync"
	"time"

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
}

const viewCacheTTL = 30 * time.Second

func (s *Server) viewData() ([]*views.Base, []*views.Record, error) {
	s.views.mu.Lock()
	defer s.views.mu.Unlock()

	var root, scanned string
	if err := s.db.QueryRow(
		`select root, coalesce(scanned_at,'') from vaults order by id limit 1`).
		Scan(&root, &scanned); err != nil {
		return nil, nil, err
	}
	// 索引し直されたら作り直す。時間でも切る（Vault は外から書き換わる）。
	if s.views.recs != nil && s.views.scanned == scanned &&
		time.Since(s.views.at) < viewCacheTTL {
		return s.views.bases, s.views.recs, nil
	}

	bases, err := views.LoadBases(s.db, 1, func(rel string) ([]byte, error) {
		return vault.Read(root, rel)
	})
	if err != nil {
		return nil, nil, err
	}
	recs, err := views.LoadRecords(s.db, 1)
	if err != nil {
		return nil, nil, err
	}
	s.views.bases, s.views.recs = bases, recs
	s.views.at, s.views.scanned = time.Now(), scanned
	return bases, recs, nil
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
				ID: viewID(b.Name, ""), Error: b.ParseError})
			continue
		}
		for i := range b.Views {
			v := &b.Views[i]
			info := ViewInfo{Base: b.Name, Name: v.Name, Kind: v.Kind,
				ID: viewID(b.Name, v.Name)}
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
	bases, recs, err := s.viewData()
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
			s.respond(w, r, res, err)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "そのビューは無い"})
}
