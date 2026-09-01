package ingest

import (
	"database/sql"
	"encoding/json"
	"strings"
	"unicode"
)

// blockRow は検索の単位。1メッセージが複数持つ。
//
// メッセージ単位にすると、`tool_result` の巨大な出力と直前の一言が
// 同じ文書に混ざってスコアが壊れる。ブロック単位なら `kind` で絞れる。
type blockRow struct {
	Idx       int
	Kind      string // text | thinking | tool_use | tool_result | image | attachment
	ToolName  string
	ToolUseID string
	Text      string
}

// extractBlocks は1行から検索対象のブロックを取り出す。
//
// `toolUseResult`（行の最上位）は索引に入れない。中身は message.content の
// `tool_result` ブロックとほぼ重複しており（実測 content 68MB 対 toolUseResult 57MB）、
// 両方入れると索引が倍になるうえ同じヒットが2回出る。
// 構造化された差分（structuredPatch 等）が要るようになったら raw_json から取れる。
func extractBlocks(l *Line) []blockRow {
	var out []blockRow

	if l.Message != nil {
		blocks, err := l.Message.Blocks()
		if err != nil {
			// content の形が想定外。行は messages に残っているので後から作り直せる。
			return nil
		}
		for i, b := range blocks {
			r := blockRow{Idx: len(out), Kind: b.Type, ToolName: b.Name, ToolUseID: b.ToolUseID}
			if r.Kind == "" {
				r.Kind = "text"
			}
			switch b.Type {
			case "text", "":
				r.Text = b.Text
			case "thinking":
				r.Text = b.Thinking
			case "tool_use":
				r.ToolUseID = b.ID // tool_use 側は id、tool_result 側は tool_use_id
				r.Text = jsonText(b.Input)
			case "tool_result":
				r.Text = flattenResult(b.Content)
			case "image":
				continue // base64 は索引しても引けない
			default:
				r.Text = b.Text
			}
			if strings.TrimSpace(r.Text) == "" {
				continue
			}
			out = append(out, r)
			_ = i
		}
	}

	// attachment 行は message を持たないことがある。ファイル名だけでも引けるようにする。
	if l.Type == "attachment" {
		if kind, name := l.AttachmentInfo(); name != "" {
			out = append(out, blockRow{Idx: len(out), Kind: "attachment", ToolName: kind, Text: name})
		}
	}
	return out
}

// jsonText は tool_use の入力を検索できる文字列にする。
// キー名ごと入れる。`file_path` や `pattern` を憶えていなくても
// 値のほうを憶えていれば引けるし、キー名で引きたいこともある。
func jsonText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	var b strings.Builder
	walkJSON(v, &b)
	return b.String()
}

func walkJSON(v any, b *strings.Builder) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			b.WriteString(k)
			b.WriteByte(' ')
			walkJSON(val, b)
		}
	case []any:
		for _, val := range t {
			walkJSON(val, b)
		}
	case string:
		b.WriteString(t)
		b.WriteByte(' ')
	case nil:
	default:
		b.WriteString(jsonScalar(t))
		b.WriteByte(' ')
	}
}

func jsonScalar(v any) string {
	out, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(out)
}

// flattenResult は tool_result の content を平らな文字列にする。
// 文字列のこともブロック配列のこともある。
func flattenResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []Block
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Text != "" {
				b.WriteString(blk.Text)
				b.WriteByte('\n')
			}
		}
		return b.String()
	}
	return string(raw)
}

// isCJK は unicode61 が語として切り出せない文字かどうか。
//
// FTS5 の unicode61 は空白と記号で切る。日本語・中国語には空白が無いので、
// 文全体が1トークンになり「設定」では引けない。trigram も2文字クエリで落ちる。
// だから CJK の連なりだけを2文字ずつずらして重ねる（M0で実証済み）。
//
// ハングルは含めない。単語間に空白を置く言語なので unicode61 で切れる。
func isCJK(r rune) bool {
	switch {
	case r >= 0x3040 && r <= 0x30FF: // ひらがな・カタカナ（長音符 30FC を含む）
		return true
	case r >= 0x3400 && r <= 0x4DBF: // CJK拡張A
		return true
	case r >= 0x4E00 && r <= 0x9FFF: // CJK統合漢字
		return true
	case r >= 0xF900 && r <= 0xFAFF: // 互換漢字
		return true
	case r >= 0xFF66 && r <= 0xFF9D: // 半角カタカナ
		return true
	case r >= 0x20000 && r <= 0x2FA1F: // CJK拡張B以降
		return true
	}
	return false
}

// HasCJK は bigram 分かち書きが要るかどうか。
func HasCJK(s string) bool {
	for _, r := range s {
		if isCJK(r) {
			return true
		}
	}
	return false
}

// Bigrams は CJK の連なりだけを2文字ずつ重ねて切り、それ以外はそのまま通す。
//
//	"Redmineで進捗報告"  →  "Redmine で進 進捗 捗報 報告"
//
// 索引と問い合わせの両方で同じ関数を通すこと。片方だけ変えると無言で0件になる。
func Bigrams(s string) string {
	if !HasCJK(s) {
		return s // ASCII だけなら unicode61 がそのまま切れる。触らない
	}
	var b strings.Builder
	b.Grow(len(s) * 2)
	// 直前に書いた文字を覚えておく。b.String() を毎回取ると O(n^2) になる。
	var last rune

	write := func(r rune) {
		b.WriteRune(r)
		last = r
	}
	sep := func() {
		if b.Len() > 0 && !unicode.IsSpace(last) {
			write(' ')
		}
	}

	runes := []rune(s)
	for i := 0; i < len(runes); {
		if !isCJK(runes[i]) {
			write(runes[i])
			i++
			continue
		}
		j := i
		for j < len(runes) && isCJK(runes[j]) {
			j++
		}
		run := runes[i:j]
		sep()
		if len(run) == 1 {
			// 1文字だけの連なり。そのまま1トークンにする。
			// 問い合わせ側は前方一致（`管*`）で拾う。
			write(run[0])
		} else {
			for k := 0; k+1 < len(run); k++ {
				if k > 0 {
					write(' ')
				}
				write(run[k])
				write(run[k+1])
			}
		}
		i = j
		if i < len(runes) && !unicode.IsSpace(runes[i]) {
			write(' ')
		}
	}
	return b.String()
}

// blockWriter は message_blocks と messages_fts を対で埋める。
//
// messages_fts は external-content なので中身は自動で追従しない。
// 片方だけ書くと doctor の integrity-check が落ちる。必ずここを通す。
type blockWriter struct {
	ins *sql.Stmt
	fts *sql.Stmt
}

func newBlockWriter(tx *sql.Tx) (*blockWriter, error) {
	ins, err := tx.Prepare(`
		insert into message_blocks(message_id, idx, kind, tool_name, tool_use_id, text, bigrams)
		values(?,?,?,?,?,?,?)`)
	if err != nil {
		return nil, err
	}
	fts, err := tx.Prepare(`insert into messages_fts(rowid, bigrams) values(?,?)`)
	if err != nil {
		ins.Close()
		return nil, err
	}
	return &blockWriter{ins: ins, fts: fts}, nil
}

func (w *blockWriter) Close() {
	w.ins.Close()
	w.fts.Close()
}

// write は1メッセージぶんのブロックを書く。書いた本数を返す。
func (w *blockWriter) write(messageID int64, l *Line) (int, error) {
	blocks := extractBlocks(l)
	for _, b := range blocks {
		bg := Bigrams(b.Text)
		res, err := w.ins.Exec(messageID, b.Idx, b.Kind, nz(b.ToolName), nz(b.ToolUseID), b.Text, bg)
		if err != nil {
			return 0, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return 0, err
		}
		if _, err := w.fts.Exec(id, bg); err != nil {
			return 0, err
		}
	}
	return len(blocks), nil
}
