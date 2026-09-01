package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration は 1 つのマイグレーション。ファイル名の先頭の連番で順序が決まる。
type Migration struct {
	Name string
	SQL  string
}

func loadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Name: e.Name(), SQL: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Migrate は未適用のマイグレーションを順に当てる。冪等。
// 適用済みの記録と DDL の実行は同じトランザクションに入れる。
// 途中で落ちても「当たったのに未記録」という状態を作らないため。
func (db *DB) Migrate() (applied []string, err error) {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	done, err := db.appliedSet()
	if err != nil {
		return nil, err
	}

	migrations, err := loadMigrations()
	if err != nil {
		return nil, err
	}

	for _, m := range migrations {
		if done[m.Name] {
			continue
		}
		if err := db.applyOne(m); err != nil {
			return applied, err
		}
		applied = append(applied, m.Name)
	}
	return applied, nil
}

func (db *DB) appliedSet() (map[string]bool, error) {
	rows, err := db.Query("SELECT name FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	done := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		done[n] = true
	}
	return done, rows.Err()
}

// noFKDirective が先頭付近にあるマイグレーションは、外部キーを切って適用する。
//
// FK参照を持つテーブルを作り直すには SQLite 公式手順どおり foreign_keys を
// 切る必要がある。PRAGMA foreign_keys はトランザクション内では効かず、
// defer_foreign_keys も「親テーブルを DROP して同名で作り直す」場合には
// 遅延カウンタが戻らずコミットに失敗する（実際にこれで 787 に当たった）。
//
// 切りっぱなしにはしない。コミット前に foreign_key_check を必ず走らせ、
// 1件でも違反があればロールバックする。
const noFKDirective = "-- camp:no-foreign-keys"

func (m Migration) needsFKOff() bool {
	head := m.SQL
	if len(head) > 512 {
		head = head[:512]
	}
	return strings.Contains(head, noFKDirective)
}

func (db *DB) applyOne(m Migration) error {
	ctx := context.Background()

	// 接続を固定する。PRAGMA は接続単位なので、プールに任せると
	// 切ったつもりの設定が別の接続に当たらない。
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("conn %s: %w", m.Name, err)
	}
	defer conn.Close()

	if m.needsFKOff() {
		if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
			return fmt.Errorf("%s: 外部キーを切れない: %w", m.Name, err)
		}
		defer conn.ExecContext(ctx, "PRAGMA foreign_keys = ON")
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin %s: %w", m.Name, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("apply %s: %w", m.Name, err)
	}

	if m.needsFKOff() {
		bad, err := fkViolations(ctx, tx)
		if err != nil {
			return fmt.Errorf("%s: foreign_key_check: %w", m.Name, err)
		}
		if bad > 0 {
			return fmt.Errorf("%s: 外部キー違反が %d 件。適用を取り消した", m.Name, bad)
		}
	}

	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations(name, applied_at) VALUES(?, ?)",
		m.Name, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("record %s: %w", m.Name, err)
	}
	return tx.Commit()
}

func fkViolations(ctx context.Context, tx *sql.Tx) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, "SELECT count(*) FROM pragma_foreign_key_check").Scan(&n)
	return n, err
}

// AppliedMigrations は適用済みのマイグレーション名を古い順に返す。
func (db *DB) AppliedMigrations() ([]string, error) {
	rows, err := db.Query("SELECT name FROM schema_migrations ORDER BY name")
	if err != nil {
		if isNoTable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func isNoTable(err error) bool {
	return err != nil && !errors.Is(err, sql.ErrNoRows) &&
		strings.Contains(err.Error(), "no such table")
}
