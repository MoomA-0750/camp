package views

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 独自定義の往復（Phase 4 / M50、2026-09-13）。
//
// **これが M50 の中心的な正しさ。** `.base` を書き出して読み直したとき、解釈する項目が
// 1つも変わらないこと。変わるなら変換器が静かに何かを失っている。
//
// 比べるのは `DiffBases`（`Extra` を見ない。`.base` の `Extra` は Obsidian のUI設定で、
// 独自定義に対応物が無いのが正しい）。

// realBases は実在の `.base` を全部読む。無い環境では飛ばす。
func realBases(t *testing.T) []*Base {
	t.Helper()
	root := realVault(t)
	var out []*Base
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if !strings.HasSuffix(p, ".base") {
			return nil
		}
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		b, err := ParseBase(p, body)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		out = append(out, b)
		return nil
	})
	if len(out) != 7 {
		t.Fatalf(".base は7つのはずが %d", len(out))
	}
	return out
}

// 実在の7本すべてで、書き出して読み直しても解釈する項目が変わらない。
func TestEveryRealBaseSurvivesTheRoundTrip(t *testing.T) {
	for _, b := range realBases(t) {
		body, err := ToNative(b)
		if err != nil {
			t.Fatalf("%s: 書き出せない: %v", b.Name, err)
		}
		got, err := ParseNative(b.Name, body)
		if err != nil {
			t.Fatalf("%s: 読み直せない: %v\n%s", b.Name, err, body)
		}
		if d := DiffBases(b, got); len(d) > 0 {
			t.Fatalf("%s: 往復で変わった:\n  %s\n--- 書き出し ---\n%s",
				b.Name, strings.Join(d, "\n  "), body)
		}
	}
}

// **差を見逃さない。** 突き合わせが本当に効いているかを、わざと壊して確かめる。
//
// 初稿の受け入れ条件（行数・列数・グループ数・集計）だと、ここで作る壊し方はどれも
// 「一致」と報告された（設計レビューの指摘1）。
func TestTheDiffCatchesWhatTheRowCountsCannot(t *testing.T) {
	base := func() *Base {
		return &Base{Name: "T", Filters: &Filter{Expr: `file.inFolder("X")`},
			Formulas: map[string]string{"f": "1"},
			Display:  map[string]string{"a": "A"},
			Views: []View{{Kind: KindTable, Name: "v",
				Order: []string{"a", "b"}, Hide: []string{"z"},
				Sort:      []SortKey{{Property: "a", Direction: "DESC"}},
				GroupBy:   &SortKey{Property: "g", Direction: "DESC"},
				Summaries: map[string]string{"a": "Sum"},
				Filters:   &Filter{And: []*Filter{{Expr: `t == "x"`}}}}}}
	}
	for _, c := range []struct {
		name   string
		break_ func(*Base)
	}{
		{"sort の向きを変える", func(b *Base) { b.Views[0].Sort[0].Direction = "ASC" }},
		{"group_by の向きを落とす", func(b *Base) { b.Views[0].GroupBy.Direction = "" }},
		{"order の列を落とす", func(b *Base) { b.Views[0].Order = []string{"a"} }},
		{"order の並びを変える", func(b *Base) { b.Views[0].Order = []string{"b", "a"} }},
		{"hide を落とす", func(b *Base) { b.Views[0].Hide = nil }},
		{"集計を落とす", func(b *Base) { b.Views[0].Summaries = nil }},
		{"絞り込みを変える", func(b *Base) { b.Views[0].Filters = &Filter{Expr: `t == "y"`} }},
		{"計算列を落とす", func(b *Base) { b.Formulas = nil }},
		{"表示名を落とす", func(b *Base) { b.Display = nil }},
		{"ビューを消す", func(b *Base) { b.Views = nil }},
		{"種別を変える", func(b *Base) { b.Views[0].Kind = KindCards }},
	} {
		a, z := base(), base()
		c.break_(z)
		if d := DiffBases(a, z); len(d) == 0 {
			t.Errorf("%s: 差として出ない（突き合わせが弱い）", c.name)
		}
	}
	// **同じものは差として出さない**（騒がしいと読まれなくなる）。
	if d := DiffBases(base(), base()); len(d) > 0 {
		t.Fatalf("同じ台紙に差が出た: %v", d)
	}
	// 空と ASC は同じ（Obsidian も空なら昇順）。意味の無い差を出さない。
	a, z := base(), base()
	a.Views[0].Sort[0].Direction = ""
	z.Views[0].Sort[0].Direction = "ASC"
	if d := DiffBases(a, z); len(d) > 0 {
		t.Fatalf("空と ASC を別物として扱った: %v", d)
	}
}

// **書き出しは人が上から読める順**（2026-09-13、実ブラウザで見つけた）。
// 写像のまま書くとアルファベット順になり、`derive` が `source` より先、`kind` が `name` より先に来た。
func TestTheWrittenDefinitionReadsTopDown(t *testing.T) {
	b := &Base{Name: "T", Filters: &Filter{Expr: `file.inFolder("X")`},
		Formulas: map[string]string{"f": "1"},
		Views: []View{{Kind: KindTable, Name: "v", Order: []string{"a"},
			Sort: []SortKey{{Property: "a", Direction: "DESC"}}}}}
	body, err := ToNative(b)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	at := func(k string) int {
		i := strings.Index(s, k)
		if i < 0 {
			t.Fatalf("%q が無い:\n%s", k, s)
		}
		return i
	}
	if !(at("base:") < at("source:") && at("source:") < at("derive:") && at("derive:") < at("views:")) {
		t.Fatalf("台紙の欄が base → source → derive → views の順でない:\n%s", s)
	}
	if !(at("name: v") < at("kind: table")) {
		t.Fatalf("ビューの name が kind より後:\n%s", s)
	}
	if !(at("order:") < at("sort:")) {
		t.Fatalf("shape の欄の順が崩れている:\n%s", s)
	}
}

// id を変えない（`base/name` が画面の URL とモデルのハンドル）。
func TestNativeRefusesNamesThatWouldBreakTheID(t *testing.T) {
	if _, err := ParseNative("A/B", []byte("base: A/B\nviews: []\n")); err == nil {
		t.Fatal("台紙の名前に / を通した")
	}
	if _, err := ParseNative("T", []byte("base: T\nviews:\n  - name: a/b\n")); err == nil {
		t.Fatal("ビュー名に / を通した")
	}
	// 台紙の行の名前と `base:` が食い違うものは弾く（取り違えて別の台紙を上書きしないため）。
	if _, err := ParseNative("T", []byte("base: U\nviews: []\n")); err == nil {
		t.Fatal("名前の食い違いを通した")
	}
}
