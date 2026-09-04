// Package secrets は「ここが認証情報っぽい」を記録する。消さない・書き換えない。
//
// D-010 のとおり、このコーパスではパターンによるリダクトは有害だった。
// 検出器は raw_json を1バイトも変えず、`sensitive_findings` に位置だけ残す。
package secrets

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MoomA-0750/camp/internal/ingest"
	"github.com/MoomA-0750/camp/internal/store"
)

// Pattern は高信頼のパターン1つ。
//
// 素朴なパターンを増やさないこと。実測（2026-09-02、162MiB）では
// `password\s*[:=]\s*…` が47件、base64 の長い連なりが 15,773件（全メッセージの
// 21%）に当たる。当たりすぎる検出器は無視されるようになり、無視される
// 検出器は無いのと同じになる。
type Pattern struct {
	Name string
	Why  string
	// Prefilter があるときは、これを含むメッセージだけ正規表現にかける。
	// 162MiB を9本ぶん舐めるので、リテラル前方一致で落とせるものは落とす。
	Prefilter []string
	Re        *regexp.Regexp
}

// Patterns は Vault の .githooks/pre-push と同じ顔ぶれ。
// 「発行元が決めた接頭辞と長さ」を持つものだけを見る。
var Patterns = []Pattern{
	{"github-pat-classic", "GitHub personal access token（classic）",
		[]string{"ghp_"}, regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`)},
	{"github-oauth", "GitHub OAuth / server / user / refresh",
		[]string{"gho_", "ghs_", "ghu_", "ghr_"}, regexp.MustCompile(`gh[osur]_[A-Za-z0-9]{36}`)},
	{"github-pat-fine", "GitHub fine-grained PAT",
		[]string{"github_pat_"}, regexp.MustCompile(`github_pat_[A-Za-z0-9_]{60,}`)},
	{"gitlab-pat", "GitLab PAT",
		[]string{"glpat-"}, regexp.MustCompile(`glpat-[A-Za-z0-9_-]{20,}`)},
	{"slack-token", "Slack トークン",
		[]string{"xox"}, regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`)},
	{"aws-access-key", "AWS access key id",
		[]string{"AKIA"}, regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{"google-api-key", "Google API key",
		[]string{"AIza"}, regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`)},
	{"pem-private-key", "PEM 秘密鍵のヘッダ",
		[]string{"-----BEGIN"}, regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"plugin-credential", "設定ファイルの credential らしいキーと値",
		nil, regexp.MustCompile(`(?i)"[a-z_]*(?:token|apikey|api_key|password|secret)[a-z_]*"\s*:\s*"[^"]{8,}"`)},
}

// knownPrefix は、既知の秘密の照合につける印。
// パターンでは絶対に見つからないものをここで拾う（下記 Known を参照）。
const knownPrefix = "known:"

// Result は1回の走査の結果。
type Result struct {
	Messages int   // 走査したメッセージ
	Blobs    int   // 走査したブロブ（ノート本文・編集前の中身）
	Bytes    int64 // 走査したバイト数
	Found    int   // 見つけた箇所（既知のぶんを含む）
	New      int   // そのうち今回はじめて記録したもの
	Known    int   // 既知の秘密として当たった箇所
	Patterns map[string]int
}

// Known は「これは秘密だと分かっている文字列」の一覧。
//
// パターン検出はこのコーパスで **偽陽性100%・偽陰性100%** だった。
// 当たった14箇所はすべてパターン文字列そのもの（リーク試験や SQL の断片）で、
// 一方で実在する平文パスワードは接頭辞も構造も持たないので1件も当たらない。
// だから「知っている秘密を突き合わせる」経路を別に用意する。
//
// 中身はリポジトリに置かない。既定は DB と同じディレクトリの known-secrets.txt
// （data/ は gitignore 済み）で、CAMP_KNOWN_SECRETS で差し替えられる。
// 1行1つ、`ラベル<TAB>文字列`。空行と # で始まる行は無視する。
type Known struct {
	Label string
	Value string
}

// LoadKnown は既知の秘密を読む。ファイルが無ければ空で返す（エラーにしない）。
func LoadKnown(path string) ([]Known, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Known
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		label, value, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("%s: ラベルと値をタブで区切ること", path)
		}
		if value == "" {
			continue
		}
		out = append(out, Known{Label: label, Value: value})
	}
	return out, nil
}

// scanChunk は一度に読むメッセージ数。SetMaxOpenConns(1) なので、
// Rows を開いたまま書けない。読み切ってから書く。
const scanChunk = 300

// Scan は raw_json を走査して見つけた場所を記録する。何も書き換えない。
//
// 冪等。同じ場所は二度記録せず、人が付けた verdict も消さない
// （ux_findings_spot ＋ ON CONFLICT DO NOTHING）。
func Scan(db *store.DB, known []Known) (*Result, error) {
	res := &Result{Patterns: map[string]int{}}
	var last int64

	if err := scanBlobs(db, known, res); err != nil {
		return nil, err
	}

	for {
		type row struct {
			id  int64
			raw []byte
		}
		rows, err := db.Query(`select id, raw_json from messages
		                        where id > ? order by id limit ?`, last, scanChunk)
		if err != nil {
			return nil, err
		}
		var batch []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.raw); err != nil {
				rows.Close()
				return nil, err
			}
			batch = append(batch, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		if len(batch) == 0 {
			break
		}

		tx, err := db.Begin()
		if err != nil {
			return nil, err
		}
		stmt, err := tx.Prepare(`
			insert into sensitive_findings(message_id, pattern, byte_offset, length, found_at)
			values(?,?,?,?,?)
			on conflict(message_id, pattern, byte_offset) where message_id is not null do nothing`)
		if err != nil {
			tx.Rollback()
			return nil, err
		}
		now := time.Now().UTC().Format(time.RFC3339)

		for _, r := range batch {
			last = r.id
			res.Messages++
			res.Bytes += int64(len(r.raw))
			for _, f := range findIn(r.raw, known) {
				res.Found++
				res.Patterns[f.pattern]++
				if strings.HasPrefix(f.pattern, knownPrefix) {
					res.Known++
				}
				out, err := stmt.Exec(r.id, f.pattern, f.offset, f.length, now)
				if err != nil {
					stmt.Close()
					tx.Rollback()
					return nil, err
				}
				if n, _ := out.RowsAffected(); n > 0 {
					res.New++
				}
			}
		}
		stmt.Close()
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	return res, nil
}

type spot struct {
	pattern string
	offset  int
	length  int
}

// findIn は1件の raw_json から当たった場所を返す。
func findIn(raw []byte, known []Known) []spot {
	var out []spot
	for _, p := range Patterns {
		if !prefiltered(raw, p.Prefilter) {
			continue
		}
		for _, m := range p.Re.FindAllIndex(raw, -1) {
			out = append(out, spot{p.Name, m[0], m[1] - m[0]})
		}
	}
	for _, k := range known {
		v := []byte(k.Value)
		for off := 0; ; {
			i := bytes.Index(raw[off:], v)
			if i < 0 {
				break
			}
			out = append(out, spot{knownPrefix + k.Label, off + i, len(v)})
			off += i + len(v)
		}
	}
	return out
}

func prefiltered(raw []byte, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if bytes.Contains(raw, []byte(p)) {
			return true
		}
	}
	return false
}

// Finding は記録済みの1件を、人が判断できる形にしたもの。
type Finding struct {
	ID        int64 `json:"id"`
	MessageID int64 `json:"message_id,omitempty"`
	// Blob は blobs 由来の所見（ノート本文・編集前の中身）。
	// Where はどちらの入れ物かを人が読める形で持つ。
	Blob      string `json:"blob_sha256,omitempty"`
	Where     string `json:"where"`
	SessionID string `json:"session_id"`
	Title     string `json:"title,omitempty"`
	Type      string `json:"type"`
	At        string `json:"timestamp"`
	Pattern   string `json:"pattern"`
	Offset    int    `json:"byte_offset"`
	Length    int    `json:"length"`
	Reviewed  bool   `json:"reviewed"`
	Verdict   string `json:"verdict,omitempty"`
	// Context は当たった場所の前後。既定では当たり自体を伏せる。
	Context string `json:"context"`
}

// listSQL は messages 由来と blobs 由来の所見を1つに並べる。
// 入れ物が2つあることを呼び出し側に押し付けない。
//
// blobs 側の raw は取らない（15MiB 級のものが混ざる）。中身は
// blobContext が必要なぶんだけ読み直す。
const listSQL = `
	select f.id, coalesce(f.message_id, 0), coalesce(f.blob_sha256, ''),
	       coalesce(m.session_id, ''),
	       coalesce(nullif(s.ai_title, ''), nullif(s.user_title, ''), ''),
	       coalesce(m.type, ''), coalesce(m.timestamp, ''),
	       f.pattern, f.byte_offset, f.length,
	       f.reviewed, coalesce(f.verdict, ''), m.raw_json
	  from sensitive_findings f
	  left join messages m on m.id = f.message_id
	  left join sessions s on s.id = m.session_id
	 where (? = 0 or f.reviewed = 0)
	 order by f.pattern, f.id`

// List は記録済みの検出結果を返す。reveal が false のとき、
// 当たった文字列そのものは伏せて長さだけ出す。
func List(db *store.DB, onlyUnreviewed, reveal bool) ([]Finding, error) {
	flag := 0
	if onlyUnreviewed {
		flag = 1
	}
	rows, err := db.Query(listSQL, flag)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Finding
	for rows.Next() {
		var f Finding
		var raw []byte
		if err := rows.Scan(&f.ID, &f.MessageID, &f.Blob, &f.SessionID, &f.Title, &f.Type,
			&f.At, &f.Pattern, &f.Offset, &f.Length, &f.Reviewed, &f.Verdict, &raw); err != nil {
			return nil, err
		}
		if f.Blob != "" {
			// 表示のために落ちない。sha256 は 64 文字のはずだが、
			// 「はず」で切らない（2026-09-04 に search が同じ形で panic した）。
			n := len(f.Blob)
			if n > 12 {
				n = 12
			}
			f.Where = "blob " + f.Blob[:n]
		} else {
			f.Where = "msg " + strconv.FormatInt(f.MessageID, 10)
			f.Context = contextAround(raw, f.Offset, f.Length, reveal)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// blobs 側の前後は、Rows を閉じてから読む（SetMaxOpenConns(1)）。
	for i := range out {
		if out[i].Blob == "" {
			continue
		}
		body, where, err := blobBody(db, out[i].Blob)
		if err != nil {
			return nil, err
		}
		if where != "" {
			out[i].Where = where
		}
		out[i].Context = contextAround(body, out[i].Offset, out[i].Length, reveal)
	}
	return out, nil
}

// contextAround は当たりの前後を1行に潰して返す。
//
// 既定で当たり自体を伏せるのは、判断のために端末へ本物を書き出す必要が
// 無いから。ほとんどの偽陽性は「まわりが SQL かシェルか」で判別できる。
func contextAround(raw []byte, off, length int, reveal bool) string {
	if off < 0 || off+length > len(raw) {
		return ""
	}
	start := max(0, off-70)
	end := min(len(raw), off+length+50)
	mid := string(raw[off : off+length])
	if !reveal {
		mid = fmt.Sprintf("«伏せた %d 字»", length)
	}
	s := string(raw[start:off]) + mid + string(raw[off+length:end])
	s = strings.NewReplacer("\n", "⏎", "\r", "", "\t", " ").Replace(s)
	return strings.TrimSpace(s)
}

// Review は1件に人の判断を付ける。検出結果は消さない。
func Review(db *store.DB, id int64, verdict string) error {
	r, err := db.Exec(`update sensitive_findings set reviewed = 1, verdict = ? where id = ?`,
		verdict, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("検出結果 %d は無い", id)
	}
	return nil
}

// DefaultKnownPath は既知の秘密の一覧の既定の置き場。DB と同じディレクトリ。
func DefaultKnownPath(dbPath string) string {
	if p := os.Getenv("CAMP_KNOWN_SECRETS"); p != "" {
		return p
	}
	i := strings.LastIndexByte(dbPath, '/')
	if i < 0 {
		return "known-secrets.txt"
	}
	return dbPath[:i+1] + "known-secrets.txt"
}

// scanBlobs は blobs も走査する。
//
// Phase 1 でノート本文が blobs に入った時点で、同じ秘密が「走査されない場所」
// にもう1つ増えた（実測: Vault の平文パスワードが messages 25件 + blobs 1個）。
// 入れ物が増えたのに検出器を広げないと、片方だけ見て「無い」と言うことになる。
//
// blobs は中身が大きいので1つずつ読む。messages のように一括で持つと、
// 15.5MiB のバックアップと 15.4MiB のノートを丸ごとメモリに載せることになる。
func scanBlobs(db *store.DB, known []Known, res *Result) error {
	var hashes []string
	rows, err := db.Query(`select sha256 from blobs order by sha256`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return err
		}
		hashes = append(hashes, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < len(hashes); i += scanChunk {
		end := min(i+scanChunk, len(hashes))
		if err := scanBlobChunk(db, known, res, hashes[i:end], now); err != nil {
			return err
		}
	}
	return nil
}

func scanBlobChunk(db *store.DB, known []Known, res *Result, hashes []string, now string) error {
	type blob struct {
		hash string
		body []byte
	}
	var batch []blob
	for _, h := range hashes {
		var codec string
		var content []byte
		if err := db.QueryRow(`select codec, content from blobs where sha256 = ?`, h).
			Scan(&codec, &content); err != nil {
			return err
		}
		body, err := ingest.UnpackBlob(codec, content)
		if err != nil {
			// 展開できないものは飛ばす。1つで走査全体を落とさない。
			continue
		}
		batch = append(batch, blob{hash: h, body: body})
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`
		insert into sensitive_findings(blob_sha256, pattern, byte_offset, length, found_at)
		values(?,?,?,?,?)
		on conflict(blob_sha256, pattern, byte_offset) where blob_sha256 is not null do nothing`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()

	for _, b := range batch {
		res.Blobs++
		res.Bytes += int64(len(b.body))
		for _, f := range findIn(b.body, known) {
			res.Found++
			res.Patterns[f.pattern]++
			if strings.HasPrefix(f.pattern, knownPrefix) {
				res.Known++
			}
			out, err := stmt.Exec(b.hash, f.pattern, f.offset, f.length, now)
			if err != nil {
				tx.Rollback()
				return err
			}
			if n, _ := out.RowsAffected(); n > 0 {
				res.New++
			}
		}
	}
	return tx.Commit()
}

// blobBody はブロブの中身と、それが何なのか（ノートのパス等）を返す。
// 同じ中身を複数のノートが共有しうるので、名前は最初に見つかった1つ。
func blobBody(db *store.DB, hash string) (body []byte, where string, err error) {
	var codec string
	var content []byte
	if err := db.QueryRow(`select codec, content from blobs where sha256 = ?`, hash).
		Scan(&codec, &content); err != nil {
		return nil, "", err
	}
	body, err = ingest.UnpackBlob(codec, content)
	if err != nil {
		return nil, "", err
	}
	var p string
	if err := db.QueryRow(
		`select path from notes where sha256 = ? limit 1`, hash).Scan(&p); err == nil {
		return body, "note " + p, nil
	}
	if err := db.QueryRow(
		`select coalesce(rel_path, abs_path) from file_backups where sha256 = ? limit 1`,
		hash).Scan(&p); err == nil {
		return body, "backup " + p, nil
	}
	return body, "", nil
}
