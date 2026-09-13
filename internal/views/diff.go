package views

// 台紙2枚を「解釈する項目だけ」で比べる（Phase 4 / M50 の併読、2026-09-13）。
//
// **なぜ実行結果の突き合わせだけでは足りないか。** 初稿の受け入れ条件は
// 「行数・列数・グループ数・集計が一致」だったが、それは穴だった（設計レビューの指摘1）:
//
//   - 列は行の `Cells` を走査して作られる（`columns`）。`order` から列を落としても
//     その列はデータから自動で出るので、**列数は変わらない**（`Pinned` が変わるだけ）
//   - `sort` を DESC から ASC にしても **行数は変わらない**
//   - `groupBy` の `direction` を落としても **グループ数は変わらない**
//
// つまりこれらの誤変換は全部「一致」と報告される。**構造で見るしかない。**
//
// **`Extra` は比べない。** `.base` の `Extra` は Obsidian のUI設定
// （`columnSize`・`gridColumns`・`columnConfigs`）、独自定義の `Extra` は
// `emit`・`time`・`measures`。中身が違うのが正しいので、ここを比べると常に差が出て
// 突き合わせが意味を失う。持ち越さなかったキーは変換器が別に報告する。

import (
	"fmt"
	"sort"
	"strings"
)

// DiffBases は a（多くは `.base` 版）と b（独自定義版）の構造の差を人に読める形で返す。
// 差が無ければ空。
func DiffBases(a, b *Base) []string {
	var out []string
	say := func(f string, args ...any) { out = append(out, fmt.Sprintf(f, args...)) }

	if a == nil || b == nil {
		if a != b {
			say("片方が無い")
		}
		return out
	}
	if a.Name != b.Name {
		say("台紙の名前が違う: %q ≠ %q", a.Name, b.Name)
	}
	if x, y := filterKey(a.Filters), filterKey(b.Filters); x != y {
		say("台紙の絞り込みが違う: %s ≠ %s", x, y)
	}
	out = append(out, diffMap("台紙の計算列", a.Formulas, b.Formulas)...)
	out = append(out, diffMap("表示名", a.Display, b.Display)...)

	// **独自定義にだけある `graph` ビューは数えない**（M52）。`.base` に対応物の無い種別なので、
	// 数えると構成図を足した瞬間に併読の確かめが「食い違い」になる。
	if n, m := len(a.Views), len(b.Views)-nativeOnlyGraphs(a, b); n != m {
		say("ビューの数が違う: %d ≠ %d", n, m)
	}
	// 名前で突き合わせる（順番が変わっただけを差として騒がない。ただし**欠け**は出す）。
	ai, bi := byName(a.Views), byName(b.Views)
	for _, name := range union(ai, bi) {
		x, okx := ai[name]
		y, oky := bi[name]
		switch {
		case !okx && y.Kind == KindGraph:
			continue
		case !okx:
			say("ビュー %q が元に無い", name)
			continue
		case !oky:
			say("ビュー %q が移行先に無い", name)
			continue
		}
		for _, d := range diffView(x, y) {
			say("ビュー %q: %s", name, d)
		}
	}
	return out
}

func nativeOnlyGraphs(a, b *Base) int {
	ai := byName(a.Views)
	n := 0
	for i := range b.Views {
		if _, ok := ai[b.Views[i].Name]; !ok && b.Views[i].Kind == KindGraph {
			n++
		}
	}
	return n
}

func diffView(a, b *View) []string {
	var out []string
	say := func(f string, args ...any) { out = append(out, fmt.Sprintf(f, args...)) }

	if canonicalKind(a.Kind) != canonicalKind(b.Kind) {
		say("種別が違う: %q ≠ %q", a.Kind, b.Kind)
	}
	// **並びも比べる。** `order` は「前に出す」指定なので、順番そのものが意味を持つ。
	if x, y := strings.Join(a.Order, ","), strings.Join(b.Order, ","); x != y {
		say("order が違う: [%s] ≠ [%s]", x, y)
	}
	if x, y := strings.Join(a.Hide, ","), strings.Join(b.Hide, ","); x != y {
		say("hide が違う: [%s] ≠ [%s]", x, y)
	}
	if x, y := sortKeys(a.Sort), sortKeys(b.Sort); x != y {
		say("sort が違う（向きも見る）: %s ≠ %s", x, y)
	}
	if x, y := groupKey(a.GroupBy), groupKey(b.GroupBy); x != y {
		say("group_by が違う（向きも見る）: %s ≠ %s", x, y)
	}
	if x, y := filterKey(a.Filters), filterKey(b.Filters); x != y {
		say("絞り込みが違う: %s ≠ %s", x, y)
	}
	out = append(out, diffMap("集計", a.Summaries, b.Summaries)...)
	return out
}

// filterKey は絞り込みを決まった文字列にする。**子の順番も保つ**
// （`and` は交換しても意味は同じだが、変換器は順番を保つので、崩れたら知りたい）。
func filterKey(f *Filter) string {
	if f == nil {
		return "（無し）"
	}
	if f.Expr != "" {
		return strings.TrimSpace(f.Expr)
	}
	part := func(name string, kids []*Filter) string {
		if len(kids) == 0 {
			return ""
		}
		ks := make([]string, 0, len(kids))
		for _, c := range kids {
			ks = append(ks, filterKey(c))
		}
		return name + "(" + strings.Join(ks, ", ") + ")"
	}
	var ps []string
	for _, s := range []string{part("and", f.And), part("or", f.Or), part("not", f.Not)} {
		if s != "" {
			ps = append(ps, s)
		}
	}
	if len(ps) == 0 {
		return "（空）"
	}
	return strings.Join(ps, " & ")
}

func sortKeys(ks []SortKey) string {
	if len(ks) == 0 {
		return "（無し）"
	}
	out := make([]string, 0, len(ks))
	for _, k := range ks {
		out = append(out, k.Property+" "+dirOr(k.Direction))
	}
	return strings.Join(out, ", ")
}

func groupKey(k *SortKey) string {
	if k == nil {
		return "（無し）"
	}
	return k.Property + " " + dirOr(k.Direction)
}

// dirOr は向きの既定を埋める。**空と ASC を同じものとして扱う**——Obsidian も
// 空なら昇順で、ここを別物にすると意味の無い差が出る。
func dirOr(d string) string {
	if strings.EqualFold(d, "") || strings.EqualFold(d, "ASC") {
		return "ASC"
	}
	return strings.ToUpper(d)
}

func diffMap(what string, a, b map[string]string) []string {
	var out []string
	for _, k := range unionKeys(a, b) {
		x, okx := a[k]
		y, oky := b[k]
		switch {
		case !okx:
			out = append(out, fmt.Sprintf("%sの %q が元に無い", what, k))
		case !oky:
			out = append(out, fmt.Sprintf("%sの %q が移行先に無い", what, k))
		case strings.TrimSpace(x) != strings.TrimSpace(y):
			out = append(out, fmt.Sprintf("%sの %q が違う: %q ≠ %q", what, k, x, y))
		}
	}
	return out
}

// DiffResults は**実行結果**を比べる（併読の2段目。M50、2026-09-13）。
//
// **順序も比べる。** 構造の差分（`DiffBases`）と合わせて2段で見るのは、行数・列数・
// グループ数では取り違えが出ないから——列は行のデータから作られ、`sort` と `groupBy` の
// 向きは並びにしか出ない。
//
// 行の中身は先頭 rows 行だけ比べる（Health は 2,687 行あり、全部並べると報告が読めない）。
// **違いは最初の1つだけ出す**（同じ原因で何百行も並ぶと、かえって読まれない）。
func DiffResults(a, b *Result, rows int) []string {
	var out []string
	say := func(f string, args ...any) { out = append(out, fmt.Sprintf(f, args...)) }

	if a == nil || b == nil {
		if a != b {
			say("片方の結果が無い")
		}
		return out
	}
	if canonicalKind(a.Kind) != canonicalKind(b.Kind) {
		say("種別が違う: %q ≠ %q", a.Kind, b.Kind)
	}
	if a.Total != b.Total {
		say("行数が違う: %d ≠ %d", a.Total, b.Total)
	}
	if x, y := strings.Join(resultColKeys(a), ","), strings.Join(resultColKeys(b), ","); x != y {
		say("列の並びが違う:\n      元   [%s]\n      移行 [%s]", x, y)
	}
	if len(a.Groups) != len(b.Groups) {
		say("グループの数が違う: %d ≠ %d", len(a.Groups), len(b.Groups))
	}
	n := min(len(a.Groups), len(b.Groups))
	for i := 0; i < n; i++ {
		ga, gb := a.Groups[i], b.Groups[i]
		if ga.Key != gb.Key {
			say("%d 番目のグループ鍵が違う（並びも見る）: %q ≠ %q", i+1, ga.Key, gb.Key)
		}
		if len(ga.Rows) != len(gb.Rows) {
			say("グループ %q の行数が違う: %d ≠ %d", ga.Key, len(ga.Rows), len(gb.Rows))
		}
		out = append(out, diffNums(fmt.Sprintf("グループ %q の集計", ga.Key), ga.Summary, gb.Summary)...)
	}
	out = append(out, diffNums("全体の集計", a.Summary, b.Summary)...)

	if rows > 0 {
		keys := resultColKeys(a)
		xa, xb := rowLines(a, keys, rows), rowLines(b, keys, rows)
		for i := 0; i < len(xa) && i < len(xb); i++ {
			if xa[i] != xb[i] {
				say("%d 行目の中身が違う（並びの取り違えもここに出る）:\n      元   %s\n      移行 %s",
					i+1, xa[i], xb[i])
				break
			}
		}
	}
	return out
}

// DroppedKeys は独自定義へ持ち越さなかった鍵（`.base` の `Extra`）。
//
// **黙って消さない。** `columnSize`・`gridColumns`・`columnConfigs`・`imageAspectRatio` は
// Obsidian のUI設定で、独自定義に対応物が無いものが多い。変換の報告に出す。
func DroppedKeys(b *Base) []string {
	seen := map[string]bool{}
	var out []string
	for i := range b.Views {
		for k := range b.Views[i].Extra {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

func resultColKeys(r *Result) []string {
	out := make([]string, 0, len(r.Columns))
	for _, c := range r.Columns {
		out = append(out, c.Key)
	}
	return out
}

// rowLines は先頭 limit 行を、keys の並びで1行の文字列にする。
func rowLines(r *Result, keys []string, limit int) []string {
	var out []string
	for _, g := range r.Groups {
		for _, rec := range g.Rows {
			if len(out) >= limit {
				return out
			}
			cells := make([]string, 0, len(keys))
			for _, k := range keys {
				cells = append(cells, rec.Cells[k])
			}
			out = append(out, strings.Join(cells, " | "))
		}
	}
	return out
}

func diffNums(what string, a, b map[string]float64) []string {
	var out []string
	seen := map[string]bool{}
	var ks []string
	for _, m := range []map[string]float64{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				ks = append(ks, k)
			}
		}
	}
	sort.Strings(ks)
	for _, k := range ks {
		x, okx := a[k]
		y, oky := b[k]
		switch {
		case !okx:
			out = append(out, fmt.Sprintf("%sの %q が元に無い", what, k))
		case !oky:
			out = append(out, fmt.Sprintf("%sの %q が移行先に無い", what, k))
		case x != y:
			out = append(out, fmt.Sprintf("%sの %q が違う: %v ≠ %v", what, k, x, y))
		}
	}
	return out
}

func byName(vs []View) map[string]*View {
	out := make(map[string]*View, len(vs))
	for i := range vs {
		out[vs[i].Name] = &vs[i]
	}
	return out
}

func union(a, b map[string]*View) []string {
	seen := map[string]bool{}
	var out []string
	for k := range a {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range b {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func unionKeys(a, b map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string]string{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// canonicalKind は `.base` の種別名を独自定義の語彙に寄せる（本人の決定、2026-09-13）。
// 独自定義の種別は table・cards・chart・graph の4つだけで、Obsidian の `life-tracker` は `chart`。
func canonicalKind(k string) string {
	if k == KindLifeTracker {
		return KindChart
	}
	return k
}

// DiffEmit は描き方のうち **`.base` で言える部分**を比べる（実装後レビュー、2026-09-13。Fable の指摘4）。
//
// チャートの描く列（`values`）・線か棒か（`chart`）・期間（`window`）は `.base` の `columnConfigs`・
// `timeFrame` から、表の列幅（`widths`）は `columnSize` から来る。変換器（`ConvertCharts`）の誤りは
// `DiffBases`（構造）にも `DiffResults`（行）にも出ないので、ここで見る。
//
// 時間軸（`time`）と集約（`measures`）は `.base` で言えない（本人が決める）ので比べない。
// 独自定義にだけある graph ビューとその色分けも比べない。
func DiffEmit(a, b *Base) []string {
	var out []string
	if a == nil || b == nil {
		return out
	}
	ai := byName(a.Views)
	for i := range b.Views {
		y := &b.Views[i]
		x, ok := ai[y.Name]
		if !ok {
			continue // 名前の欠けは DiffBases が言う
		}
		if ex, ey := emitKey(x), emitKey(y); ex != ey {
			out = append(out, fmt.Sprintf("ビュー %q: 描き方が違う: %s ≠ %s", y.Name, ex, ey))
		}
	}
	return out
}

func emitKey(v *View) string {
	if v.Emit == nil {
		return "（無し）"
	}
	var parts []string
	for _, h := range v.Emit.Human {
		switch h.Kind {
		case "chart":
			parts = append(parts, fmt.Sprintf("chart[%s|%s|%s]", strings.Join(h.Values, ","), h.Chart, h.Window))
		case "table":
			ks := make([]string, 0, len(h.Widths))
			for k, w := range h.Widths {
				ks = append(ks, fmt.Sprintf("%s=%d", k, w))
			}
			sort.Strings(ks)
			parts = append(parts, "table["+strings.Join(ks, ",")+"]")
		}
	}
	if len(parts) == 0 {
		return "（無し）"
	}
	return strings.Join(parts, " ")
}
