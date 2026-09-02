package views

import (
	"strings"
	"sync"
	"testing"
)

// **実行順で結果が変わらないこと。**
//
// Run が共有の Record を書き換えていたころは、あるビューの計算列が
// 後から実行した無関係なビューの「実在する全列」に残っていた。
func TestRunIsOrderIndependent(t *testing.T) {
	recs := []*Record{
		rec("Data/A/x.md", []string{"bank"}, map[string]Value{"a": Num(1)}),
		rec("Data/B/y.md", nil, map[string]Value{"b": Num(2)}),
	}
	withFormula := &Base{Name: "f", Formulas: map[string]string{"種別": `"銀行"`}}
	plain := &Base{Name: "p"}
	v := &View{Kind: KindTable, Name: "v"}

	alone, err := Run(plain, v, recs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(withFormula, v, recs); err != nil {
		t.Fatal(err)
	}
	after, err := Run(plain, v, recs)
	if err != nil {
		t.Fatal(err)
	}
	if len(alone.Columns) != len(after.Columns) {
		t.Fatalf("別の .base を挟んだら列が %d → %d に変わった",
			len(alone.Columns), len(after.Columns))
	}
	for _, c := range after.Columns {
		if strings.HasPrefix(c.Key, "formula.") {
			t.Errorf("他の .base の計算列が混ざっている: %s", c.Key)
		}
	}
}

// **同時に走らせても落ちないこと。**
//
// 共有の map へ書き込んでいたころは、`/api/views` を2本同時に叩くと
// Go ランタイムの fatal error: concurrent map writes でプロセスごと落ちた。
// fatal error は recover できないので、これは「たまに500が返る」ではなく
// 「campd が死ぬ」という壊れ方だった。
func TestRunIsSafeUnderConcurrency(t *testing.T) {
	var recs []*Record
	for i := 0; i < 200; i++ {
		recs = append(recs, rec("Data/A/x.md", []string{"bank"},
			map[string]Value{"a": Num(float64(i)), "b": Str("x")}))
	}
	b := &Base{Name: "t", Formulas: map[string]string{
		"種別": `if(file.hasTag("bank"), "銀行", "その他")`,
	}}
	views := []*View{
		{Kind: KindTable, Name: "v1", Order: []string{"a"}},
		{Kind: KindTable, Name: "v2", Sort: []SortKey{{Property: "a", Direction: "DESC"}}},
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := Run(b, views[(i+j)%2], recs); err != nil {
					t.Error(err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

// file.* での並べ替えが効くこと。
//
// 旧実装は Props に無い列を全部 Numeric 扱いにし、比較を Props から
// 引いていたので全行 0 になり、less(i,j) と less(j,i) が両方 true を
// 返す不整合な比較器になっていた（Note-Taking/All が更新順に並ばない）。
func TestSortByFileFields(t *testing.T) {
	mk := func(name, mtime string) *Record {
		r := rec("Data/N/"+name+".md", nil, nil)
		r.MTime = mtime
		return r
	}
	recs := []*Record{
		mk("a", "2026-08-24T00:00:00Z"),
		mk("b", "2026-09-02T00:00:00Z"),
		mk("c", "2026-08-29T00:00:00Z"),
	}
	b := &Base{Name: "t"}
	v := &View{Kind: KindTable, Name: "v",
		Sort: []SortKey{{Property: "file.mtime", Direction: "DESC"}}}
	res, err := Run(b, v, recs)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2026-09-02T00:00:00Z", "2026-08-29T00:00:00Z", "2026-08-24T00:00:00Z"}
	for i, row := range res.Groups[0].Rows {
		if row.MTime != want[i] {
			t.Fatalf("%d番目が %s（%s のはず）", i, row.MTime, want[i])
		}
	}
}

// `note.foo` は frontmatter の foo。実在の .base は order: に
// note.note を5ビューで挙げている。
func TestNoteQualifiedNames(t *testing.T) {
	b, err := ParseBase("X.base", []byte(
		"views:\n  - type: table\n    name: v\n    order: [note.note, note.n]\n"+
			"    sort:\n      - property: note.n\n        direction: DESC\n"+
			"    summaries:\n      note.n: Sum\n"))
	if err != nil {
		t.Fatal(err)
	}
	recs := []*Record{
		rec("a.md", nil, map[string]Value{"note": Str("めも"), "n": Num(1)}),
		rec("b.md", nil, map[string]Value{"note": Str("めも2"), "n": Num(5)}),
	}
	res, err := Run(b, &b.Views[0], recs)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Columns {
		if strings.HasPrefix(c.Key, "note.") {
			t.Errorf("note. が剥がれていない空列がある: %s", c.Key)
		}
	}
	if res.Columns[0].Key != "note" || res.Columns[0].Filled != 2 {
		t.Errorf("note 列が空: %+v", res.Columns[0])
	}
	if res.Summary["n"] != 6 {
		t.Errorf("note.n の Sum が効いていない: %v", res.Summary)
	}
	if res.Groups[0].Rows[0].Cells["n"] != "5" {
		t.Error("note.n での並べ替えが効いていない")
	}
}

// `not:` は「どれも真でない」。Obsidian のフィルタUIの none。
func TestNotFilter(t *testing.T) {
	b, err := ParseBase("X.base", []byte(
		"filters:\n  not:\n    - file.hasTag(\"book\")\nviews:\n  - type: table\n    name: v\n"))
	if err != nil {
		t.Fatalf("not: が読めない: %v", err)
	}
	recs := []*Record{
		rec("a.md", []string{"book"}, nil),
		rec("b.md", []string{"note"}, nil),
	}
	res, err := Run(b, &b.Views[0], recs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || res.Groups[0].Rows[0].NPath != "b.md" {
		t.Errorf("not: が効いていない: %d件", res.Total)
	}
}

// 解釈しないキーは全部残す（許可リストにしない）。
func TestUnknownKeysSurvive(t *testing.T) {
	b, err := ParseBase("X.base", []byte(
		"properties:\n  note.category:\n    displayName: Place\n"+
			"views:\n  - type: table\n    name: v\n    未知のキー: 42\n    cardSize: 200\n"))
	if err != nil {
		t.Fatal(err)
	}
	if b.Views[0].Extra["未知のキー"] == nil {
		t.Error("知らないキーを捨てている")
	}
	if b.Views[0].Extra["cardSize"] == nil {
		t.Error("cardSize を捨てている")
	}
	if b.Display["category"] != "Place" {
		t.Errorf("properties: の displayName を捨てている: %v", b.Display)
	}
}
