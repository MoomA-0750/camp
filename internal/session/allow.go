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
	ID int64 `json:"id"`
	// Host は ssh の Host 名。空ならこのマシン。
	Host    string `json:"host,omitempty"`
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
		if i := strings.Index(e.Path, ":"); i > 0 && !strings.HasPrefix(e.Path, "/") {
			return fmt.Sprintf(
				"%s で許した場所が無いので起こせない（%s）。先に足す: campd allow add -host %s <dir>",
				e.Path[:i], e.Path, e.Path[:i])
		}
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

// ---------------------------------------------------------------- 向こうの場所

// AddRemoteAllowed は向こうの場所を1つ許す。**台帳に載っている接続先だけ。**
//
// campd は向こうのパスを実パスに直せないので、正規化だけして書かれたとおりに入れる。
// 実パスへは起こすときに向こうで直す（remote.go の wrapperScript）。
func AddRemoteAllowed(db *store.DB, host, p, note, by string) (Allowed, error) {
	if err := validAlias(host); err != nil {
		return Allowed{}, err
	}
	if _, err := getDestination(db, host); err != nil {
		return Allowed{}, err
	}
	c, err := cleanRemotePath(p)
	if err != nil {
		return Allowed{}, err
	}
	at := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`
		insert into allowed_remote_cwd(host, path, note, added_at, added_by) values(?,?,?,?,?)
		on conflict(host, path) do update set note=excluded.note`,
		host, c, note, at, by); err != nil {
		return Allowed{}, err
	}
	a := Allowed{Host: host}
	err = db.QueryRow(`
		select id, path, coalesce(note,''), added_at, added_by
		from allowed_remote_cwd where host=? and path=?`, host, c).
		Scan(&a.ID, &a.Path, &a.Note, &a.AddedAt, &a.AddedBy)
	return a, err
}

// RemoveRemoteAllowed は向こうの場所を1つ外す。
func RemoveRemoteAllowed(db *store.DB, host, p string) (bool, error) {
	r, err := db.Exec(`delete from allowed_remote_cwd where host=? and path=?`, host, p)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n > 0, nil
}

// ListRemoteAllowed は向こうの場所を返す。host が空なら全部。
func ListRemoteAllowed(db *store.DB, host string) ([]Allowed, error) {
	q := `select id, host, path, coalesce(note,''), added_at, added_by from allowed_remote_cwd`
	var args []any
	if host != "" {
		q += ` where host=?`
		args = append(args, host)
	}
	rows, err := db.Query(q+` order by host, path`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Allowed{}
	for rows.Next() {
		var a Allowed
		if err := rows.Scan(&a.ID, &a.Host, &a.Path, &a.Note, &a.AddedAt, &a.AddedBy); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAllAllowed はこのマシンの場所と向こうの場所をまとめて返す。画面用。
func ListAllAllowed(db *store.DB) ([]Allowed, error) {
	local, err := ListAllowed(db)
	if err != nil {
		return nil, err
	}
	remote, err := ListRemoteAllowed(db, "")
	if err != nil {
		return nil, err
	}
	return append(local, remote...), nil
}

// matchedRemoteRoot は cwd を通した向こうの行を返す。**既定は deny。**
//
// ここで見るのは文字の上だけ。symlink で外へ出る道は、向こうの sh が
// 実パスに直してから塞ぐ（許した行のほうも向こうで直して比べる）。
func matchedRemoteRoot(db *store.DB, host, cwd string) (string, error) {
	list, err := ListRemoteAllowed(db, host)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "", ErrNotAllowed{Path: host + ":" + cwd, Empty: true}
	}
	best := ""
	for _, a := range list {
		if under(cwd, a.Path) && len(a.Path) > len(best) {
			best = a.Path
		}
	}
	if best == "" {
		return "", ErrNotAllowed{Path: host + ":" + cwd}
	}
	return best, nil
}

// checkRemote は向こうに起こしてよいかを見る。**照合するのは campd 側。**
//
// 返すのは正規化した場所、それを通した行、実行面へ渡す行き先。
func checkRemote(db *store.DB, agent, host, cwd string) (string, string, *RemoteSpec, error) {
	if err := validAlias(host); err != nil {
		return "", "", nil, err
	}
	d, err := getDestination(db, host)
	if err != nil {
		return "", "", nil, err
	}
	if !d.Allowed {
		return "", "", nil, fmt.Errorf("まだ許していない接続先: %s", host)
	}
	if !d.Pinned.Pinned() {
		return "", "", nil, fmt.Errorf(
			"%s は行き先（ホスト鍵）を固定しないまま許されている（固定の無いときに許したもの）。許し直す", host)
	}
	c, err := cleanRemotePath(cwd)
	if err != nil {
		return "", "", nil, err
	}
	root, err := matchedRemoteRoot(db, host, c)
	if err != nil {
		return "", "", nil, err
	}
	return c, root, &RemoteSpec{Alias: host, Pin: *d.Pinned, Bin: d.AgentPaths[agent]}, nil
}

// checkRecord は向こうのホストの記録を読んでよいかを見る（M47）。**照合するのは campd 側。**
//
// checkRemote と違い、作業場所は見ない——読むのは記録の置き場だけで、そこは向こうの sh が
// $HOME と環境変数から解決する。代わりに**記録の台帳の行**を見る: 起こしてよい接続先でも、
// 記録を読むかは別に選ぶ（行が無ければ読まない。本人の決定 2026-09-12）。
//
// 「いまの行き先が固定と同じか」はここでは見られない（`ssh -G` を引けるのは実行面だけ）。
// 実行面が繋ぐ前に照らす（recDial）。
func checkRecord(db *store.DB, agent, host string) (*RemoteSpec, error) {
	if err := validAlias(host); err != nil {
		return nil, err
	}
	d, err := getDestination(db, host)
	if err != nil {
		return nil, err
	}
	if !d.Allowed {
		return nil, fmt.Errorf("まだ許していない接続先: %s", host)
	}
	if !d.Pinned.Pinned() {
		return nil, fmt.Errorf(
			"%s は行き先（ホスト鍵）を固定しないまま許されている。許し直す", host)
	}
	on, err := recordRootEnabled(db, host, agent)
	if err != nil {
		return nil, err
	}
	if !on {
		return nil, fmt.Errorf("%s の %s の記録は読まない設定（台帳に行が無いか、止めてある）", host, agent)
	}
	return &RemoteSpec{Alias: host, Pin: *d.Pinned}, nil
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
