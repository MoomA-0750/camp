package mcp

import (
	"sync"
	"time"

	"github.com/MoomA-0750/camp/internal/vault"
	"github.com/MoomA-0750/camp/internal/views"
)

// viewData はビューの材料を持ち回す。HTTP 側と同じ理由——
// LoadRecords は全ノート＋87,401件のプロパティを読むので、
// 道具を呼ぶたびに走らせると遅い。索引が変わるまで使い回す。
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
	s.cache.mu.Lock()
	defer s.cache.mu.Unlock()

	var root, scanned string
	if err := s.db.QueryRow(
		`select root, coalesce(scanned_at,'') from vaults order by id limit 1`).
		Scan(&root, &scanned); err != nil {
		return nil, nil, nil, err
	}
	rev, err := views.Revision(s.db, 1)
	if err != nil {
		return nil, nil, nil, err
	}
	if s.cache.recs != nil && s.cache.scanned == scanned && s.cache.rev == rev &&
		time.Since(s.cache.at) < viewCacheTTL {
		return s.cache.bases, s.cache.recs, s.cache.links, nil
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
	s.cache.bases, s.cache.recs, s.cache.links = bases, recs, links
	s.cache.at, s.cache.scanned, s.cache.rev = time.Now(), scanned, rev
	return bases, recs, links, nil
}
