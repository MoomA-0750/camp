package vault

import "testing"

func propMap(t *testing.T, src string) map[string][]Prop {
	t.Helper()
	out := map[string][]Prop{}
	for _, p := range ExtractProps([]byte(src)) {
		out[p.Key] = append(out[p.Key], p)
	}
	return out
}

// **seq は畳んだ後の通し番号。**
//
// 子の添字を外側の添字で上書きしていたころは、入れ子の配列が
// 同じ (note_id,key,seq) を2行作り、note_props の主キーに当たって
// Vault索引のトランザクション全体がロールバックしていた。
// 1ノートの形が変わっただけで索引が止まる、という壊れ方だった。
func TestNestedListSeqIsUnique(t *testing.T) {
	got := propMap(t, "---\nk:\n  - [a, b]\n  - [c, d]\n---\n本文\n")["k"]
	if len(got) != 4 {
		t.Fatalf("%d件（4件のはず）", len(got))
	}
	seen := map[int]bool{}
	for _, p := range got {
		if seen[p.Seq] {
			t.Fatalf("seq %d が重複している（主キーに当たる）", p.Seq)
		}
		seen[p.Seq] = true
	}
}

// YAMLが数として書いたものだけを数にする。
// 引用符付きのゼロ埋めIDや真偽値が数値ソートと Sum に混ざらないこと。
func TestOnlyRealNumbersBecomeNum(t *testing.T) {
	m := propMap(t, "---\nn: 9980\nf: 1.5\ns: \"007\"\nb: true\n---\nx\n")
	for _, k := range []string{"n", "f"} {
		if m[k][0].Num == nil {
			t.Errorf("%s が数になっていない", k)
		}
	}
	for _, k := range []string{"s", "b"} {
		if m[k][0].Num != nil {
			t.Errorf("%s（%q）を数として扱っている", k, m[k][0].Text)
		}
	}
	if m["s"][0].Text != "007" {
		t.Errorf("ゼロ埋めが落ちている: %q", m["s"][0].Text)
	}
}

// **引用符の無い日付は書かれたとおりに戻す。**
//
// YAML は `date: 2026-08-19` を時刻として解決するので、素直に文字列化すると
// `2026-08-19T00:00:00Z` になる。実DBでは Health の2,687件すべてが
// この形で入っていて、Obsidian が見せている値と違っていた。
func TestBareDateKeepsItsShape(t *testing.T) {
	m := propMap(t, "---\nd: 2026-08-19\nq: \"2026-08-19\"\nts: 2026-08-19 10:30:00\n---\nx\n")
	if m["d"][0].Text != "2026-08-19" {
		t.Errorf("引用符なしの日付が %q になっている", m["d"][0].Text)
	}
	if m["q"][0].Text != "2026-08-19" {
		t.Errorf("引用符ありの日付が %q になっている", m["q"][0].Text)
	}
	if m["ts"][0].Text == "2026-08-19" {
		t.Error("時刻まで落としている")
	}
}
