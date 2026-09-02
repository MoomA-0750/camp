package ingest

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/MoomA-0750/camp/internal/store"
)

// バックアップの参照元。version 1 は delta にしか出ず、version 2 以上は
// snapshot にしか出ない。片方だけ読むと、実体の半分以上に名前が付かない。
const (
	backupFromDelta    = "delta"
	backupFromSnapshot = "snapshot"
	backupOrphan       = "orphan"
)

// codecRaw / codecGzip は blobs.content の入れ方。
const (
	codecRaw  = "raw"
	codecGzip = "gzip"
)

// BackupResult は捕獲の結果。
type BackupResult struct {
	Dir      string
	Scanned  int   // ディスクにあったブロブ
	Captured int   // 今回新しく取り込んだ
	Known    int   // すでに持っていた
	Orphans  int   // JSONL に参照が無かった
	Missing  int   // 記録はあるのに実体が消えていた（今回見つけた分）
	Bytes    int64 // 取り込んだ展開後のバイト数
	Stored   int64 // 実際に blobs へ書いたバイト数
	Deduped  int   // 同じ中身をすでに持っていた件数
}

// backupMeta は JSONL 側が持っているバックアップ1件の素性。
type backupMeta struct {
	AbsPath string
	Version int64
	At      string
	Origin  string
}

// historyBackup は delta の backup、snapshot の trackedFileBackups の値。
// どちらも同じ形をしている。
type historyBackup struct {
	BackupFileName string `json:"backupFileName"`
	Version        int64  `json:"version"`
	BackupTime     string `json:"backupTime"`
	RealParentDir  string `json:"realParentDir"`
}

// historyRow は file-history-delta と file-history-snapshot の両方を受ける形。
type historyRow struct {
	TrackingPath string         `json:"trackingPath"`
	Backup       *historyBackup `json:"backup"`
	Snapshot     *struct {
		TrackedFileBackups map[string]historyBackup `json:"trackedFileBackups"`
	} `json:"snapshot"`
}

// backupKey は保管の同一性。名前だけでは別セッションのブロブと衝突する。
func backupKey(sessionID, name string) string { return sessionID + "\x00" + name }

// backupIndex は取り込み済みの JSONL から「どのブロブが何のバックアップか」を作る。
//
// SetMaxOpenConns(1) なので、Rows を開いたまま書けない。先に全部読み切る。
// 実コーパスでは delta 403 行 + snapshot 248 行で、展開しても 5 千件に満たない。
func backupIndex(db *store.DB) (map[string]backupMeta, error) {
	rows, err := db.Query(`
		select session_id, type, cast(raw_json as text)
		  from messages
		 where type in ('file-history-delta', 'file-history-snapshot')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	idx := make(map[string]backupMeta)
	for rows.Next() {
		var sess, typ, raw string
		if err := rows.Scan(&sess, &typ, &raw); err != nil {
			return nil, err
		}
		var h historyRow
		if err := json.Unmarshal([]byte(raw), &h); err != nil {
			continue // 壊れた行は飛ばす。取り込み側で raw は残っている
		}
		if h.Backup != nil {
			addBackup(idx, sess, h.TrackingPath, *h.Backup, backupFromDelta)
		}
		if h.Snapshot != nil {
			for tp, b := range h.Snapshot.TrackedFileBackups {
				addBackup(idx, sess, tp, b, backupFromSnapshot)
			}
		}
	}
	return idx, rows.Err()
}

// addBackup は1件を索引に入れる。名前が無いものは実体も無いので捨てる。
//
// 同じ (session, name) は snapshot の中で何度も繰り返し現れる（実測
// 4754 件のうち一意な追跡パスは 283）。中身は同じなので先勝ちでよい。
func addBackup(idx map[string]backupMeta, sess, trackingPath string, b historyBackup, origin string) {
	if b.BackupFileName == "" {
		return
	}
	k := backupKey(sess, b.BackupFileName)
	if _, ok := idx[k]; ok {
		return
	}
	// trackingPath は相対のことも絶対のこともある。realParentDir と
	// basename を繋ぐのが唯一の正しい組み立て方（M3 で実測）。
	var abs string
	if b.RealParentDir != "" && trackingPath != "" {
		abs = b.RealParentDir + "/" + filepath.Base(trackingPath)
	}
	idx[k] = backupMeta{AbsPath: abs, Version: b.Version, At: b.BackupTime, Origin: origin}
}

// CaptureBackups は dir 配下のバックアップ実体を DB に取り込む。
//
// dir は ~/.claude/file-history。<sessionId>/<hash>@v<N> という2階層で、
// ブロブは名前に版が入っているので一度書かれたら変わらない。だから
// すでに持っている (session, name) は中身を読まずに飛ばせる。
//
// 冪等。二度目以降は Captured 0 になる。
func CaptureBackups(db *store.DB, dir string) (*BackupResult, error) {
	res := &BackupResult{Dir: dir}

	sessions, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return res, nil // file-history が無いホストもある
	}
	if err != nil {
		return nil, err
	}

	idx, err := backupIndex(db)
	if err != nil {
		return nil, err
	}
	known, err := knownBackups(db)
	if err != nil {
		return nil, err
	}
	roots, err := projectRoots(db)
	if err != nil {
		return nil, err
	}
	live, err := liveSessions(db)
	if err != nil {
		return nil, err
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	insBlob, err := tx.Prepare(`
		insert into blobs(sha256, size, codec, content, stored_at)
		values(?,?,?,?,?) on conflict(sha256) do nothing`)
	if err != nil {
		return nil, err
	}
	defer insBlob.Close()

	insBackup, err := tx.Prepare(`
		insert into file_backups(
			session_id, backup_name, version, abs_path, rel_path,
			backup_time, sha256, origin, captured_at)
		values(?,?,?,?,?,?,?,?,?)
		on conflict(session_id, backup_name) do nothing`)
	if err != nil {
		return nil, err
	}
	defer insBackup.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	seen := make(map[string]struct{}, len(known))

	for _, s := range sessions {
		if !s.IsDir() {
			continue
		}
		sessID := s.Name()
		files, err := os.ReadDir(filepath.Join(dir, sessID))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			res.Scanned++
			key := backupKey(sessID, f.Name())
			seen[key] = struct{}{}
			if _, ok := known[key]; ok {
				res.Known++
				continue
			}
			// セッションを知らないなら外部キーを張れない。JSONL より先に
			// 実体を見つけた場合なので、次の取り込みのあとで拾えばよい。
			if _, ok := live[sessID]; !ok {
				continue
			}

			body, err := os.ReadFile(filepath.Join(dir, sessID, f.Name()))
			if err != nil {
				if os.IsNotExist(err) {
					continue // 走査中に GC された
				}
				return nil, err
			}

			sum := sha256.Sum256(body)
			hash := hex.EncodeToString(sum[:])
			codec, stored := packBlob(body)
			r, err := insBlob.Exec(hash, len(body), codec, stored, now)
			if err != nil {
				return nil, fmt.Errorf("blob %s: %w", f.Name(), err)
			}
			if n, _ := r.RowsAffected(); n > 0 {
				res.Stored += int64(len(stored))
			} else {
				res.Deduped++
			}

			meta, ok := idx[key]
			if !ok {
				meta = backupMeta{Origin: backupOrphan}
				res.Orphans++
			}
			if _, err := insBackup.Exec(sessID, f.Name(), nullInt(meta.Version),
				nullStr(meta.AbsPath), nullStr(relTo(roots[sessID], meta.AbsPath)),
				nullStr(meta.At), hash, meta.Origin, now); err != nil {
				return nil, fmt.Errorf("file_backups %s/%s: %w", sessID, f.Name(), err)
			}
			res.Captured++
			res.Bytes += int64(len(body))
		}
	}

	// 実体が消えたものに印を付ける。行は消さない。ここが「独立保持」の
	// 実際の効き目で、消えたあとも中身は blobs 側に残る。
	n, err := markMissingBackups(tx, known, seen, now)
	if err != nil {
		return nil, err
	}
	res.Missing = n

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}

// packBlob は中身を格納形に直す。縮まなかったら生のまま入れる。
func packBlob(body []byte) (codec string, out []byte) {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := zw.Write(body); err != nil {
		return codecRaw, body
	}
	if err := zw.Close(); err != nil {
		return codecRaw, body
	}
	if buf.Len() >= len(body) {
		return codecRaw, body
	}
	return codecGzip, buf.Bytes()
}

// UnpackBlob は blobs.content を元の中身に戻す。
func UnpackBlob(codec string, content []byte) ([]byte, error) {
	if codec != codecGzip {
		return content, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(content))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// knownBackups は取り込み済みの (session, name) を返す。
func knownBackups(db *store.DB) (map[string]struct{}, error) {
	rows, err := db.Query(`select session_id, backup_name from file_backups`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var s, n string
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[backupKey(s, n)] = struct{}{}
	}
	return out, rows.Err()
}

// liveSessions は sessions に居るセッションIDの集合。外部キーを張れるか判定に使う。
func liveSessions(db *store.DB) (map[string]struct{}, error) {
	rows, err := db.Query(`select id from sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out[s] = struct{}{}
	}
	return out, rows.Err()
}

// projectRoots はセッションごとのプロジェクトルート。rel_path を作るのに使う。
func projectRoots(db *store.DB) (map[string]string, error) {
	rows, err := db.Query(`
		select s.id, coalesce(p.repo_path, '')
		  from sessions s left join projects p on p.id = s.project_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var id, root string
		if err := rows.Scan(&id, &root); err != nil {
			return nil, err
		}
		out[id] = root
	}
	return out, rows.Err()
}

// markMissingBackups は今回ディスクに無かった行に missing_at を入れる。
// 一度印を付けた行は上書きしない（消えた時刻は最初に気付いたときのもの）。
func markMissingBackups(tx *sql.Tx, known, seen map[string]struct{}, now string) (int, error) {
	stmt, err := tx.Prepare(`
		update file_backups set missing_at = ?
		 where session_id = ? and backup_name = ? and missing_at is null`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	n := 0
	for k := range known {
		if _, ok := seen[k]; ok {
			continue
		}
		sess, name, ok := splitBackupKey(k)
		if !ok {
			continue
		}
		r, err := stmt.Exec(now, sess, name)
		if err != nil {
			return n, err
		}
		if c, _ := r.RowsAffected(); c > 0 {
			n++
		}
	}
	return n, nil
}

func splitBackupKey(k string) (sess, name string, ok bool) {
	i := bytes.IndexByte([]byte(k), 0)
	if i < 0 {
		return "", "", false
	}
	return k[:i], k[i+1:], true
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// DefaultFileHistoryDir は projects ルートから file-history の場所を導く。
// どちらも ~/.claude の直下に並んでいる。
func DefaultFileHistoryDir(projectsRoot string) string {
	return filepath.Join(filepath.Dir(projectsRoot), "file-history")
}

// BackfillBackupMeta は捕獲済みの行の素性（パス・版・参照元）を
// messages から作り直す。中身（blobs）には触らない。
//
// 実体はディスクからしか取れないが、素性は raw_json から導ける派生値なので、
// D-014 のとおり作り直せなければならない。パスの組み立てを直したとき、
// すでに捕獲した行にもそれを当てられる。
func BackfillBackupMeta(db *store.DB) (updated int, err error) {
	idx, err := backupIndex(db)
	if err != nil {
		return 0, err
	}
	roots, err := projectRoots(db)
	if err != nil {
		return 0, err
	}

	type row struct {
		id        int64
		sess      string
		name      string
		abs, rel  sql.NullString
		ver       sql.NullInt64
		origin    string
		wantAbs   string
		wantRel   string
		wantVer   int64
		wantOrig  string
		needsWork bool
	}

	// SetMaxOpenConns(1) なので、Rows を開いたまま Exec できない。先に読み切る。
	rows, err := db.Query(`select id, session_id, backup_name, abs_path, rel_path,
	                              version, origin from file_backups`)
	if err != nil {
		return 0, err
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.sess, &r.name, &r.abs, &r.rel, &r.ver, &r.origin); err != nil {
			rows.Close()
			return 0, err
		}
		m, ok := idx[backupKey(r.sess, r.name)]
		if !ok {
			m = backupMeta{Origin: backupOrphan}
		}
		r.wantAbs, r.wantVer, r.wantOrig = m.AbsPath, m.Version, m.Origin
		r.wantRel = relTo(roots[r.sess], m.AbsPath)
		r.needsWork = r.abs.String != r.wantAbs || r.rel.String != r.wantRel ||
			r.ver.Int64 != r.wantVer || r.origin != r.wantOrig
		if r.needsWork {
			todo = append(todo, r)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if len(todo) == 0 {
		return 0, nil
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`update file_backups
	                            set abs_path = ?, rel_path = ?, version = ?, origin = ?
	                          where id = ?`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for _, r := range todo {
		if _, err := stmt.Exec(nullStr(r.wantAbs), nullStr(r.wantRel),
			nullInt(r.wantVer), r.wantOrig, r.id); err != nil {
			return updated, err
		}
		updated++
	}
	return updated, tx.Commit()
}
