// campd は Camp のバックエンド。単一バイナリで取り込み・API・セッション駆動を担う。
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/MoomA-0750/camp/internal/files"
	"github.com/MoomA-0750/camp/internal/httpapi"
	"github.com/MoomA-0750/camp/internal/ingest"
	"github.com/MoomA-0750/camp/internal/limits"
	"github.com/MoomA-0750/camp/internal/search"
	"github.com/MoomA-0750/camp/internal/secrets"
	"github.com/MoomA-0750/camp/internal/store"
	"github.com/MoomA-0750/camp/internal/thread"
	"github.com/MoomA-0750/camp/internal/vault"
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
	case "secrets":
		return cmdSecrets(rest)
	case "serve":
		return cmdServe(rest)
	case "passwd":
		return cmdPasswd(rest)
	case "vault":
		return cmdVault(rest)
	case "limits":
		return cmdLimits(rest)
	case "login-url":
		return cmdLoginURL(rest)
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
  campd secrets [-list]   認証情報らしい場所を記録して並べる（何も書き換えない）
  campd passwd            ログインパスワードを設定する（開いている口は全部閉じる）
  campd serve   [-addr]   HTTP で待ち受ける（認証必須・SPA フォールバックあり）
  campd login-url         使い捨てのログインURLを1本出す（開発中の入口）
  campd vault scan  [DIR] Vault を歩いて内訳を出す（DBには書かない）
  campd vault index [DIR] Vault を索引する（Vault側には一切書かない）
  campd vault ghosts      触った記録はあるが実体が無いパスを並べる
  campd limits record     statusLine の JSON を stdin から読んで残量を記録する
  campd limits show       記録済みの窓を新しい順に並べる（-current で現在ぶんだけ）
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

func cmdSecrets(args []string) error {
	fs := flag.NewFlagSet("secrets", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	list := fs.Bool("list", false, "走査せず、記録済みのものだけ並べる")
	open := fs.Bool("open", false, "未判定のものだけ並べる")
	reveal := fs.Bool("reveal", false, "当たった文字列そのものを出す（既定では伏せる）")
	known := fs.String("known", "", "既知の秘密の一覧（既定は DB と同じディレクトリの known-secrets.txt）")
	ok := fs.Int64("ok", 0, "この ID に判断を付ける")
	verdict := fs.String("verdict", "false-positive", "-ok で付ける判断")
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

	if *ok > 0 {
		if err := secrets.Review(db, *ok, *verdict); err != nil {
			return err
		}
		fmt.Printf("検出 %d を %s として記録した（行は消さない）\n", *ok, *verdict)
		return nil
	}

	if !*list {
		path := *known
		if path == "" {
			path = secrets.DefaultKnownPath(*dbPath)
		}
		kn, err := secrets.LoadKnown(path)
		if err != nil {
			return err
		}
		started := time.Now()
		r, err := secrets.Scan(db, kn)
		if err != nil {
			return err
		}
		fmt.Printf("走査            %d メッセージ + %d ブロブ / %s\n既知の秘密      %d 個（%s）\n",
			r.Messages, r.Blobs, humanBytes(r.Bytes), len(kn), path)
		fmt.Printf("当たり          %d 箇所（うち新規 %d・既知の突合 %d）\n",
			r.Found, r.New, r.Known)
		names := make([]string, 0, len(r.Patterns))
		for n := range r.Patterns {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Printf("  %-20s %d\n", n, r.Patterns[n])
		}
		fmt.Printf("所要            %s\n\n", time.Since(started).Round(time.Millisecond))
	}

	rows, err := secrets.List(db, *open, *reveal)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("記録なし")
		return nil
	}
	for _, f := range rows {
		mark := "未判定"
		if f.Reviewed {
			mark = f.Verdict
		}
		fmt.Printf("%4d  %-20s %-14s msg %d  %s  %s\n",
			f.ID, f.Pattern, mark, f.MessageID, short(f.At), f.SessionID[:8])
		fmt.Printf("      %s\n", firstN(f.Context, 200))
	}
	fmt.Printf("\n%d 件。判定は campd secrets -ok <ID> [-verdict 文字列]\n", len(rows))
	return nil
}

func cmdPasswd(args []string) error {
	fs := flag.NewFlagSet("passwd", flag.ContinueOnError)
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

	// 端末ならエコーを止めて読む。パイプで渡されたときは1行そのまま。
	pw, err := readPassword("パスワード: ")
	if err != nil {
		return err
	}
	again, err := readPassword("もう一度: ")
	if err != nil {
		return err
	}
	if pw != again {
		return fmt.Errorf("一致しない")
	}
	if err := httpapi.SetPassword(db, pw); err != nil {
		return err
	}
	fmt.Println("設定した。開いていたログインセッションはすべて閉じた。")
	return nil
}

// readPassword は端末ならエコーを止めて1行読む。
//
// x/term を足さずに stty へ寄せている。依存を増やさないための割り切りで、
// 端末が無い（パイプ・CI）ときはそのまま読む。
func readPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	restore := func() {}
	if isTerminal() {
		if err := exec.Command("stty", "-echo").Run(); err == nil {
			restore = func() {
				exec.Command("stty", "echo").Run()
				fmt.Fprintln(os.Stderr)
			}
		}
	}
	defer restore()

	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// stdin は1本だけ持つ。呼ぶたびに bufio を作ると、1回目が2行とも
// 読み込んでしまい、2回目が EOF になる（確認をパイプで渡すと必ず踏む）。
var stdin = bufio.NewReader(os.Stdin)

func isTerminal() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	addr := fs.String("addr", defaultAddr(), "待ち受けアドレス")
	web := fs.String("web", "", "フロントのビルド成果物のディレクトリ（空なら組み込みの仮の殻）")
	origins := fs.String("origin", "", "追加で許すオリジン（カンマ区切り）")
	secure := fs.Bool("secure-cookie", false, "Cookie に Secure を付ける（TLS 終端の後ろに置くとき）")
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
	if n, err := httpapi.PurgeExpiredSessions(db); err == nil && n > 0 {
		fmt.Printf("期限切れのログイン %d 件を掃除した\n", n)
	}
	if !httpapi.HasPassword(db) {
		return fmt.Errorf("パスワードが未設定。先に campd passwd を実行すること（D-011）")
	}

	var allow []string
	for _, o := range strings.Split(*origins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			allow = append(allow, o)
		}
	}
	srv, err := httpapi.New(db, httpapi.Options{
		Addr: *addr, WebDir: *web, Origins: allow, SecureCookie: *secure,
	})
	if err != nil {
		return err
	}

	hs := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	fmt.Printf("db      %s\naddr    http://%s\n画面    %s\n認証    必須（/healthz を除く全経路）\n",
		*dbPath, *addr, srv.Source())

	// Ctrl-C で受け付けをやめ、走っている要求を待つ。
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	errc := make(chan error, 1)
	go func() {
		err := hs.ListenAndServe()
		if err == http.ErrServerClosed {
			err = nil
		}
		errc <- err
	}()

	select {
	case err := <-errc:
		return err
	case <-stop:
		fmt.Println("\n止める")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return hs.Shutdown(ctx)
	}
}

// defaultAddr は待ち受け先の既定。
//
// 8787 は避ける。このマシンでは既に別のサービスが 0.0.0.0:8787 を
// 掴んでいて、既定のまま起動すると bind に失敗した。よくある開発用
// ポート（3000 / 5000 / 8000 / 8080 / 5173 / 8787）から離す。
func defaultAddr() string {
	if a := os.Getenv("CAMP_ADDR"); a != "" {
		return a
	}
	return "127.0.0.1:8785"
}

func cmdLoginURL(args []string) error {
	fs := flag.NewFlagSet("login-url", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	base := fs.String("base", "http://"+defaultAddr(), "サーバーのベースURL")
	ttl := fs.Duration("ttl", httpapi.LoginTokenTTL, "有効時間")
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

	tok, exp, err := httpapi.MintLoginToken(db, *ttl)
	if err != nil {
		return err
	}
	fmt.Printf("%s/login?t=%s\n", strings.TrimRight(*base, "/"), tok)
	fmt.Fprintf(os.Stderr, "1回だけ使える。%s まで（%s）。\n",
		exp.Local().Format("15:04:05"), *ttl)
	return nil
}

func cmdLimits(args []string) error {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "record":
		return cmdLimitsRecord(args)
	case "", "show", "list":
		return cmdLimitsShow(args)
	default:
		return fmt.Errorf("limits の使い方: campd limits record | campd limits show")
	}
}

// cmdLimitsRecord は statusLine の stdin JSON を受け取る。
// statusLine のフックから毎描画呼ばれるので、失敗しても静かに 0 で抜ける。
// プロンプトを壊さないことが最優先。
func cmdLimitsRecord(args []string) error {
	fs := flag.NewFlagSet("limits record", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	agent := fs.String("agent", limits.AgentClaudeCode, "エージェント識別子")
	source := fs.String("source", limits.SourceStatusLine, "観測元")
	quiet := fs.Bool("quiet", true, "何も出力しない（フックからの既定）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		if *quiet {
			return nil
		}
		return err
	}
	defer db.Close()

	got, err := limits.Record(db, os.Stdin, *agent, *source)
	if err != nil {
		if *quiet || errors.Is(err, limits.ErrNoWindows) {
			return nil
		}
		return err
	}
	if *quiet {
		return nil
	}
	for _, r := range got {
		mark := "更新"
		if r.New {
			mark = "新規"
		}
		fmt.Printf("%s  %-10s %5.1f%%  リセット %s\n", mark, r.Kind, r.UsedPct, r.EndsAt)
	}
	return nil
}

func cmdLimitsShow(args []string) error {
	fs := flag.NewFlagSet("limits show", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	kind := fs.String("kind", "", "窓の種類で絞る（five_hour / seven_day）")
	limit := fs.Int("n", 20, "件数")
	current := fs.Bool("current", false, "いま拘束されている窓だけ出す")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	var rows []limits.Window
	if *current {
		rows, err = limits.Current(db)
	} else {
		rows, err = limits.Windows(db, limits.Opts{Kind: *kind, Limit: *limit})
	}
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("記録がない。statusLine のフックがまだ一度も走っていない可能性がある。")
		fmt.Println("確認: campd limits record -quiet=false < サンプルJSON")
		return nil
	}
	fmt.Printf("%-10s %7s %7s %6s  %-20s %s\n", "窓", "最新", "ピーク", "観測", "リセット", "")
	for _, w := range rows {
		now := ""
		if w.Current {
			now = "← 進行中"
		}
		fmt.Printf("%-10s %6.1f%% %6.1f%% %6d  %-20s %s\n",
			w.Kind, w.UsedPct, w.PeakPct, w.Samples, w.EndsAt, now)
	}
	return nil
}

func cmdVault(args []string) error {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "scan":
		return cmdVaultScan(args)
	case "index":
		return cmdVaultIndex(args)
	case "ghosts":
		return cmdVaultGhosts(args)
	default:
		return fmt.Errorf("vault の使い方: campd vault scan | campd vault index")
	}
}

// defaultVaultRoot は CAMP_VAULT があればそれを、無ければカレント。
func defaultVaultRoot() string {
	if p := os.Getenv("CAMP_VAULT"); p != "" {
		return p
	}
	return "."
}

func cmdVaultScan(args []string) error {
	fs := flag.NewFlagSet("vault scan", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	root := defaultVaultRoot()
	if fs.NArg() > 0 {
		root = fs.Arg(0)
	}

	started := time.Now()
	res, err := vault.Scan(root)
	if err != nil {
		return err
	}
	fmt.Printf("root      %s\n", res.Root)
	fmt.Printf("ファイル  %d（%s）\n", len(res.Files), humanBytes(res.Bytes))
	kinds := make([]string, 0, len(res.ByKind))
	for k := range res.ByKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		fmt.Printf("  %-9s %d\n", k, res.ByKind[k])
	}
	if len(res.Pruned) > 0 {
		fmt.Printf("切った    %d ディレクトリ（降りていない）: %s\n",
			len(res.Pruned), strings.Join(clipList(res.Pruned, 6), " "))
	}
	if res.Skipped > 0 {
		fmt.Printf("飛ばした  %d（シンボリックリンク・読めないもの）\n", res.Skipped)
	}
	fmt.Printf("所要      %s\n", time.Since(started).Round(time.Millisecond))
	return nil
}

func cmdVaultIndex(args []string) error {
	fs := flag.NewFlagSet("vault index", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	host := fs.String("host", defaultHost(), "ホスト名")
	name := fs.String("name", "", "Vault の名前（既定はディレクトリ名）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root := defaultVaultRoot()
	if fs.NArg() > 0 {
		root = fs.Arg(0)
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	res, err := vault.Index(db, *host, root, *name)
	if err != nil {
		return err
	}
	fmt.Printf("vault     #%d %s\n", res.VaultID, res.Root)
	fmt.Printf("走査      %d ファイル（%s）\n", res.Scanned, humanBytes(res.Bytes))
	fmt.Printf("索引      新規 %d / 変化 %d / 同じ %d\n", res.Added, res.Changed, res.Same)
	if res.Restored > 0 {
		fmt.Printf("          戻ってきた %d\n", res.Restored)
	}
	if res.Missing > 0 {
		fmt.Printf("消えた    %d（行は残す。中身も blobs に残っている）\n", res.Missing)
	}
	fmt.Printf("中身      blobs に %d 個追加\n", res.Stored)
	fmt.Printf("プロパティ延べ %d 件 / %d 種のキー\n", res.Props, res.PropKeys)
	fmt.Printf("リンク    延べ %d 本 → %d 行（同じ先へは1本に畳む）\n", res.Links, res.LinkRows)
	fmt.Printf("          解決 %d / 宙吊り %d / うち曖昧 %d\n",
		res.Resolved, res.Dangling, res.Ambiguous)
	if len(res.Pruned) > 0 {
		fmt.Printf("切った    %d ディレクトリ: %s\n",
			len(res.Pruned), strings.Join(clipList(res.Pruned, 6), " "))
	}
	fmt.Printf("所要      %s\n", res.Took.Round(time.Millisecond))
	return nil
}

func clipList(xs []string, n int) []string {
	if len(xs) <= n {
		return xs
	}
	return append(append([]string{}, xs[:n]...), fmt.Sprintf("…他%d", len(xs)-n))
}

func cmdVaultGhosts(args []string) error {
	fs := flag.NewFlagSet("vault ghosts", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	vaultID := fs.Int64("vault", 1, "Vault の id")
	only := fs.String("reason", "", "分類で絞る（gone / worktree / hidden / other-case）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	gs, err := vault.Ghosts(db, *vaultID)
	if err != nil {
		return err
	}
	byReason := map[string]int{}
	shown := 0
	for _, g := range gs {
		byReason[g.Reason]++
		if *only != "" && g.Reason != *only {
			continue
		}
		body := ""
		if g.Backups > 0 {
			body = fmt.Sprintf("  中身 %d版", g.Backups)
		}
		fmt.Printf("%-10s %-16s %s%s\n", g.Reason, shortTime(g.Last), g.Path, body)
		shown++
	}
	if shown == 0 {
		fmt.Println("該当なし")
	}
	fmt.Println()
	for _, r := range []string{vault.GhostGone, vault.GhostOtherCase, vault.GhostWorktree, vault.GhostHidden} {
		if byReason[r] > 0 {
			fmt.Printf("%-10s %d\n", r, byReason[r])
		}
	}
	return nil
}
