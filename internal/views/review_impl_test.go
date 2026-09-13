package views

import (
	"math"
	"strings"
	"testing"
)

// Phase 4 の実装後レビュー（codex・Fable、2026-09-13）で見つかった穴を縛る。
// 記録は `dev/active/review-codex-phase4-2026-09-13.md`・`review-fable-phase4-impl-2026-09-13.md`。

// mustRefuse は、その定義が**狙った理由で**読めないことを確かめる。
//
// 初めは台紙の名前を "T" と決め打ちして読んでいたので、`base: Payments` の定義は「台紙の名前と違う」で
// 必ず弾かれ、確かめたい検査を外しても試験が通っていた（2026-09-13、変異が素通りして気づいた）。
// 名前は本文の `base:` に任せ、断った理由に want が入っているかも見る。
func mustRefuse(t *testing.T, name, body, want string) {
	t.Helper()
	_, err := ParseNative("", []byte(body))
	if err == nil {
		t.Errorf("%s: 読めてしまった:\n%s", name, body)
		return
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("%s: 狙いと違う理由で弾かれた: %v（want %q）", name, err, want)
	}
}

// 向きは ASC・DESC だけ。**`descending` は昇順として黙って通っていた**（codex 3・Fable 2）。
func TestDirectionsOtherThanAscOrDescAreRefused(t *testing.T) {
	ok := "base: T\nviews:\n  - name: v\n    kind: table\n    shape:\n      sort: [{property: a, direction: desc}]\n      group_by: {property: g, direction: Asc}\n"
	b, err := ParseNative("T", []byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	if b.Views[0].Sort[0].Direction != "DESC" || b.Views[0].GroupBy.Direction != "ASC" {
		t.Fatalf("向きをそろえていない: %+v %+v", b.Views[0].Sort, b.Views[0].GroupBy)
	}
	mustRefuse(t, "sort", strings.Replace(ok, "direction: desc", "direction: descending", 1), "direction は ASC か DESC")
	mustRefuse(t, "group_by", strings.Replace(ok, "direction: Asc", "direction: up", 1), "direction は ASC か DESC")
	mustRefuse(t, "within", strings.Replace(chartDef, "direction: DESC", "direction: DSC", 1), "direction は ASC か DESC")
}

// **within が書いてあっても、その日の中の並びを決めていなければ止める**（Fable 2 の実データの形）。
// `within: [{property: date}]` だと同じ日の行は全部同じ値で、元の順に落ちて違う残高を描いた。
func TestWithinThatDoesNotOrderTheDayStops(t *testing.T) {
	b, v := bankView(&TimeSpec{Axis: "date", Bucket: "day",
		Within: []SortKey{{Property: "date", Direction: "DESC"}}}, "last")
	s := onlySeries(t, b, v, bankRows())
	if s.Error == "" || !strings.Contains(s.Error, "並びが決まらない") || len(s.Points) != 0 {
		t.Fatalf("効いていない within で描いた: %+v", s)
	}
	// sum は並びに依らないので描ける。
	v.Measures["balance"] = "sum"
	if s := onlySeries(t, b, v, bankRows()); s.Error != "" {
		t.Fatalf("sum まで止めた: %s", s.Error)
	}
}

// 日付は**値の全体を**書かれた暦日として読む（codex 5、本人の決定）。
func TestAxisValuesAreReadWholeAsWrittenCalendarDates(t *testing.T) {
	cases := []struct {
		raw, bucket, want string
		ok                bool
	}{
		{"2026-09-13", "day", "2026-09-13", true},
		{"2026-09-13", "month", "2026-09", true},
		{"2026-09", "month", "2026-09", true},
		{"2026-09", "day", "", false},      // 区切りより粗い
		{"2026-09-99", "month", "", false}, // 存在しない日（以前は 9 月の点になった）
		{"2026-09-garbage", "month", "", false},
		{"2026-09Tnot-a-time", "month", "", false},
		{"2026-09-13T23:30:00-05:00", "day", "2026-09-13", true}, // 時差は直さない（書かれた日付）
		{"2026-09-13T08:00:00Z", "day", "2026-09-13", true},
		{"2026-09-13 10:00", "day", "2026-09-13", true},
		{"2026", "year", "2026", true},
		{"20261", "year", "", false},
	}
	for _, c := range cases {
		got, ok := bucketOf(c.raw, c.bucket)
		if ok != c.ok || got != c.want {
			t.Errorf("bucketOf(%q, %s) = %q, %v（want %q, %v）", c.raw, c.bucket, got, ok, c.want, c.ok)
		}
	}
}

// **読めない値を飛ばして残りだけで点を作らない**（codex 6）。1000・2,000・3000 の合計を 4000 と描いていた。
func TestABucketWithAnUnreadableValueIsNotPlotted(t *testing.T) {
	r := func(p, ym, amount string) *Record {
		return rec(p, nil, map[string]Value{"ym": Str(ym), "amount": Str(amount)})
	}
	recs := []*Record{r("a", "2026-08", "1000"), r("b", "2026-08", "2,000"), r("c", "2026-08", "3000"),
		r("d", "2026-09", "500")}
	v := &View{Name: "m", Time: &TimeSpec{Axis: "ym", Bucket: "month"}, Measures: map[string]string{"amount": "sum"}}
	s := onlySeries(t, &Base{Name: "P"}, v, recs)
	if len(s.Points) != 1 || s.Points[0].T != "2026-09" || s.Holes != 1 || s.Skipped != 1 {
		t.Fatalf("読めない値のある月を描いた: %+v", s)
	}
	// only でも「数1行＋読めない1行」を1行と数えない。
	v.Measures["amount"] = "only"
	recs = []*Record{r("a", "2026-08", "1000"), r("b", "2026-08", "たくさん")}
	if s := onlySeries(t, &Base{Name: "P"}, v, recs); len(s.Points) != 0 || s.Holes != 1 {
		t.Fatalf("%+v", s)
	}
}

// NaN・Inf は数ではない（JSON に書けず、応答が丸ごと落ちる。codex 6・Fable 10）。
func TestNaNAndInfAreNotNumbers(t *testing.T) {
	for _, x := range []string{"NaN", "nan", "Inf", "-Inf", "+Infinity"} {
		if f, ok := cellNum(x); ok || math.IsNaN(f) {
			t.Errorf("%q を数として読んだ", x)
		}
	}
}

// 形式の決めごと（本人の決定と Fable 5・6・7・9）。
func TestTheDefinitionFormatIsFixed(t *testing.T) {
	// 種別は table・cards・chart・graph だけ。
	mustRefuse(t, "life-tracker", strings.Replace(oneView, "kind: table", "kind: life-tracker", 1), "chart と書く")
	mustRefuse(t, "知らない種別", strings.Replace(oneView, "kind: table", "kind: pie", 1), "知らない種別")
	// 表示名は台紙のいちばん上。ビューの shape には置けない。
	top := "base: T\nlabels: {note.category: 場所}\nviews:\n  - name: v\n    kind: table\n"
	b, err := ParseNative("T", []byte(top))
	if err != nil || b.Display["category"] != "場所" {
		t.Fatalf("台紙の labels を読めない: %v %+v", err, b)
	}
	mustRefuse(t, "shape の labels", "base: T\nviews:\n  - name: v\n    kind: table\n    shape:\n      labels: {a: A}\n", "labels は台紙のいちばん上")
	// 新しすぎる版は読まない。
	mustRefuse(t, "version 2", oneView+"version: 2\n", "version 2")
	// 描く列には集約が要る（本人の決定）。
	mustRefuse(t, "集約の無い列", strings.Replace(chartDef, "        note.balance: last\n", "        note.other: last\n", 1), "集約が無い")
}

// 変換は `life-tracker` を `chart` と書き、表示名を台紙のいちばん上に書く。突き合わせでは同じとみなす。
func TestConversionWritesTheFixedFormat(t *testing.T) {
	for _, b := range realBases(t) {
		body, err := ToNative(b)
		if err != nil {
			t.Fatal(err)
		}
		s := string(body)
		if strings.Contains(s, "life-tracker") {
			t.Fatalf("%s: life-tracker を書いた", b.Name)
		}
		if len(b.Display) > 0 && !strings.Contains(s, "\nlabels:") {
			t.Fatalf("%s: 表示名を台紙のいちばん上に書いていない:\n%s", b.Name, s)
		}
		got, err := ParseNative(b.Name, body)
		if err != nil {
			t.Fatal(err)
		}
		if d := DiffBases(b, got); len(d) > 0 {
			t.Fatalf("%s: %v", b.Name, d)
		}
	}
}

// 描き方の突き合わせは、`.base` で言える部分の誤変換を捕まえる（Fable 4）。時間軸・集約・色は見ない。
func TestDiffEmitCatchesChartConversionMistakes(t *testing.T) {
	base := func() *Base {
		b, err := ParseNative("Payments", []byte(chartDef))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	a := base()
	for name, mut := range map[string]func(v *View){
		"列が欠けた":  func(v *View) { v.Emit.Human[0].Values = nil },
		"線→棒":    func(v *View) { v.Emit.Human[0].Chart = "bar" },
		"期間が落ちた": func(v *View) { v.Emit.Human[0].Window = "" },
		"列幅":     func(v *View) { v.Emit.Human[1].Widths["file.name"] = 1 },
	} {
		b := base()
		mut(&b.Views[0])
		if d := DiffEmit(a, b); len(d) != 1 {
			t.Errorf("%s を見逃した: %v", name, d)
		}
	}
	b := base()
	b.Views[0].Time.Bucket = "month"
	b.Views[0].Measures["balance"] = "sum"
	if d := DiffEmit(a, b); len(d) != 0 {
		t.Fatalf("時間軸と集約（.base で言えない）を差にした: %v", d)
	}
}
