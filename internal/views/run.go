package views

import (
	"fmt"
	"sort"
	"strings"
)

// Record は1レコード。Row を満たしつつ、素性も持つ。
type Record struct {
	NoteID int64             `json:"note_id"`
	NPath  string            `json:"path"`
	NName  string            `json:"name"`
	NExt   string            `json:"ext"`
	MTime  string            `json:"mtime,omitempty"`
	Tags   []string          `json:"tags,omitempty"`
	Props  map[string]Value  `json:"-"`
	Cells  map[string]string `json:"cells"`
}

func (r *Record) Get(k string) Value {
	if v, ok := r.Props[k]; ok {
		return v
	}
	return Null()
}
func (r *Record) HasTag(t string) bool {
	for _, x := range r.Tags {
		if x == t {
			return true
		}
	}
	return false
}
func (r *Record) Folder() string {
	if i := strings.LastIndex(r.NPath, "/"); i >= 0 {
		return r.NPath[:i]
	}
	return ""
}
func (r *Record) Ext() string  { return r.NExt }
func (r *Record) Name() string { return r.NName }
func (r *Record) Path() string { return r.NPath }

// Column は結果の1列。
type Column struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Formula bool   `json:"formula,omitempty"`
	// Pinned は定義側が order: で前に出した列。
	Pinned bool `json:"pinned,omitempty"`
	// Numeric はその列の値が全部数として読めた。並べ替えと Sum に使う。
	Numeric bool `json:"numeric,omitempty"`
	// Filled は値の入っている行数。列が多いので、密度が見えないと読めない。
	Filled int `json:"filled"`
}

// Group は groupBy の1かたまり。groupBy が無ければ1つだけ返る。
type Group struct {
	Key     string             `json:"key"`
	Rows    []*Record          `json:"rows"`
	Summary map[string]float64 `json:"summary,omitempty"`
}

// Result は1ビューの実行結果。
type Result struct {
	View     string             `json:"view"`
	Kind     string             `json:"kind"`
	Columns  []Column           `json:"columns"`
	Groups   []Group            `json:"groups"`
	Total    int                `json:"total"`
	Summary  map[string]float64 `json:"summary,omitempty"`
	Warnings []string           `json:"warnings,omitempty"`
}

// Run は1ビューを回す。
//
// **列モデルは反転してある。既定で全列を出し、定義側は「隠す」と「並べる」
// だけを言う。**
//
// Bases の `order:` は許可リストで、そこに手で書かなかったフィールドは
// ビューから見て存在しない。実測すると、実在の7ファイルで136列中100列（73%）が
// どのビューからも見えなかった。Health は102列中91列。これは設定の書き忘れでは
// なく、許可リストという形が持つ性質——列は増え続けるのに定義は手で書いた
// ときのまま止まる。だから `order:` は「前に出す列」として読み、残りは
// その後ろに全部続ける。消したいものだけ `hide:` に書く。
func Run(b *Base, v *View, recs []*Record) (*Result, error) {
	res := &Result{View: v.Name, Kind: v.Kind}

	// 1) 絞り込み。base 全体と、ビュー個別の両方を満たすもの。
	var kept []*Record
	for _, r := range recs {
		ok, err := match(b.Filters, r)
		if err != nil {
			return nil, fmt.Errorf("%s: base の filters: %w", v.Name, err)
		}
		if !ok {
			continue
		}
		ok, err = match(v.Filters, r)
		if err != nil {
			return nil, fmt.Errorf("%s: view の filters: %w", v.Name, err)
		}
		if ok {
			kept = append(kept, r)
		}
	}

	// 2) 計算列。行ごとに評価して Cells に入れる。
	formulaKeys := make([]string, 0, len(b.Formulas))
	for k := range b.Formulas {
		formulaKeys = append(formulaKeys, k)
	}
	sort.Strings(formulaKeys)
	for _, r := range kept {
		if r.Cells == nil {
			r.Cells = map[string]string{}
		}
		// 素性も列として出す。実在の .base は order: に file.name（15箇所）・
		// file.mtime（2）・file.folder（3）・file.ext（1）を挙げている。
		// Cells に入れないと、定義が指名しているのに空欄になる。
		r.Cells["file.name"] = r.NName
		r.Cells["file.path"] = r.NPath
		r.Cells["file.folder"] = r.Folder()
		r.Cells["file.ext"] = r.NExt
		if r.MTime != "" {
			r.Cells["file.mtime"] = r.MTime
		}
		for k, v := range r.Props {
			r.Cells[k] = v.Str
		}
		for _, k := range formulaKeys {
			val, err := Eval(b.Formulas[k], r)
			if err != nil {
				// 1つの式が読めなくてもビュー全体は出す。ただし黙らない。
				res.Warnings = append(res.Warnings,
					fmt.Sprintf("formula %q: %v", k, err))
				continue
			}
			r.Cells[formulaKey(k)] = val.Str
		}
	}

	// 3) 列を決める。**まず実在する全部を集め、そこから隠す。**
	res.Columns = columns(b, v, kept, formulaKeys)

	// 4) 並べ替え。
	sortRows(kept, v.Sort, res.Columns)

	// 5) グループ化と集計。
	res.Total = len(kept)
	res.Groups = group(kept, v.GroupBy)
	res.Summary = summarize(kept, v.Summaries)
	for i := range res.Groups {
		res.Groups[i].Summary = summarize(res.Groups[i].Rows, v.Summaries)
	}
	return res, nil
}

func formulaKey(k string) string { return "formula." + k }

// columns は出す列を決める。ここが反転の実体。
func columns(b *Base, v *View, rows []*Record, formulaKeys []string) []Column {
	hidden := map[string]bool{}
	for _, h := range v.Hide {
		hidden[h] = true
	}

	// 実在する列を全部数える（値の入っている行数つき）。
	filled := map[string]int{}
	for _, r := range rows {
		for k, val := range r.Cells {
			if val != "" {
				filled[k]++
			} else if _, ok := filled[k]; !ok {
				filled[k] = 0
			}
		}
	}
	isFormula := map[string]bool{}
	for _, k := range formulaKeys {
		isFormula[formulaKey(k)] = true
	}

	numeric := map[string]bool{}
	for k := range filled {
		numeric[k] = true
	}
	for _, r := range rows {
		for k, v := range r.Props {
			if !v.IsNum && !v.Null && v.Str != "" {
				numeric[k] = false
			}
		}
	}

	seen := map[string]bool{}
	var out []Column
	add := func(k string, pinned bool) {
		if seen[k] || hidden[k] {
			return
		}
		seen[k] = true
		out = append(out, Column{
			Key: k, Label: label(k), Formula: isFormula[k],
			Pinned: pinned, Numeric: numeric[k], Filled: filled[k],
		})
	}

	// order: に挙げたものが先頭（＝許可リストではなく、前に出す指定）。
	for _, k := range v.Order {
		add(k, true)
	}
	// 残りは全部その後ろ。ここが Bases と違うところ。
	rest := make([]string, 0, len(filled))
	for k := range filled {
		rest = append(rest, k)
	}
	sort.Strings(rest)
	for _, k := range rest {
		add(k, false)
	}
	return out
}

// label は表示名。`formula.金額` は `金額` として出す。
func label(k string) string {
	return strings.TrimPrefix(k, "formula.")
}

func match(f *Filter, r Row) (bool, error) {
	if f == nil {
		return true, nil
	}
	if f.Expr != "" {
		v, err := Eval(f.Expr, r)
		if err != nil {
			return false, err
		}
		return v.Truthy(), nil
	}
	for _, c := range f.And {
		ok, err := match(c, r)
		if err != nil || !ok {
			return false, err
		}
	}
	if len(f.Or) > 0 {
		any := false
		for _, c := range f.Or {
			ok, err := match(c, r)
			if err != nil {
				return false, err
			}
			if ok {
				any = true
				break
			}
		}
		if !any {
			return false, nil
		}
	}
	return true, nil
}

func sortRows(rows []*Record, keys []SortKey, cols []Column) {
	if len(keys) == 0 {
		return
	}
	numeric := map[string]bool{}
	for _, c := range cols {
		numeric[c.Key] = c.Numeric
	}
	sort.SliceStable(rows, func(i, j int) bool {
		for _, k := range keys {
			a, b := cellOf(rows[i], k.Property), cellOf(rows[j], k.Property)
			if a == b {
				continue
			}
			less := a < b
			if numeric[k.Property] {
				less = rows[i].Get(k.Property).Num < rows[j].Get(k.Property).Num
			}
			if strings.EqualFold(k.Direction, "DESC") {
				return !less
			}
			return less
		}
		return false
	})
}

func cellOf(r *Record, key string) string {
	if v, ok := r.Cells[key]; ok {
		return v
	}
	switch key {
	case "file.name":
		return r.NName
	case "file.path":
		return r.NPath
	case "file.folder":
		return r.Folder()
	case "file.mtime":
		return r.MTime
	case "file.ext":
		return r.NExt
	}
	return ""
}

func group(rows []*Record, by *SortKey) []Group {
	if by == nil || by.Property == "" {
		return []Group{{Rows: rows}}
	}
	order := []string{}
	m := map[string][]*Record{}
	for _, r := range rows {
		k := cellOf(r, by.Property)
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = append(m[k], r)
	}
	sort.Strings(order)
	if strings.EqualFold(by.Direction, "DESC") {
		for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
			order[i], order[j] = order[j], order[i]
		}
	}
	out := make([]Group, 0, len(order))
	for _, k := range order {
		out = append(out, Group{Key: k, Rows: m[k]})
	}
	return out
}

// summarize は集計。実在するのは Sum だけだが、素直に足せるものは足す。
func summarize(rows []*Record, spec map[string]string) map[string]float64 {
	if len(spec) == 0 {
		return nil
	}
	out := map[string]float64{}
	for key, fn := range spec {
		var sum float64
		var n int
		for _, r := range rows {
			v := r.Get(key)
			if v.IsNum {
				sum += v.Num
				n++
			}
		}
		switch strings.ToLower(fn) {
		case "sum":
			out[key] = sum
		case "average":
			if n > 0 {
				out[key] = sum / float64(n)
			}
		case "count":
			out[key] = float64(n)
		default:
			out[key] = sum
		}
	}
	return out
}

// Mtime は式から file.mtime を引くため。
func (r *Record) Mtime() string { return r.MTime }
