// Package store は Camp の SQLite を開き、スキーマを管理する。
package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// DB は Camp のデータベースハンドル。
type DB struct {
	*sql.DB
	Path string
}

// pragmas は接続ごとに必ず当てる設定。
// foreign_keys は接続単位なので、プールの各接続に効くよう DSN で渡す。
var pragmas = []string{
	"_pragma=journal_mode(WAL)",
	"_pragma=foreign_keys(1)",
	"_pragma=busy_timeout(5000)",
	"_pragma=synchronous(NORMAL)",
}

// Open は path のデータベースを開く。ファイルが無ければ作る。
func Open(path string) (*DB, error) {
	dsn := path
	for i, p := range pragmas {
		sep := "&"
		if i == 0 {
			sep = "?"
		}
		dsn += sep + p
	}

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping %s: %w", path, err)
	}

	// 書き込みは単一接続に寄せる。SQLite の writer は1つしか居られないので、
	// プールに複数の writer を持たせても待たされるだけで得がない。
	sqlDB.SetMaxOpenConns(1)

	return &DB{DB: sqlDB, Path: path}, nil
}

// Version は SQLite 本体のバージョンを返す。
func (db *DB) Version() (string, error) {
	var v string
	err := db.QueryRow("select sqlite_version()").Scan(&v)
	return v, err
}
