package noteedit

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"

	"github.com/MoomA-0750/camp/internal/vault"
)

// プレビューの wikilink の行き先（Phase 5 / M54）。**解決の規則は resolve.go の1つだけ**——画面に二重に書かない。

// 1回に訊ける宛先の数と長さ（Fable の M54 設計レビュー 6）。
const (
	MaxTargets   = 500
	MaxTargetLen = 512
)

// Target は1つの宛先の解決。
type Target struct {
	ToID       int64    `json:"to_id,omitempty"`
	ToPath     string   `json:"to_path,omitempty"`
	Ambiguous  bool     `json:"ambiguous,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
}

// ErrBadRequest は訊き方が悪い（宛先が多すぎる・長すぎる）。
var ErrBadRequest = errors.New("訊き方が悪い")

type linkCache struct {
	mu  sync.Mutex
	key map[int64]string
	ix  map[int64]*vault.LinkIndex
	ids map[int64]map[string]int64
}

// Resolve はノート noteID から見た宛先を解決する。空の宛先（`[[#見出し]]`）は自分を指す。
//
// 索引は Vault の行の世代（数・最大の id・索引した時刻）で覚える。全部の行を毎回読み直さない。
func (s *Service) Resolve(noteID int64, targets []string) (map[string]Target, error) {
	if len(targets) > MaxTargets {
		return nil, fmt.Errorf("%w: 宛先が多すぎる（%d 個まで）", ErrBadRequest, MaxTargets)
	}
	n, err := vault.OneNote(s.DB, noteID)
	if err != nil {
		return nil, err
	}
	if n == nil {
		return nil, fs.ErrNotExist
	}
	ix, ids, _, err := s.linkIndexKey(n.VaultID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Target, len(targets))
	for _, t := range targets {
		if len(t) > MaxTargetLen {
			return nil, fmt.Errorf("%w: 宛先が長すぎる", ErrBadRequest)
		}
		if strings.TrimSpace(t) == "" {
			out[t] = Target{ToID: n.ID, ToPath: n.Path}
			continue
		}
		r := ix.Resolve(n.Path, t)
		if r.To == "" {
			out[t] = Target{}
			continue
		}
		out[t] = Target{ToID: ids[r.To], ToPath: r.To, Ambiguous: r.Ambiguous, Candidates: r.Candidates}
	}
	return out, nil
}

func (s *Service) linkIndexKey(vaultID int64) (*vault.LinkIndex, map[string]int64, string, error) {
	var count, maxID int64
	var scanned string
	if err := s.DB.QueryRow(`select count(*), coalesce(max(n.id), 0), coalesce(v.scanned_at, '')
		from vaults v left join notes n on n.vault_id = v.id and n.missing_at is null where v.id = ?`, vaultID).
		Scan(&count, &maxID, &scanned); err != nil {
		return nil, nil, "", err
	}
	key := fmt.Sprintf("%d/%d/%s", count, maxID, scanned)
	cache := &s.links
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.key == nil {
		cache.key, cache.ix, cache.ids = map[int64]string{}, map[int64]*vault.LinkIndex{}, map[int64]map[string]int64{}
	}
	if cache.key[vaultID] == key {
		return cache.ix[vaultID], cache.ids[vaultID], key, nil
	}
	rows, err := s.DB.Query(`select id, path from notes where vault_id = ? and missing_at is null`, vaultID)
	if err != nil {
		return nil, nil, "", err
	}
	defer rows.Close()
	ids := map[string]int64{}
	var paths []string
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			return nil, nil, "", err
		}
		ids[p] = id
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, "", err
	}
	ix := vault.NewLinkIndex(paths)
	cache.key[vaultID], cache.ix[vaultID], cache.ids[vaultID] = key, ix, ids
	return ix, ids, key, nil
}
