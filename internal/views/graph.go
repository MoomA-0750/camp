package views

// グラフ（Phase 4 / M52、2026-09-13）。ノートを節、`note_links` を辺にする。
//
// **リンクを1本も持たないノートは節にしない。** 実データでは解決済みの
// リンクを持つノートは1割に満たず、全部を節にすると点の海になる。出さなかった数は必ず言う。
//
// 入口は2つ:
//   - `Neighborhood` … ノートから N 歩以内（向きを問わない）。中心が無ければリンクの一番多いノート
//   - `AttachGraph` … ビュー（`kind: graph`）の行を節にし、その間のリンクを辺にする。
//     機材の構成図（あるフォルダだけ、タグで色分け）を表と同じ台紙に置くため

import (
	"fmt"
	"sort"

	"github.com/MoomA-0750/camp/internal/store"
)

// KindGraph は独自定義だけにある種別（`.base` に対応物が無い）。
const KindGraph = "graph"

// Link は解決済みのリンク1本（向きあり）。
type Link struct {
	From int64
	To   int64
}

// LoadLinks は解決済みのリンクを読む。**両端が markdown で、消えていないものだけ。**
// 添付（画像）と `.base` の埋め込みは節にしない（Obsidian の既定も添付を出さない）。
// 自己リンクと、同じ2点の重複は落とす。
func LoadLinks(db *store.DB, vaultID int64) ([]Link, error) {
	rows, err := db.Query(`
		select distinct l.from_note_id, l.to_note_id
		  from note_links l
		  join notes f on f.id = l.from_note_id
		  join notes t on t.id = l.to_note_id
		 where l.resolved = 1 and f.vault_id = ?
		   and f.missing_at is null and t.missing_at is null
		   and f.kind = 'markdown' and t.kind = 'markdown'
		   and l.from_note_id != l.to_note_id
		 order by 1, 2`, vaultID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Link
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.From, &l.To); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// GraphNode は節。Hops は中心からの歩数（ビューのグラフでは 0）。Degree は**このグラフの中の**辺の数。
type GraphNode struct {
	ID     int64    `json:"id"`
	Path   string   `json:"path"`
	Name   string   `json:"name"`
	Tags   []string `json:"tags,omitempty"`
	Degree int      `json:"degree"`
	Hops   int      `json:"hops"`
}

// GraphEdge は辺。向きはリンクの向き（From が To を指す）。
type GraphEdge struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// Graph はグラフ1枚。
type Graph struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
	// Center は中心のノート（ノートから辿るときだけ）。
	Center int64 `json:"center,omitempty"`
	Depth  int   `json:"depth,omitempty"`
	// Unlinked は対象だったのにリンクを持たないので出さなかったノートの数。**黙って消さない。**
	Unlinked int `json:"unlinked"`
	// Truncated は節の上限で切ったとき。遠いほうから切る。
	Truncated string `json:"truncated,omitempty"`
	// Total は切る前の節の数（MCP がもう一度切るとき、元の数を失わないため）。
	Total int `json:"total"`
}

// MaxGraphNodes は1枚に出す節の上限。画面で配置を計算するので、それに見合う数。
const MaxGraphNodes = 400

// Neighborhood はノートから depth 歩以内のグラフを作る。center が 0 ならリンクの一番多いノート。
// depth が 0 なら、中心から辿れるかにかかわらず**リンクを持つノート全部**（辿れないものは Hops -1）。
func Neighborhood(recs []*Record, links []Link, center int64, depth int) (*Graph, error) {
	byID := make(map[int64]*Record, len(recs))
	for _, r := range recs {
		byID[r.NoteID] = r
	}
	adj := map[int64]map[int64]bool{}
	connect := func(a, b int64) {
		if adj[a] == nil {
			adj[a] = map[int64]bool{}
		}
		adj[a][b] = true
	}
	for _, l := range links {
		if byID[l.From] == nil || byID[l.To] == nil {
			continue
		}
		connect(l.From, l.To)
		connect(l.To, l.From)
	}

	g := &Graph{Depth: depth}
	if center == 0 {
		// リンクの一番多いノート。並んだら path の若いほう（開くたびに中心が変わらないように）。
		for id, ns := range adj {
			if center == 0 || len(ns) > len(adj[center]) ||
				(len(ns) == len(adj[center]) && byID[id].NPath < byID[center].NPath) {
				center = id
			}
		}
		if center == 0 {
			return g, nil // リンクが1本も無い Vault
		}
	}
	if byID[center] == nil {
		return nil, fmt.Errorf("そのノートは無い（id %d）", center)
	}
	g.Center = center

	hops := map[int64]int{center: 0}
	frontier := []int64{center}
	for d := 1; len(frontier) > 0 && (depth == 0 || d <= depth); d++ {
		var next []int64
		for _, x := range frontier {
			for y := range adj[x] {
				if _, seen := hops[y]; !seen {
					hops[y] = d
					next = append(next, y)
				}
			}
		}
		frontier = next
	}
	if depth == 0 {
		for id := range adj {
			if _, ok := hops[id]; !ok {
				hops[id] = -1
			}
		}
	}
	// 中心がリンクを持たないノートでも、中心だけは出す（「このノートにはリンクが無い」と見せる）。
	g.Unlinked = len(recs) - len(adj)
	if len(adj[center]) == 0 {
		g.Unlinked--
	}

	ids := make([]int64, 0, len(hops))
	for id := range hops {
		ids = append(ids, id)
	}
	fill(g, byID, ids, func(id int64) int { return hops[id] }, func(a, b int64) bool {
		ha, hb := hops[a], hops[b]
		if (ha < 0) != (hb < 0) {
			return hb < 0 // 辿れないものは後ろ
		}
		if ha != hb {
			return ha < hb
		}
		if len(adj[a]) != len(adj[b]) {
			return len(adj[a]) > len(adj[b])
		}
		return byID[a].NPath < byID[b].NPath
	}, links)
	return g, nil
}

// AttachGraph はビュー（`kind: graph`）の行を節にしたグラフを結果に付ける。
// **行のうち、ほかの行とリンクで繋がっていないものは出さない**（数は言う）。
func AttachGraph(res *Result, v *View, links []Link) {
	if v.Kind != KindGraph {
		return
	}
	byID := map[int64]*Record{}
	for _, gr := range res.Groups {
		for _, r := range gr.Rows {
			byID[r.NoteID] = r
		}
	}
	linked := map[int64]int{}
	for _, l := range links {
		if byID[l.From] != nil && byID[l.To] != nil {
			linked[l.From]++
			linked[l.To]++
		}
	}
	g := &Graph{Unlinked: len(byID) - len(linked)}
	ids := make([]int64, 0, len(linked))
	for id := range linked {
		ids = append(ids, id)
	}
	fill(g, byID, ids, func(int64) int { return 0 }, func(a, b int64) bool {
		if linked[a] != linked[b] {
			return linked[a] > linked[b]
		}
		return byID[a].NPath < byID[b].NPath
	}, links)
	res.Graph = g
}

// fill は節を並べ（less の順）、上限で切り、残った節の間の辺を張る。
func fill(g *Graph, byID map[int64]*Record, ids []int64, hops func(int64) int,
	less func(a, b int64) bool, links []Link) {
	sort.Slice(ids, func(i, j int) bool { return less(ids[i], ids[j]) })
	g.Total = len(ids)
	if len(ids) > MaxGraphNodes {
		g.Truncated = fmt.Sprintf("全 %d 節のうち近い %d 節", len(ids), MaxGraphNodes)
		ids = ids[:MaxGraphNodes]
	}
	in := make(map[int64]int, len(ids))
	for i, id := range ids {
		in[id] = i
		r := byID[id]
		g.Nodes = append(g.Nodes, GraphNode{ID: id, Path: r.NPath, Name: r.NName, Tags: r.Tags, Hops: hops(id)})
	}
	seen := map[[2]int64]bool{}
	for _, l := range links {
		i, okA := in[l.From]
		j, okB := in[l.To]
		if !okA || !okB || seen[[2]int64{l.From, l.To}] {
			continue
		}
		seen[[2]int64{l.From, l.To}] = true
		g.Edges = append(g.Edges, GraphEdge{From: l.From, To: l.To})
		// 往復のリンクは辺2本だが、つながりの数としては1つに数える。
		if !seen[[2]int64{l.To, l.From}] {
			g.Nodes[i].Degree++
			g.Nodes[j].Degree++
		}
	}
	if g.Nodes == nil {
		g.Nodes = []GraphNode{}
	}
	if g.Edges == nil {
		g.Edges = []GraphEdge{}
	}
}
