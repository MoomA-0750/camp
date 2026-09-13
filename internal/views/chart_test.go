package views

import (
	"reflect"
	"strings"
	"testing"
)

// 時系列（M51、2026-09-13）。

// bankRows は実データの形を写す: 同じ日のノートが並び、**ファイル名の逆順が時の流れ**。
// 金額は作り物（公開する写しに本人の残高を出さない）。
func bankRows() []*Record {
	r := func(name, date string, bal float64) *Record {
		return rec("Data/bank/"+name+".md", []string{"bank"},
			map[string]Value{"date": Str(date), "balance": Num(bal)})
	}
	return []*Record{
		r("2025-12-26_01", "2025-12-26", 1200),
		r("2025-12-27_01", "2025-12-27", 1500), // その日の最後
		r("2025-12-27_02", "2025-12-27", 900),
		r("2025-12-27_03", "2025-12-27", 1300), // その日の最初
	}
}

func bankView(t *TimeSpec, fn string) (*Base, *View) {
	v := View{Kind: KindLifeTracker, Name: "残高", Time: t,
		Measures: map[string]string{"balance": fn},
		Emit:     &Emit{Human: []Encoding{{Kind: "chart", Values: []string{"balance"}, Chart: "line"}}}}
	return &Base{Name: "Payments"}, &v
}

func onlySeries(t *testing.T, b *Base, v *View, recs []*Record) Series {
	t.Helper()
	res, err := Run(b, v, recs)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Series) != 1 {
		t.Fatalf("時系列は1本のはずが %d", len(res.Series))
	}
	return res.Series[0]
}

// **M51 の受け入れの芯。** 同じ日に複数行ある残高は、その日の中の並びを書かないと描かない。
// ファイル名の順で黙って `last` を取ると、その日の**最初の**残高（1,300）を描いてしまう。
func TestLastRefusesToGuessTheOrderWithinACrowdedDay(t *testing.T) {
	b, v := bankView(&TimeSpec{Axis: "date", Bucket: "day"}, "last")
	s := onlySeries(t, b, v, bankRows())
	if s.Error == "" || !strings.Contains(s.Error, "time.within") {
		t.Fatalf("並びが無いのに描いた: %+v", s)
	}
	if len(s.Points) != 0 {
		t.Fatalf("描けないのに点を出した: %+v", s.Points)
	}
}

func TestLastFollowsTheOrderWithinTheDay(t *testing.T) {
	b, v := bankView(&TimeSpec{Axis: "date", Bucket: "day",
		Within: []SortKey{{Property: "file.name", Direction: "DESC"}}}, "last")
	s := onlySeries(t, b, v, bankRows())
	if s.Error != "" {
		t.Fatal(s.Error)
	}
	want := []Point{{T: "2025-12-26", V: 1200, N: 1}, {T: "2025-12-27", V: 1500, N: 3}}
	if !reflect.DeepEqual(s.Points, want) {
		t.Fatalf("点が違う:\n got %+v\nwant %+v", s.Points, want)
	}
	// first は同じ並びで最初。
	v.Measures["balance"] = "first"
	if s := onlySeries(t, b, v, bankRows()); s.Points[1].V != 1300 {
		t.Fatalf("first が違う: %+v", s.Points)
	}
}

// `only` は「区切りに1行しか無い」という主張。破れていれば止まる（d払いや体重は通る）。
func TestOnlyStopsWhenTheClaimIsBroken(t *testing.T) {
	b, v := bankView(&TimeSpec{Axis: "date", Bucket: "day"}, "only")
	s := onlySeries(t, b, v, bankRows())
	if !strings.Contains(s.Error, "2025-12-27 に 3 行") {
		t.Fatalf("混んだ日を名指ししていない: %q", s.Error)
	}
	s = onlySeries(t, b, v, bankRows()[:2])
	if s.Error != "" || len(s.Points) != 2 {
		t.Fatalf("1日1行なのに描かない: %+v", s)
	}
}

func TestTheOtherMeasures(t *testing.T) {
	cases := map[string]float64{
		"sum": 1500 + 900 + 1300, "avg": (1500 + 900 + 1300) / 3.0,
		"min": 900, "max": 1500,
	}
	for fn, want := range cases {
		b, v := bankView(&TimeSpec{Axis: "date", Bucket: "day"}, fn)
		s := onlySeries(t, b, v, bankRows())
		if s.Error != "" || s.Points[1].V != want || s.Points[1].N != 3 {
			t.Errorf("%s: %+v（want %v）", fn, s, want)
		}
	}
}

// 月で区切れば日の値も読める。**日で区切るのに月までの値は読まない**（1日に寄せると嘘になる）。
func TestBucketsNeverInventPrecision(t *testing.T) {
	b, v := bankView(&TimeSpec{Axis: "date", Bucket: "month"}, "sum")
	s := onlySeries(t, b, v, bankRows())
	if len(s.Points) != 1 || s.Points[0].T != "2025-12" || s.Points[0].N != 4 {
		t.Fatalf("月に寄せられていない: %+v", s)
	}

	monthly := []*Record{
		rec("Data/pay/2025-11.md", nil, map[string]Value{"year_month": Str("2025-11"), "total_jpy": Num(4200)}),
		rec("Data/pay/2025-12.md", nil, map[string]Value{"year_month": Str("2025-12"), "total_jpy": Num(100)}),
	}
	v2 := &View{Name: "d", Time: &TimeSpec{Axis: "year_month", Bucket: "day"},
		Measures: map[string]string{"total_jpy": "sum"}}
	s = onlySeries(t, &Base{Name: "P"}, v2, monthly)
	if len(s.Points) != 0 || s.Skipped != 2 || !strings.Contains(s.Error, "2025-11") {
		t.Fatalf("月の値を日で読んだ: %+v", s)
	}
}

// 値はあるのに読めない行は数える。値の無い行（体重の無い日）は数えない。
func TestUnreadableRowsAreCountedNotDropped(t *testing.T) {
	recs := []*Record{
		rec("a.md", nil, map[string]Value{"date": Str("2026-08-18"), "weight_kg": Num(56)}),
		rec("b.md", nil, map[string]Value{"date": Str("2026-08-19")}),
		rec("c.md", nil, map[string]Value{"date": Str("そのうち"), "weight_kg": Num(57)}),
		rec("d.md", nil, map[string]Value{"date": Str("2026-08-20"), "weight_kg": Str("重い")}),
	}
	v := &View{Name: "w", Time: &TimeSpec{Axis: "date", Bucket: "day"},
		Measures: map[string]string{"weight_kg": "only"}}
	s := onlySeries(t, &Base{Name: "H"}, v, recs)
	if s.Error != "" || len(s.Points) != 1 || s.Rows != 1 || s.Skipped != 2 {
		t.Fatalf("%+v", s)
	}
}

// 時間軸や集約が無ければ、描く列があっても点を出さずに理由を言う。
func TestUndecidedSeriesSayWhy(t *testing.T) {
	b, v := bankView(nil, "last")
	if s := onlySeries(t, b, v, bankRows()); !strings.Contains(s.Error, "time.axis") {
		t.Fatalf("%+v", s)
	}
	b, v = bankView(&TimeSpec{Axis: "date", Bucket: "day"}, "")
	delete(v.Measures, "balance")
	if s := onlySeries(t, b, v, bankRows()); !strings.Contains(s.Error, "measures.balance") {
		t.Fatalf("%+v", s)
	}
}

const chartDef = `base: Payments
source:
  filter: 'file.hasTag("bank")'
views:
  - name: 銀行-残高推移
    kind: chart
    shape:
      order: [balance]
      time:
        axis: date
        bucket: day
        within:
          - property: note.file_seq
            direction: DESC
      measures:
        note.balance: last
    emit:
      human:
        - kind: chart
          values: [note.balance]
          chart: line
          window: last-365-days
        - kind: table
          widths: {file.name: 120}
`

// 独自定義の往復で時系列と描き方が落ちない（`DiffBases` は見ないので別に確かめる）。
func TestChartDefinitionsSurviveTheRoundTrip(t *testing.T) {
	b, err := ParseNative("Payments", []byte(chartDef))
	if err != nil {
		t.Fatal(err)
	}
	v := &b.Views[0]
	if v.Time == nil || v.Time.Within[0].Property != "file_seq" || v.Measures["balance"] != "last" ||
		v.Emit.Human[0].Values[0] != "balance" || v.Emit.Human[1].Widths["file.name"] != 120 {
		t.Fatalf("読めていない: time=%+v measures=%v emit=%+v", v.Time, v.Measures, v.Emit)
	}
	body, err := ToNative(b)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseNative("Payments", body)
	if err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	w := &again.Views[0]
	if !reflect.DeepEqual(v.Time, w.Time) || !reflect.DeepEqual(v.Measures, w.Measures) ||
		!reflect.DeepEqual(v.Emit, w.Emit) {
		t.Fatalf("往復で変わった:\n%s", body)
	}
	// 人が上から読める順（`shape` の中で time・measures は後ろ、emit は shape の後）。
	s := string(body)
	if !(strings.Index(s, "shape:") < strings.Index(s, "time:") &&
		strings.Index(s, "time:") < strings.Index(s, "measures:") &&
		strings.Index(s, "measures:") < strings.Index(s, "emit:")) {
		t.Fatalf("書き出しの順が読みにくい:\n%s", s)
	}
}

// 読めない時系列の定義は読めない（＝保存もされない）。黙って既定値で描かない。
func TestBrokenChartDefinitionsAreRefused(t *testing.T) {
	broken := map[string][2]string{
		"区切りが無い":         {"bucket: day", ""},
		"知らない区切り":        {"bucket: day", "bucket: week"},
		"軸が無い":           {"axis: date", ""},
		"知らない集約":         {"note.balance: last", "note.balance: median"},
		"知らない描き方":        {"- kind: table", "- kind: pie"},
		"values の無いチャート": {"values: [note.balance]", ""},
		"知らない線":          {"chart: line", "chart: area"},
		"知らない期間":         {"window: last-365-days", "window: last-year"},
	}
	for name, r := range broken {
		body := strings.Replace(chartDef, r[0], r[1], 1)
		if body == chartDef {
			t.Fatalf("%s: 置換が当たっていない", name)
		}
		if _, err := ParseNative("Payments", []byte(body)); err == nil {
			t.Errorf("%s: 読めてしまった", name)
		}
	}
}
