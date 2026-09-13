package views

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// グラフ（M52、2026-09-13）。形は実データを写した: ハブは出ていくリンクだけを持ち、
// **被リンクは 0 本**。だから「ハブから辿って戻れる」は、行った先の被リンクにハブがいること。

func graphRecs(paths ...string) []*Record {
	out := make([]*Record, 0, len(paths))
	for i, p := range paths {
		r := rec(p, nil, map[string]Value{})
		r.NoteID = int64(i + 1)
		out = append(out, r)
	}
	return out
}

// 1 index → 2 lab, 1 → 3 work, 2 → 4 server, 5 app → 2, 6 island-a ↔ 7 island-b, 8 lonely（リンク無し）
func sample() ([]*Record, []Link) {
	recs := graphRecs("hub/index.md", "ctx/lab.md", "ctx/work.md",
		"lab/server.md", "proj/app.md", "Inbox/a.md", "Inbox/b.md", "Inbox/lonely.md")
	links := []Link{{1, 2}, {1, 3}, {2, 4}, {5, 2}, {6, 7}, {7, 6}}
	return recs, links
}

func paths(g *Graph) []string {
	out := []string{}
	for _, n := range g.Nodes {
		out = append(out, fmt.Sprintf("%s@%d", n.Path, n.Hops))
	}
	return out
}

func node(g *Graph, path string) *GraphNode {
	for i := range g.Nodes {
		if g.Nodes[i].Path == path {
			return &g.Nodes[i]
		}
	}
	return nil
}

// **M52 の受け入れの芯。** ハブから出て行った先で、その先の被リンクとしてハブが見え、戻れる。
func TestFromTheHubAndBackAgain(t *testing.T) {
	recs, links := sample()
	there, err := Neighborhood(recs, links, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if node(there, "ctx/lab.md") == nil {
		t.Fatalf("ハブから出ていく先が無い: %v", paths(there))
	}
	// 1歩なら、2歩先（lab の先の server）は出さない。
	// （変異で見つけた: 例のグラフは2歩で全部に届くので、歩数の上限を外しても2歩の試験は通っていた）
	if node(there, "lab/server.md") != nil {
		t.Fatalf("1歩なのに2歩先まで出した: %v", paths(there))
	}
	back, err := Neighborhood(recs, links, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	hub := node(back, "hub/index.md")
	if hub == nil || hub.Hops != 1 {
		t.Fatalf("行った先からハブ（被リンク）へ戻れない: %v", paths(back))
	}
	// 辺の向きはリンクの向きのまま（ハブが homelab を指す）。
	if !reflect.DeepEqual(back.Edges[0], GraphEdge{From: 1, To: 2}) {
		t.Fatalf("辺の向きが違う: %+v", back.Edges)
	}
}

func TestDepthCountsStepsInEitherDirection(t *testing.T) {
	recs, links := sample()
	g, _ := Neighborhood(recs, links, 1, 2)
	want := []string{"hub/index.md@0", "ctx/lab.md@1", "ctx/work.md@1",
		"lab/server.md@2", "proj/app.md@2"}
	if !reflect.DeepEqual(paths(g), want) {
		t.Fatalf("\n got %v\nwant %v", paths(g), want)
	}
	if len(g.Edges) != 4 {
		t.Fatalf("辺: %+v", g.Edges)
	}
}

// リンクを1本も持たないノートは出さない。**出さなかった数は言う。**
func TestUnlinkedNotesAreCountedNotDrawn(t *testing.T) {
	recs, links := sample()
	g, _ := Neighborhood(recs, links, 1, 0)
	if node(g, "Inbox/lonely.md") != nil || g.Unlinked != 1 {
		t.Fatalf("リンクの無いノート: unlinked=%d %v", g.Unlinked, paths(g))
	}
	// depth 0 は全部。中心から辿れない島は後ろに、歩数 -1 で。
	last := g.Nodes[len(g.Nodes)-1]
	if len(g.Nodes) != 7 || last.Hops != -1 || !strings.HasPrefix(last.Path, "Inbox/") {
		t.Fatalf("%v", paths(g))
	}
	// リンクの無いノートを中心にしたら、そのノートだけを出す（「リンクが無い」と見せる）。
	lone, _ := Neighborhood(recs, links, 8, 2)
	if !reflect.DeepEqual(paths(lone), []string{"Inbox/lonely.md@0"}) || lone.Unlinked != 0 {
		t.Fatalf("%v unlinked=%d", paths(lone), lone.Unlinked)
	}
}

// 中心が無ければリンクの一番多いノート。並んだら path の若いほう（開くたびに変わらない）。
func TestTheDefaultCenterIsTheMostLinkedNote(t *testing.T) {
	recs, links := sample()
	g, _ := Neighborhood(recs, links, 0, 1)
	if g.Center != 2 {
		t.Fatalf("中心 %d", g.Center)
	}
	// index も 3 本に並ぶ。path は "ctx/lab.md" < "hub/index.md" なので lab。
	// 写像の順は毎回変わるので、決め方が無ければ 20 回のうちに揺れる。
	links = append(links, Link{1, 5})
	for i := 0; i < 20; i++ {
		if g, _ := Neighborhood(recs, links, 0, 1); g.Center != 2 {
			t.Fatalf("並んだときの中心が path 順でない（%d 回目で %d）", i, g.Center)
		}
	}
	if _, err := Neighborhood(recs, links, 99, 1); err == nil {
		t.Fatal("無いノートを中心にできた")
	}
}

// 往復のリンクは辺2本だが、つながりの数は1つ。
func TestAMutualLinkIsOneConnection(t *testing.T) {
	recs, links := sample()
	g, _ := Neighborhood(recs, links, 6, 1)
	if len(g.Edges) != 2 || node(g, "Inbox/a.md").Degree != 1 {
		t.Fatalf("%+v %+v", g.Edges, g.Nodes)
	}
}

func TestTooManyNodesKeepsTheNearest(t *testing.T) {
	var ps []string
	for i := 0; i < MaxGraphNodes+50; i++ {
		ps = append(ps, fmt.Sprintf("n/%04d.md", i))
	}
	recs := graphRecs(ps...)
	var links []Link
	for i := 2; i <= len(ps); i++ {
		links = append(links, Link{int64(i - 1), int64(i)}) // 一列につなぐ
	}
	g, _ := Neighborhood(recs, links, 1, 0)
	if len(g.Nodes) != MaxGraphNodes || g.Truncated == "" {
		t.Fatalf("%d 節 truncated=%q", len(g.Nodes), g.Truncated)
	}
	if g.Nodes[len(g.Nodes)-1].Hops != MaxGraphNodes-1 {
		t.Fatalf("遠いほうから切っていない: 最後 %+v", g.Nodes[len(g.Nodes)-1])
	}
	for _, e := range g.Edges {
		if e.To > MaxGraphNodes {
			t.Fatalf("切った節への辺が残った: %+v", e)
		}
	}
}

// ビューのグラフは行を節にし、行どうしのリンクだけを辺にする（機材の構成図）。
func TestAGraphViewConnectsItsOwnRows(t *testing.T) {
	recs, links := sample()
	b := &Base{Name: "Homelab"}
	v := &View{Kind: KindGraph, Name: "構成図", Filters: &Filter{Expr: `file.inFolder("ctx") || file.inFolder("lab")`}}
	res, err := Run(b, v, recs)
	if err != nil {
		t.Fatal(err)
	}
	AttachGraph(res, v, links)
	g := res.Graph
	// work.md は行だが、行どうしのリンクが無いので出さない。_index からの辺は行の外なので張らない。
	if !reflect.DeepEqual(paths(g), []string{"ctx/lab.md@0", "lab/server.md@0"}) ||
		g.Unlinked != 1 || len(g.Edges) != 1 {
		t.Fatalf("%v unlinked=%d edges=%+v", paths(g), g.Unlinked, g.Edges)
	}

	table := &View{Kind: KindTable, Name: "表"}
	res, _ = Run(b, table, recs)
	if AttachGraph(res, table, links); res.Graph != nil {
		t.Fatal("表にグラフを付けた")
	}
}

// 独自定義にだけある graph ビューは、併読の突き合わせで差にしない。表ビューなら差にする。
func TestCompareIgnoresGraphViewsTheBaseCannotHave(t *testing.T) {
	a := &Base{Name: "Homelab", Views: []View{{Kind: KindTable, Name: "すべて"}}}
	b := &Base{Name: "Homelab", Views: []View{{Kind: KindTable, Name: "すべて"}, {Kind: KindGraph, Name: "構成図"}}}
	if d := DiffBases(a, b); len(d) != 0 {
		t.Fatalf("構成図を足しただけで食い違いになった: %v", d)
	}
	b.Views = append(b.Views, View{Kind: KindTable, Name: "足した表"})
	if d := DiffBases(a, b); len(d) != 2 {
		t.Fatalf("独自定義にだけある表を見逃した（数と名前の2件のはず）: %v", d)
	}
}

const graphDef = `base: Homelab
views:
  - name: 構成図
    kind: graph
    shape:
      filter: 'file.inFolder("lab")'
    emit:
      human:
        - kind: graph
          colors:
            - {tag: "#east", color: "#2e7d32"}
            - {tag: west, color: "#1565c0"}
`

func TestGraphColorsAreReadAndChecked(t *testing.T) {
	b, err := ParseNative("Homelab", []byte(graphDef))
	if err != nil {
		t.Fatal(err)
	}
	c := b.Views[0].Emit.Human[0].Colors
	if c[0].Tag != "east" || c[1].Color != "#1565c0" {
		t.Fatalf("%+v", c)
	}
	for name, bad := range map[string]string{
		"色が無い": strings.Replace(graphDef, `, color: "#1565c0"`, "", 1),
		"色の形":  strings.Replace(graphDef, `"#1565c0"`, `blue`, 1),
		"タグが空": strings.Replace(graphDef, `tag: west`, `tag: ""`, 1),
	} {
		if bad == graphDef {
			t.Fatalf("%s: 置換が当たっていない", name)
		}
		if _, err := ParseNative("Homelab", []byte(bad)); err == nil {
			t.Errorf("%s: 読めてしまった", name)
		}
	}
}

// 読むのは解決済みで、両端が消えていない markdown のリンクだけ。自己リンクと重複は落とす。
func TestLoadLinksKeepsOnlyNoteToNoteLinks(t *testing.T) {
	db := defDB(t)
	for _, q := range []string{
		`insert into notes(id,vault_id,path,title,kind,ext) values
			(1,1,'a.md','a','markdown','.md'), (2,1,'b.md','b','markdown','.md'),
			(3,1,'img.png','img','asset','.png'), (4,1,'H.base','H','base','.base')`,
		`insert into notes(id,vault_id,path,title,kind,ext,missing_at) values (5,1,'gone.md','gone','markdown','.md','2026-01-01')`,
		`insert into note_links(from_note_id,raw_target,to_note_id,resolved) values
			(1,'b',2,1), (1,'b#見出し',2,1), (1,'img.png',3,1), (1,'H.base',4,1),
			(1,'gone',5,1), (1,'a',1,1), (2,'どこにも無い',null,0), (2,'a',1,1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	got, err := LoadLinks(db, 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Link{{1, 2}, {2, 1}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got %v\nwant %v", got, want)
	}
}
