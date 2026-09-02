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
}

const viewCacheTTL = 30 * time.Second

func (s *Server) viewData() ([]*views.Base, []*views.Record, error) {
	s.cache.mu.Lock()
	defer s.cache.mu.Unlock()

	var root, scanned string
	if err := s.db.QueryRow(
		`select root, coalesce(scanned_at,'') from vaults order by id limit 1`).
		Scan(&root, &scanned); err != nil {
		return nil, nil, err
	}
	if s.cache.recs != nil && s.cache.scanned == scanned &&
		time.Since(s.cache.at) < viewCacheTTL {
		return s.cache.bases, s.cache.recs, nil
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
	s.cache.bases, s.cache.recs = bases, recs
	s.cache.at, s.cache.scanned = time.Now(), scanned
	return bases, recs, nil
}
