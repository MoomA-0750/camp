package files

import (
	"database/sql"
	"fmt"

	"github.com/MoomA-0750/camp/internal/ingest"
	"github.com/MoomA-0750/camp/internal/store"
)

// Backup は捕獲済みのバックアップ1件。
type Backup struct {
	ID        int64
	AbsPath   string
	RelPath   string
	Version   int64
	At        string // backup_time。編集される直前の時刻
	SessionID string
	Title     string
	Name      string // <hash>@v<N>
	Origin    string
	Size      int64  // 展開後のバイト数
	Missing   string // 実体が消えたのを見つけた時刻。空なら実体もまだある
}

const backupSQL = `
	select b.id, coalesce(b.abs_path, ''), coalesce(b.rel_path, ''),
	       coalesce(b.version, 0), coalesce(b.backup_time, ''),
	       b.session_id, coalesce(nullif(s.ai_title, ''), nullif(s.user_title, ''), ''),
	       b.backup_name, b.origin, l.size, coalesce(b.missing_at, '')
	  from file_backups b
	  join blobs l on l.sha256 = b.sha256
	  join sessions s on s.id = b.session_id
	 where (? = '' or instr(coalesce(b.abs_path, ''), ?) > 0)
	   and (? = '' or b.session_id like ? || '%')
	 order by b.backup_time desc, b.id desc
	 limit ?`

// Backups は捕獲済みのバックアップを新しい順に返す。
func Backups(db *store.DB, o Opts) ([]Backup, error) {
	if o.Limit <= 0 {
		o.Limit = 50
	}
	rows, err := db.Query(backupSQL, o.Path, o.Path, o.Session, o.Session, o.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Backup
	for rows.Next() {
		var b Backup
		if err := rows.Scan(&b.ID, &b.AbsPath, &b.RelPath, &b.Version, &b.At,
			&b.SessionID, &b.Title, &b.Name, &b.Origin, &b.Size, &b.Missing); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BackupContent は捕獲した中身そのものを返す。
// 元のファイルが消えていても、消えたと印が付いていても読める。
func BackupContent(db *store.DB, id int64) (*Backup, []byte, error) {
	var b Backup
	var codec string
	var content []byte
	err := db.QueryRow(`
		select b.id, coalesce(b.abs_path, ''), coalesce(b.rel_path, ''),
		       coalesce(b.version, 0), coalesce(b.backup_time, ''),
		       b.session_id, '', b.backup_name, b.origin, l.size,
		       coalesce(b.missing_at, ''), l.codec, l.content
		  from file_backups b join blobs l on l.sha256 = b.sha256
		 where b.id = ?`, id).Scan(&b.ID, &b.AbsPath, &b.RelPath, &b.Version,
		&b.At, &b.SessionID, &b.Title, &b.Name, &b.Origin, &b.Size,
		&b.Missing, &codec, &content)
	if err == sql.ErrNoRows {
		return nil, nil, fmt.Errorf("バックアップ %d は持っていない", id)
	}
	if err != nil {
		return nil, nil, err
	}
	body, err := ingest.UnpackBlob(codec, content)
	if err != nil {
		return nil, nil, err
	}
	return &b, body, nil
}
