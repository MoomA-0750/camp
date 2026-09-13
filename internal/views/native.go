package views

// Camp 独自のビュー定義（Phase 4 / M50、2026-09-13）。`.base` から移した先の形。
//
// **単位は `.base` と同じ「台紙1枚に複数ビュー」。** ビュー1つ1文書にすると、
// `Payments` の `formulas` 3本と台紙の `filters` が 14 ビューに複製され、1つ直すと
// 残りが静かにずれる（設計レビューの指摘2）。
//
// **読み終わった形は `ParseBase` と同じ `*Base`。** 回すのは同じ `Run` で、
// `.base` 版と独自定義版の結果を突き合わせられる（併読。本人の決定3）。
//
// **`kind` は table・cards・chart・graph の4つだけ**（本人の決定、2026-09-13 の実装後レビューのあと）。
// M50 では `.base` の `type` と同じ `life-tracker` を書いていたが、手書きの `chart` も通り、同じものに
// 名前が2つあった。定義が本番に入る前に固めた。`.base` の `life-tracker` は変換で `chart` になり、
// 突き合わせでは同じとみなす（`canonicalKind`）。
//
// **表示名（`labels`）は台紙のいちばん上に1つ。** 初めは最初のビューの `shape` に載せて読みで畳んで
// いたので、ビューを消す・並べ替えると表示名が黙って変わった（Fable の指摘6）。

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// nativeDoc は台紙1枚ぶんの YAML。
type nativeDoc struct {
	Base    string            `yaml:"base"`
	Source  nativeSource      `yaml:"source"`
	Derive  map[string]string `yaml:"derive"`
	Labels  map[string]string `yaml:"labels"`
	Views   []yaml.Node       `yaml:"views"`
	Version int               `yaml:"version"`
}

// NativeVersion は読める定義の版。これより新しい版は読まない（黙って一部だけ読むより止まる）。
const NativeVersion = 1

// nativeKinds は独自定義の種別。
var nativeKinds = map[string]bool{KindTable: true, KindCards: true, KindChart: true, KindGraph: true}

type nativeSource struct {
	Filter yaml.Node `yaml:"filter"`
}

// ParseNative は独自定義を読む。name は台紙の名前（`base:` が無いときの既定）。
//
// **`base:` を錨にする。** `viewID` は `base + "/" + name` で、画面の URL とモデルが
// 持ち回すハンドルがこれで出来ている。台紙の行の `base` 列と食い違ったら弾く。
func ParseNative(name string, body []byte) (*Base, error) {
	var doc nativeDoc
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if doc.Base == "" {
		doc.Base = name
	}
	if name != "" && doc.Base != name {
		return nil, fmt.Errorf("%s: base が %q（台紙の名前と違う）", name, doc.Base)
	}
	if doc.Version > NativeVersion {
		return nil, fmt.Errorf("%s: version %d の定義は読めない（この Camp が読めるのは %d まで）",
			name, doc.Version, NativeVersion)
	}
	if strings.Contains(doc.Base, "/") {
		// `/` が入ると `base/name` の id が壊れる（設計の危うさ4）。
		return nil, fmt.Errorf("%s: base に / を含められない", doc.Base)
	}

	b := &Base{Path: "db:" + doc.Base, Name: doc.Base, Formulas: doc.Derive}
	for k, label := range doc.Labels {
		if b.Display == nil {
			b.Display = map[string]string{}
		}
		b.Display[unqualify(k)] = label
	}
	f, err := parseFilter(&doc.Source.Filter)
	if err != nil {
		return nil, fmt.Errorf("%s: source.filter: %w", doc.Base, err)
	}
	b.Filters = f

	for i := range doc.Views {
		v, err := parseNativeView(&doc.Views[i])
		if err != nil {
			return nil, fmt.Errorf("%s: views[%d]: %w", doc.Base, i, err)
		}
		if strings.Contains(v.Name, "/") {
			return nil, fmt.Errorf("%s: ビュー名に / を含められない: %q", doc.Base, v.Name)
		}
		b.Views = append(b.Views, *v)
	}
	return b, nil
}

// nativeShape は `shape:`。**人とモデルが共有する分析**（設計レビューの指摘3で
// `time`・`measures` をここへ移した。`emit` に置くとモデルは人と違うものを見る）。
type nativeShape struct {
	Filter    yaml.Node         `yaml:"filter"`
	Order     []string          `yaml:"order"`
	Hide      []string          `yaml:"hide"`
	Sort      []SortKey         `yaml:"sort"`
	GroupBy   *SortKey          `yaml:"group_by"`
	Summaries map[string]string `yaml:"summaries"`
	Labels    map[string]string `yaml:"labels"`
	Time      *TimeSpec         `yaml:"time"`
	Measures  map[string]string `yaml:"measures"`
}

func parseNativeView(n *yaml.Node) (*View, error) {
	var head struct {
		Name  string      `yaml:"name"`
		Kind  string      `yaml:"kind"`
		Shape nativeShape `yaml:"shape"`
		Emit  *Emit       `yaml:"emit"`
	}
	if err := n.Decode(&head); err != nil {
		return nil, err
	}
	if head.Name == "" {
		return nil, fmt.Errorf("name が無い")
	}
	if head.Kind == "" {
		head.Kind = KindTable
	}
	if !nativeKinds[head.Kind] {
		if head.Kind == KindLifeTracker {
			return nil, fmt.Errorf("kind は chart と書く（life-tracker は Obsidian の名前）")
		}
		return nil, fmt.Errorf("知らない種別 %q（table・cards・chart・graph）", head.Kind)
	}
	if len(head.Shape.Labels) > 0 {
		return nil, fmt.Errorf("labels は台紙のいちばん上に書く（ビューの shape には置けない）")
	}
	s := head.Shape
	v := &View{
		Kind: head.Kind, Name: head.Name,
		Order: unqualifyAll(s.Order), Hide: unqualifyAll(s.Hide),
		Sort: s.Sort, GroupBy: s.GroupBy,
	}
	// 向きは ASC か DESC だけ（大文字小文字は問わない）。**それ以外の綴りは昇順として黙って
	// 通っていた**（`sortRows` は DESC しか見ない。codex の指摘3・Fable の指摘2）。
	for i := range v.Sort {
		v.Sort[i].Property = unqualify(v.Sort[i].Property)
		if err := checkDirection(&v.Sort[i], fmt.Sprintf("shape.sort[%d]", i)); err != nil {
			return nil, err
		}
	}
	if v.GroupBy != nil {
		v.GroupBy.Property = unqualify(v.GroupBy.Property)
		if err := checkDirection(v.GroupBy, "shape.group_by"); err != nil {
			return nil, err
		}
	}
	if len(s.Summaries) > 0 {
		v.Summaries = make(map[string]string, len(s.Summaries))
		for k, fn := range s.Summaries {
			v.Summaries[unqualify(k)] = fn
		}
	}
	f, err := parseFilter(&s.Filter)
	if err != nil {
		return nil, fmt.Errorf("shape.filter: %w", err)
	}
	v.Filters = f

	// 時系列と描き方（M51）。**読めない値はここで弾く**——保存（`SaveDef`）も同じ読み手を
	// 通るので、壊れた定義は入らない。
	if err := s.Time.validate(); err != nil {
		return nil, err
	}
	v.Time = s.Time
	if len(s.Measures) > 0 {
		v.Measures = make(map[string]string, len(s.Measures))
		for k, fn := range s.Measures {
			v.Measures[unqualify(k)] = fn
		}
		if err := validateMeasures(v.Measures); err != nil {
			return nil, err
		}
	}
	if err := head.Emit.validate(); err != nil {
		return nil, err
	}
	v.Emit = head.Emit
	// **描く列には集約が要る**（本人の決定、2026-09-13）。書き忘れると保存は通り、画面を開いて
	// 初めて「集約が決まっていない」と出ていた。
	if v.Emit != nil {
		for _, h := range v.Emit.Human {
			for _, k := range h.Values {
				if v.Measures[k] == "" {
					return nil, fmt.Errorf("emit の values に挙げた %s に集約が無い（shape.measures に書く）", k)
				}
			}
		}
	}
	return v, nil
}

// checkDirection は並べ替えの向きを ASC・DESC にそろえる。空は ASC。
func checkDirection(k *SortKey, where string) error {
	switch strings.ToUpper(k.Direction) {
	case "", "ASC":
		k.Direction = strings.ToUpper(k.Direction)
	case "DESC":
		k.Direction = "DESC"
	default:
		return fmt.Errorf("%s: direction は ASC か DESC（%q）", where, k.Direction)
	}
	return nil
}

// ToNative は `*Base` を独自定義の YAML にする（変換器が使う）。
//
// **`Extra` は持ち越さない。** `.base` の `Extra` は Obsidian のUI設定
// （`columnSize`・`gridColumns`・`columnConfigs`・`imageAspectRatio`）で、独自定義に
// 対応物が無いものが多い。**何を持ち越さなかったかは呼び出し側が報告する**
// （黙って消すのが一番悪い）。持ち越せるもの（チャートと列幅）は、先に `ConvertCharts` が
// `Time`・`Measures`・`Emit` へ移しておく（M51）。
func ToNative(b *Base) ([]byte, error) {
	// **人が上から読める順で書く**（2026-09-13、実ブラウザで見つけた）。写像のまま Marshal すると
	// 鍵がアルファベット順になり、`base, derive, source, version, views` と並んで、
	// ビューも `kind` が `name` より先に来ていた。定義は人が直すものなので、構造体で順を決める。
	doc := outDoc{Base: b.Name, Derive: b.Formulas, Labels: b.Display, Version: NativeVersion}
	if b.Filters != nil {
		doc.Source = &outSource{Filter: filterYAML(b.Filters)}
	}
	for i := range b.Views {
		v := &b.Views[i]
		s := outShape{Order: v.Order, Hide: v.Hide, Summaries: v.Summaries,
			Time: v.Time, Measures: v.Measures}
		if v.Filters != nil {
			s.Filter = filterYAML(v.Filters)
		}
		for _, k := range v.Sort {
			s.Sort = append(s.Sort, outSort(k))
		}
		if v.GroupBy != nil {
			g := outSort(*v.GroupBy)
			s.GroupBy = &g
		}
		ent := outView{Name: v.Name, Kind: canonicalKind(v.Kind), Emit: v.Emit}
		if !s.empty() {
			ent.Shape = &s
		}
		doc.Views = append(doc.Views, ent)
	}
	return yaml.Marshal(doc)
}

// 書き出しの形。**欄の順がそのまま YAML の順になる**（読み手は nativeDoc で、順を問わない）。
type outDoc struct {
	Base    string            `yaml:"base"`
	Source  *outSource        `yaml:"source,omitempty"`
	Derive  map[string]string `yaml:"derive,omitempty"`
	Labels  map[string]string `yaml:"labels,omitempty"`
	Views   []outView         `yaml:"views"`
	Version int               `yaml:"version"`
}

type outSource struct {
	Filter any `yaml:"filter,omitempty"`
}

type outView struct {
	Name  string    `yaml:"name"`
	Kind  string    `yaml:"kind"`
	Shape *outShape `yaml:"shape,omitempty"`
	Emit  *Emit     `yaml:"emit,omitempty"`
}

type outShape struct {
	Filter    any               `yaml:"filter,omitempty"`
	Order     []string          `yaml:"order,omitempty"`
	Hide      []string          `yaml:"hide,omitempty"`
	GroupBy   *outSortKey       `yaml:"group_by,omitempty"`
	Sort      []outSortKey      `yaml:"sort,omitempty"`
	Summaries map[string]string `yaml:"summaries,omitempty"`
	Time      *TimeSpec         `yaml:"time,omitempty"`
	Measures  map[string]string `yaml:"measures,omitempty"`
}

func (s outShape) empty() bool {
	return s.Filter == nil && len(s.Order) == 0 && len(s.Hide) == 0 &&
		s.GroupBy == nil && len(s.Sort) == 0 && len(s.Summaries) == 0 &&
		s.Time == nil && len(s.Measures) == 0
}

type outSortKey struct {
	Property  string `yaml:"property"`
	Direction string `yaml:"direction,omitempty"`
}

func outSort(k SortKey) outSortKey { return outSortKey{Property: k.Property, Direction: k.Direction} }

// filterYAML は `Filter` を YAML に戻す。葉は文字列、結合子は写像。
func filterYAML(f *Filter) any {
	if f == nil {
		return nil
	}
	if f.Expr != "" {
		return f.Expr
	}
	m := map[string]any{}
	add := func(key string, kids []*Filter) {
		if len(kids) == 0 {
			return
		}
		arr := make([]any, 0, len(kids))
		for _, c := range kids {
			if y := filterYAML(c); y != nil {
				arr = append(arr, y)
			}
		}
		if len(arr) > 0 {
			m[key] = arr
		}
	}
	add("and", f.And)
	add("or", f.Or)
	add("not", f.Not)
	if len(m) == 0 {
		return nil
	}
	return m
}
