package views

import (
	"fmt"
	"sort"
	"strings"
)

// ConvertCharts は `.base` の Obsidian 向け設定のうち、独自定義に移せるものを移す
// （Phase 4 / M51、2026-09-13）。`ToNative` の前に呼ぶ。返すのは**決められなかったこと・持ち越せなかったこと**の報告。
//
//   - life-tracker の `columnConfigs`・`timeFrame` → `shape.time`・`shape.measures`・`emit` の chart
//   - `columnSize` → `emit` の table の `widths`（本人の調整値）
//
// 移したキーは `Extra` から消す。残ったものは `DroppedKeys` が「持ち越さなかった」と報告する。
//
// **時間軸は行を見て決める。決められなければ書かずに報告する**（既定値で描くと静かに嘘を描く）:
// 全行に値があり、全部が `YYYY-MM-DD` か `YYYY-MM` の列がちょうど1つなら、それを軸にする。
// 給与の明細は「対象の月」と「発行日」の2つが候補になるので、決めない。
//
// **集約は `only` と書く**（「区切りに1行しか無い」という主張）。破れていれば `Run` が
// 止まる——残高を `last` にするか `sum` にするかは列の意味で、行を見ても分からない。
// 描けるかどうかの報告は、今の定義から引き継いだ（`CarryOver`）あとで `ChartProblems` が出す。
//
// 種別は `chart` と書く（本人の決定、2026-09-13。`life-tracker` は Obsidian の名前）。
func ConvertCharts(b *Base, recs []*Record) []Note {
	var notes []Note
	say := func(v *View, f string, args ...any) {
		notes = append(notes, Note{View: v.Name, Text: fmt.Sprintf(f, args...)})
	}
	for i := range b.Views {
		v := &b.Views[i]
		if w, bad := widthsOf(v.Extra["columnSize"]); w != nil || len(bad) > 0 {
			if w != nil {
				v.Emit = addEncoding(v.Emit, Encoding{Kind: "table", Widths: w})
			}
			for _, k := range bad {
				say(v, "列幅 %s は正の数でないので持ち越さない", k)
			}
			delete(v.Extra, "columnSize")
		}
		if v.Kind != KindLifeTracker {
			continue
		}
		v.Kind = KindChart
		convertLifeTracker(b, v, recs, func(f string, args ...any) { say(v, f, args...) },
			func(f string, args ...any) {
				notes = append(notes, Note{View: v.Name, Text: fmt.Sprintf(f, args...), Axis: true})
			})
	}
	return notes
}

// Note は変換の報告1つ。Axis は「時間軸を決められなかった」——引き継ぎで時間軸が入れば言わない。
type Note struct {
	View string
	Text string
	Axis bool
}

func (n Note) String() string { return fmt.Sprintf("ビュー %q: %s", n.View, n.Text) }

// ChartProblems は描けない時系列を挙げる（変換と引き継ぎのあとの、実際に保存する定義で）。
func ChartProblems(b *Base, recs []*Record) []Note {
	var out []Note
	for i := range b.Views {
		v := &b.Views[i]
		if v.Emit == nil || len(seriesKeys(v)) == 0 {
			continue
		}
		res, err := Run(b, v, recs)
		if err != nil {
			out = append(out, Note{View: v.Name, Text: "回せない: " + err.Error()})
			continue
		}
		for _, s := range res.Series {
			if s.Error != "" {
				out = append(out, Note{View: v.Name, Text: s.Key + ": " + s.Error})
			}
		}
	}
	return out
}

// Report は報告を並べる。時間軸が入ったビューの「時間軸を決められない」は落とす。
func Report(b *Base, notes ...[]Note) []string {
	var out []string
	for _, ns := range notes {
		for _, n := range ns {
			if n.Axis {
				if v := findView(b, n.View); v != nil && v.Time != nil {
					continue
				}
			}
			out = append(out, n.String())
		}
	}
	return out
}

func findView(b *Base, name string) *View {
	for i := range b.Views {
		if b.Views[i].Name == name {
			return &b.Views[i]
		}
	}
	return nil
}

// CarryOver は再変換で、**`.base` で言えない部分を今の定義から引き継ぐ**（本人の決定、2026-09-13。
// 実装後レビューの Fable の指摘1）。
//
// 本番では銀行の `last`＋`within`、給与の合計、Homelab の構成図を画面で書く。以前は再変換が
// 「飛ばす」か「-force で .base から作り直す」しかなく、Obsidian 側を1文字直すたびにそれが消えた。
//
//   - 同じ名前のビューの `time` は今の定義のものを使う（変換器の推測より、人が書いたもの）
//   - `measures` は変換器のもの（描く列ごとに only）に、今の定義のものを重ねる
//   - 独自定義にだけある `graph` ビューは、今の定義の並びのまま後ろに付ける
//
// 絞り込み・列・並び・集計・描き方（`.base` で言える部分）は `.base` から作り直す。そこを人が
// 直した台紙は `SaveDef` が手編集として守る（`-force` が要る）。
func CarryOver(converted, current *Base) {
	if current == nil {
		return
	}
	names := map[string]bool{}
	for i := range converted.Views {
		v := &converted.Views[i]
		names[v.Name] = true
		w := findView(current, v.Name)
		if w == nil {
			continue
		}
		if w.Time != nil {
			t := *w.Time
			v.Time = &t
		}
		for k, fn := range w.Measures {
			if v.Measures == nil {
				v.Measures = map[string]string{}
			}
			v.Measures[k] = fn
		}
	}
	for i := range current.Views {
		if w := current.Views[i]; w.Kind == KindGraph && !names[w.Name] {
			converted.Views = append(converted.Views, w)
		}
	}
}

func convertLifeTracker(b *Base, v *View, recs []*Record, say, sayAxis func(string, ...any)) {
	enc := Encoding{Kind: "chart"}

	// 描く列。`columnConfigs` に挙がったものだけ（Obsidian もそれしか描かない）。
	// **挙がっていなければ `order`**——d払いは `columnConfigs` が無く、Obsidian では何も
	// 描かれていなかった。描けるようにするのが M51 の受け入れ。
	charts := map[string]bool{}
	if cfg, ok := v.Extra["columnConfigs"].(map[string]any); ok {
		for k, entries := range cfg {
			key := unqualify(k)
			enc.Values = append(enc.Values, key)
			if list, ok := entries.([]any); ok && len(list) > 0 {
				if m, ok := list[0].(map[string]any); ok {
					switch t, _ := m["visualizationType"].(string); t {
					case "line-chart":
						charts["line"] = true
					case "bar-chart":
						charts["bar"] = true
					default:
						say("%s の描き方 %q は持ち越せないので線にする", key, t)
						charts["line"] = true
					}
				}
			}
		}
		sortByOrder(enc.Values, v.Order)
	}
	if len(enc.Values) == 0 {
		enc.Values = append(enc.Values, v.Order...)
	}
	if len(enc.Values) == 0 {
		say("描く列を決められない（columnConfigs も order も無い）")
		return
	}
	enc.Chart = "line"
	if charts["bar"] && !charts["line"] {
		enc.Chart = "bar"
	}

	switch tf, _ := v.Extra["timeFrame"].(string); {
	case tf == "" || tf == "all":
		enc.Window = tf
	case windowRe.MatchString(tf):
		enc.Window = tf
	default:
		say("期間 %q は持ち越せないので全期間にする", tf)
	}
	delete(v.Extra, "columnConfigs")
	delete(v.Extra, "timeFrame")

	v.Measures = map[string]string{}
	for _, k := range enc.Values {
		v.Measures[k] = "only"
	}
	v.Emit = addEncoding(v.Emit, enc)

	res, err := Run(b, v, recs)
	if err != nil {
		sayAxis("回せないので時間軸を決められない: %v", err)
		return
	}
	axis, bucket, cands := guessAxis(res)
	if axis == "" {
		if len(cands) == 0 {
			sayAxis("時間軸を決められない（全行に日付の入った列が無い）。time を手で書く")
		} else {
			sayAxis("時間軸を決められない（候補が %s）。time を手で書く", strings.Join(cands, "・"))
		}
		return
	}
	v.Time = &TimeSpec{Axis: axis, Bucket: bucket}
}

// guessAxis は時間軸の候補を探す。**ちょうど1つのときだけ決める。**
func guessAxis(res *Result) (axis, bucket string, cands []string) {
	if res.Total == 0 {
		return "", "", nil
	}
	found := map[string]string{}
	for _, c := range res.Columns {
		if c.Filled != res.Total || strings.HasPrefix(c.Key, "file.") || c.Formula {
			continue
		}
		kind := ""
		for _, g := range res.Groups {
			for _, r := range g.Rows {
				k, ok := dateLike(strings.TrimSpace(r.Cells[c.Key]))
				if !ok || (kind != "" && k != kind) {
					kind = "x"
					break
				}
				kind = k
			}
			if kind == "x" {
				break
			}
		}
		if kind != "" && kind != "x" {
			found[c.Key] = kind
			cands = append(cands, c.Key)
		}
	}
	sort.Strings(cands)
	if len(cands) != 1 {
		return "", "", cands
	}
	return cands[0], found[cands[0]], cands
}

// widthsOf は `columnSize` を列幅にする。正でない値（実在の To-Do に -12 がある）は持ち越さない。
func widthsOf(raw any) (map[string]int, []string) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, nil
	}
	var out map[string]int
	var bad []string
	for k, val := range m {
		n, ok := val.(int)
		if !ok || n <= 0 {
			bad = append(bad, unqualify(k))
			continue
		}
		if out == nil {
			out = map[string]int{}
		}
		out[unqualify(k)] = n
	}
	sort.Strings(bad)
	return out, bad
}

func addEncoding(e *Emit, enc Encoding) *Emit {
	if e == nil {
		e = &Emit{}
	}
	e.Human = append(e.Human, enc)
	return e
}

// sortByOrder は `order` に挙がった順に並べる（写像から来た列の順を決めるため）。
func sortByOrder(keys, order []string) {
	pos := map[string]int{}
	for i, k := range order {
		pos[k] = i + 1
	}
	sort.SliceStable(keys, func(i, j int) bool {
		pi, pj := pos[keys[i]], pos[keys[j]]
		switch {
		case pi != 0 && pj != 0:
			return pi < pj
		case pi != pj:
			return pi != 0
		}
		return keys[i] < keys[j]
	})
}
