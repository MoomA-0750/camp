package vault

import (
	"strings"

	"github.com/MoomA-0750/camp/internal/store"
)

// Touch は「あるセッションがあるノートを触った」1件。
type Touch struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title,omitempty"`
	Op        string `json:"op"`
	Origin    string `json:"origin"`
	At        string `json:"at"`
	NotePath  string `json:"note_path"`
	NoteID    int64  `json:"note_id,omitempty"`
	BackupID  int64  `json:"backup_id,omitempty"`
}

// 突き合わせは **abs_path の完全一致**で行う。大文字小文字を畳まない。
//
// 畳むと何が起きるかは実測してある: この Vault は改名で
// `Obsidian-vault`（小文字v）→ `Obsidian-Vault` になっており、
// 小文字側を指す記録が18パス・85行ある。畳むと、**もう存在しない
// ディレクトリへの操作を、現存するノートへの操作として表示する**ことになる。
//
// 相対パスは SQL 側で substr して (vault_id, path) の UNIQUE 索引に当てる。
// ノート側を全走査しない。
const relExpr = `substr(sf.abs_path, length(v.root) + 2)`

// NoteTouches はそのノートを触ったセッションを新しい順に返す。
func NoteTouches(db *store.DB, noteID int64, limit int) ([]Touch, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := db.Query(`
		select sf.session_id,
		       coalesce(nullif(s.ai_title,''), nullif(s.user_title,''), sf.session_id),
		       sf.op, sf.origin, sf.at, n.path, n.id,
		       coalesce(fb.id, 0)
		  from notes n
		  join vaults v on v.id = n.vault_id
		  join session_files sf
		       on sf.abs_path = v.root || '/' || n.path
		  join sessions s on s.id = sf.session_id
		  left join file_backups fb
		       on fb.session_id = sf.session_id and fb.backup_name = sf.backup_name
		 where n.id = ?
		 order by sf.at desc
		 limit ?`, noteID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Touch{}
	for rows.Next() {
		var t Touch
		if err := rows.Scan(&t.SessionID, &t.Title, &t.Op, &t.Origin, &t.At,
			&t.NotePath, &t.NoteID, &t.BackupID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SessionNotes はそのセッションが触ったノートを返す。
func SessionNotes(db *store.DB, sessionID string, limit int) ([]Touch, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := db.Query(`
		select sf.session_id, n.title, sf.op, sf.origin, max(sf.at), n.path, n.id
		  from session_files sf
		  join vaults v on instr(sf.abs_path, v.root || '/') = 1
		  join notes n on n.vault_id = v.id and n.path = `+relExpr+`
		 where sf.session_id = ?
		 group by n.id, sf.op
		 order by max(sf.at) desc
		 limit ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Touch{}
	for rows.Next() {
		var t Touch
		if err := rows.Scan(&t.SessionID, &t.Title, &t.Op, &t.Origin, &t.At,
			&t.NotePath, &t.NoteID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Ghost は「触った記録はあるが、いま Vault に無いパス」。
// **Campにしか残っていないもの**がここに出る。
type Ghost struct {
	Path     string `json:"path"`     // Vault ルートからの相対パス
	AbsPath  string `json:"abs_path"` // 記録されている絶対パス（畳んでいない）
	Reason   string `json:"reason"`   // gone / worktree / hidden / other-case
	Touches  int    `json:"touches"`
	Sessions int    `json:"sessions"`
	Last     string `json:"last_at"`
	Backups  int    `json:"backups"` // 中身が blobs に残っている版数

	// Versions は読める版。Camp にしか残っていない中身への入口。
	Versions []GhostVersion `json:"versions,omitempty"`
}

// GhostVersion は消えたパスの、ある時点の中身。
type GhostVersion struct {
	BackupID int64  `json:"backup_id"`
	Version  int    `json:"version"`
	At       string `json:"at,omitempty"`
	Size     int64  `json:"size"`
	Session  string `json:"session_id,omitempty"`
}

const (
	GhostGone      = "gone"       // Vault の下だが実体が無い
	GhostWorktree  = "worktree"   // .claude/worktrees の中のコピー
	GhostHidden    = "hidden"     // ドット始まり。索引の対象外
	GhostOtherCase = "other-case" // 表記違いのルート（改名前など）
)

// Ghosts は索引に繋がらなかった触り跡をまとめる。
func Ghosts(db *store.DB, vaultID int64) ([]Ghost, error) {
	var root string
	if err := db.QueryRow(`select root from vaults where id = ?`, vaultID).Scan(&root); err != nil {
		return nil, err
	}

	// 索引側を**先に**読み切る。SetMaxOpenConns(1) なので、外側の Rows を
	// 開いたまま内側で Query すると接続を待って止まる。
	have, err := indexedAbs(db, vaultID, root)
	if err != nil {
		return nil, err
	}
	versions, err := backupVersions(db)
	if err != nil {
		return nil, err
	}

	// 中身の版数は file_backups.abs_path から直に数える。
	//
	// session_files 側の (session_id, backup_name) 経由で数えてはいけない:
	// backup_name が入っているのは file-history 由来の行だけで、実測 2,418行中
	// 2,246行が NULL。GROUP BY abs_path が任意の1行を拾うので、ほぼ全部
	// 空振りして「中身なし」に見える（実際は Obsidian-Vault 配下に91版ある）。
	rows, err := db.Query(`
		select sf.abs_path, count(*), count(distinct sf.session_id), max(sf.at),
		       (select count(*) from file_backups fb where fb.abs_path = sf.abs_path)
		  from session_files sf
		 group by sf.abs_path
		 order by max(sf.at) desc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 表記違いのルートを見分けるための小文字版。**判定にだけ使い、
	// 保存や表示には使わない。**
	lowRoot := strings.ToLower(root) + "/"

	out := []Ghost{}
	for rows.Next() {
		var g Ghost
		if err := rows.Scan(&g.AbsPath, &g.Touches, &g.Sessions, &g.Last, &g.Backups); err != nil {
			return nil, err
		}
		if _, ok := have[g.AbsPath]; ok {
			continue
		}
		under := strings.HasPrefix(g.AbsPath, root+"/")
		lowUnder := strings.HasPrefix(strings.ToLower(g.AbsPath), lowRoot)
		switch {
		case under && strings.Contains(g.AbsPath, "/.claude/worktrees/"):
			g.Reason = GhostWorktree
		case under && strings.Contains(g.AbsPath, "/."):
			g.Reason = GhostHidden
		case under:
			g.Reason = GhostGone
		case lowUnder:
			g.Reason = GhostOtherCase
		default:
			continue // Vault と関係ないパス
		}
		if under {
			g.Path = g.AbsPath[len(root)+1:]
		} else {
			g.Path = g.AbsPath
		}
		g.Versions = versions[g.AbsPath]
		out = append(out, g)
	}
	return out, rows.Err()
}

func indexedAbs(db *store.DB, vaultID int64, root string) (map[string]struct{}, error) {
	rows, err := db.Query(`select path from notes where vault_id = ?`, vaultID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[root+"/"+p] = struct{}{}
	}
	return out, rows.Err()
}

// backupVersions は絶対パスごとの、読める版の一覧を返す。
// 中身は file_backups → blobs にあるので、ノートが消えていても取り出せる。
//
// 並びは**版番号ではなく時刻**。版番号はセッションの中でしか意味を持たず
// （同じパスを別セッションが触ると 1 から振り直される）、版順に並べると
// 「v3(08-11) v2(08-17) v2(08-11) v1(08-17)」のように時系列が壊れる。
func backupVersions(db *store.DB) (map[string][]GhostVersion, error) {
	rows, err := db.Query(`
		select fb.abs_path, fb.id, fb.version, coalesce(fb.backup_time, ''),
		       coalesce(b.size, 0), fb.session_id
		  from file_backups fb
		  left join blobs b on b.sha256 = fb.sha256
		 where fb.abs_path is not null
		 order by fb.abs_path, fb.backup_time desc, fb.version desc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]GhostVersion{}
	for rows.Next() {
		var p string
		var v GhostVersion
		if err := rows.Scan(&p, &v.BackupID, &v.Version, &v.At, &v.Size, &v.Session); err != nil {
			return nil, err
		}
		out[p] = append(out[p], v)
	}
	return out, rows.Err()
}
