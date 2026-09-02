// campd は Camp のバックエンド。単一バイナリで取り込み・API・セッション駆動を担う。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MoomA-0750/camp/internal/files"
	"github.com/MoomA-0750/camp/internal/ingest"
	"github.com/MoomA-0750/camp/internal/search"
	"github.com/MoomA-0750/camp/internal/store"
	"github.com/MoomA-0750/camp/internal/thread"
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
	case "backfill":
		return cmdBackfill(rest)
	case "search":
		return cmdSearch(rest)
	case "thread":
		return cmdThread(rest)
	case "files":
		return cmdFiles(rest)
	case "capture":
		return cmdCapture(rest)
	case "backup":
		return cmdBackup(rest)
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
  campd backfill [-db P]  messages から派生テーブル（usage・索引・ファイル結合）を作り直す
  campd search  QUERY     全文検索（日本語は2文字から引ける）
  campd thread  SESSION   会話を木に組み立てて形を見る
  campd files   [PATH]    ノートを触ったターンを引く（-session でセッション側から）
  campd capture [-dir D]  file-history の実体（編集前の中身）を DB に取り込む
  campd backup  [PATH]    捕獲したバックアップを一覧する（-show ID で中身を出す）
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
	fmt.Printf("\nprojects        %d（cwd %d 個から）\nsessions        %d\nruns            %d\nsession_runs    %d\nsource_files    %d\nmessages 追加   %d\nblocks 追加     %d\nusage 計上      %d\nファイル結合    %d（うち %d 件を発行元のターンに繋ぎ直した）\n",
		res.Projects, res.CWDs, res.Sessions, res.Runs, res.SessionRuns, res.SourceFiles, res.Messages, res.Blocks, res.Usage, res.Files, res.Relinked)
	if res.Reread > 0 {
		fmt.Printf("世代を進めた   %d\n", res.Reread)
	}
	if res.Missing > 0 {
		fmt.Printf("消えていた     %d（行は残す）\n", res.Missing)
	}
	if b := res.Backups; b != nil {
		fmt.Printf("実体の捕獲      %d 個（新規 %d・既知 %d）%s\n",
			b.Scanned, b.Captured, b.Known, missingNote(b.Missing))
	}
	fmt.Printf("読み飛ばし      %d ファイル（追記なし）\n", res.Unchanged)
	fmt.Printf("所要            %s\n", time.Since(started).Round(time.Millisecond))
	return nil
}

func cmdBackfill(args []string) error {
	fs := flag.NewFlagSet("backfill", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
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
	rows, sessions, err := ingest.BackfillUsage(db)
	if err != nil {
		return err
	}
	fmt.Printf("usage 再構築    %d 行を走査\ntotal_cost_usd  %d セッション\n", rows, sessions)

	blocks, msgs, err := ingest.BackfillBlocks(db)
	if err != nil {
		return err
	}
	fmt.Printf("索引 再構築     %d ブロック（%d メッセージから）\n", blocks, msgs)

	links, _, err := ingest.BackfillSessionFiles(db)
	if err != nil {
		return err
	}
	fmt.Printf("ファイル結合    %d 件\n", links)

	meta, err := ingest.BackfillBackupMeta(db)
	if err != nil {
		return err
	}
	fmt.Printf("バックアップ    %d 行の素性を作り直した（中身には触らない）\n所要            %s\n",
		meta, time.Since(started).Round(time.Millisecond))
	return nil
}

func cmdSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	kind := fs.String("kind", "", "ブロック種別で絞る（text / thinking / tool_use / tool_result）")
	sess := fs.String("session", "", "セッションIDで絞る")
	limit := fs.Int("n", 20, "件数")
	explain := fs.Bool("explain", false, "組み立てた MATCH 式も表示する")
	if err := fs.Parse(args); err != nil {
		return err
	}
	q := strings.Join(fs.Args(), " ")
	if strings.TrimSpace(q) == "" {
		return fmt.Errorf("検索語がない")
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if *explain {
		expr, _ := search.BuildMatch(q)
		fmt.Printf("MATCH %s\n\n", expr)
	}

	started := time.Now()
	hits, err := search.Query(db, q, search.Opts{Kind: *kind, Session: *sess, Limit: *limit})
	if err != nil {
		return err
	}
	for _, h := range hits {
		title := h.Title
		if len([]rune(title)) > 40 {
			title = string([]rune(title)[:40]) + "…"
		}
		label := h.Kind
		if h.ToolName != "" {
			label += ":" + h.ToolName
		}
		fmt.Printf("%s  %-22s %s\n  %s\n  session %s  score %.1f\n\n",
			shortTime(h.Timestamp), label, title, h.Snippet, h.SessionID[:8], h.Score)
	}
	fmt.Printf("%d 件 / %s\n", len(hits), time.Since(started).Round(time.Millisecond))
	return nil
}

func cmdThread(args []string) error {
	fs := flag.NewFlagSet("thread", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	show := fs.Int("show", 0, "先頭から何行ぶん中身を出すか")
	all := fs.Bool("all", false, "全セッションの形を一覧する")
	if err := fs.Parse(args); err != nil {
		return err
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	var ids []string
	if *all {
		rows, err := db.Query(`
			select id from sessions where conversation_count > 0
			 order by conversation_count desc`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
	} else {
		if fs.NArg() == 0 {
			return fmt.Errorf("セッションIDを指定する（前方一致でよい）。一覧は -all")
		}
		rows, err := db.Query(`select id from sessions where id like ?`, fs.Arg(0)+"%")
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if len(ids) == 0 {
			return fmt.Errorf("%q に当たるセッションが無い", fs.Arg(0))
		}
	}

	totalRepaired, fragmented := 0, 0
	for _, id := range ids {
		t, err := thread.Build(db, id)
		if err != nil {
			return err
		}
		totalRepaired += t.Repaired
		if t.Repaired > 0 {
			fragmented++
		}
		fmt.Printf("%s  %s\n", id[:8], t.Describe())
		for i, n := range t.Order {
			if i >= *show {
				break
			}
			label := n.Type
			if n.Subtype != "" {
				label += "/" + n.Subtype
			}
			via := ""
			if n.ViaLogical {
				via = " ←logical"
			}
			fmt.Printf("  %*s%s %s%s\n", n.Depth*2, "", shortTime(n.Timestamp), label, via)
		}
	}
	if len(ids) > 1 {
		fmt.Printf("\n%d セッション / 繋ぎ直し %d 箇所 / 要約で切れていたのは %d セッション\n",
			len(ids), totalRepaired, fragmented)
	}
	return nil
}

func shortTime(ts string) string {
	if len(ts) >= 16 {
		return ts[:16]
	}
	return ts
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

func cmdFiles(args []string) error {
	fs := flag.NewFlagSet("files", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	session := fs.String("session", "", "セッションID（前方一致）で絞る")
	op := fs.String("op", "", "read / edit / write / backup / external-edit / attach / mention")
	n := fs.Int("n", 20, "最大件数")
	summary := fs.Bool("summary", false, "1行1ターンではなくパスごとに畳む")
	rest, err := parseAround(fs, args)
	if err != nil {
		return err
	}
	path := ""
	if len(rest) > 0 {
		path = rest[0]
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	o := files.Opts{Path: path, Session: *session, Op: *op, Limit: *n}

	if *summary {
		rows, err := files.Summarize(db, o)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			fmt.Println("該当なし")
			return nil
		}
		for _, r := range rows {
			fmt.Printf("%3d回 / %d セッション  %s\n      %s .. %s  [%s]\n",
				r.Touches, r.Sessions, r.AbsPath, short(r.First), short(r.Last), r.Ops)
		}
		return nil
	}

	rows, err := files.Touches(db, o)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("該当なし")
		return nil
	}
	for _, r := range rows {
		name := r.RelPath
		if name == "" {
			name = r.AbsPath
		}
		fmt.Printf("%s  %-13s %-12s %s\n", short(r.At), r.Op, r.Origin, name)
		fmt.Printf("    session %s  turn %s", r.SessionID[:8], firstN(r.MessageUUID, 8))
		if r.Title != "" {
			fmt.Printf("  %s", r.Title)
		}
		fmt.Println()
	}
	return nil
}

// short は RFC3339 を「日付 時刻」に詰める。秒より下は見ない。
func short(ts string) string {
	if len(ts) >= 16 {
		return ts[:10] + " " + ts[11:16]
	}
	return ts
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// parseAround は位置引数のうしろに書かれたフラグも拾う。
//
// flag は最初の非フラグで解釈をやめる。campd files PATH -n 5 のように
// 後ろに付けるのが自然な形なので、位置引数を1つ食べては解釈し直す。
func parseAround(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

// missingNote は「実体が消えた」件数を添える。0 なら何も言わない。
func missingNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("／実体が消えた %d 個（中身は保持）", n)
}

func cmdCapture(args []string) error {
	fs := flag.NewFlagSet("capture", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	dir := fs.String("dir", ingest.DefaultFileHistoryDir(defaultClaudeProjects()),
		"~/.claude/file-history 相当のディレクトリ")
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
	r, err := ingest.CaptureBackups(db, *dir)
	if err != nil {
		return err
	}
	fmt.Printf("dir             %s\n走査            %d 個\n新規            %d 個（%s）\n既知            %d 個\n",
		r.Dir, r.Scanned, r.Captured, humanBytes(r.Bytes), r.Known)
	fmt.Printf("保管            %s（同じ中身で済んだ %d 個）\n", humanBytes(r.Stored), r.Deduped)
	if r.Orphans > 0 {
		fmt.Printf("参照なし        %d 個（中身だけ残す）\n", r.Orphans)
	}
	if r.Missing > 0 {
		fmt.Printf("実体が消えた    %d 個（行と中身は残す）\n", r.Missing)
	}
	fmt.Printf("所要            %s\n", time.Since(started).Round(time.Millisecond))
	return nil
}

func cmdBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	session := fs.String("session", "", "セッションID（前方一致）で絞る")
	n := fs.Int("n", 20, "最大件数")
	show := fs.Int64("show", 0, "この ID の中身を標準出力に出す")
	rest, err := parseAround(fs, args)
	if err != nil {
		return err
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if *show > 0 {
		b, body, err := files.BackupContent(db, *show)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "# %s\n# %s v%d  session %s  %d bytes%s\n",
			b.AbsPath, short(b.At), b.Version, b.SessionID[:8], b.Size, missingMark(b.Missing))
		_, err = os.Stdout.Write(body)
		return err
	}

	path := ""
	if len(rest) > 0 {
		path = rest[0]
	}
	rows, err := files.Backups(db, files.Opts{Path: path, Session: *session, Limit: *n})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("該当なし")
		return nil
	}
	for _, b := range rows {
		name := b.RelPath
		if name == "" {
			name = b.AbsPath
		}
		if name == "" {
			name = "(パス不明: " + b.Name + ")"
		}
		fmt.Printf("%6d  %s  v%-3d %8s  %s%s\n",
			b.ID, short(b.At), b.Version, humanBytes(b.Size), name, missingMark(b.Missing))
		fmt.Printf("        session %s  %s\n", b.SessionID[:8], b.Title)
	}
	return nil
}

// missingMark は実体が消えている行に印を付ける。Camp にしか無い中身の目印。
func missingMark(missingAt string) string {
	if missingAt == "" {
		return ""
	}
	return "  [実体は消滅 " + short(missingAt) + "]"
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}
