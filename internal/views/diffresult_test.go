package views

import "testing"

// 実行結果の突き合わせ（併読の2段目。M50、2026-09-13）。
//
// **ここが M50 の受け入れの片翼。** 構造の差分（`DiffBases`）が定義の取り違えを捕まえ、
// こちらが「同じ行セットに解決されたか」を捕まえる。初稿の受け入れ条件
// （行数・列数・グループ数・集計）では出なかった取り違えを、ここで出す。

func resTwoGroups() *Result {
	row := func(v string) *Record { return &Record{Cells: map[string]string{"a": v, "b": "x"}} }
	return &Result{
		View: "T/v", Kind: KindTable,
		Columns: []Column{{Key: "a", Filled: 4}, {Key: "b", Filled: 4}},
		Groups: []Group{
			{Key: "2026-01", Rows: []*Record{row("1"), row("2")},
				Summary: map[string]float64{"a": 3}},
			{Key: "2025-12", Rows: []*Record{row("3"), row("4")},
				Summary: map[string]float64{"a": 7}},
		},
		Total:   4,
		Summary: map[string]float64{"a": 10},
	}
}

func TestTheResultDiffCatchesOrderAndAggregateChanges(t *testing.T) {
	// 同じものは差として出さない（騒がしいと読まれなくなる）。
	if d := DiffResults(resTwoGroups(), resTwoGroups(), 20); len(d) > 0 {
		t.Fatalf("同じ結果に差が出た: %v", d)
	}

	for _, c := range []struct {
		name   string
		break_ func(*Result)
	}{
		// **数では出ないもの。** ここが初稿の受け入れ条件の穴だった。
		{"グループの並びが逆", func(r *Result) {
			r.Groups[0], r.Groups[1] = r.Groups[1], r.Groups[0]
		}},
		{"グループの中の行の並びが逆", func(r *Result) {
			g := r.Groups[0].Rows
			g[0], g[1] = g[1], g[0]
		}},
		{"列の並びが逆", func(r *Result) {
			r.Columns[0], r.Columns[1] = r.Columns[1], r.Columns[0]
		}},
		// 数で出るもの（それでも落ちることを確かめる）。
		{"列が1つ減る", func(r *Result) { r.Columns = r.Columns[:1] }},
		{"行数が違う", func(r *Result) { r.Total = 5 }},
		{"グループが1つ減る", func(r *Result) { r.Groups = r.Groups[:1] }},
		{"グループの行数が違う", func(r *Result) { r.Groups[0].Rows = r.Groups[0].Rows[:1] }},
		// **行の中身に出ない形で行数だけ変える。** 上の切り詰めは先頭の行も変わるので、
		// 中身の比較が先に捕まえてしまい、行数の検査そのものは縛れていなかった
		// （2026-09-13、変異が素通りして気づいた）。末尾のグループに足せば中身は動かない。
		{"末尾のグループだけ行数が増える", func(r *Result) {
			last := len(r.Groups) - 1
			r.Groups[last].Rows = append(r.Groups[last].Rows,
				&Record{Cells: map[string]string{"a": "4", "b": "x"}})
		}},
		{"グループ鍵が違う", func(r *Result) { r.Groups[0].Key = "2026-02" }},
		{"グループ別集計の値が違う", func(r *Result) { r.Groups[0].Summary["a"] = 99 }},
		{"グループ別集計が落ちる", func(r *Result) { r.Groups[0].Summary = nil }},
		{"全体の集計の値が違う", func(r *Result) { r.Summary["a"] = 99 }},
		{"全体の集計が落ちる", func(r *Result) { r.Summary = nil }},
		{"種別が違う", func(r *Result) { r.Kind = KindCards }},
		{"セルの中身が違う", func(r *Result) { r.Groups[0].Rows[0].Cells["a"] = "9" }},
	} {
		a, z := resTwoGroups(), resTwoGroups()
		c.break_(z)
		if d := DiffResults(a, z, 20); len(d) == 0 {
			t.Errorf("%s: 差として出ない（突き合わせが弱い）", c.name)
		}
	}
}

// 行を見る数を 0 にすると中身は比べない（集計と構造だけ見たいとき）。
func TestTheResultDiffCanSkipTheRowBodies(t *testing.T) {
	a, z := resTwoGroups(), resTwoGroups()
	z.Groups[0].Rows[0].Cells["a"] = "9"
	if d := DiffResults(a, z, 0); len(d) != 0 {
		t.Fatalf("行を見ない設定なのに中身を比べた: %v", d)
	}
	// それでも集計の違いは出る。
	z.Summary["a"] = 99
	if d := DiffResults(a, z, 0); len(d) == 0 {
		t.Fatal("集計の違いが出ない")
	}
	// **行数の違いも、中身を見ない設定でも出る。** ここを中身の比較に頼ると、
	// 行数の検査が実は縛られていない状態になる（変異で確かめた）。
	a2, z2 := resTwoGroups(), resTwoGroups()
	z2.Groups[1].Rows = z2.Groups[1].Rows[:1]
	if d := DiffResults(a2, z2, 0); len(d) == 0 {
		t.Fatal("行を見ない設定で、グループの行数の違いが出ない")
	}
}

// 持ち越さなかった鍵を数える（変換の報告に出すもの）。
func TestDroppedKeysListsTheObsidianOnlySettings(t *testing.T) {
	b := &Base{Name: "T", Views: []View{
		{Name: "v1", Extra: map[string]any{"columnSize": 1, "gridColumns": 1}},
		{Name: "v2", Extra: map[string]any{"columnSize": 2, "timeFrame": "x"}},
	}}
	got := DroppedKeys(b)
	if len(got) != 3 || got[0] != "columnSize" || got[1] != "gridColumns" || got[2] != "timeFrame" {
		t.Fatalf("持ち越さなかった鍵が違う: %v", got)
	}
	if len(DroppedKeys(&Base{Name: "T", Views: []View{{Name: "v"}}})) != 0 {
		t.Fatal("Extra が無いのに鍵を出した")
	}
}
