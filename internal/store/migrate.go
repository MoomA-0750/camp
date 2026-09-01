package store

import (
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

func (db *DB) applyOne(m Migration) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin %s: %w", m.Name, err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(m.SQL); err != nil {
		return fmt.Errorf("apply %s: %w", m.Name, err)
	}
	if _, err := tx.Exec(
		"INSERT INTO schema_migrations(name, applied_at) VALUES(?, ?)",
		m.Name, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("record %s: %w", m.Name, err)
	}
	return tx.Commit()
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
