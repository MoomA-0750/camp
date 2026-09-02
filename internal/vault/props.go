package vault

import (
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Prop はノートのfrontmatter1件。リスト値は Seq で並びを保つ。
type Prop struct {
	Key  string
	Seq  int
	Text string
	Num  *float64
}

// ExtractProps は frontmatter を取り出す。
//
// 値を text と num の両方に入れるのは、並べ替えと Sum のため。
// 文字列だけで持つと "10" < "9" になる。
//
// リスト値（`category: [General]` や `source: - "[[x]]"`）は1件1行にほどく。
// このVaultの To-Do は multitext を常用しているので、畳むと中身が消える。
func ExtractProps(body []byte) []Prop {
	fm := frontmatter(body)
	if len(fm) == 0 {
		return nil
	}
	var doc map[string]any
	if err := yaml.Unmarshal(fm, &doc); err != nil {
		return nil
	}
	var out []Prop
	for k, v := range doc {
		ps := flattenProp(k, v)
		// **seq は畳んだ後の通し番号にする。** 子の添字を外側の添字で
		// 上書きすると、入れ子の配列（`k: [[a,b],[c,d]]`）が同じ
		// (note_id,key,seq) を2行作り、note_props の主キーに当たって
		// **Vault索引のトランザクション全体がロールバックする。**
		for i := range ps {
			ps[i].Seq = i
		}
		out = append(out, ps...)
	}
	return out
}

func flattenProp(key string, v any) []Prop {
	switch t := v.(type) {
	case nil:
		return nil
	case []any:
		var out []Prop
		for _, e := range t {
			out = append(out, flattenProp(key, e)...)
		}
		return out
	case map[string]any:
		// 入れ子は行にほどかず、そのまま文字列で1件持つ。
		// このVaultには実在しないが、落として無かったことにはしない。
		b, _ := yaml.Marshal(t)
		return []Prop{{Key: key, Text: strings.TrimSpace(string(b))}}
	}
	p := Prop{Key: key, Text: scalarText(v)}
	if n, ok := scalarNum(v); ok {
		p.Num = &n
	}
	return []Prop{p}
}

func scalarText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case time.Time:
		// **YAMLは引用符の無い `date: 2026-08-19` を時刻として解決する。**
		// そのまま Marshal すると `2026-08-19T00:00:00Z` になり、Obsidian が
		// 見せている `2026-08-19` と違う値が列に出る。実測で Health の
		// 2,687件すべてがこれに当たっていた。書かれたとおりに戻す。
		if t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0 {
			return t.Format("2006-01-02")
		}
		return t.Format(time.RFC3339)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(sprint(v)), "\n"))
}

// scalarNum は **YAMLが数として書いたものだけ** を数にする。
//
// 引用符付きの文字列や真偽値まで拾うと、ゼロ埋めのID（`"007"`）や
// `complete: true` が数値ソートと Sum の対象に化ける。書き手が引用符を
// 付けたのは「数ではない」という意思表示なので、それを尊重する。
func scalarNum(v any) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case uint64:
		return float64(t), true
	case float64:
		return t, true
	}
	return 0, false
}

func sprint(v any) string {
	b, _ := yaml.Marshal(v)
	return string(b)
}

// frontmatter は先頭の `---` に挟まれた部分を返す。
// 無ければ空。**本文側は返さない。**
func frontmatter(body []byte) []byte {
	s := string(body)
	if !strings.HasPrefix(s, "---") {
		return nil
	}
	rest := s[3:]
	if !strings.HasPrefix(rest, "\n") && !strings.HasPrefix(rest, "\r\n") {
		return nil
	}
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil
	}
	return []byte(rest[:end])
}
