package views

import (
	"strings"
	"testing"
)

func rec(path string, tags []string, props map[string]Value) *Record {
	name := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		name = path[i+1:]
	}
	return &Record{NPath: path, NName: name, NExt: "md", Tags: tags, Props: props}
}

func colKeys(r *Result) []string {
	out := make([]string, 0, len(r.Columns))
	for _, c := range r.Columns {
		out = append(out, c.Key)
	}
	return out
}

func has(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

// **Phase 2 の中心。** order: は許可リストではなく「前に出す指定」。
// 挙げなかった列も全部出る。
//
// Bases の挙動（許可リスト）だと、実測で136列中100列（73%）がビューから
// 存在しないことになっていた。Health は102列中91列。これは設定の書き忘れでは
// なく、許可リストという形が持つ性質——列は増え続けるのに定義は止まる。
func TestOrderIsPinningNotAnAllowlist(t *testing.T) {
	b := &Base{Name: "t"}
	v := &View{Kind: KindTable, Name: "v", Order: []string{"date", "steps"}}
	rows := []*Record{
		rec("Data/Health/2026-01-01.md", nil, map[string]Value{
			"date": Str("2026-01-01"), "steps": Num(8000),
			"blood_oxygen": Num(97), "weight_kg": Num(60.2), "vo2_max": Num(41),
		}),
	}
	res, err := Run(b, v, rows)
	if err != nil {
		t.Fatal(err)
	}
	keys := colKeys(res)
	// 挙げた2つが先頭。
	if keys[0] != "date" || keys[1] != "steps" {
		t.Fatalf("order: が前に来ていない: %v", keys)
	}
	if !res.Columns[0].Pinned || res.Columns[0].Formula {
		t.Error("order: の列に印が付いていない")
	}
	// 挙げなかった3つも出る。ここが反転の実体。
	for _, k := range []string{"blood_oxygen", "weight_kg", "vo2_max"} {
		if !has(keys, k) {
			t.Errorf("%s が消えている（order: を許可リストとして読んでいる）: %v", k, keys)
		}
	}
}

// 隠すのは明示したときだけ。隠した判断がファイルに残る。
func TestHideIsExplicit(t *testing.T) {
	b := &Base{Name: "t"}
	v := &View{Kind: KindTable, Name: "v", Order: []string{"a"}, Hide: []string{"secret"}}
	rows := []*Record{rec("x.md", nil, map[string]Value{
		"a": Str("1"), "b": Str("2"), "secret": Str("見せない"),
	})}
	res, _ := Run(b, v, rows)
	keys := colKeys(res)
	if has(keys, "secret") {
		t.Error("hide: が効いていない")
	}
	if !has(keys, "b") {
		t.Error("hide: に書いていない列まで消している")
	}
}

// 計算列は formula. の前置きで衝突を避けつつ、表示名は素の名前。
func TestFormulaColumns(t *testing.T) {
	b := &Base{Name: "t", Formulas: map[string]string{
		"種別": `if(file.hasTag("bank"), "銀行", "その他")`,
	}}
	v := &View{Kind: KindTable, Name: "v"}
	rows := []*Record{rec("x.md", []string{"bank"}, map[string]Value{"種別": Str("生データ")})}
	res, err := Run(b, v, rows)
	if err != nil {
		t.Fatal(err)
	}
	got := res.Groups[0].Rows[0]
	if got.Cells["formula.種別"] != "銀行" {
		t.Fatalf("計算列が出ていない: %v", got.Cells)
	}
	// 同名の生プロパティを潰していないこと。
	if got.Cells["種別"] != "生データ" {
		t.Error("同名のプロパティを計算列で上書きしている")
	}
	// **入力は書き換えない。** 共有キャッシュを踏むため。
	if rows[0].Cells != nil {
		t.Errorf("Run が入力の Record を書き換えている: %v", rows[0].Cells)
	}
	for _, c := range res.Columns {
		if c.Key == "formula.種別" && c.Label != "種別" {
			t.Errorf("表示名が formula. のまま: %q", c.Label)
		}
	}
}

// 式が読めなくてもビューは出す。ただし黙らない。
// 黙って空にすると「該当なし」と「読めない式」が区別できなくなる。
func TestBadFormulaWarnsButStillRenders(t *testing.T) {
	b := &Base{Name: "t", Formulas: map[string]string{"壊": `someUnknownFn()`}}
	v := &View{Kind: KindTable, Name: "v"}
	res, err := Run(b, v, []*Record{rec("x.md", nil, map[string]Value{"a": Str("1")})})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 {
		t.Error("行が消えている")
	}
	if len(res.Warnings) == 0 {
		t.Error("読めない式を黙って捨てている")
	}
}

// filters が読めないときは黙って空にせずエラーにする。
func TestBadFilterIsAnError(t *testing.T) {
	b := &Base{Name: "t"}
	v := &View{Kind: KindTable, Name: "v", Filters: &Filter{Expr: `nope(`}}
	if _, err := Run(b, v, []*Record{rec("x.md", nil, nil)}); err == nil {
		t.Fatal("読めない filters を黙って通している")
	}
}

// and / or の入れ子が効く。Payments.base の横断ビューがこの形。
func TestNestedFilters(t *testing.T) {
	b := &Base{Name: "t", Filters: &Filter{Or: []*Filter{
		{Expr: `file.hasTag("bank")`}, {Expr: `file.hasTag("card")`},
	}}}
	v := &View{Kind: KindTable, Name: "v", Filters: &Filter{And: []*Filter{
		{Expr: `withdrawal != null`},
	}}}
	rows := []*Record{
		rec("a.md", []string{"bank"}, map[string]Value{"withdrawal": Num(100)}),
		rec("b.md", []string{"bank"}, map[string]Value{"deposit": Num(100)}),
		rec("c.md", []string{"paypay"}, map[string]Value{"withdrawal": Num(100)}),
	}
	res, err := Run(b, v, rows)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 {
		t.Fatalf("1行のはずが %d", res.Total)
	}
}

// Sum は数として足す。文字列で足すと 0 になる。
func TestSummaries(t *testing.T) {
	b := &Base{Name: "t"}
	v := &View{Kind: KindTable, Name: "v", Summaries: map[string]string{"withdrawal": "Sum"}}
	rows := []*Record{
		rec("a.md", nil, map[string]Value{"withdrawal": Num(1200)}),
		rec("b.md", nil, map[string]Value{"withdrawal": Num(800)}),
		rec("c.md", nil, map[string]Value{"withdrawal": Null()}),
	}
	res, _ := Run(b, v, rows)
	if res.Summary["withdrawal"] != 2000 {
		t.Fatalf("Sum=%v, want 2000", res.Summary["withdrawal"])
	}
}

// 並べ替えは数のとき数として。"10" < "9" になると壊れる。
func TestNumericSort(t *testing.T) {
	b := &Base{Name: "t"}
	v := &View{Kind: KindTable, Name: "v",
		Sort: []SortKey{{Property: "n", Direction: "ASC"}}}
	rows := []*Record{
		rec("a.md", nil, map[string]Value{"n": Num(10)}),
		rec("b.md", nil, map[string]Value{"n": Num(9)}),
	}
	res, _ := Run(b, v, rows)
	if res.Groups[0].Rows[0].Cells["n"] != "9" {
		t.Fatalf("数として並べていない: %v", res.Groups[0].Rows[0].Cells)
	}
}

// 素性も列として出す。実在の .base は order: に file.name を15箇所で挙げている。
func TestFileFieldsAreCells(t *testing.T) {
	b := &Base{Name: "t"}
	v := &View{Kind: KindTable, Name: "v", Order: []string{"file.name"}}
	r := rec("Data/Notes/メモ.md", nil, nil)
	r.MTime = "2026-09-02T00:00:00Z"
	res, _ := Run(b, v, []*Record{r})
	got := res.Groups[0].Rows[0]
	if got.Cells["file.name"] != "メモ.md" {
		t.Errorf("file.name が空: %v", got.Cells)
	}
	if got.Cells["file.folder"] != "Data/Notes" {
		t.Errorf("file.folder が違う: %q", got.Cells["file.folder"])
	}
	if got.Cells["file.mtime"] == "" {
		t.Error("file.mtime が空")
	}
	if !has(colKeys(res), "file.name") {
		t.Error("file.name が列に出ていない")
	}
}
