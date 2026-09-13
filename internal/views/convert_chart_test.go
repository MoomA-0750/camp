package views

import (
	"reflect"
	"strings"
	"testing"
)

// 変換器が life-tracker を独自定義のチャートへ移す（M51、2026-09-13）。
// `.base` は実在の台紙から形だけ写した（列の名前と値は作り物）。

const trackerBase = `filters:
  and:
    - file.inFolder("Data")
views:
  - type: life-tracker
    name: 残高
    filters:
      and:
        - file.hasTag("bank")
    order:
      - balance
    gridColumns: 1
    timeFrame: last-365-days
    columnConfigs:
      note.balance:
        - id: x
          propertyId: note.balance
          visualizationType: line-chart
  - type: life-tracker
    name: d払い
    filters:
      and:
        - file.hasTag("dpayment")
    order:
      - total_jpy
      - bandlecard_charge_jpy
    timeFrame: last-365-days
  - type: life-tracker
    name: 給与
    filters:
      and:
        - file.hasTag("salary")
    order:
      - net_pay
  - type: table
    name: 表
    columnSize:
      note.completed: -12
      file.name: 487
`

func trackerRecs() []*Record {
	rows := bankRows()
	for _, r := range rows {
		r.NPath = "Data/" + strings.TrimPrefix(r.NPath, "Data/")
	}
	d := func(ym string, total float64) *Record {
		return rec("Data/pay/"+ym+".md", []string{"dpayment"},
			map[string]Value{"year_month": Str(ym), "total_jpy": Num(total), "bandlecard_charge_jpy": Num(total)})
	}
	s := func(name, ym, issued string, pay float64) *Record {
		return rec("Data/slips/"+name+".md", []string{"salary"},
			map[string]Value{"year_month": Str(ym), "issued_on": Str(issued), "net_pay": Num(pay)})
	}
	return append(rows, d("2025-11", 4200), d("2025-12", 100),
		s("2025-06", "2025-06", "2025-06-25", 200000), s("2025-06_bonus", "2025-06", "2025-06-30", 100000))
}

func convertTracker(t *testing.T) (*Base, []string) {
	t.Helper()
	b, err := ParseBase("Payments.base", []byte(trackerBase))
	if err != nil {
		t.Fatal(err)
	}
	notes := ConvertCharts(b, trackerRecs())
	return b, Report(b, notes, ChartProblems(b, trackerRecs()))
}

func noteAbout(notes []string, view string) string {
	var out []string
	for _, n := range notes {
		if strings.HasPrefix(n, "ビュー \""+view+"\"") {
			out = append(out, n)
		}
	}
	return strings.Join(out, "\n")
}

func TestConvertMovesTheTrackerIntoTheDefinition(t *testing.T) {
	b, notes := convertTracker(t)
	v := &b.Views[0]
	if !reflect.DeepEqual(v.Time, &TimeSpec{Axis: "date", Bucket: "day"}) {
		t.Fatalf("時間軸を決めていない: %+v", v.Time)
	}
	want := &Emit{Human: []Encoding{{Kind: "chart", Values: []string{"balance"}, Chart: "line", Window: "last-365-days"}}}
	if !reflect.DeepEqual(v.Emit, want) || v.Measures["balance"] != "only" {
		t.Fatalf("emit=%+v measures=%v", v.Emit, v.Measures)
	}
	// 移した鍵は「持ち越さなかった」と言わない。移せない gridColumns は言う。
	if _, ok := v.Extra["columnConfigs"]; ok {
		t.Error("columnConfigs が Extra に残っている")
	}
	if d := DroppedKeys(b); !reflect.DeepEqual(d, []string{"gridColumns"}) {
		t.Errorf("持ち越さなかった鍵: %v", d)
	}
	// **同じ日に複数行ある残高は `only` のままでは描けない。変換器は黙らずに言う。**
	if n := noteAbout(notes, "残高"); !strings.Contains(n, "2025-12-27 に 3 行") {
		t.Fatalf("混んだ日を報告していない: %q", n)
	}
}

// Obsidian では `columnConfigs` が無くて何も描かれなかった d払いも、`order` の列で描ける。
func TestConvertDrawsTheTrackerObsidianLeftEmpty(t *testing.T) {
	b, notes := convertTracker(t)
	v := &b.Views[1]
	if v.Time == nil || v.Time.Axis != "year_month" || v.Time.Bucket != "month" {
		t.Fatalf("%+v", v.Time)
	}
	if !reflect.DeepEqual(v.Emit.Human[0].Values, []string{"total_jpy", "bandlecard_charge_jpy"}) {
		t.Fatalf("%+v", v.Emit)
	}
	if n := noteAbout(notes, "d払い"); n != "" {
		t.Fatalf("決められるのに報告した: %s", n)
	}
	res, err := Run(b, v, trackerRecs())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Series) != 2 || res.Series[0].Error != "" || len(res.Series[0].Points) != 2 {
		t.Fatalf("描けない: %+v", res.Series)
	}
}

// 時間軸の候補が2つ（`year_month` と `issued_on`）なら決めない。
func TestConvertDoesNotPickBetweenTwoTimeAxes(t *testing.T) {
	b, notes := convertTracker(t)
	v := &b.Views[2]
	if v.Time != nil {
		t.Fatalf("候補が2つなのに決めた: %+v", v.Time)
	}
	if n := noteAbout(notes, "給与"); !strings.Contains(n, "issued_on・year_month") {
		t.Fatalf("候補を挙げていない: %q", n)
	}
}

func TestConvertKeepsColumnWidths(t *testing.T) {
	b, notes := convertTracker(t)
	v := &b.Views[3]
	if v.Emit == nil || !reflect.DeepEqual(v.Emit.Human, []Encoding{{Kind: "table", Widths: map[string]int{"file.name": 487}}}) {
		t.Fatalf("%+v", v.Emit)
	}
	if n := noteAbout(notes, "表"); !strings.Contains(n, "completed") {
		t.Fatalf("負の列幅を黙って捨てた: %q", n)
	}
	if v.Time != nil {
		t.Fatal("表に時間軸を付けた")
	}
}

// 変換した定義は書き出して読み直せる（時系列と描き方が落ちない）。
func TestConvertedChartsSurviveToNative(t *testing.T) {
	b, _ := convertTracker(t)
	body, err := ToNative(b)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseNative(b.Name, body)
	if err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	if d := DiffBases(b, got); len(d) > 0 {
		t.Fatalf("%v", d)
	}
	for i := range b.Views {
		x, y := &b.Views[i], &got.Views[i]
		if !reflect.DeepEqual(x.Time, y.Time) || !reflect.DeepEqual(x.Measures, y.Measures) ||
			!reflect.DeepEqual(x.Emit, y.Emit) {
			t.Fatalf("%s が往復で変わった:\n%s", x.Name, body)
		}
	}
}

// **再変換は `.base` で言えない部分を今の定義から引き継ぐ**（本人の決定、2026-09-13。Fable の指摘1）。
// 本番では銀行の last＋within・給与の合計・Homelab の構成図を画面で書く。以前は再変換がそれを消した。
func TestReconversionCarriesOverWhatTheBaseCannotSay(t *testing.T) {
	b, _ := convertTracker(t)
	cur, err := ParseNative(b.Name, []byte(`base: Payments
views:
  - name: 残高
    kind: chart
    shape:
      time: {axis: date, bucket: day, within: [{property: file.name, direction: DESC}]}
      measures: {balance: last}
    emit:
      human:
        - {kind: chart, values: [balance], chart: line}
  - name: 構成図
    kind: graph
`))
	if err != nil {
		t.Fatal(err)
	}
	CarryOver(b, cur)
	v := findView(b, "残高")
	if v.Measures["balance"] != "last" || v.Time == nil || len(v.Time.Within) != 1 {
		t.Fatalf("時間軸と集約を引き継いでいない: time=%+v measures=%v", v.Time, v.Measures)
	}
	// 描き方は .base から（window は .base の timeFrame のまま）。
	if v.Emit.Human[0].Window != "last-365-days" {
		t.Fatalf("描き方を .base から作っていない: %+v", v.Emit)
	}
	if g := findView(b, "構成図"); g == nil || g.Kind != KindGraph {
		t.Fatalf("構成図のビューが消えた: %+v", b.Views)
	}
	// 引き継いだあとは描ける（同じ日に複数行あっても within で決まる）。
	if p := ChartProblems(b, trackerRecs()); len(Report(b, p)) != 0 {
		for _, n := range Report(b, p) {
			if strings.HasPrefix(n, "ビュー \"残高\"") {
				t.Fatalf("引き継いだのに描けない: %s", n)
			}
		}
	}
	// 書き出して読み直せる（引き継いだビューも形式の検査を通る）。
	body, err := ToNative(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseNative(b.Name, body); err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
}
