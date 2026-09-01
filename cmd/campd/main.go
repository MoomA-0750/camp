// campd は Camp のバックエンド。単一バイナリで取り込み・API・セッション駆動を担う。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/MoomA-0750/camp/internal/store"
)

// Version はビルド時に -ldflags で埋める。
var Version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "campd:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println("campd", Version)
		return nil
	case "migrate":
		return cmdMigrate(rest)
	case "doctor":
		return cmdDoctor(rest)
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `campd - Camp backend

usage:
  campd version           バージョンを表示する
  campd migrate [-db P]   スキーマを最新まで適用する（冪等）
  campd doctor  [-db P]   DB の状態を点検する
`)
}

func cmdMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	applied, err := db.Migrate()
	if err != nil {
		return err
	}

	ver, err := db.Version()
	if err != nil {
		return err
	}

	fmt.Printf("db      %s\nsqlite  %s\n", db.Path, ver)
	if len(applied) == 0 {
		fmt.Println("applied なし（すでに最新）")
		return nil
	}
	for _, name := range applied {
		fmt.Println("applied", name)
	}
	return nil
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	verbose := fs.Bool("v", false, "テーブルごとの行数も出す")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	checks, err := db.Doctor()
	if err != nil {
		return err
	}

	failed := 0
	for _, c := range checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
			failed++
		}
		fmt.Printf("%s  %-24s %s\n", mark, c.Name, c.Detail)
	}

	if *verbose {
		counts, err := db.TableCounts()
		if err != nil {
			return err
		}
		names := make([]string, 0, len(counts))
		for n := range counts {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Println()
		for _, n := range names {
			fmt.Printf("      %-24s %d\n", n, counts[n])
		}
	}

	if failed > 0 {
		return fmt.Errorf("%d 件の点検に失敗", failed)
	}
	return nil
}

// defaultDBPath は CAMP_DB があればそれを、無ければ ./data/camp.sqlite を返す。
func defaultDBPath() string {
	if p := os.Getenv("CAMP_DB"); p != "" {
		return p
	}
	return filepath.Join("data", "camp.sqlite")
}
