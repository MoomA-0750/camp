package views

import "testing"

// fake は式から見える1レコード。
type fake struct {
	props  map[string]Value
	tags   []string
	folder string
	ext    string
	name   string
}

func (f fake) Get(k string) Value {
	if v, ok := f.props[k]; ok {
		return v
	}
	return Null()
}
func (f fake) HasTag(t string) bool {
	for _, x := range f.tags {
		if x == t {
			return true
		}
	}
	return false
}
func (f fake) Folder() string { return f.folder }
func (f fake) Ext() string    { return f.ext }
func (f fake) Name() string   { return f.name }
func (f fake) Path() string   { return f.folder + "/" + f.name + "." + f.ext }

func evalT(t *testing.T, src string, r Row) Value {
	t.Helper()
	v, err := Eval(src, r)
	if err != nil {
		t.Fatalf("%q: %v", src, err)
	}
	return v
}

// 実在する30ビューの filters に現れる18種類の式が全部動く。
// ここが1つでも欠けると、そのビューが黙って空になる。
func TestEveryRealFilterExpression(t *testing.T) {
	bank := fake{
		props:  map[string]Value{"withdrawal": Num(1200), "deposit": Null(), "status": Str("未確定")},
		tags:   []string{"bank"},
		folder: "Data/Bank", ext: "md", name: "2026-01-05_01",
	}
	note := fake{folder: "Data/Notes", ext: "md", name: "x"}
	income := fake{props: map[string]Value{"type": Str("salary-data")},
		folder: "Data/Income", ext: "md", name: "2026-01"}
	paypay := fake{props: map[string]Value{"transaction_type": Str("Payment")},
		tags: []string{"paypay"}, folder: "Data/PayPay", ext: "md", name: "p"}

	cases := []struct {
		src  string
		row  Row
		want bool
	}{
		{`deposit != null`, bank, false},
		{`withdrawal != null`, bank, true},
		{`file.ext == "md"`, bank, true},
		{`file.folder == "Data/Notes"`, note, true},
		{`file.folder == "Data/Todo"`, note, false},
		{`file.hasTag("bank")`, bank, true},
		{`file.hasTag("card")`, bank, false},
		{`file.hasTag("dpayment")`, bank, false},
		{`file.hasTag("paypay")`, paypay, true},
		{`file.inFolder("Data/Income")`, income, true},
		{`installment_total != null`, bank, false},
		{`status == "未確定"`, bank, true},
		{`transaction_type == "Money Received"`, paypay, false},
		{`transaction_type == "Payment"`, paypay, true},
		{`type == "bonus-data"`, income, false},
		{`type == "salary-data"`, income, true},
	}
	for _, c := range cases {
		got := evalT(t, c.src, c.row)
		if got.Truthy() != c.want {
			t.Errorf("%q → %v, want %v", c.src, got.Truthy(), c.want)
		}
	}
}

// Payments.base の「金額」formula は if の4段入れ子。
// 正規表現で当てにいくと必ず壊れる形。
func TestNestedIfFromPaymentsBase(t *testing.T) {
	const amount = `if(file.hasTag("bank"), if(withdrawal, "-" + withdrawal.toString(), ` +
		`if(deposit, "+" + deposit.toString(), "")), if(file.hasTag("card"), ` +
		`"-" + amount.toString(), if(file.hasTag("paypay"), if(amount_out, ` +
		`"-" + amount_out.toString(), if(amount_in, "+" + amount_in.toString(), "")), ` +
		`if(file.hasTag("dpayment"), total_jpy.toString(), ""))))`

	for _, c := range []struct {
		name string
		row  fake
		want string
	}{
		{"銀行の出金", fake{tags: []string{"bank"}, props: map[string]Value{"withdrawal": Num(1200)}}, "-1200"},
		{"銀行の入金", fake{tags: []string{"bank"}, props: map[string]Value{"deposit": Num(300000)}}, "+300000"},
		{"カード", fake{tags: []string{"card"}, props: map[string]Value{"amount": Num(4980)}}, "-4980"},
		{"PayPay出金", fake{tags: []string{"paypay"}, props: map[string]Value{"amount_out": Num(500)}}, "-500"},
		{"PayPay入金", fake{tags: []string{"paypay"}, props: map[string]Value{"amount_in": Num(1000)}}, "+1000"},
		{"d払い", fake{tags: []string{"dpayment"}, props: map[string]Value{"total_jpy": Num(7700)}}, "7700"},
		{"どれでもない", fake{}, ""},
	} {
		got := evalT(t, amount, c.row)
		if got.Str != c.want {
			t.Errorf("%s: %q, want %q", c.name, got.Str, c.want)
		}
	}
}

// 「種別」formula。
func TestKindFormula(t *testing.T) {
	const kind = `if(file.hasTag("bank"), "銀行", if(file.hasTag("card"), "カード", ` +
		`if(file.hasTag("paypay"), "PayPay", if(file.hasTag("dpayment"), "d払い", ""))))`
	for tag, want := range map[string]string{
		"bank": "銀行", "card": "カード", "paypay": "PayPay", "dpayment": "d払い",
	} {
		if got := evalT(t, kind, fake{tags: []string{tag}}); got.Str != want {
			t.Errorf("%s → %q, want %q", tag, got.Str, want)
		}
	}
}

// 「日付」formula は値のあるほうを採る。
func TestDateFallbackFormula(t *testing.T) {
	const d = `if(date, date, if(year_month, year_month, ""))`
	if got := evalT(t, d, fake{props: map[string]Value{"date": Str("2026-01-05")}}); got.Str != "2026-01-05" {
		t.Errorf("date 優先が効かない: %q", got.Str)
	}
	if got := evalT(t, d, fake{props: map[string]Value{"year_month": Str("2026-01")}}); got.Str != "2026-01" {
		t.Errorf("year_month への落ちが効かない: %q", got.Str)
	}
	if got := evalT(t, d, fake{}); got.Str != "" {
		t.Errorf("どちらも無いとき: %q", got.Str)
	}
}

// 数は文字列として比べない。"10" < "9" になると並べ替えが壊れる。
func TestNumbersCompareAsNumbers(t *testing.T) {
	r := fake{props: map[string]Value{"n": Num(10)}}
	if !evalT(t, `n > 9`, r).Truthy() {
		t.Error("10 > 9 が偽になっている")
	}
}

// null は「値が無い」であって空文字ではない。
func TestNullIsNotEmptyString(t *testing.T) {
	r := fake{props: map[string]Value{"empty": Str("")}}
	if evalT(t, `missing != null`, r).Truthy() {
		t.Error("無いプロパティが != null になっている")
	}
	if !evalT(t, `empty != null`, r).Truthy() {
		t.Error("空文字は値としては存在するはず")
	}
}

// 知らない関数は黙って偽にせず、エラーにする。
// 黙って偽にすると、ビューが空なのが「該当なし」なのか「読めない式」なのか
// 区別できなくなる。
func TestUnknownFunctionIsAnError(t *testing.T) {
	if _, err := Eval(`someUnknownFn("x")`, fake{}); err == nil {
		t.Fatal("知らない関数を黙って通している")
	}
}

// 連結の右側が読めないときは、左側だけ返して黙らない。
// 黙ると、読めない式が「途中まで読めた値」に化けて、後段の無関係な場所で落ちる
// （実際に「引数の区切りが読めない」という嘘のエラーになった）。
func TestConcatDoesNotSwallowErrors(t *testing.T) {
	v, err := Eval(`"x" + someUnknownFn()`, fake{})
	if err == nil {
		t.Fatalf("エラーにならず %q を返した", v.Str)
	}
}

// Bases 側で普通に書ける構文が、Camp だけ読めない状態にしない。
// filter で落ちるとビュー全体が error になるので、ここは広く受ける。
func TestExprAcceptsOrdinarySyntax(t *testing.T) {
	r := rec("Data/A/x.md", []string{"bank"}, map[string]Value{
		"amount": Num(10), "price": Num(5), "name": Str("あ"),
	})
	for _, c := range []struct {
		src  string
		want string
	}{
		{`amount > -100`, "true"},
		{`price * 2`, "10"},
		{`amount - price`, "5"},
		{`amount / price`, "2"},
		{`amount / 0`, ""}, // 0除算は null。行ごと落とさない
		{`-price + amount`, "5"},
		{`file.hasTag("bank") && amount > 5`, "true"},
		{`file.hasTag("x") || amount > 5`, "true"},
		{`file.hasTag("x") and amount > 5`, "false"},
		{`file.hasTag("x") or amount > 500`, "false"},
		{`"a\"b"`, `a"b`},
		{`name + "い"`, "あい"},
	} {
		v, err := Eval(c.src, r)
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		if v.Str != c.want {
			t.Errorf("%s → %q（%q のはず）", c.src, v.Str, c.want)
		}
	}
}

// **if は選ばれた枝だけ評価する。** 通らない枝に知らない関数が
// 1つあるだけで式全体が落ちるのは、4段入れ子の Payments で効いてくる。
func TestIfIsLazy(t *testing.T) {
	r := rec("Data/A/x.md", []string{"bank"}, map[string]Value{"w": Num(3)})
	v, err := Eval(`if(file.hasTag("bank"), "銀行", いない関数(1, 2))`, r)
	if err != nil {
		t.Fatalf("通らない枝を評価している: %v", err)
	}
	if v.Str != "銀行" {
		t.Errorf("%q", v.Str)
	}
	// 逆側も同じ。
	v2, err := Eval(`if(file.hasTag("card"), いない関数(), "その他")`, r)
	if err != nil {
		t.Fatalf("通らない枝を評価している: %v", err)
	}
	if v2.Str != "その他" {
		t.Errorf("%q", v2.Str)
	}
}
