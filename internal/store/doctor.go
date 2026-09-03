package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Check は健全性チェック1件の結果。
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// Doctor は DB の状態を点検する。
// システムの sqlite3 CLI は FTS5 を持たないことがあるので、
// FTS まわりの確認は必ずこちら（modernc ドライバ）を通す。
func (db *DB) Doctor() ([]Check, error) {
	var checks []Check

	add := func(name string, err error, detail string) {
		if err != nil {
			checks = append(checks, Check{Name: name, OK: false, Detail: err.Error()})
			return
		}
		checks = append(checks, Check{Name: name, OK: true, Detail: detail})
	}

	ver, err := db.Version()
	add("sqlite", err, ver)

	// **境界が実際に効いているか。**
	// M25.5 で campd を専用ユーザーへ移した。設定を忘れても気づけるように、
	// DB そのものの見え方をここで見る。全会話履歴・Vault索引・伏字化前の
	// バックアップが、他のユーザーから読めてはいけない。
	fexErr, fexDetail := db.fileExposure()
	add("DB の見え方", fexErr, fexDetail)

	var jm string
	err = db.QueryRow("PRAGMA journal_mode").Scan(&jm)
	if err == nil && jm != "wal" {
		err = fmt.Errorf("journal_mode=%s (wal を期待)", jm)
	}
	add("journal_mode", err, jm)

	var fk int
	err = db.QueryRow("PRAGMA foreign_keys").Scan(&fk)
	if err == nil && fk != 1 {
		err = fmt.Errorf("foreign_keys=%d (1 を期待)", fk)
	}
	add("foreign_keys", err, "on")

	// FTS5 モジュールが実際に使えるかを、一時テーブルを作って確かめる。
	_, err = db.Exec(`CREATE VIRTUAL TABLE temp.fts5_probe USING fts5(x)`)
	if err == nil {
		defer db.Exec(`DROP TABLE temp.fts5_probe`)
	}
	add("fts5", err, "利用可")

	// external-content の索引が本体とずれていないか。
	//
	// **rank=1 を渡す。** 引数なしの integrity-check は索引の内部整合しか見ず、
	// content 表（message_blocks）との突き合わせをしない。実測（2026-09-03）で、
	// message_blocks から行だけ消しても引数なしは通り、`messages_fts MATCH` は
	// 消したはずの語を返し続けた。**消したものが検索から引ける状態を通す点検は
	// 点検ではない。**
	_, err = db.Exec(`INSERT INTO messages_fts(messages_fts, rank) VALUES('integrity-check', 1)`)
	add("messages_fts integrity", err, "本体と整合")

	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		add("foreign_key_check", err, "")
	} else {
		n := 0
		for rows.Next() {
			n++
		}
		rows.Close()
		if n > 0 {
			err = fmt.Errorf("%d 件の外部キー違反", n)
		}
		add("foreign_key_check", err, "違反なし")
	}

	applied, err := db.AppliedMigrations()
	add("migrations", err, fmt.Sprintf("%d 件適用済み", len(applied)))

	counts, err := db.TableCounts()
	if err != nil {
		add("tables", err, "")
	} else {
		add("tables", nil, fmt.Sprintf("%d テーブル", len(counts)))
	}

	// ここから先は「戻せるか」の点検。削除を許す以上、ここが正しくないと
	// 判断の土台が無い。
	src, err := db.SourceFileStatus()
	if err != nil {
		add("source_files 実体", err, "")
	} else {
		detail := fmt.Sprintf("%d 本すべて実在", src.Total)
		if src.Gone > 0 {
			detail = fmt.Sprintf("%d 本中 %d 本が実体なし（%d 行 / %s がここにしか無い）",
				src.Total, src.Gone, src.OrphanMessages, humanBytes(src.OrphanBytes))
			if src.Stale > 0 {
				err = fmt.Errorf("%s。うち %d 本は missing_at が NULL のまま（doctor -fix で直す）",
					detail, src.Stale)
			}
		}
		add("source_files 実体", err, detail)
	}

	tomb, err := db.TombstoneStatus()
	if err != nil {
		add("tombstones", err, "")
	} else {
		if tomb.Dangling > 0 {
			err = fmt.Errorf("tombstone の無い空 raw_json が %d 行", tomb.Dangling)
		}
		add("tombstones", err, fmt.Sprintf("%d 件（うち作り直せない削除 %d 件）",
			tomb.Total, tomb.Unrecoverable))
	}

	// raw_json のバイト位置で持っている所見が、いまも中身を指しているか。
	// M22 で raw_json が不変でなくなったので、ここを見ないと気づけない。
	var dangling int
	err = db.QueryRow(`
		select count(*) from sensitive_findings f
		  join messages m on m.id = f.message_id
		 where f.byte_offset is not null
		   and f.byte_offset + coalesce(f.length, 0) > length(m.raw_json)`).Scan(&dangling)
	if err == nil && dangling > 0 {
		err = fmt.Errorf("%d 件の所見が raw_json の外を指している", dangling)
	}
	add("findings の位置", err, "中身を指している")

	// 監査ログ。追記専用が守られているか、連鎖が切れていないか。
	//
	// トリガは DROP TRIGGER で外せ、連鎖も張り直せる（2026-09-03 実測）。
	// ここで見えるのは「事故が起きていないこと」まで。
	// 改竄を止めるのは権限境界であって、この2項目ではない。
	tr, err := db.AuditTriggers()
	if err != nil {
		add("audit 追記専用", err, "")
	} else {
		detail := "アプリ経由の書き換えと削除は拒む"
		if len(tr) > 0 {
			err = fmt.Errorf("トリガが外れている: %s", strings.Join(tr, ", "))
		}
		add("audit 追記専用", err, detail)
	}

	n, err := db.AuditChain()
	add("audit 連鎖", err, fmt.Sprintf("%d 行が繋がっている（張り直された改竄は見えない）", n))

	pv, err := db.ParserVersions()
	if err != nil {
		add("parser_version", err, "")
	} else {
		parts := make([]string, 0, len(pv))
		for _, v := range sortedKeys(pv) {
			label := fmt.Sprint(v)
			if v == 0 {
				label = "不明"
			}
			parts = append(parts, fmt.Sprintf("v%s:%d", label, pv[v]))
		}
		add("parser_version", nil, strings.Join(parts, " "))
	}

	return checks, nil
}

// SourceFiles は「元ファイルが今もあるか」の点検結果。
type SourceFiles struct {
	Total          int   // 現行の source_files
	Gone           int   // ディスクに実体が無い
	Stale          int   // 実体が無いのに missing_at が NULL
	OrphanMessages int   // 実体の無いファイル由来のメッセージ
	OrphanBytes    int64 // その raw_json の合計
}

// SourceFileStatus は source_files の実体をディスクと突き合わせる。
//
// **列ではなく実体を見る。** missing_at は前回の走査時点の話でしかなく、
// そのあとに消えたファイルは NULL のまま残る。実測（2026-09-03）で
// 83本中1本が既に消えていて、DBは83本すべてあると思っていた。
func (db *DB) SourceFileStatus() (SourceFiles, error) {
	var out SourceFiles
	rows, err := db.Query(`
		select id, path, missing_at from source_files where superseded_at is null`)
	if err != nil {
		return out, err
	}
	var goneIDs []int64
	for rows.Next() {
		var id int64
		var path string
		var missing sql.NullString
		if err := rows.Scan(&id, &path, &missing); err != nil {
			rows.Close()
			return out, err
		}
		out.Total++
		if _, err := os.Stat(path); err != nil {
			out.Gone++
			goneIDs = append(goneIDs, id)
			if !missing.Valid {
				out.Stale++
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	for _, id := range goneIDs {
		var n int
		var b sql.NullInt64
		if err := db.QueryRow(`
			select count(*), sum(length(raw_json)) from messages where source_file_id = ?`,
			id).Scan(&n, &b); err != nil {
			return out, err
		}
		out.OrphanMessages += n
		out.OrphanBytes += b.Int64
	}
	return out, nil
}

// MarkMissingSourceFiles は実体の無いファイルに missing_at を入れる。
// 行は消さない（消えたことを知っているのが Camp の値打ちなので）。
func (db *DB) MarkMissingSourceFiles() (int, error) {
	rows, err := db.Query(`
		select id, path from source_files
		 where superseded_at is null and missing_at is null`)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			rows.Close()
			return 0, err
		}
		if _, err := os.Stat(path); err != nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, id := range ids {
		if _, err := db.Exec(`update source_files set missing_at = ? where id = ?`, now, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// Tombstones は削除記録の点検結果。
type Tombstones struct {
	Total         int
	Unrecoverable int
	Dangling      int // raw_json が空なのに tombstone が無いメッセージ
}

// TombstoneStatus は「消したのに記録が無い」を探す。
//
// 記録の無い削除は、あとから「元から空だったのか消したのか」を区別できない。
// このパッケージを通さずに raw_json を空にした痕跡がここに出る。
func (db *DB) TombstoneStatus() (Tombstones, error) {
	var out Tombstones
	if err := db.QueryRow(`select count(*), coalesce(sum(1-recoverable),0) from tombstones`).
		Scan(&out.Total, &out.Unrecoverable); err != nil {
		return out, err
	}
	err := db.QueryRow(`
		select count(*) from messages m
		 where length(m.raw_json) = 0
		   and not exists(select 1 from tombstones t
		                   where t.kind = 'message.raw_json' and t.ref = cast(m.id as text))`).
		Scan(&out.Dangling)
	return out, err
}

// ParserVersions は派生行を作ったパーサの世代ごとの行数を返す。0 は「不明」。
func (db *DB) ParserVersions() (map[int]int64, error) {
	rows, err := db.Query(`select parser_version, count(*) from messages group by 1 order by 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]int64{}
	for rows.Next() {
		var v int
		var n int64
		if err := rows.Scan(&v, &n); err != nil {
			return nil, err
		}
		out[v] = n
	}
	return out, rows.Err()
}

func sortedKeys(m map[int]int64) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// TableCounts は各テーブルの行数を返す（FTS の内部テーブルは除く）。
func (db *DB) TableCounts() (map[string]int64, error) {
	rows, err := db.Query(`
		SELECT name FROM sqlite_schema
		WHERE type='table'
		  AND name NOT LIKE 'sqlite_%'
		  AND name NOT LIKE 'messages_fts%'
		ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(names)

	out := make(map[string]int64, len(names))
	for _, n := range names {
		var c int64
		// テーブル名は sqlite_schema 由来なので識別子として安全。
		if err := db.QueryRow(`SELECT count(*) FROM "` + n + `"`).Scan(&c); err != nil {
			return nil, fmt.Errorf("count %s: %w", n, err)
		}
		out[n] = c
	}
	return out, nil
}

// AuditTriggers は監査ログを守るトリガのうち、外れているものの名前を返す。
// 空なら全部ある。
func (db *DB) AuditTriggers() ([]string, error) {
	rows, err := db.Query(`
		select name from sqlite_schema
		 where type='trigger' and tbl_name='audit'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		have[n] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var missing []string
	for _, w := range []string{"audit_no_delete", "audit_no_update"} {
		if !have[w] {
			missing = append(missing, w)
		}
	}
	return missing, nil
}

// AuditChain は監査ログの連鎖をたどり、抜けと書き換えを探す。
//
// internal/audit と同じ計算をここに置いているのは、store が audit を
// 参照すると import が逆流するため。**式を変えるときは両方直す。**
// audit_test の TestTheDoctorAgreesWithTheAuditPackage が食い違いを見張る。
func (db *DB) AuditChain() (int, error) {
	rows, err := db.Query(`
		select id, at, actor, action, coalesce(target,''), coalesce(session_id,''),
		       coalesce(detail_json,''), outcome, prev_hash, hash
		  from audit order by id`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	prev := ""
	first := true
	n := 0
	for rows.Next() {
		var id int64
		var at, actor, action, target, session, detail, outcome, gotPrev, hash string
		if err := rows.Scan(&id, &at, &actor, &action, &target, &session,
			&detail, &outcome, &gotPrev, &hash); err != nil {
			return n, err
		}
		n++
		if first && hash == "genesis" {
			prev, first = hash, false
			continue
		}
		first = false
		if gotPrev != prev {
			return n, fmt.Errorf("id=%d で連鎖が切れている。前の行が消されたか差し替えられた", id)
		}
		h := sha256.New()
		for _, f := range []string{prev, at, actor, action, target, session, detail, outcome} {
			h.Write([]byte(f))
			h.Write([]byte{0})
		}
		if want := hex.EncodeToString(h.Sum(nil)); want != hash {
			return n, fmt.Errorf("id=%d の中身が書き換わっている", id)
		}
		prev = hash
	}
	return n, rows.Err()
}

// fileExposure は DB ファイルとその置き場が、持ち主以外から見えるかを調べる。
//
// M25.5 の権限境界は「エージェントと同じユーザーからは届かない」ことで成り立つ。
// その前提が崩れていたら、ここで言う。
func (db *DB) fileExposure() (error, string) {
	fi, err := os.Stat(db.Path)
	if err != nil {
		return err, ""
	}
	dir := filepath.Dir(db.Path)
	di, err := os.Stat(dir)
	if err != nil {
		return err, ""
	}

	owner, uid := "?", uint32(0)
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		uid = st.Uid
		owner = fmt.Sprint(uid)
		if u, err := user.LookupId(owner); err == nil {
			owner = u.Username
		}
	}
	// **持ち主が通常のログインアカウントなら、境界はまだ入っていない。**
	// M25.5 の後は専用のシステムユーザー（uid < 1000）のものになる。
	// エージェントは人間のユーザーで動くので、そこが同じである限り
	// DB も監査ログも触れる。
	boundary := "境界 済（専用ユーザーのもの）"
	if uid >= 1000 {
		boundary = "境界 未（ログインユーザーのもの。エージェントから届く）"
	}
	detail := fmt.Sprintf("%s / ファイル %04o / 置き場 %04o / %s",
		owner, fi.Mode().Perm(), di.Mode().Perm(), boundary)

	// **落とすのはファイルの mode だけ。** 置き場が 0755 でも、ファイルが
	// 0600 なら中身は読めない（辿れて名前が見えるだけ）。置き場の緩さは
	// 念のため書き添えるにとどめる。
	if di.Mode().Perm()&0o007 != 0 {
		detail += "（置き場は誰でも辿れる）"
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("中身が持ち主以外から読める。%s", detail), detail
	}
	return nil, detail
}
