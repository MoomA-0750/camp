// campd は Camp のバックエンド。単一バイナリで取り込み・API・セッション駆動を担う。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/MoomA-0750/camp/internal/ingest"
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
	case "scan":
		return cmdScan(rest)
	case "ingest":
		return cmdIngest(rest)
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
  campd scan    [-root D] 会話記録を読んで実測レポートを出す（DBには書かない）
  campd ingest  [-root D] 会話記録を DB に取り込む（再実行しても重複しない）
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

func cmdScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	root := fs.String("root", defaultClaudeProjects(), "~/.claude/projects 相当のディレクトリ")
	if err := fs.Parse(args); err != nil {
		return err
	}

	rep, err := ingest.ScanDir(*root)
	if err != nil {
		return err
	}
	fmt.Printf("root            %s\n", *root)
	rep.Print(os.Stdout)

	if len(rep.ParseErrors) > 0 {
		return fmt.Errorf("%d ファイルでパースに失敗", len(rep.ParseErrors))
	}
	return nil
}

func cmdIngest(args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	root := fs.String("root", defaultClaudeProjects(), "~/.claude/projects 相当のディレクトリ")
	host := fs.String("host", defaultHost(), "このコーパスを持つホスト名")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if _, err := db.Migrate(); err != nil {
		return err
	}

	started := time.Now()
	res, err := ingest.Ingest(db, *host, *root)
	if err != nil {
		return err
	}

	fmt.Printf("host            %s\nroot            %s\n\n", res.Host, res.Root)
	roles := make([]string, 0, len(res.RoleCounts))
	for r := range res.RoleCounts {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	fmt.Println("ファイルの役割:")
	for _, r := range roles {
		fmt.Printf("  %-16s %d\n", r, res.RoleCounts[r])
	}
	fmt.Printf("\nprojects        %d（cwd %d 個から）\nsessions        %d\nruns            %d\nsession_runs    %d\nsource_files    %d\nmessages 追加   %d\n",
		res.Projects, res.CWDs, res.Sessions, res.Runs, res.SessionRuns, res.SourceFiles, res.Messages)
	if res.Reread > 0 {
		fmt.Printf("世代を進めた   %d\n", res.Reread)
	}
	if res.Missing > 0 {
		fmt.Printf("消えていた     %d（行は残す）\n", res.Missing)
	}
	fmt.Printf("読み飛ばし      %d ファイル（追記なし）\n", res.Unchanged)
	fmt.Printf("所要            %s\n", time.Since(started).Round(time.Millisecond))
	return nil
}

func defaultHost() string {
	if h := os.Getenv("CAMP_HOST"); h != "" {
		return h
	}
	h, err := os.Hostname()
	if err != nil {
		return "localhost"
	}
	return h
}

func defaultClaudeProjects() string {
	if p := os.Getenv("CAMP_CLAUDE_PROJECTS"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".claude/projects"
	}
	return filepath.Join(home, ".claude", "projects")
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
