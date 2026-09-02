// Package files は「どのノートを、どのターンで触ったか」を引く。
package files

import (
	"strings"

	"github.com/MoomA-0750/camp/internal/store"
)

// Touch は session_files の1行を、人が読める形にしたもの。
type Touch struct {
	AbsPath   string
	RelPath   string
	Op        string
	Origin    string
	At        string
	SessionID string
	// MessageUUID は「触ったターン」そのもの。これがあれば thread から
	// 前後の会話をそのまま引ける。
	MessageUUID string
	Title       string
	Backup      string
}

// Opts は絞り込み。空の項目は絞らない。
type Opts struct {
	Path    string // abs_path の部分一致（大文字小文字は区別する）
	Session string // セッションID（前方一致）
	Op      string
	Limit   int
}

const touchSQL = `
	select f.abs_path, coalesce(f.rel_path, ''), f.op, f.origin, f.at,
	       f.session_id, coalesce(m.uuid, ''), coalesce(f.backup_name, ''),
	       coalesce(nullif(s.ai_title, ''), nullif(s.user_title, ''), '')
	  from session_files f
	  left join messages m on m.id = f.message_id
	  join sessions s on s.id = f.session_id
	 where (? = '' or instr(f.abs_path, ?) > 0)
	   and (? = '' or f.session_id like ? || '%')
	   and (? = '' or f.op = ?)
	 order by f.at desc, f.id desc
	 limit ?`

// Touches はファイルに触った記録を新しい順に返す。
func Touches(db *store.DB, o Opts) ([]Touch, error) {
	if o.Limit <= 0 {
		o.Limit = 50
	}
	rows, err := db.Query(touchSQL,
		o.Path, o.Path, o.Session, o.Session, o.Op, o.Op, o.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Touch
	for rows.Next() {
		var t Touch
		if err := rows.Scan(&t.AbsPath, &t.RelPath, &t.Op, &t.Origin, &t.At,
			&t.SessionID, &t.MessageUUID, &t.Backup, &t.Title); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PathSummary は1つのパスについての要約。
type PathSummary struct {
	AbsPath  string
	Sessions int
	Touches  int
	Ops      string
	First    string
	Last     string
}

const summarySQL = `
	select abs_path, count(distinct session_id), count(*),
	       group_concat(distinct op), min(at), max(at)
	  from session_files
	 where (? = '' or instr(abs_path, ?) > 0)
	   and (? = '' or session_id like ? || '%')
	 group by abs_path
	 order by count(*) desc, abs_path
	 limit ?`

// Summarize はパスごとに畳んで返す。「このセッションは何を触ったか」を見る用。
func Summarize(db *store.DB, o Opts) ([]PathSummary, error) {
	if o.Limit <= 0 {
		o.Limit = 50
	}
	rows, err := db.Query(summarySQL, o.Path, o.Path, o.Session, o.Session, o.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PathSummary
	for rows.Next() {
		var s PathSummary
		if err := rows.Scan(&s.AbsPath, &s.Sessions, &s.Touches, &s.Ops, &s.First, &s.Last); err != nil {
			return nil, err
		}
		s.Ops = strings.ReplaceAll(s.Ops, ",", " ")
		out = append(out, s)
	}
	return out, rows.Err()
}
