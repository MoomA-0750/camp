// campd は Camp のバックエンド。単一バイナリで取り込み・API・セッション駆動を担う。
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/MoomA-0750/camp/internal/audit"
	"github.com/MoomA-0750/camp/internal/files"
	"github.com/MoomA-0750/camp/internal/httpapi"
	"github.com/MoomA-0750/camp/internal/ingest"
	"github.com/MoomA-0750/camp/internal/limits"
	"github.com/MoomA-0750/camp/internal/mcp"
	"github.com/MoomA-0750/camp/internal/report"
	"github.com/MoomA-0750/camp/internal/retain"
	"github.com/MoomA-0750/camp/internal/search"
	"github.com/MoomA-0750/camp/internal/secrets"
	"github.com/MoomA-0750/camp/internal/snapshot"
	"github.com/MoomA-0750/camp/internal/store"
	"github.com/MoomA-0750/camp/internal/thread"
	"github.com/MoomA-0750/camp/internal/vault"
	"github.com/MoomA-0750/camp/internal/views"
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

	// 引数で -db を指していない場合だけ見る。空のDBを作って
	// それについて報告するのを止める（詳しくは checkNotAGhostDB）。
	if !hasFlag(rest, "-db") {
		sub := ""
		if len(rest) > 0 {
			sub = rest[0]
		}
		if err := checkNotAGhostDB(cmd, sub, defaultDBPath()); err != nil {
			return err
		}
	}

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
	case "tombstones":
		return cmdTombstones(rest)
	case "retain":
		return cmdRetain(rest)
	case "snapshot":
		return cmdSnapshot(rest)
	case "audit":
		return cmdAudit(rest)
	case "report":
		return cmdReport(rest)
	case "redact":
		return cmdRedact(rest)
	case "secrets":
		return cmdSecrets(rest)
	case "serve":
		return cmdServe(rest)
	case "passwd":
		return cmdPasswd(rest)
	case "vault":
		return cmdVault(rest)
	case "views":
		return cmdViews(rest)
	case "mcp":
		return cmdMCP(rest)
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
  campd doctor  [-db P]   DB の状態を点検する（-fix で missing_at を直す）
  campd scan    [-root D] 会話記録を読んで実測レポートを出す（DBには書かない）
  campd ingest  [-root D] 会話記録を DB に取り込む（再実行しても重複しない）
  campd backfill [-db P]  messages から派生テーブル（usage・索引・ファイル結合）を作り直す
  campd search  QUERY     全文検索（日本語は2文字から引ける）
  campd thread  SESSION   会話を木に組み立てて形を見る
  campd files   [PATH]    ノートを触ったターンを引く（-session でセッション側から）
  campd capture [-dir D]  file-history の実体（編集前の中身）を DB に取り込む
  campd backup  [PATH]    捕獲したバックアップを一覧する（-show ID で中身を出す）
  campd tombstones        消したものの記録を新しい順に並べる
  campd retain            保持の規則を見る・切り替える・当てる（既定は --dry-run）
  campd audit             監査ログを新しい順に読む（-verify で連鎖を確かめる）
  campd report -action A  境界の外から監査ログへ1行だけ足す（DBには触らない）
  campd redact            既知の値を標準入力から受け取り、写っている場所を全部伏せる
  campd snapshot -out F   DBの一貫したスナップショットを暗号化して書き出す
  campd snapshot -restore F -out P  スナップショットを戻して doctor まで通す
  campd secrets [-list]   認証情報らしい場所を記録して並べる（何も書き換えない）
  campd passwd            ログインパスワードを設定する（開いている口は全部閉じる）
  campd serve   [-addr]   HTTP で待ち受ける（認証必須・SPA フォールバックあり）
  campd login-url         使い捨てのログインURLを1本出す（開発中の入口）
  campd vault scan  [DIR] Vault を歩いて内訳を出す（DBには書かない）
  campd vault index [DIR] Vault を索引する（Vault側には一切書かない）
  campd vault ghosts      触った記録はあるが実体が無いパスを並べる
  campd views [NAME]      .base のビューを一覧・実行する
  campd mcp               MCPサーバーとして stdio で待つ（読み取り専用）
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
	// 消した行を黙って飛ばさない。数えていたのに、どこにも出していなかった。
	if res.Suppressed > 0 {
		fmt.Printf("抑止            %d 行（消した記録があるので取り込まない）\n", res.Suppressed)
	}
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
	fix := fs.Bool("fix", false, "missing_at を入れ、tombstone に行の身元を埋め戻す")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if *fix {
		n, err := db.MarkMissingSourceFiles()
		if err != nil {
			return err
		}
		fmt.Printf("fix   %-24s %d 本に missing_at を入れた\n", "source_files", n)

		// 元ファイルが手元にあるうちに、行そのものの身元を入れておく。
		// **失われたあとでは二度と埋められない。**
		filled, skipped, err := retain.BackfillLineHashes(db)
		if err != nil {
			return err
		}
		detail := fmt.Sprintf("%d 件に行の身元を入れた", filled)
		if skipped > 0 {
			detail += fmt.Sprintf("（%d 件は元ファイルが無く埋められない）", skipped)
		}
		fmt.Printf("fix   %-24s %s\n", "tombstones", detail)
	}

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

// systemDBPath は常駐している campd が使う場所。
const systemDBPath = "/var/lib/camp/camp.sqlite"

// checkNotAGhostDB は、**空のDBを黙って作って、それについて報告する**のを止める。
//
// M25.5 で本番の DB は /var/lib/camp へ移った。それでも `campd doctor` を
// 引数なしで叩くと、リポジトリの data/camp.sqlite が無ければ SQLite が
// 作ってしまい、空のDBに対する点検結果が「ほぼ ok」で出る（実測 2026-09-04）。
// 中身が無いから ok なのを、中身が正しいから ok と読み違える。
//
// 作ってよいのは migrate と passwd だけ。
func checkNotAGhostDB(cmd, sub, path string) error {
	switch cmd {
	// 作ってよいもの。
	case "migrate", "passwd", "version", "help", "":
		return nil
	// DB を開かないもの。**止める理由が無い。**
	case "report", "scan":
		return nil
	}
	// `limits record` は DB を開けなければ自分で報告口へ回す。
	// `limits show` は DB を読むので止める。
	if cmd == "limits" && sub == "record" {
		return nil
	}
	// `vault scan` は歩くだけ。`vault index` と `vault ghosts` は DB を使う。
	if cmd == "vault" && sub == "scan" {
		return nil
	}
	if os.Getenv("CAMP_DB") != "" {
		return nil // 明示的に指した場所なら口を出さない
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	// **`systemDBPath` の有無は確かめない。** 境界が効いていれば、
	// このプロセス（人間のユーザー）からは置き場を辿れず Stat が失敗する。
	// 「無い」と「見えない」を区別できないので、どちらの道も出す。
	return fmt.Errorf(`%s が無い。空のDBを作って点検しても意味が無いので止める
  常駐しているほうを見るなら: sudo -u camp /usr/local/bin/campd %s -db %s
  ここに新しく作るなら:       campd migrate -db %s`,
		path, cmd, systemDBPath, path)
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
	sock := fs.String("report-sock", defaultReportSock(), "追記専用の報告口（空なら開かない）")
	sockGroup := fs.String("report-group", os.Getenv("CAMP_REPORT_GROUP"), "報告口を持たせるグループ（空なら変えない）")
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

	// **境界の外から監査ログへ追記するためだけの口。**
	// Phase 3 のセッションは人間と同じユーザーで動き、DB には触れない。
	// 触れないまま「何をしたか」を残せるように、追記だけを受ける。
	if *sock != "" {
		rl, err := report.Listen(db, *sock, *sockGroup)
		if err != nil {
			return fmt.Errorf("報告口を開けない: %w", err)
		}
		defer rl.Close()
		go rl.Serve()
		fmt.Printf("報告口    %s（追記だけ。読み出しも書き換えも命令が無い）\n", rl.Addr())
	}

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
	sock := fs.String("sock", defaultReportSock(), "DBを開けないときに使う報告口")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// **どこへ渡すかは、この順で決める。**
	//
	//  1. -db か CAMP_DB で明示されていれば、そこへ直接書く（開発中の DB 用）
	//  2. 報告口があれば、そこへ渡す。**常駐している campd が持ち主**なので
	//     こちらが本番の経路
	//  3. どちらも無ければ、既定のパスへ直接書く（campd 単体で使う場合）
	//
	// 2 を 1 より後に置くのは意図。逆にすると、cwd にたまたま古い
	// data/camp.sqlite があるだけでそちらへ書き、本番へ届いていないのに
	// 届いたつもりになる（2026-09-04 に実際に起きた）。
	explicit := hasFlag(args, "-db") || os.Getenv("CAMP_DB") != ""
	if limitsTarget(explicit, *sock, *dbPath) == targetSocket {
		err := reportLimits(*sock, os.Stdin)
		if err != nil && !*quiet {
			return err
		}
		return nil
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

func cmdViews(args []string) error {
	fs := flag.NewFlagSet("views", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	vaultID := fs.Int64("vault", 1, "Vault の id")
	limit := fs.Int("n", 5, "出す行数")
	cols := fs.Int("c", 8, "出す列数")
	if err := fs.Parse(args); err != nil {
		return err
	}
	want := strings.Join(fs.Args(), " ")

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	var root string
	if err := db.QueryRow(`select root from vaults where id = ?`, *vaultID).Scan(&root); err != nil {
		return err
	}
	bases, err := views.LoadBases(db, *vaultID, func(rel string) ([]byte, error) {
		return vault.Read(root, rel)
	})
	if err != nil {
		return err
	}
	recs, err := views.LoadRecords(db, *vaultID)
	if err != nil {
		return err
	}

	if want == "" {
		fmt.Printf("%-18s %-28s %-14s %6s\n", "base", "ビュー", "種別", "行")
		for _, b := range bases {
			for i := range b.Views {
				r, err := views.Run(b, &b.Views[i], recs)
				n := "—"
				if err == nil {
					n = fmt.Sprint(r.Total)
				}
				fmt.Printf("%-18s %-28s %-14s %6s\n", b.Name, b.Views[i].Name, b.Views[i].Kind, n)
			}
		}
		return nil
	}

	for _, b := range bases {
		for i := range b.Views {
			v := &b.Views[i]
			if !strings.Contains(b.Name+"/"+v.Name, want) {
				continue
			}
			r, err := views.Run(b, v, recs)
			if err != nil {
				return err
			}
			fmt.Printf("== %s / %s（%s）\n", b.Name, v.Name, v.Kind)
			fmt.Printf("行 %d / 列 %d", r.Total, len(r.Columns))
			pinned := 0
			for _, c := range r.Columns {
				if c.Pinned {
					pinned++
				}
			}
			fmt.Printf("（定義が挙げたのは %d、残り %d は自動で出た）\n", pinned, len(r.Columns)-pinned)
			for _, w := range r.Warnings {
				fmt.Printf("  警告: %s\n", w)
			}
			show := min(*cols, len(r.Columns))
			hdr := make([]string, 0, show)
			for _, c := range r.Columns[:show] {
				hdr = append(hdr, c.Label)
			}
			fmt.Println("  " + strings.Join(hdr, " | "))
			shown := 0
			for _, g := range r.Groups {
				for _, row := range g.Rows {
					if shown >= *limit {
						break
					}
					cells := make([]string, 0, show)
					for _, c := range r.Columns[:show] {
						cells = append(cells, clipCell(row.Cells[c.Key]))
					}
					fmt.Println("  " + strings.Join(cells, " | "))
					shown++
				}
			}
			if len(r.Summary) > 0 {
				fmt.Printf("  集計: %v\n", r.Summary)
			}
			fmt.Println()
		}
	}
	return nil
}

func clipCell(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) > 16 {
		return string([]rune(s)[:16]) + "…"
	}
	if s == "" {
		return "·"
	}
	return s
}

func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	if err := fs.Parse(args); err != nil {
		return err
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	// stdout はプロトコル 専用。ログは stderr へ出す。
	return mcp.New(db, os.Stdin, os.Stdout, Version).Serve()
}

// cmdTombstones は削除の記録を並べる。**消したことが見えない削除を作らない**ための
// 表側の口。ここに出ないものは、このリポジトリの削除経路を通っていない。
func cmdTombstones(args []string) error {
	fs := flag.NewFlagSet("tombstones", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	n := fs.Int("n", 50, "最大件数")
	kind := fs.String("kind", "", "kind で絞る（message.raw_json / message_blocks / blob）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := db.Query(`
		select redacted_at, kind, ref, reason, actor, bytes_removed, recoverable,
		       coalesce(note,'')
		  from tombstones
		 where (? = '' or kind = ?)
		 order by redacted_at desc, id desc
		 limit ?`, *kind, *kind, *n)
	if err != nil {
		return err
	}
	defer rows.Close()

	count := 0
	var total int64
	for rows.Next() {
		var at, k, ref, reason, actor, note string
		var bytes int64
		var recoverable int
		if err := rows.Scan(&at, &k, &ref, &reason, &actor, &bytes, &recoverable, &note); err != nil {
			return err
		}
		mark := "戻せる  "
		if recoverable == 0 {
			mark = "戻せない"
		}
		fmt.Printf("%s  %s  %-18s %-10s %8d B  %s\n", at, mark, k, ref, bytes, reason)
		if note != "" {
			fmt.Printf("%*s%s\n", 22, "", note)
		}
		count++
		total += bytes
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count == 0 {
		fmt.Println("まだ何も消していない")
		return nil
	}
	fmt.Printf("\n%d 件 / 合計 %d バイト\n", count, total)
	return nil
}

// cmdRetain は保持の規則を扱う。**既定は何もしない。**
//
// 引数なしで叩くと規則の一覧と見積りだけを出す。実際に落とすのは
// --apply を明示したときだけで、そのときも Plan が返したものしか触らない。
func cmdRetain(args []string) error {
	fs := flag.NewFlagSet("retain", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	apply := fs.Bool("apply", false, "実際に落とす（既定は見積りだけ）")
	enable := fs.String("enable", "", "規則を有効にする（名前で指定）")
	disable := fs.String("disable", "", "規則を無効にする（名前で指定）")
	includeUnrecoverable := fs.Bool("include-unrecoverable", false,
		"元ファイルが消えていて戻せない行も対象にする")
	if err := fs.Parse(args); err != nil {
		return err
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if *enable != "" {
		if err := retain.SetEnabled(db, *enable, true); err != nil {
			return err
		}
		fmt.Printf("有効にした: %s\n", *enable)
	}
	if *disable != "" {
		if err := retain.SetEnabled(db, *disable, false); err != nil {
			return err
		}
		fmt.Printf("無効にした: %s\n", *disable)
	}

	pols, err := retain.Policies(db, false)
	if err != nil {
		return err
	}
	fmt.Println("規則:")
	for _, p := range pols {
		mark := "  "
		if p.Enabled {
			mark = "on"
		}
		fmt.Printf("  [%s] %-24s %s\n", mark, p.Name, p.Kind)
		if p.Note != "" {
			fmt.Printf("       %s\n", p.Note)
		}
	}

	plan, err := retain.Plan(db, *includeUnrecoverable)
	if err != nil {
		return err
	}
	if len(plan) == 0 {
		fmt.Println("\n対象なし。有効な規則が無いか、落とせるものが無い。")
		return nil
	}

	var bytes int64
	sessions := map[string]int{}
	unrecoverable := 0
	for _, r := range plan {
		bytes += int64(r.Bytes)
		sessions[r.SessionID]++
		if !r.Recoverable {
			unrecoverable++
		}
	}
	fmt.Printf("\n対象 %d 行 / %d バイト / %d セッション\n", len(plan), bytes, len(sessions))
	if unrecoverable > 0 {
		fmt.Printf("  うち %d 行は元ファイルが無く、戻せない\n", unrecoverable)
	}

	// 対象の多いセッションを上から少しだけ。全部並べても読めない。
	type sc struct {
		id string
		n  int
	}
	var top []sc
	for id, n := range sessions {
		top = append(top, sc{id, n})
	}
	sort.Slice(top, func(i, j int) bool { return top[i].n > top[j].n })
	if len(top) > 5 {
		top = top[:5]
	}
	for _, t := range top {
		fmt.Printf("  %s  %d 行\n", t.id, t.n)
	}

	if !*apply {
		fmt.Println("\n見積りだけ。実際に落とすには --apply を付ける。")
		return nil
	}

	out, err := retain.Apply(db, plan, "campd retain")
	if err != nil {
		return err
	}
	fmt.Printf("\n落とした: %d 行 / %d バイト", out.Messages, out.BytesRemoved)
	if out.Unrecoverable > 0 {
		fmt.Printf("（うち戻せない %d 行）", out.Unrecoverable)
	}
	fmt.Println()
	if int64(len(plan)) != int64(out.Messages) {
		return fmt.Errorf("見積り %d 行に対して実際は %d 行。食い違っている", len(plan), out.Messages)
	}
	return nil
}

// cmdSnapshot は退避先を作る／戻す。
//
// **campd は鍵を持たない。** 標準入力から受け取り、終わったら忘れる。
// 稼働中のDBは平文のままなので（SQLCipher は systemd 常駐と相性が悪い）、
// 守れるのは持ち出す先だけ。そこは確実に守る。
func cmdSnapshot(args []string) error {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	out := fs.String("out", "", "書き出し先")
	restore := fs.String("restore", "", "戻すスナップショット")
	keyFile := fs.String("key-file", "", "鍵のファイル（既定は標準入力から読む）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("-out が要る")
	}

	key, err := readKey(*keyFile)
	if err != nil {
		return err
	}
	defer wipe(key)

	if *restore != "" {
		info, err := snapshot.Restore(*restore, *out, key)
		if err != nil {
			return err
		}
		fmt.Printf("戻した     %s\n", info.Path)
		fmt.Printf("  中身     %d バイト / sha256 %s\n", info.PlainBytes, info.SHA256)

		// **戻しただけでは戻ったことにならない。** 開いて点検まで通す。
		db, err := store.Open(info.Path)
		if err != nil {
			return fmt.Errorf("戻したが開けない: %w", err)
		}
		defer db.Close()
		checks, err := db.Doctor()
		if err != nil {
			return err
		}
		failed := 0
		for _, c := range checks {
			if !c.OK {
				fmt.Printf("FAIL  %-24s %s\n", c.Name, c.Detail)
				failed++
			}
		}
		if failed > 0 {
			return fmt.Errorf("戻したDBが %d 件の点検に落ちた", failed)
		}
		fmt.Println("  点検     すべて通った")
		return nil
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	info, err := snapshot.Create(db, *out, key)
	outcome := audit.OK
	if err != nil {
		outcome = audit.Error
	}
	// 退避先を作ったことも記録する。**持ち出しは監査の対象。**
	if _, aerr := audit.Append(db, audit.Entry{
		Actor: "campd snapshot", Action: "snapshot.create", Target: *out,
		Detail: fmt.Sprintf("sha256 %s", info.SHA256), Outcome: outcome,
	}); aerr != nil {
		return aerr
	}
	if err != nil {
		return err
	}
	fmt.Printf("取った     %s\n", info.Path)
	fmt.Printf("  中身     %d バイト / sha256 %s\n", info.PlainBytes, info.SHA256)
	fmt.Printf("  暗号文   %d バイト\n", info.CipherBytes)
	fmt.Println("  この sha256 を控えておく。戻したときに同じ値が出れば中身は同じ。")
	return nil
}

// readKey は鍵を読む。**どこにも書かない。**
func readKey(file string) ([]byte, error) {
	var b []byte
	var err error
	if file != "" {
		b, err = os.ReadFile(file)
	} else {
		fmt.Fprintln(os.Stderr, "鍵を標準入力から読む（1Password から渡す）")
		b, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		return nil, err
	}
	b = bytes.TrimRight(b, "\r\n")
	if len(b) < 8 {
		return nil, errors.New("鍵が短すぎる（8バイト以上）")
	}
	return b, nil
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// cmdAudit は監査ログを読む。**足す口はここに置かない。**
func cmdAudit(args []string) error {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	session := fs.String("session", "", "セッションIDで絞る")
	action := fs.String("action", "", "action で絞る")
	n := fs.Int("n", 50, "最大件数")
	verify := fs.Bool("verify", false, "連鎖をたどって抜けと書き換えを探す")
	if err := fs.Parse(args); err != nil {
		return err
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if *verify {
		have, err := audit.TriggersInPlace(db)
		if err != nil {
			return err
		}
		if miss := audit.Missing(have); len(miss) > 0 {
			fmt.Printf("FAIL  トリガが外れている: %s\n", strings.Join(miss, ", "))
		} else {
			fmt.Println("ok    追記専用のトリガはある")
		}
		checked, err := audit.Verify(db)
		if err != nil {
			fmt.Printf("FAIL  連鎖: %v（%d 行まで確かめた）\n", err, checked)
			return errors.New("監査ログが書き換わっている")
		}
		fmt.Printf("ok    連鎖: %d 行が繋がっている\n", checked)
		return nil
	}

	rows, err := audit.List(db, audit.Opts{Session: *session, Action: *action, Limit: *n})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("まだ何も記録されていない")
		return nil
	}
	for _, e := range rows {
		mark := " "
		if e.Outcome != audit.OK {
			mark = "!"
		}
		fmt.Printf("%s %s  %-14s %-18s %s\n", mark, e.At, e.Actor, e.Action, e.Target)
		if e.SessionID != "" {
			fmt.Printf("    session %s  → %s\n", e.SessionID, e.Outcome)
		}
	}
	fmt.Printf("\n%d 件\n", len(rows))
	return nil
}

// cmdRedact は既知の値を伏せる。**値は標準入力からだけ受け取り、決して表示しない。**
//
// パターンで探す検出器はこのコーパスで偽陽性100%・偽陰性100%だった（D-010）。
// だから「知っている値で数えて、知っている値だけ伏せる」に限る。
func cmdRedact(args []string) error {
	fs := flag.NewFlagSet("redact", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath(), "SQLite ファイルのパス")
	reason := fs.String("reason", "", "なぜ伏せるか（必須）")
	apply := fs.Bool("apply", false, "実際に伏せる（既定は見積りだけ）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *apply && *reason == "" {
		return errors.New("-reason が要る。理由の無い削除は残さない")
	}

	fmt.Fprintln(os.Stderr, "伏せたい値を標準入力から読む（表示はしない）")
	secret, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	secret = bytes.TrimRight(secret, "\r\n")
	if len(secret) < 4 {
		return errors.New("短すぎる。関係ない場所まで潰す")
	}
	defer wipe(secret)

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	plan, err := retain.FindSecret(db, secret)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(secret)
	fmt.Printf("照合語: 長さ %d / sha256先頭8 %s\n\n", len(secret), hex.EncodeToString(sum[:])[:8])
	fmt.Printf("  messages.raw_json            %5d 件\n", len(plan.Messages))
	fmt.Printf("  message_blocks.text          %5d 件\n", plan.Blocks)
	fmt.Printf("  message_blocks.bigrams(FTS)  %5d 件\n", plan.Bigrams)
	fmt.Printf("  blobs.content(展開後)        %5d 件\n", len(plan.Blobs))

	// **知っている置き場だけを数えない。**2026-09-03 の outer gate まで、
	// ここで挙げた4か所しか見ておらず、sessions.first_user_message に
	// 平文が残ったまま「0件」と表示していた。
	hits, err := retain.Sweep(db, secret)
	if err != nil {
		return err
	}
	fmt.Printf("\n  総なめ（全表・全列）\n")
	if len(hits) == 0 {
		fmt.Println("    どの表のどの列にも無い")
	}
	for _, h := range hits {
		fmt.Printf("    %-38s %5d 件\n", h.Table+"."+h.Column, h.Count)
	}
	total := retain.Total(hits)
	fmt.Printf("\n  合計 %d 件\n", total)

	if total == 0 {
		return nil
	}
	if !*apply {
		fmt.Println("\n見積りだけ。実際に伏せるには --apply -reason '…' を付ける。")
		return nil
	}

	out, err := retain.Secret(db, secret, retain.Op{Reason: *reason, Actor: "campd redact"})
	if err != nil {
		return err
	}
	fmt.Printf("\n伏せた: messages %d / blocks 作り直し %d / blobs %d / 派生列 %d 行 / %d バイト\n",
		out.Messages, out.Blocks, out.Blobs, out.Columns, out.BytesRemoved)

	// 確かめ直すのも総なめで。伏せた経路と同じ範囲しか見ないと、
	// 「見ていないから0件」を「消えたから0件」と取り違える。
	after, err := retain.Sweep(db, secret)
	if err != nil {
		return err
	}
	if n := retain.Total(after); n != 0 {
		for _, h := range after {
			fmt.Printf("  残: %-38s %5d 件\n", h.Table+"."+h.Column, h.Count)
		}
		return fmt.Errorf("まだ %d 件残っている", n)
	}
	fmt.Println("全表・全列を走査し直して 0 件。")
	return nil
}

// defaultReportSock は追記専用の報告口の場所。
//
// 常駐させるときは systemd の RuntimeDirectory=camp が /run/camp を作る。
// 手元で動かすときは XDG_RUNTIME_DIR の下。無ければ開かない。
func defaultReportSock() string {
	if p := os.Getenv("CAMP_REPORT_SOCK"); p != "" {
		return p
	}
	if fi, err := os.Stat("/run/camp"); err == nil && fi.IsDir() {
		return "/run/camp/report.sock"
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "camp-report.sock")
	}
	return ""
}

// cmdReport は境界の外から監査ログへ1行追記する。**DB は開かない。**
//
// これが campd と同じ権限を要らない唯一の書き込み経路。Phase 3 の launcher は
// これ（か同じ socket）を使う。名乗る欄が無いのは意図で、actor は campd が
// カーネルに聞いて決める。
func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	sock := fs.String("sock", defaultReportSock(), "報告口のパス")
	action := fs.String("action", "", "何をしたか（必須）")
	target := fs.String("target", "", "対象")
	session := fs.String("session", "", "セッションID")
	outcome := fs.String("outcome", "ok", "ok / denied / error / timeout")
	detail := fs.String("detail", "", "詳細（自由文）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *action == "" {
		return fmt.Errorf("-action が要る")
	}
	if *sock == "" {
		return fmt.Errorf("報告口が見つからない。-sock で指定する")
	}

	ev := map[string]any{
		"action": *action, "target": *target,
		"session": *session, "outcome": *outcome,
	}
	if *detail != "" {
		ev["detail"] = *detail
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}

	c, err := net.DialTimeout("unix", *sock, 5*time.Second)
	if err != nil {
		return fmt.Errorf("報告口へ繋がらない: %w", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write(append(body, '\n')); err != nil {
		return err
	}
	sc := bufio.NewScanner(c)
	if !sc.Scan() {
		return fmt.Errorf("返事が無い")
	}
	var r report.Reply
	if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
		return fmt.Errorf("返事を読めない: %s", sc.Text())
	}
	if !r.OK {
		return fmt.Errorf("断られた: %s", r.Error)
	}
	fmt.Printf("記録した  id=%d\n", r.ID)
	return nil
}

// hasFlag は引数に -name / --name / -name=… があるかを見る。
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name || a == "-"+name ||
			strings.HasPrefix(a, name+"=") || strings.HasPrefix(a, "-"+name+"=") {
			return true
		}
	}
	return false
}

// canOpen は、そのパスの DB を自分で開いて書けるかを見る。
// **無ければ「開ける」とは言わない。** 空のDBを作って書き込むのは、
// 本番へ届いていないのに届いたつもりになる一番まずい形（2026-09-04）。
func canOpen(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// reportLimits は statusLine の観測を報告口へ流す。
func reportLimits(sock string, r io.Reader) error {
	if sock == "" {
		return fmt.Errorf("報告口が無い")
	}
	body, err := io.ReadAll(io.LimitReader(r, 64*1024))
	if err != nil {
		return err
	}
	if !json.Valid(body) {
		return fmt.Errorf("statusLine の JSON が読めない")
	}
	msg, err := json.Marshal(report.Event{Kind: "limits", Payload: body})
	if err != nil {
		return err
	}
	c, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(append(msg, '\n')); err != nil {
		return err
	}
	sc := bufio.NewScanner(c)
	if !sc.Scan() {
		return fmt.Errorf("返事が無い")
	}
	var rep report.Reply
	if err := json.Unmarshal(sc.Bytes(), &rep); err != nil {
		return err
	}
	if !rep.OK {
		return fmt.Errorf("断られた: %s", rep.Error)
	}
	return nil
}

// socketExists は、そこに socket があるかだけを見る（繋がるかは見ない）。
func socketExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

// 残量をどこへ渡すか。
const (
	targetDB     = "db"
	targetSocket = "socket"
)

// limitsTarget は渡し先を決める。**この順序が肝。**
//
//  1. 明示された DB があれば、そこへ直接（開発中の DB 用）
//  2. 報告口があれば、そこへ。常駐している campd が DB の持ち主
//  3. どちらも無ければ既定のパスへ直接（campd 単体で使う場合）
//
// 2 を 1 より後に置くのは意図。逆にすると、cwd にたまたま古い
// data/camp.sqlite があるだけでそちらへ書き、**本番へ届いていないのに
// 届いたつもりになる**（2026-09-04 に実際に起きた）。
func limitsTarget(explicit bool, sock, dbPath string) string {
	if explicit {
		return targetDB
	}
	if sock != "" && socketExists(sock) {
		return targetSocket
	}
	if !canOpen(dbPath) {
		return targetSocket
	}
	return targetDB
}
