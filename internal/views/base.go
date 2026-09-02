// Package views は Obsidian Bases の `.base` を読み、同じ定義から
// 人向けの表とモデル向けの文脈の両方を作る。
//
// **Camp は `.base` を書き戻さない。** 読むだけ（Phase 1 と同じ方針）。
package views

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ビューの種別。実在するのはこの3つだけ（実測30ビュー）。
const (
	KindTable       = "table"
	KindCards       = "cards"
	KindLifeTracker = "life-tracker"
)

// Base は1つの `.base` ファイル。
type Base struct {
	Path     string            `json:"path"`
	Name     string            `json:"name"`
	Filters  *Filter           `json:"filters,omitempty"`
	Formulas map[string]string `json:"formulas,omitempty"`
	Views    []View            `json:"views"`

	// Display は `properties:` の displayName。列の見出しに使う。
	Display map[string]string `json:"display,omitempty"`

	// ParseError は読めなかった `.base` に入る。**Views は空になるが、
	// Base の行そのものは残す。** 1ファイルが読めないだけで
	// ビュー機構が全部落ちるのは、原因が見えないぶん質が悪い。
	ParseError string `json:"parse_error,omitempty"`
}

// View は1つのビュー。
type View struct {
	Kind      string            `json:"kind"`
	Name      string            `json:"name"`
	Filters   *Filter           `json:"filters,omitempty"`
	Order     []string          `json:"order,omitempty"`
	Hide      []string          `json:"hide,omitempty"`
	Sort      []SortKey         `json:"sort,omitempty"`
	GroupBy   *SortKey          `json:"group_by,omitempty"`
	Summaries map[string]string `json:"summaries,omitempty"`

	// Extra は Camp が解釈しないキー（gridColumns・columnSize・cardSize・
	// columnConfigs・timeFrame など）。**捨てずに持つ。** 捨てると、
	// Obsidian が書いた設定が Camp を経由しただけで消える。
	Extra map[string]any `json:"extra,omitempty"`
}

// SortKey は並べ替え1段。
type SortKey struct {
	Property  string `json:"property"`
	Direction string `json:"direction,omitempty"` // ASC / DESC
}

// Filter は絞り込み。and / or / not の入れ子か、葉の式。
//
// Obsidian のフィルタUIは「all / any / none」の3つで、それぞれ
// `and` / `or` / `not` に対応する。**`not` は「どれも真でない」**
// （NOR）として読む。
type Filter struct {
	And  []*Filter `json:"and,omitempty"`
	Or   []*Filter `json:"or,omitempty"`
	Not  []*Filter `json:"not,omitempty"`
	Expr string    `json:"expr,omitempty"`
}

// **解釈するキー**。Extra はこれ以外の全部を持つ。
//
// 逆にしてある（許可リストではなく除外リスト）。許可リストにすると、
// Obsidian が将来足したキーが Camp を通っただけで消える——`order:` の
// 列モデルと同じ失敗の形。実際に旧実装は `properties:` を丸ごと捨てていた。
var interpretedViewKeys = map[string]bool{
	"type": true, "name": true, "filters": true, "order": true,
	"hide": true, "sort": true, "groupBy": true, "summaries": true,
}

// unqualify は `note.foo` の名前空間接頭辞を外す。
//
// Bases は frontmatter のプロパティを `note.<名前>` と完全修飾でも書ける。
// 実在の `.base` は `order:` に `note.note` を5ビューで挙げている。
// 剥がさないと、定義が指名しているのに空欄の列が出る（`file.*` で
// 直したのと同じ欠陥）。`file.` と `formula.` は本物の名前空間なので触らない。
func unqualify(k string) string {
	if rest, ok := strings.CutPrefix(k, "note."); ok && rest != "" {
		return rest
	}
	return k
}

func unqualifyAll(ks []string) []string {
	for i, k := range ks {
		ks[i] = unqualify(k)
	}
	return ks
}

// ParseBase は `.base` の中身を読む。
func ParseBase(path string, body []byte) (*Base, error) {
	var raw struct {
		Filters    yaml.Node         `yaml:"filters"`
		Formulas   map[string]string `yaml:"formulas"`
		Views      []yaml.Node       `yaml:"views"`
		Properties map[string]struct {
			DisplayName string `yaml:"displayName"`
		} `yaml:"properties"`
	}
	if err := yaml.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	b := &Base{Path: path, Name: baseName(path), Formulas: raw.Formulas}
	for k, pr := range raw.Properties {
		if pr.DisplayName == "" {
			continue
		}
		if b.Display == nil {
			b.Display = map[string]string{}
		}
		b.Display[unqualify(k)] = pr.DisplayName
	}
	f, err := parseFilter(&raw.Filters)
	if err != nil {
		return nil, fmt.Errorf("%s: filters: %w", path, err)
	}
	b.Filters = f

	for i, vn := range raw.Views {
		v, err := parseView(&vn)
		if err != nil {
			return nil, fmt.Errorf("%s: views[%d]: %w", path, i, err)
		}
		b.Views = append(b.Views, *v)
	}
	return b, nil
}

func parseView(n *yaml.Node) (*View, error) {
	var head struct {
		Type      string            `yaml:"type"`
		Name      string            `yaml:"name"`
		Filters   yaml.Node         `yaml:"filters"`
		Order     []string          `yaml:"order"`
		Hide      []string          `yaml:"hide"`
		Sort      []SortKey         `yaml:"sort"`
		GroupBy   *SortKey          `yaml:"groupBy"`
		Summaries map[string]string `yaml:"summaries"`
	}
	if err := n.Decode(&head); err != nil {
		return nil, err
	}
	v := &View{
		Kind: head.Type, Name: head.Name,
		Order: unqualifyAll(head.Order), Hide: unqualifyAll(head.Hide),
		Sort: head.Sort, GroupBy: head.GroupBy,
	}
	for i := range v.Sort {
		v.Sort[i].Property = unqualify(v.Sort[i].Property)
	}
	if v.GroupBy != nil {
		v.GroupBy.Property = unqualify(v.GroupBy.Property)
	}
	if len(head.Summaries) > 0 {
		v.Summaries = make(map[string]string, len(head.Summaries))
		for k, fn := range head.Summaries {
			v.Summaries[unqualify(k)] = fn
		}
	}
	f, err := parseFilter(&head.Filters)
	if err != nil {
		return nil, err
	}
	v.Filters = f

	// 解釈しないキーは**全部**拾う。捨てると往復で消える。
	var all map[string]any
	if err := n.Decode(&all); err == nil {
		for k, val := range all {
			if interpretedViewKeys[k] {
				continue
			}
			if v.Extra == nil {
				v.Extra = map[string]any{}
			}
			v.Extra[k] = val
		}
	}
	return v, nil
}

// parseFilter は `and:` / `or:` の入れ子と、文字列の葉を読む。
func parseFilter(n *yaml.Node) (*Filter, error) {
	if n == nil || n.Kind == 0 {
		return nil, nil
	}
	switch n.Kind {
	case yaml.ScalarNode:
		s := strings.TrimSpace(n.Value)
		if s == "" {
			return nil, nil
		}
		return &Filter{Expr: s}, nil
	case yaml.SequenceNode:
		// 素の配列は and 扱い（Obsidian も同じ）。
		f := &Filter{}
		for i := range n.Content {
			c, err := parseFilter(n.Content[i])
			if err != nil {
				return nil, err
			}
			if c != nil {
				f.And = append(f.And, c)
			}
		}
		return f, nil
	case yaml.MappingNode:
		f := &Filter{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i].Value
			child, err := parseFilter(n.Content[i+1])
			if err != nil {
				return nil, err
			}
			if child == nil {
				continue
			}
			switch key {
			case "and":
				f.And = append(f.And, flatten(child)...)
			case "or":
				f.Or = append(f.Or, flatten(child)...)
			case "not":
				f.Not = append(f.Not, flatten(child)...)
			default:
				return nil, fmt.Errorf("知らない結合子 %q", key)
			}
		}
		return f, nil
	}
	return nil, fmt.Errorf("filters の形が読めない（kind=%d）", n.Kind)
}

// flatten は and/or の中身が配列で来たときに1段ならす。
func flatten(f *Filter) []*Filter {
	if f.Expr == "" && len(f.Or) == 0 && len(f.Not) == 0 && len(f.And) > 0 {
		return f.And
	}
	return []*Filter{f}
}

func baseName(path string) string {
	s := path
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSuffix(s, ".base")
}

// Exprs は葉の式を全部返す（テストと点検用）。
func (f *Filter) Exprs() []string {
	if f == nil {
		return nil
	}
	if f.Expr != "" {
		return []string{f.Expr}
	}
	var out []string
	kids := append([]*Filter{}, f.And...)
	kids = append(kids, f.Or...)
	kids = append(kids, f.Not...)
	for _, c := range kids {
		out = append(out, c.Exprs()...)
	}
	sort.Strings(out)
	return out
}
