package views

// 時系列（Phase 4 / M51、2026-09-13）。
//
// **時間軸と集約は定義に書いてあるものだけを使い、既定値で補わない。** 実データを見ると
// ビューごとに意味が違い、既定値で描くと静かに嘘を描く:
//
//   - 銀行の残高は `last`。同じ日付のノートが最大8件あり、平均や合計にすると残高が嘘になる
//   - しかも**同じ日の中の並びはファイル名の逆順**（取り込みが新しい順）。ファイル名の順で
//     `last` を取ると、その日の**最初の**残高を描く。残高の連鎖（前の残高 − 出金 + 入金 = 残高）を
//     実データで確かめた: 逆順ならほぼすべて合い、正順だと大半が合わない
//   - d払い・給与は `year_month` が軸で、`date` を持たない
//
// だから `last`/`first` は**同じ区切りに2行以上あれば `time.within`（区切りの中の並び）を
// 求めて止まる**。`only` は「区切りに1行しか無い」という主張で、破れていれば止まる。

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// TimeSpec は時間軸（`shape.time`）。
type TimeSpec struct {
	Axis   string `yaml:"axis" json:"axis"`
	Bucket string `yaml:"bucket" json:"bucket"` // day / month / year
	// Within は区切りの中の並び。`last`/`first` はこの順で最後／最初を取る。
	Within []SortKey `yaml:"within,omitempty" json:"within,omitempty"`
}

// Emit は描き方（`emit`）。分析の中身は持たない。
type Emit struct {
	Human []Encoding `yaml:"human,omitempty" json:"human,omitempty"`
}

// Encoding は人向けの描き方1つ。
type Encoding struct {
	Kind   string         `yaml:"kind" json:"kind"`                         // table / chart
	Values []string       `yaml:"values,omitempty" json:"values,omitempty"` // chart: 描く列（`y` にしない。YAML 1.1 の真偽値と読まれ、書き出しで引用符が付く）
	Chart  string         `yaml:"chart,omitempty" json:"chart,omitempty"`   // chart: line / bar
	Window string         `yaml:"window,omitempty" json:"window,omitempty"` // chart: last-N-days / all
	Widths map[string]int `yaml:"widths,omitempty" json:"widths,omitempty"` // table: 列幅（本人の調整値）
	Colors []TagColor     `yaml:"colors,omitempty" json:"colors,omitempty"` // graph: タグで色分け（先に書いたものが勝つ）
}

// TagColor はグラフの色分け1つ（Obsidian の colorGroups の `tag:#…` と同じ考え方）。
type TagColor struct {
	Tag   string `yaml:"tag" json:"tag"`
	Color string `yaml:"color" json:"color"`
}

// 区切りと集約。**ここに無いものは定義として読めない**（保存も弾かれる）。
var (
	buckets  = map[string]bool{"day": true, "month": true, "year": true}
	measures = map[string]bool{"only": true, "last": true, "first": true,
		"sum": true, "avg": true, "min": true, "max": true}
	windowRe = regexp.MustCompile(`^last-[1-9][0-9]*-days$`)
	colorRe  = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

func (t *TimeSpec) validate() error {
	if t == nil {
		return nil
	}
	if t.Axis == "" {
		return fmt.Errorf("time.axis が無い")
	}
	// 区切りを既定値にしない。`date` を月で見るか日で見るかは意味が違う。
	if !buckets[t.Bucket] {
		return fmt.Errorf("time.bucket は day・month・year のどれか（%q）", t.Bucket)
	}
	for i := range t.Within {
		t.Within[i].Property = unqualify(t.Within[i].Property)
		if t.Within[i].Property == "" {
			return fmt.Errorf("time.within[%d] に property が無い", i)
		}
		if err := checkDirection(&t.Within[i], fmt.Sprintf("time.within[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}

func validateMeasures(m map[string]string) error {
	for k, fn := range m {
		if !measures[fn] {
			return fmt.Errorf("measures.%s: 知らない集約 %q（only・last・first・sum・avg・min・max）", k, fn)
		}
	}
	return nil
}

func (e *Emit) validate() error {
	if e == nil {
		return nil
	}
	for i := range e.Human {
		h := &e.Human[i]
		switch h.Kind {
		case "table":
		case "chart":
			if len(h.Values) == 0 {
				return fmt.Errorf("emit.human[%d]: chart に values が無い", i)
			}
			h.Values = unqualifyAll(h.Values)
			if h.Chart != "" && h.Chart != "line" && h.Chart != "bar" {
				return fmt.Errorf("emit.human[%d]: chart は line か bar（%q）", i, h.Chart)
			}
			if h.Window != "" && h.Window != "all" && !windowRe.MatchString(h.Window) {
				return fmt.Errorf("emit.human[%d]: window は last-N-days か all（%q）", i, h.Window)
			}
		case "graph":
			for j, c := range h.Colors {
				c.Tag = strings.TrimPrefix(c.Tag, "#")
				h.Colors[j].Tag = c.Tag
				if c.Tag == "" || !colorRe.MatchString(c.Color) {
					return fmt.Errorf("emit.human[%d].colors[%d]: tag と #rrggbb の color が要る", i, j)
				}
			}
		default:
			return fmt.Errorf("emit.human[%d]: 知らない描き方 %q（table・chart・graph）", i, h.Kind)
		}
	}
	return nil
}

// Series は時系列1本。
type Series struct {
	Key     string  `json:"key"`
	Label   string  `json:"label"`
	Measure string  `json:"measure,omitempty"`
	Points  []Point `json:"points,omitempty"`
	// Rows は点に使った行（値のあった行）。
	Rows int `json:"rows"`
	// Skipped は値があるのに時間軸か値が読めなかった行。**黙って落とさない。**
	Skipped int `json:"skipped,omitempty"`
	// Holes は値はあるのに数として読めない行を含むので、点にしなかった区切りの数。
	Holes int `json:"holes,omitempty"`
	// Error は描けない理由。点は出さない（嘘の点を出すより、描けないと言う）。
	Error string `json:"error,omitempty"`
}

// Point は区切り1つ。N はそこに入った行の数（`last` でも数える——同日8件が見えるように）。
type Point struct {
	T string  `json:"t"`
	V float64 `json:"v"`
	N int     `json:"n"`
}

// seriesKeys は時系列にする列。描く列（emit の values）を先に、集約だけ書いた列を後ろに。
// **集約だけの列もモデルは見る**（shape の側なので）。
func seriesKeys(v *View) []string {
	seen := map[string]bool{}
	var out []string
	if v.Emit != nil {
		for _, h := range v.Emit.Human {
			for _, k := range h.Values {
				if !seen[k] {
					seen[k] = true
					out = append(out, k)
				}
			}
		}
	}
	rest := make([]string, 0, len(v.Measures))
	for k := range v.Measures {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func buildSeries(b *Base, v *View, rows []*Record, cols []Column) []Series {
	keys := seriesKeys(v)
	if len(keys) == 0 {
		return nil
	}
	ordered := rows
	if v.Time != nil && len(v.Time.Within) > 0 {
		// 区切りの中の並び。表の並び（`sort`）とは別に持つ——表は新しい順に見たいが、
		// `last` は時の流れの順で最後を取る。
		ordered = append([]*Record(nil), rows...)
		sortRows(ordered, v.Time.Within, cols)
	}
	out := make([]Series, 0, len(keys))
	for _, k := range keys {
		s := Series{Key: k, Label: label(b, k), Measure: v.Measures[k]}
		switch {
		case v.Time == nil:
			s.Error = "時間軸（time.axis）が決まっていない"
		case s.Measure == "":
			s.Error = "集約（measures." + k + "）が決まっていない。last・sum などを書く"
		default:
			fillSeries(&s, v.Time, ordered)
		}
		out = append(out, s)
	}
	return out
}

var bucketWord = map[string]string{"day": "日", "month": "月", "year": "年"}

func fillSeries(s *Series, t *TimeSpec, rows []*Record) {
	vals := map[string][]float64{}
	orderKeys := map[string][]string{} // 区切りごとの、行の「区切りの中の並び」の鍵
	holes := map[string]bool{}         // 値はあるのに数として読めない行を含む区切り
	var bad string                     // 読めなかった時間軸の値（最初の1つ）
	badAxis := 0
	for _, r := range rows {
		raw := strings.TrimSpace(cellOf(r, s.Key))
		if raw == "" {
			continue // 値の無い行は点にならないだけ（体重のように、ごく一部の行にしか無い列もある）
		}
		at, okT := bucketOf(cellOf(r, t.Axis), t.Bucket)
		if !okT {
			if badAxis == 0 {
				bad = cellOf(r, t.Axis)
			}
			badAxis++
			s.Skipped++
			continue
		}
		f, ok := cellNum(raw)
		if !ok {
			// **読めない値を飛ばして残りだけで点を作らない**（実装後レビュー、codex の指摘6）。
			// `1000`・`2,000`・`3000` の合計を 4000 と描くのは嘘。その区切りは点にしない。
			holes[at] = true
			s.Skipped++
			continue
		}
		vals[at] = append(vals[at], f)
		orderKeys[at] = append(orderKeys[at], withinKey(r, t.Within))
		s.Rows++
	}
	for at := range holes {
		if _, ok := vals[at]; ok {
			delete(vals, at)
		}
		s.Holes++
	}

	ts := make([]string, 0, len(vals))
	for at := range vals {
		ts = append(ts, at)
	}
	sort.Strings(ts)

	// 同じ区切りに2行以上あるのに、どれを取るか決まらないなら止める。
	crowded, most := "", 0
	for _, at := range ts {
		if n := len(vals[at]); n > most {
			crowded, most = at, n
		}
	}
	word := bucketWord[t.Bucket]
	if most > 1 {
		switch {
		case s.Measure == "only":
			s.Error = fmt.Sprintf("同じ%sに複数行ある（最多 %s に %d 行）。集約（last・sum など）を決める",
				word, crowded, most)
			return
		case (s.Measure == "last" || s.Measure == "first") && len(t.Within) == 0:
			s.Error = fmt.Sprintf("同じ%sに複数行あり（最多 %s に %d 行）、%s を決める並びが無い。time.within に%sの中の順を書く",
				word, crowded, most, s.Measure, word)
			return
		case s.Measure == "last" || s.Measure == "first":
			// **within が書いてあっても、並びを決めていなければ止める**（codex の指摘3・Fable の指摘2）。
			// `within: [{property: date}]` は同じ日の中が全部同じ値で、元の順（path 順）に落ちて
			// その日の最後ではない残高を、黙って描いていた。
			for _, at := range ts {
				seen := map[string]bool{}
				for _, k := range orderKeys[at] {
					if seen[k] {
						s.Error = fmt.Sprintf("time.within で%sの中の並びが決まらない（%s に並びの値が同じ行がある）。"+
							"%sの中で行ごとに違う値の列（例: file.name）を書く", word, at, word)
						return
					}
					seen[k] = true
				}
			}
		}
	}

	for _, at := range ts {
		xs := vals[at]
		p := Point{T: at, N: len(xs)}
		switch s.Measure {
		case "only", "first":
			p.V = xs[0]
		case "last":
			p.V = xs[len(xs)-1]
		case "sum", "avg":
			for _, x := range xs {
				p.V += x
			}
			if s.Measure == "avg" {
				p.V /= float64(len(xs))
			}
		case "min", "max":
			p.V = xs[0]
			for _, x := range xs[1:] {
				if (s.Measure == "min" && x < p.V) || (s.Measure == "max" && x > p.V) {
					p.V = x
				}
			}
		}
		s.Points = append(s.Points, p)
	}
	if len(s.Points) == 0 && badAxis > 0 {
		s.Error = fmt.Sprintf("時間軸 %s を%sで読めない（例: %q）", t.Axis, word, bad)
	}
}

func withinKey(r *Record, keys []SortKey) string {
	if len(keys) == 0 {
		return ""
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = cellOf(r, k.Property)
	}
	return strings.Join(parts, "\x00")
}

// 時間軸として読む形。**値の全体を読む**（実装後レビュー、codex の指摘5）。以前は区切りの幅だけ
// 先頭を切り出していたので、`2026-09-99` や `2026-09-garbage` が 9 月の点になっていた。
//
// **日付は書かれた暦日として読む**（本人の決定、2026-09-13）。時差の付いた日時も、日本時間に
// 直さず、書かれた日付で区切る。銀行・d払い・給与・体重はどれも日付だけの値。
var axisLayouts = []struct {
	layout string
	rank   int // 3 = 日まで、2 = 月まで、1 = 年まで
}{
	{time.RFC3339Nano, 3}, {"2006-01-02T15:04:05", 3}, {"2006-01-02T15:04", 3},
	{"2006-01-02 15:04:05", 3}, {"2006-01-02 15:04", 3},
	{"2006-01-02", 3}, {"2006-01", 2}, {"2006", 1},
}

var bucketRank = map[string]int{"day": 3, "month": 2, "year": 1}

func parseAxis(raw string) (time.Time, int, bool) {
	s := strings.TrimSpace(raw)
	for _, l := range axisLayouts {
		if t, err := time.Parse(l.layout, s); err == nil {
			return t, l.rank, true
		}
	}
	return time.Time{}, 0, false
}

// bucketOf は時間軸の値を区切りの名前にする。**区切りより粗い値は読まない**
// （`2025-11` を日で区切ると 1日に寄せるしかなく、それは嘘になる）。
func bucketOf(raw, bucket string) (string, bool) {
	want, ok := bucketRank[bucket]
	if !ok {
		return "", false
	}
	t, rank, ok := parseAxis(raw)
	if !ok || rank < want {
		return "", false
	}
	switch bucket {
	case "day":
		return t.Format("2006-01-02"), true
	case "month":
		return t.Format("2006-01"), true
	default:
		return t.Format("2006"), true
	}
}

// dateLike は時間軸の候補になる値か（ちょうど `YYYY-MM-DD` か `YYYY-MM`）。区切りも返す。
func dateLike(s string) (string, bool) {
	_, rank, ok := parseAxis(s)
	switch {
	case ok && rank == 3 && len(s) == 10:
		return "day", true
	case ok && rank == 2 && len(s) == 7:
		return "month", true
	}
	return "", false
}
