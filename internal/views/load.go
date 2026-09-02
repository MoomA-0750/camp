package views

import (
	"sort"
	"strings"

	"github.com/MoomA-0750/camp/internal/store"
)

// LoadRecords は Vault のノートを式から見える形にして返す。
//
// note_props を一括で読んで組み立てる。ノート1件ごとに引くと、
// Health だけで 2,687行 × 102列 の往復になる。
func LoadRecords(db *store.DB, vaultID int64) ([]*Record, error) {
	byID := map[int64]*Record{}
	var order []int64

	rows, err := db.Query(`
		select id, path, coalesce(title,''), coalesce(ext,''), coalesce(mtime,'')
		  from notes
		 where vault_id = ? and missing_at is null and kind = 'markdown'
		 order by path`, vaultID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.NoteID, &r.NPath, &r.NName, &r.NExt, &r.MTime); err != nil {
			rows.Close()
			return nil, err
		}
		r.NExt = strings.TrimPrefix(r.NExt, ".")
		r.Props = map[string]Value{}
		byID[r.NoteID] = &r
		order = append(order, r.NoteID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	prows, err := db.Query(`
		select p.note_id, p.key, p.seq, coalesce(p.text,''), p.num
		  from note_props p join notes n on n.id = p.note_id
		 where n.vault_id = ? and n.missing_at is null
		 order by p.note_id, p.key, p.seq`, vaultID)
	if err != nil {
		return nil, err
	}
	// リスト値は Seq ごとに行が来る。表示は結合し、tags だけは配列として持つ。
	multi := map[int64]map[string][]string{}
	for prows.Next() {
		var id int64
		var key, text string
		var seq int
		var num *float64
		if err := prows.Scan(&id, &key, &seq, &text, &num); err != nil {
			prows.Close()
			return nil, err
		}
		r := byID[id]
		if r == nil {
			continue
		}
		if multi[id] == nil {
			multi[id] = map[string][]string{}
		}
		multi[id][key] = append(multi[id][key], text)
		if key == "tags" {
			r.Tags = append(r.Tags, text)
		}
		v := Str(text)
		if num != nil {
			v = Num(*num)
		}
		// 同じキーが複数来たら最初のものを代表値にする（比較の対象）。
		if _, ok := r.Props[key]; !ok {
			r.Props[key] = v
		}
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return nil, err
	}

	// 複数値のキーは表示用に結合し直す。
	for id, keys := range multi {
		r := byID[id]
		for k, vals := range keys {
			if len(vals) > 1 {
				r.Props[k] = Str(strings.Join(vals, ", "))
			}
		}
	}

	out := make([]*Record, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out, nil
}

// LoadBases は Vault の中の `.base` を全部読む。
func LoadBases(db *store.DB, vaultID int64, read func(rel string) ([]byte, error)) ([]*Base, error) {
	rows, err := db.Query(`
		select path from notes
		 where vault_id = ? and kind = 'base' and missing_at is null
		 order by path`, vaultID)
	if err != nil {
		return nil, err
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		paths = append(paths, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []*Base
	for _, p := range paths {
		body, err := read(p)
		if err != nil {
			continue
		}
		b, err := ParseBase(p, body)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
