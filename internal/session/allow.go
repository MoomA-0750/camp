package session

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/MoomA-0750/camp/internal/store"
)

// Allowed は許可された cwd 1件。
type Allowed struct {
	ID      int64  `json:"id"`
	Path    string `json:"path"`
	Note    string `json:"note,omitempty"`
	AddedAt string `json:"added_at"`
	AddedBy string `json:"added_by"`
}

// ErrNotAllowed は許可リストに無い cwd。
type ErrNotAllowed struct {
	Path string
	// Empty は許可リストが空だったか。**「まだ設定していない」と
	// 「その場所が外れている」を区別する。**
	Empty bool
}

func (e ErrNotAllowed) Error() string {
	if e.Empty {
		return fmt.Sprintf(
			"許可リストが空なので、どこでも起こせない（%s）。先に足す: campd allow add <dir>", e.Path)
	}
	return fmt.Sprintf("許可リストに無い場所: %s", e.Path)
}

// ListAllowed は許可リストを返す。
func ListAllowed(db *store.DB) ([]Allowed, error) {
	rows, err := db.Query(
		`select id, path, coalesce(note,''), added_at, added_by from allowed_cwd order by path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Allowed{} // **nil を返さない**（JSON で null になる）
	for rows.Next() {
		var a Allowed
		if err := rows.Scan(&a.ID, &a.Path, &a.Note, &a.AddedAt, &a.AddedBy); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AddAllowed は1つ足す。**実パスに直してから入れる。**
//
// 文字列のまま入れると、あとで symlink 越しに指されたときに照合できない。
func AddAllowed(db *store.DB, path, note, by string) (Allowed, error) {
	real, err := resolveCwd(path)
	if err != nil {
		return Allowed{}, err
	}
	at := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`
		insert into allowed_cwd(path, note, added_at, added_by) values(?,?,?,?)
		on conflict(path) do update set note=excluded.note`,
		real, note, at, by); err != nil {
		return Allowed{}, err
	}
	var a Allowed
	err = db.QueryRow(
		`select id, path, coalesce(note,''), added_at, added_by from allowed_cwd where path=?`,
		real).Scan(&a.ID, &a.Path, &a.Note, &a.AddedAt, &a.AddedBy)
	return a, err
}

// RemoveAllowed は1つ外す。
func RemoveAllowed(db *store.DB, path string) (bool, error) {
	// **外すときは実パスに直さない。** 消したい行が指すディレクトリが
	// もう無い場合、EvalSymlinks が失敗して外せなくなる。
	r, err := db.Exec(`delete from allowed_cwd where path=?`, path)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n > 0, nil
}

// CheckCwd は起こしてよい場所かを見る。返すのは実パス。
//
// **既定は deny。** 空なら何も通さない。
func CheckCwd(db *store.DB, cwd string) (string, error) {
	real, err := resolveCwd(cwd)
	if err != nil {
		return "", err
	}
	list, err := ListAllowed(db)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "", ErrNotAllowed{Path: real, Empty: true}
	}
	for _, a := range list {
		if under(real, a.Path) {
			return real, nil
		}
	}
	return "", ErrNotAllowed{Path: real}
}

// under は p が root と同じか、その下かを返す。
//
// **文字列の前方一致では駄目。** `/home/x/work` を許したときに
// `/home/x/workspace` まで通ってしまう。区切りまで含めて見る。
func under(p, root string) bool {
	if p == root {
		return true
	}
	if !strings.HasSuffix(root, string(filepath.Separator)) {
		root += string(filepath.Separator)
	}
	return strings.HasPrefix(p, root)
}
