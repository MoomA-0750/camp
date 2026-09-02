package views

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// この言語は小さい。実測で、実在する30ビューの filters に現れる式は18種類しかなく、
// 全部が次のどれか:
//
//	プロパティ == "定数"    プロパティ != null
//	file.hasTag("x")        file.inFolder("x")
//	file.folder == "x"      file.ext == "md"
//
// formulas は `if(cond, a, b)` の入れ子と文字列連結と `.toString()` だけ。
// JOIN もウィンドウ関数も無い（Bases に無い）。だから本物のパーサを書く。
// 正規表現で当てにいくと、入れ子の if で破綻する。

// Value は式の値。null は Null で表す（Go の nil と区別する必要があるため）。
type Value struct {
	Null   bool
	Str    string
	Num    float64
	IsNum  bool
	Bool   bool
	IsBool bool
}

func Str(s string) Value  { return Value{Str: s} }
func Num(f float64) Value { return Value{Num: f, IsNum: true, Str: formatNum(f)} }
func Bool(b bool) Value   { return Value{Bool: b, IsBool: true, Str: boolStr(b)} }
func Null() Value         { return Value{Null: true} }

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func formatNum(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// Truthy は if() の条件として読む。null と空文字と 0 と false が偽。
func (v Value) Truthy() bool {
	switch {
	case v.Null:
		return false
	case v.IsBool:
		return v.Bool
	case v.IsNum:
		return v.Num != 0
	}
	return v.Str != ""
}

// Row は1レコード。式から見える世界はこれだけ。
type Row interface {
	// Get はプロパティを引く。無ければ Null。
	Get(key string) Value
	// HasTag は file.hasTag()。
	HasTag(tag string) bool
	// Folder / Ext / Name / Path は file.* を返す。
	Folder() string
	Ext() string
	Name() string
	Path() string
}

// Eval は式を1行に対して評価する。
func Eval(src string, r Row) (Value, error) {
	p := &parser{src: src}
	v, err := p.or(r)
	if err != nil {
		return Null(), err
	}
	p.ws()
	if p.i < len(p.src) {
		return Null(), fmt.Errorf("%q: %d文字目以降を読めない", src, p.i)
	}
	return v, nil
}

type parser struct {
	src string
	i   int
}

func (p *parser) ws() {
	for p.i < len(p.src) && (p.src[p.i] == ' ' || p.src[p.i] == '\t' || p.src[p.i] == '\n') {
		p.i++
	}
}

func (p *parser) accept(s string) bool {
	p.ws()
	if strings.HasPrefix(p.src[p.i:], s) {
		p.i += len(s)
		return true
	}
	return false
}

// acceptOp は演算子。**より長い演算子の一部を食わないこと。**
// `+` を取るときに `+=` を、`|` を取るときに `||` を食うと後段が壊れる。
func (p *parser) acceptOp(op string) bool {
	p.ws()
	rest := p.src[p.i:]
	if !strings.HasPrefix(rest, op) {
		return false
	}
	if next := rest[len(op):]; next != "" && strings.ContainsAny(next[:1], "=&|") {
		switch op + next[:1] {
		case "==", "!=", ">=", "<=", "&&", "||":
			return false // これから読むべきもっと長い演算子
		}
	}
	p.i += len(op)
	return true
}

// acceptWord は `and` / `or` のような語の演算子。識別子の頭を食わないよう、
// 直後が識別子の続きでないことを確かめる。
func (p *parser) acceptWord(w string) bool {
	p.ws()
	rest := p.src[p.i:]
	if !strings.HasPrefix(rest, w) {
		return false
	}
	if next := rest[len(w):]; next != "" {
		c := next[0]
		if c == '_' || c == '-' || (c >= '0' && c <= '9') ||
			(c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			return false
		}
	}
	p.i += len(w)
	return true
}

// 優先順位は低いほうから
//
//	or < and < 比較 < 加減（+ は連結も兼ねる）< 乗除 < 単項
//
// `||` と `&&` は Bases のフィルタUIでは `or:` / `and:` になるが、
// 式として手で書けてしまう。読めないと**そのビュー全体が error になる**ので、
// 素直に実装しておく。`and` / `or` の綴りも受ける。

// or は `||`。短絡する。
func (p *parser) or(r Row) (Value, error) {
	left, err := p.and(r)
	if err != nil {
		return Null(), err
	}
	for p.acceptOp("||") || p.acceptWord("or") {
		right, err := p.and(r)
		if err != nil {
			return Null(), err
		}
		left = Bool(left.Truthy() || right.Truthy())
	}
	return left, nil
}

// and は `&&`。
func (p *parser) and(r Row) (Value, error) {
	left, err := p.expr(r)
	if err != nil {
		return Null(), err
	}
	for p.acceptOp("&&") || p.acceptWord("and") {
		right, err := p.expr(r)
		if err != nil {
			return Null(), err
		}
		left = Bool(left.Truthy() && right.Truthy())
	}
	return left, nil
}

// expr は比較1段。
func (p *parser) expr(r Row) (Value, error) {
	left, err := p.additive(r)
	if err != nil {
		return Null(), err
	}
	p.ws()
	for _, op := range []string{"==", "!=", ">=", "<=", ">", "<"} {
		if p.acceptOp(op) {
			right, err := p.additive(r)
			if err != nil {
				return Null(), err
			}
			return compare(op, left, right)
		}
	}
	return left, nil
}

// additive は `+` と `-`。`+` は両方が数のときだけ足し算で、
// それ以外は連結（Bases も同じ挙動）。
func (p *parser) additive(r Row) (Value, error) {
	left, err := p.multiplicative(r)
	if err != nil {
		return Null(), err
	}
	for {
		var op string
		switch {
		case p.acceptOp("+"):
			op = "+"
		case p.acceptOp("-"):
			op = "-"
		default:
			return left, nil
		}
		// ここでエラーを握り潰してはいけない。潰すと、読めない式が
		// 「途中まで読めた値」に化けて、後段の無関係な場所で落ちる。
		right, err := p.multiplicative(r)
		if err != nil {
			return Null(), err
		}
		switch {
		case op == "+" && left.IsNum && right.IsNum:
			left = Num(left.Num + right.Num)
		case op == "+":
			left = Str(left.Str + right.Str)
		case left.IsNum && right.IsNum:
			left = Num(left.Num - right.Num)
		default:
			return Null(), fmt.Errorf("数でないものを引き算している")
		}
	}
}

// multiplicative は `*` と `/`。
func (p *parser) multiplicative(r Row) (Value, error) {
	left, err := p.unary(r)
	if err != nil {
		return Null(), err
	}
	for {
		var op string
		switch {
		case p.acceptOp("*"):
			op = "*"
		case p.acceptOp("/"):
			op = "/"
		default:
			return left, nil
		}
		right, err := p.unary(r)
		if err != nil {
			return Null(), err
		}
		if !left.IsNum || !right.IsNum {
			return Null(), fmt.Errorf("数でないものを %s している", op)
		}
		if op == "/" {
			if right.Num == 0 {
				return Null(), nil // 0除算は null。行ごと落とさない
			}
			left = Num(left.Num / right.Num)
			continue
		}
		left = Num(left.Num * right.Num)
	}
}

// identRe に `.` を入れてはいけない。入れると `withdrawal.toString` が
// 1つの識別子として食われ、postfix（メソッド呼び出し）が一生走らない。
// `file.hasTag` のような素性だけは、`file` を読んだ直後に明示的に繋ぐ。
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*`)

func (p *parser) unary(r Row) (Value, error) {
	p.ws()
	if p.i >= len(p.src) {
		return Null(), fmt.Errorf("式が途中で終わっている")
	}
	if p.accept("!") {
		v, err := p.unary(r)
		if err != nil {
			return Null(), err
		}
		return Bool(!v.Truthy()), nil
	}
	// 負の数と単項プラス。`amount > -100` が読めないと式ごと落ちる。
	if p.accept("-") {
		v, err := p.unary(r)
		if err != nil {
			return Null(), err
		}
		if !v.IsNum {
			return Null(), fmt.Errorf("数でないものに単項マイナスを付けている")
		}
		return Num(-v.Num), nil
	}
	if p.accept("+") {
		return p.unary(r)
	}
	if p.accept("(") {
		v, err := p.or(r)
		if err != nil {
			return Null(), err
		}
		if !p.accept(")") {
			return Null(), fmt.Errorf("閉じ括弧が無い")
		}
		return p.postfix(v, r)
	}
	// 文字列。`\"` と `\\` は畳む。
	if c := p.src[p.i]; c == '"' || c == '\'' {
		p.i++
		var b strings.Builder
		for p.i < len(p.src) && p.src[p.i] != c {
			if p.src[p.i] == '\\' && p.i+1 < len(p.src) {
				p.i++
				switch p.src[p.i] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				default:
					b.WriteByte(p.src[p.i])
				}
				p.i++
				continue
			}
			b.WriteByte(p.src[p.i])
			p.i++
		}
		if p.i >= len(p.src) {
			return Null(), fmt.Errorf("閉じていない文字列")
		}
		p.i++
		return p.postfix(Str(b.String()), r)
	}
	// 数
	if p.src[p.i] >= '0' && p.src[p.i] <= '9' {
		start := p.i
		for p.i < len(p.src) && (p.src[p.i] == '.' || (p.src[p.i] >= '0' && p.src[p.i] <= '9')) {
			p.i++
		}
		f, err := strconv.ParseFloat(p.src[start:p.i], 64)
		if err != nil {
			return Null(), err
		}
		return p.postfix(Num(f), r)
	}
	// 識別子・関数呼び出し
	m := identRe.FindString(p.src[p.i:])
	if m == "" {
		return Null(), fmt.Errorf("%d文字目 %q を読めない", p.i, p.src[p.i:min(p.i+8, len(p.src))])
	}
	p.i += len(m)
	// `file.hasTag` `file.folder` のような素性は1つの名前として扱う。
	if m == "file" && p.i < len(p.src) && p.src[p.i] == '.' {
		if sub := identRe.FindString(p.src[p.i+1:]); sub != "" {
			p.i += 1 + len(sub)
			m = "file." + sub
		}
	}
	p.ws()
	if p.i < len(p.src) && p.src[p.i] == '(' {
		p.i++
		// **`if` は選ばれた枝だけ評価する。** 全部先に評価すると、
		// 通らない枝に知らない関数が1つあるだけで式全体が落ちる。
		// Payments の「金額」は if の4段入れ子なので、ここは効く。
		if m == "if" {
			v, err := p.callIf(r)
			if err != nil {
				return Null(), err
			}
			return p.postfix(v, r)
		}
		args, err := p.args(r)
		if err != nil {
			return Null(), err
		}
		v, err := call(m, args, r)
		if err != nil {
			return Null(), err
		}
		return p.postfix(v, r)
	}
	switch m {
	case "null":
		return Null(), nil
	case "true":
		return Bool(true), nil
	case "false":
		return Bool(false), nil
	}
	return p.postfix(resolve(m, r), r)
}

// postfix は `.toString()` のようなメソッド呼び出しを処理する。
func (p *parser) postfix(v Value, r Row) (Value, error) {
	for {
		save := p.i
		if !p.accept(".") {
			return v, nil
		}
		m := identRe.FindString(p.src[p.i:])
		if m == "" {
			p.i = save
			return v, nil
		}
		p.i += len(m)
		if !p.accept("(") {
			p.i = save
			return v, nil
		}
		args, err := p.args(r)
		if err != nil {
			return Null(), err
		}
		switch m {
		case "toString":
			v = Str(v.Str)
		case "contains":
			if len(args) != 1 {
				return Null(), fmt.Errorf("contains は引数1つ")
			}
			v = Bool(strings.Contains(v.Str, args[0].Str))
		case "startsWith":
			if len(args) != 1 {
				return Null(), fmt.Errorf("startsWith は引数1つ")
			}
			v = Bool(strings.HasPrefix(v.Str, args[0].Str))
		case "isEmpty":
			v = Bool(v.Null || v.Str == "")
		default:
			return Null(), fmt.Errorf("知らないメソッド .%s()", m)
		}
	}
}

// callIf は `if(cond, a, b)` を短絡で読む。**選ばれなかった枝は
// 「読み飛ばす」だけで評価しない。**
func (p *parser) callIf(r Row) (Value, error) {
	cond, err := p.or(r)
	if err != nil {
		return Null(), err
	}
	if !p.accept(",") {
		return Null(), fmt.Errorf("if は引数2つ以上")
	}
	taken := cond.Truthy()
	var out Value
	if taken {
		if out, err = p.or(r); err != nil {
			return Null(), err
		}
	} else if err := p.skipArg(); err != nil {
		return Null(), err
	}
	if p.accept(")") {
		if taken {
			return out, nil
		}
		return Null(), nil
	}
	if !p.accept(",") {
		return Null(), fmt.Errorf("引数の区切りが読めない")
	}
	if taken {
		if err := p.skipArg(); err != nil {
			return Null(), err
		}
	} else if out, err = p.or(r); err != nil {
		return Null(), err
	}
	if !p.accept(")") {
		return Null(), fmt.Errorf("引数の区切りが読めない")
	}
	return out, nil
}

// skipArg は評価せずに引数1つぶんを読み飛ばす。括弧と文字列の中の
// カンマは区切りではない。
func (p *parser) skipArg() error {
	depth := 0
	for p.i < len(p.src) {
		c := p.src[p.i]
		switch {
		case c == '"' || c == '\'':
			q := c
			p.i++
			for p.i < len(p.src) && p.src[p.i] != q {
				if p.src[p.i] == '\\' {
					p.i++
				}
				p.i++
			}
			if p.i >= len(p.src) {
				return fmt.Errorf("閉じていない文字列")
			}
		case c == '(':
			depth++
		case c == ')':
			if depth == 0 {
				return nil
			}
			depth--
		case c == ',' && depth == 0:
			return nil
		}
		p.i++
	}
	return fmt.Errorf("引数が途中で終わっている")
}

func (p *parser) args(r Row) ([]Value, error) {
	var out []Value
	p.ws()
	if p.accept(")") {
		return out, nil
	}
	for {
		v, err := p.or(r)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		if p.accept(",") {
			continue
		}
		if p.accept(")") {
			return out, nil
		}
		return nil, fmt.Errorf("引数の区切りが読めない")
	}
}

func compare(op string, a, b Value) (Value, error) {
	// null との比較は「値があるか」を見る。Bases も同じ。
	if a.Null || b.Null {
		switch op {
		case "==":
			return Bool(a.Null && b.Null), nil
		case "!=":
			return Bool(a.Null != b.Null), nil
		}
		return Bool(false), nil
	}
	if a.IsNum && b.IsNum {
		switch op {
		case "==":
			return Bool(a.Num == b.Num), nil
		case "!=":
			return Bool(a.Num != b.Num), nil
		case ">":
			return Bool(a.Num > b.Num), nil
		case "<":
			return Bool(a.Num < b.Num), nil
		case ">=":
			return Bool(a.Num >= b.Num), nil
		case "<=":
			return Bool(a.Num <= b.Num), nil
		}
	}
	switch op {
	case "==":
		return Bool(a.Str == b.Str), nil
	case "!=":
		return Bool(a.Str != b.Str), nil
	case ">":
		return Bool(a.Str > b.Str), nil
	case "<":
		return Bool(a.Str < b.Str), nil
	case ">=":
		return Bool(a.Str >= b.Str), nil
	case "<=":
		return Bool(a.Str <= b.Str), nil
	}
	return Null(), fmt.Errorf("知らない演算子 %q", op)
}

func call(name string, args []Value, r Row) (Value, error) {
	switch name {
	case "if": // 通常は callIf が短絡で処理する。ここは保険
		if len(args) < 2 {
			return Null(), fmt.Errorf("if は引数2つ以上")
		}
		if args[0].Truthy() {
			return args[1], nil
		}
		if len(args) >= 3 {
			return args[2], nil
		}
		return Null(), nil
	case "file.hasTag":
		if len(args) != 1 {
			return Null(), fmt.Errorf("file.hasTag は引数1つ")
		}
		return Bool(r.HasTag(args[0].Str)), nil
	case "file.inFolder":
		if len(args) != 1 {
			return Null(), fmt.Errorf("file.inFolder は引数1つ")
		}
		f := args[0].Str
		return Bool(r.Folder() == f || strings.HasPrefix(r.Folder(), f+"/")), nil
	case "number":
		if len(args) != 1 {
			return Null(), fmt.Errorf("number は引数1つ")
		}
		if n, err := strconv.ParseFloat(strings.TrimSpace(args[0].Str), 64); err == nil {
			return Num(n), nil
		}
		return Null(), nil
	}
	return Null(), fmt.Errorf("知らない関数 %s()", name)
}

// resolve は識別子を値にする。`file.*` はレコードの素性、それ以外は
// frontmatter のプロパティ。
func resolve(name string, r Row) Value {
	switch name {
	case "file.folder":
		return Str(r.Folder())
	case "file.ext":
		return Str(r.Ext())
	case "file.name":
		return Str(r.Name())
	case "file.path":
		return Str(r.Path())
	case "file.mtime":
		if m, ok := r.(interface{ Mtime() string }); ok {
			return Str(m.Mtime())
		}
		return Null()
	}
	return r.Get(name)
}
