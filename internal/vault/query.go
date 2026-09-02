package vault

import (
	"database/sql"
	"strings"

	"github.com/MoomA-0750/camp/internal/ingest"
	"github.com/MoomA-0750/camp/internal/store"
)

// Vault は1つのVault。
type Vault struct {
	ID        int64  `json:"id"`
	Host      string `json:"host"`
	Name      string `json:"name"`
	Root      string `json:"root"`
	Notes     int    `json:"notes"`
	Missing   int    `json:"missing"`
	ScannedAt string `json:"scanned_at,omitempty"`
}

func Vaults(db *store.DB) ([]Vault, error) {
	rows, err := db.Query(`
		select v.id, h.name, v.name, v.root, coalesce(v.scanned_at, ''),
		       (select count(*) from notes n where n.vault_id = v.id and n.missing_at is null),
		       (select count(*) from notes n where n.vault_id = v.id and n.missing_at is not null)
		  from vaults v join hosts h on h.id = v.host_id
		 order by v.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Vault{}
	for rows.Next() {
		var v Vault
		if err := rows.Scan(&v.ID, &v.Host, &v.Name, &v.Root, &v.ScannedAt, &v.Notes, &v.Missing); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Note は一覧・詳細で返す1ノート。
type Note struct {
	ID        int64  `json:"id"`
	VaultID   int64  `json:"vault_id"`
	Path      string `json:"path"`
	Title     string `json:"title"`
	Kind      string `json:"kind"`
	Size      int64  `json:"size"`
	MTime     string `json:"mtime,omitempty"`
	Missing   string `json:"missing_at,omitempty"`
	Links     int    `json:"links"`     // このノートから出るリンク
	Backlinks int    `json:"backlinks"` // このノートへ来るリンク
	Touches   int    `json:"touches"`   // セッションが触った回数
}

// NoteOpts は一覧の絞り込み。
type NoteOpts struct {
	VaultID int64
	Folder  string
	Kind    string
	Q       string // パス・タイトルの部分一致（大文字小文字を区別する）
	Missing string // "" 全部 / "only" 消えたものだけ / "hide" 現存だけ
	Limit   int
}

// Notes はノート一覧。
//
// パスの部分一致は instr() で見る。**SQLite の LIKE は ASCII について
// 大文字小文字を区別しない**ので、LIKE で書くと `Obsidian-Vault` と
// `Obsidian-vault` が同じものとして当たってしまう。
func Notes(db *store.DB, o NoteOpts) ([]Note, error) {
	if o.Limit <= 0 || o.Limit > 2000 {
		o.Limit = 200
	}
	missing := ""
	switch o.Missing {
	case "only":
		missing = " and n.missing_at is not null"
	case "hide":
		missing = " and n.missing_at is null"
	}
	rows, err := db.Query(`
		select n.id, n.vault_id, n.path, coalesce(n.title,''), coalesce(n.kind,''),
		       coalesce(n.size,0), coalesce(n.mtime,''), coalesce(n.missing_at,''),
		       (select count(*) from note_links l where l.from_note_id = n.id),
		       (select count(*) from note_links l where l.to_note_id = n.id),
		       (select count(*) from session_files sf join vaults v on v.id = n.vault_id
		         where sf.abs_path = v.root || '/' || n.path)
		  from notes n
		 where (? = 0 or n.vault_id = ?)
		   and (? = '' or instr(n.path, ?) = 1)
		   and (? = '' or n.kind = ?)
		   and (? = '' or instr(n.path, ?) > 0)`+missing+`
		 order by n.path
		 limit ?`,
		o.VaultID, o.VaultID, o.Folder, o.Folder, o.Kind, o.Kind, o.Q, o.Q, o.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Note{}
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.VaultID, &n.Path, &n.Title, &n.Kind, &n.Size,
			&n.MTime, &n.Missing, &n.Links, &n.Backlinks, &n.Touches); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// OneNote は1ノートのメタ情報。
func OneNote(db *store.DB, id int64) (*Note, error) {
	var n Note
	err := db.QueryRow(`
		select n.id, n.vault_id, n.path, coalesce(n.title,''), coalesce(n.kind,''),
		       coalesce(n.size,0), coalesce(n.mtime,''), coalesce(n.missing_at,''),
		       (select count(*) from note_links l where l.from_note_id = n.id),
		       (select count(*) from note_links l where l.to_note_id = n.id),
		       (select count(*) from session_files sf join vaults v on v.id = n.vault_id
		         where sf.abs_path = v.root || '/' || n.path)
		  from notes n where n.id = ?`, id).Scan(
		&n.ID, &n.VaultID, &n.Path, &n.Title, &n.Kind, &n.Size,
		&n.MTime, &n.Missing, &n.Links, &n.Backlinks, &n.Touches)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &n, err
}

// NoteBody はノートの中身を返す。**Vault のファイルではなく blobs から読む。**
// ノートが消えていても中身は返せる。
func NoteBody(db *store.DB, id int64) ([]byte, error) {
	var codec string
	var content []byte
	err := db.QueryRow(`
		select b.codec, b.content from notes n join blobs b on b.sha256 = n.sha256
		 where n.id = ?`, id).Scan(&codec, &content)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ingest.UnpackBlob(codec, content)
}

// Ref は1本のリンク（出ていく側／来る側の両方でこの形）。
type Ref struct {
	FromID     int64    `json:"from_id"`
	FromPath   string   `json:"from_path"`
	ToID       int64    `json:"to_id,omitempty"`
	ToPath     string   `json:"to_path,omitempty"`
	Target     string   `json:"target"`
	Alias      string   `json:"alias,omitempty"`
	Frag       string   `json:"frag,omitempty"`
	Embed      bool     `json:"embed"`
	Resolved   bool     `json:"resolved"`
	Ambiguous  bool     `json:"ambiguous"`
	Candidates []string `json:"candidates,omitempty"`
	Line       int      `json:"line,omitempty"`
}

const refCols = `
	l.from_note_id, f.path, coalesce(l.to_note_id, 0), coalesce(t.path, ''),
	l.raw_target, coalesce(l.alias,''), coalesce(l.frag,''),
	l.embed, l.resolved, l.ambiguous, coalesce(l.candidates,''), coalesce(l.line, 0)`

func scanRefs(rows *sql.Rows) ([]Ref, error) {
	defer rows.Close()
	out := []Ref{}
	for rows.Next() {
		var r Ref
		var cands string
		if err := rows.Scan(&r.FromID, &r.FromPath, &r.ToID, &r.ToPath, &r.Target,
			&r.Alias, &r.Frag, &r.Embed, &r.Resolved, &r.Ambiguous, &cands, &r.Line); err != nil {
			return nil, err
		}
		if cands != "" {
			r.Candidates = strings.Split(cands, "\n")
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// OutLinks はこのノートから出ていくリンク。
func OutLinks(db *store.DB, noteID int64) ([]Ref, error) {
	rows, err := db.Query(`select`+refCols+`
		  from note_links l
		  join notes f on f.id = l.from_note_id
		  left join notes t on t.id = l.to_note_id
		 where l.from_note_id = ? order by l.line`, noteID)
	if err != nil {
		return nil, err
	}
	return scanRefs(rows)
}

// Backlinks はこのノートへ来るリンク。
func Backlinks(db *store.DB, noteID int64) ([]Ref, error) {
	rows, err := db.Query(`select`+refCols+`
		  from note_links l
		  join notes f on f.id = l.from_note_id
		  left join notes t on t.id = l.to_note_id
		 where l.to_note_id = ? order by f.path`, noteID)
	if err != nil {
		return nil, err
	}
	return scanRefs(rows)
}

// Ambiguous は曖昧に解決したリンクを全部返す。
// 「Obsidian と食い違うかもしれない場所」の一覧。
func Ambiguous(db *store.DB, vaultID int64) ([]Ref, error) {
	rows, err := db.Query(`select`+refCols+`
		  from note_links l
		  join notes f on f.id = l.from_note_id
		  left join notes t on t.id = l.to_note_id
		 where l.ambiguous = 1 and (? = 0 or f.vault_id = ?)
		 order by l.raw_target, f.path`, vaultID, vaultID)
	if err != nil {
		return nil, err
	}
	return scanRefs(rows)
}

// Dangling は解決先の無いリンク。
func Dangling(db *store.DB, vaultID int64) ([]Ref, error) {
	rows, err := db.Query(`select`+refCols+`
		  from note_links l
		  join notes f on f.id = l.from_note_id
		  left join notes t on t.id = l.to_note_id
		 where l.resolved = 0 and (? = 0 or f.vault_id = ?)
		 order by l.raw_target, f.path`, vaultID, vaultID)
	if err != nil {
		return nil, err
	}
	return scanRefs(rows)
}
