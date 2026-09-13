package mcp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/MoomA-0750/camp/internal/views"
)

// モデルへ集計を渡す（M50、2026-09-13）。
//
// **D-004 の約束**は「集計は Camp が計算し、モデルには算術させない」。それまで
// `clipResult` は groups を平らにして行を積むだけで、**グループ鍵も グループ別集計も
// 全体集計も落としていた**——モデルは行を読んで自分で足すしかなかった。
//
// 行は切り詰めても**集計は切り詰めない**（数個の数で、これが正確さの源だから）。

// oneResult は groups と集計を持つ結果を組み立てる（`Run` を通さずに `clipResult` だけを縛る）。
func oneResult() *views.Result {
	row := func(amount string) *views.Record {
		return &views.Record{Cells: map[string]string{"amount": amount}}
	}
	return &views.Result{
		View: "Payments/カード-すべて", Kind: views.KindTable,
		Columns: []views.Column{{Key: "amount", Label: "amount", Numeric: true, Filled: 4}},
		Groups: []views.Group{
			{Key: "2026-01", Rows: []*views.Record{row("100"), row("200")},
				Summary: map[string]float64{"amount": 300}},
			{Key: "2025-12", Rows: []*views.Record{row("30"), row("40")},
				Summary: map[string]float64{"amount": 70}},
		},
		Total:   4,
		Summary: map[string]float64{"amount": 370},
	}
}

func TestTheModelGetsTheAggregatesEvenWhenRowsAreClipped(t *testing.T) {
	// **行を1行に切り詰めても**集計は full で渡る。ここが肝。
	out := clipResult(oneResult(), 1, false)

	rows, _ := out["rows"].([]map[string]string)
	if len(rows) != 1 {
		t.Fatalf("行が切り詰められていない: %d", len(rows))
	}
	sum, ok := out["summary"].(map[string]float64)
	if !ok || sum["amount"] != 370 {
		t.Fatalf("全体の集計が渡っていない: %#v", out["summary"])
	}

	gs, ok := out["groups"].([]map[string]any)
	if !ok || len(gs) != 2 {
		t.Fatalf("グループが渡っていない: %#v", out["groups"])
	}
	// グループ鍵と件数と集計が、行の切り詰めに関係なく全部入る。
	want := map[string]float64{"2026-01": 300, "2025-12": 70}
	wantRows := map[string]int{"2026-01": 2, "2025-12": 2}
	for _, g := range gs {
		key, _ := g["key"].(string)
		if _, known := want[key]; !known {
			t.Fatalf("知らないグループ鍵: %#v", g)
		}
		gsum, ok := g["summary"].(map[string]float64)
		if !ok || gsum["amount"] != want[key] {
			t.Fatalf("%s のグループ別集計が違う: %#v", key, g["summary"])
		}
		if n, _ := g["rows"].(int); n != wantRows[key] {
			t.Fatalf("%s の件数が違う: %#v", key, g["rows"])
		}
	}

	// 切ったことは書く（元からの約束）。
	if s, _ := out["truncated"].(string); !strings.Contains(s, "4") {
		t.Fatalf("切ったことが書かれていない: %#v", out["truncated"])
	}
}

// グループ分けしていないビューでは、鍵の無いグループ1つと全体集計が渡る。
func TestAnUngroupedViewStillCarriesItsSummary(t *testing.T) {
	r := oneResult()
	r.Groups = []views.Group{{Rows: r.Groups[0].Rows}}
	r.Total = 2
	out := clipResult(r, 50, false)

	gs, _ := out["groups"].([]map[string]any)
	if len(gs) != 1 {
		t.Fatalf("グループが1つにならない: %#v", out["groups"])
	}
	if _, has := gs[0]["key"]; has {
		t.Fatalf("鍵の無いグループに鍵が入った: %#v", gs[0])
	}
	if sum, _ := out["summary"].(map[string]float64); sum["amount"] != 370 {
		t.Fatalf("全体の集計が渡っていない: %#v", out["summary"])
	}
	// 集計が無い結果では、欄そのものを出さない（空の欄でモデルを惑わせない）。
	r.Summary = nil
	if _, has := clipResult(r, 50, false)["summary"]; has {
		t.Fatal("集計が無いのに summary を出した")
	}
}

// **時系列もモデルへ渡す**（M51、2026-09-13）。人が「日ごとの残高推移」を見ているのに、
// モデルが行の先頭だけを読んで自分で集約し直すと、同日8件の日で残高を間違える。
// 点は新しいほうを残して切り詰め、描けない理由（error）は必ず渡す。
func TestTheModelGetsTheSameSeriesAsThePerson(t *testing.T) {
	r := oneResult()
	pts := make([]views.Point, 0, maxSeriesPoints+10)
	for i := 0; i < maxSeriesPoints+10; i++ {
		pts = append(pts, views.Point{T: fmt.Sprintf("p%04d", i), V: float64(i), N: 1})
	}
	r.Time = &views.TimeSpec{Axis: "date", Bucket: "day"}
	r.Series = []views.Series{
		{Key: "balance", Label: "balance", Measure: "last", Points: pts, Rows: len(pts)},
		{Key: "withdrawal", Label: "withdrawal", Measure: "only", Error: "同じ日に複数行ある"},
	}
	out := clipResult(r, 1, false)

	if out["time"] != r.Time {
		t.Fatalf("時間軸が渡っていない: %#v", out["time"])
	}
	ss, ok := out["series"].([]map[string]any)
	if !ok || len(ss) != 2 {
		t.Fatalf("時系列が渡っていない: %#v", out["series"])
	}
	got, _ := ss[0]["points"].([]views.Point)
	if len(got) != maxSeriesPoints || got[len(got)-1].T != pts[len(pts)-1].T {
		t.Fatalf("新しい点を残して切り詰めていない: %d 点、最後 %v", len(got), got[len(got)-1])
	}
	if s, _ := ss[0]["truncated"].(string); !strings.Contains(s, fmt.Sprint(len(pts))) {
		t.Fatalf("切ったことが書かれていない: %#v", ss[0]["truncated"])
	}
	if ss[0]["measure"] != "last" {
		t.Fatalf("集約が渡っていない: %#v", ss[0])
	}
	if ss[1]["error"] != "同じ日に複数行ある" {
		t.Fatalf("描けない理由が渡っていない: %#v", ss[1])
	}
}

// モデルへ渡すグラフは、辺をパスで書き、節の上限で遠いほうから切る（M52、2026-09-13）。
func TestTheModelGetsTheGraphByPath(t *testing.T) {
	g := &views.Graph{Center: 1, Depth: 1, Unlinked: 7, Total: 450,
		Nodes: []views.GraphNode{{ID: 1, Path: "hub/index.md", Hops: 0, Degree: 2},
			{ID: 2, Path: "ctx/lab.md", Hops: 1, Degree: 1, Tags: []string{"lab"}},
			{ID: 3, Path: "far.md", Hops: 2, Degree: 1}},
		Edges: []views.GraphEdge{{From: 1, To: 2}, {From: 3, To: 1}}}
	out := graphForModel(g, 2)
	if out["center"] != "hub/index.md" || out["unlinked"] != 7 {
		t.Fatalf("%#v", out)
	}
	ns, _ := out["nodes"].([]map[string]any)
	es, _ := out["edges"].([]map[string]string)
	if len(ns) != 2 || ns[1]["path"] != "ctx/lab.md" {
		t.Fatalf("近いほうを残していない: %#v", ns)
	}
	if len(es) != 1 || es[0]["from"] != "hub/index.md" || es[0]["to"] != "ctx/lab.md" {
		t.Fatalf("切った節への辺が残った、またはパスで書いていない: %#v", es)
	}
	// 切る前の全体の数（サーバーが先に切っていても 450）を言う。degree は返したグラフの中で数え直す。
	if s, _ := out["truncated"].(string); !strings.Contains(s, "450") {
		t.Fatalf("切る前の数が書かれていない: %#v", out["truncated"])
	}
	if ns[0]["degree"] != 1 {
		t.Fatalf("degree を返したグラフの中で数え直していない: %#v", ns[0])
	}
}
