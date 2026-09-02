// Package search は会話記録の全文検索を行う。
//
// 日本語は無言で失敗する。unicode61 は空白でしか切らないので文が丸ごと
// 1トークンになり、trigram は2文字クエリで0件になる。だから索引側で
// CJK を bigram に刻んである（ingest.Bigrams）。
// **問い合わせ側も必ず同じ関数を通すこと。** 片方だけ変えると
// エラーも警告も出ないまま常に0件になる。
package search

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/MoomA-0750/camp/internal/ingest"
	"github.com/MoomA-0750/camp/internal/store"
)

type Hit struct {
	BlockID   int64   `json:"block_id"`
	MessageID int64   `json:"message_id"`
	SessionID string  `json:"session_id"`
	Title     string  `json:"title"`
	Kind      string  `json:"kind"`
	ToolName  string  `json:"tool_name,omitempty"`
	Timestamp string  `json:"timestamp"`
	Snippet   string  `json:"snippet"`
	Score     float64 `json:"score"`
}

type Opts struct {
	Kind    string // 空なら全種別
	Session string // 空なら全セッション
	Limit   int
}

// Query は検索する。q は人間が打った文字列そのまま。
func Query(db *store.DB, q string, o Opts) ([]Hit, error) {
	expr, terms := BuildMatch(q)
	if expr == "" {
		return nil, nil
	}
	if o.Limit <= 0 {
		o.Limit = 20
	}

	rows, err := db.Query(`
		select b.id, b.message_id, b.kind, coalesce(b.tool_name, ''), b.text,
		       m.session_id, coalesce(m.timestamp, ''),
		       coalesce(s.ai_title, s.first_user_message, ''),
		       bm25(messages_fts) as score
		  from messages_fts f
		  join message_blocks b on b.id = f.rowid
		  join messages m       on m.id = b.message_id
		  join sessions s       on s.id = m.session_id
		 where messages_fts match ?
		   and (? = '' or b.kind = ?)
		   and (? = '' or m.session_id = ?)
		 order by score
		 limit ?`,
		expr, o.Kind, o.Kind, o.Session, o.Session, o.Limit)
	if err != nil {
		return nil, fmt.Errorf("検索 %q（式 %q）: %w", q, expr, err)
	}
	defer rows.Close()

	var out []Hit
	for rows.Next() {
		var h Hit
		var text string
		if err := rows.Scan(&h.BlockID, &h.MessageID, &h.Kind, &h.ToolName, &text,
			&h.SessionID, &h.Timestamp, &h.Title, &h.Score); err != nil {
			return nil, err
		}
		// 抜粋は bigram 列ではなく元のテキストから作る。
		// FTS5 の snippet() は「設定 定を 変え」のような分かち書き済みの
		// 文字列を返すので、人間に見せるものにはならない。
		h.Snippet = Excerpt(text, terms, 80)
		out = append(out, h)
	}
	return out, rows.Err()
}

// BuildMatch は人間の入力を FTS5 の MATCH 式に変換する。
// 併せて、抜粋を作るための元の語も返す。
func BuildMatch(q string) (expr string, terms []string) {
	for _, t := range strings.Fields(q) {
		if t == "" {
			continue
		}
		terms = append(terms, t)
		bg := ingest.Bigrams(t)
		switch {
		case !ingest.HasCJK(t):
			expr += " " + quote(t)
		case utf8.RuneCountInString(t) == 1:
			// 1文字のCJK。索引には bigram しか無いので完全一致では引けない。
			// 前方一致で「その文字で始まるbigram」を拾う。
			// 語の途中や末尾にある1文字（保"管" の 管）は取りこぼす。
			expr += " " + quote(t) + "*"
		default:
			// bigram の並びをフレーズとして問う。連続していることまで見るので
			// 「設定を変更」が「変更を設定」に当たらない。
			expr += " " + quote(bg)
		}
	}
	return strings.TrimSpace(expr), terms
}

// quote は FTS5 の文字列リテラルにする。中の " は重ねて逃がす。
func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// Excerpt は元のテキストから、最初に当たった語の周りを切り出す。
func Excerpt(text string, terms []string, width int) string {
	flat := strings.Join(strings.Fields(text), " ")
	runes := []rune(flat)
	at := -1
	for _, t := range terms {
		if i := indexFold(flat, t); i >= 0 {
			at = utf8.RuneCountInString(flat[:i])
			break
		}
	}
	if at < 0 {
		if len(runes) <= width {
			return flat
		}
		return string(runes[:width]) + "…"
	}
	start := at - width/2
	if start < 0 {
		start = 0
	}
	end := start + width
	if end > len(runes) {
		end = len(runes)
		if start = end - width; start < 0 {
			start = 0
		}
	}
	s := string(runes[start:end])
	if start > 0 {
		s = "…" + s
	}
	if end < len(runes) {
		s += "…"
	}
	return s
}

func indexFold(hay, needle string) int {
	return strings.Index(strings.ToLower(hay), strings.ToLower(needle))
}
