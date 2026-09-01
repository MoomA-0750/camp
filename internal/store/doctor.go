package store

import (
	"fmt"
	"sort"
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
	_, err = db.Exec(`INSERT INTO messages_fts(messages_fts) VALUES('integrity-check')`)
	add("messages_fts integrity", err, "整合")

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

	return checks, nil
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
