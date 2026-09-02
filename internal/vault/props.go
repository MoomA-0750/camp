package vault

import (
	"strconv"
	"strings"

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
		out = append(out, flattenProp(k, v)...)
	}
	return out
}

func flattenProp(key string, v any) []Prop {
	switch t := v.(type) {
	case nil:
		return nil
	case []any:
		var out []Prop
		for i, e := range t {
			for _, p := range flattenProp(key, e) {
				p.Seq = i
				out = append(out, p)
			}
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

func scalarNum(v any) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case float64:
		return t, true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		if n, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return n, true
		}
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
