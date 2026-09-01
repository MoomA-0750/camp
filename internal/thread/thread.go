// Package thread は1セッションの会話を木に組み立てる。
//
// 木に参加する行は uuid を持つ4種類だけ（user / assistant / attachment / system）。
// 残り11種（last-prompt, ai-title, mode, cost-state …）は uuid も parentUuid も
// 持たない付帯記録で、木の外にある。
package thread

import (
	"fmt"

	"github.com/MoomA-0750/camp/internal/store"
)

type Node struct {
	ID           int64
	UUID         string
	Parent       string // 実効の親。ルートなら空
	ViaLogical   bool   // logicalParentUuid で繋ぎ直した
	Type         string
	Subtype      string
	Role         string
	Timestamp    string
	SourceFileID int64
	ByteOffset   int64
	Depth        int
	Children     []*Node
}

type Tree struct {
	SessionID string
	// Order は表示順。**(source_file_id, byte_offset) で並べる。**
	// タイムスタンプで並べ替えてはいけない（実測: 72ファイル中47本、
	// 計681箇所で時刻が逆行する）。書かれた順こそが起きた順である。
	Order    []*Node
	Roots    []*Node
	Index    map[string]*Node
	Repaired int // logicalParentUuid で繋ぎ直した数
	Orphans  int // 親を指しているのに見つからなかった数
}

// Build は1セッションぶんの木を組む。
func Build(db *store.DB, sessionID string) (*Tree, error) {
	rows, err := db.Query(`
		select id, uuid, coalesce(parent_uuid, ''), coalesce(logical_parent_uuid, ''),
		       type, coalesce(subtype, ''), coalesce(role, ''), coalesce(timestamp, ''),
		       source_file_id, byte_offset
		  from messages
		 where session_id = ? and coalesce(uuid, '') <> ''
		 order by source_file_id, byte_offset`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	t := &Tree{SessionID: sessionID, Index: map[string]*Node{}}
	type link struct {
		node    *Node
		parent  string
		logical bool
	}
	var links []link

	for rows.Next() {
		n := &Node{}
		var parent, logical string
		if err := rows.Scan(&n.ID, &n.UUID, &parent, &logical, &n.Type, &n.Subtype,
			&n.Role, &n.Timestamp, &n.SourceFileID, &n.ByteOffset); err != nil {
			return nil, err
		}
		// compact_boundary は parentUuid を持たず logicalParentUuid だけを持つ。
		// これを使わないと、要約のたびに木が切れて別のスレッドに見える。
		p, viaLogical := parent, false
		if logical != "" {
			p, viaLogical = logical, true
		}
		n.Parent, n.ViaLogical = p, viaLogical
		t.Order = append(t.Order, n)
		t.Index[n.UUID] = n // uuid はセッション内では一意（実測、重複0件）
		links = append(links, link{n, p, viaLogical})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, l := range links {
		if l.parent == "" {
			t.Roots = append(t.Roots, l.node)
			continue
		}
		p, ok := t.Index[l.parent]
		if !ok {
			// 親が同じセッションに居ない。ルート扱いにして数える。
			// uuid はセッションを跨ぐと重複する（fork）ので、
			// 他セッションから拾ってきて繋いではいけない。
			t.Orphans++
			t.Roots = append(t.Roots, l.node)
			continue
		}
		p.Children = append(p.Children, l.node)
		if l.logical {
			t.Repaired++
		}
	}

	assignDepth(t)
	return t, nil
}

// assignDepth はルートから深さを振る。輪ができていても止まる。
func assignDepth(t *Tree) {
	seen := make(map[int64]bool, len(t.Order))
	var walk func(n *Node, d int)
	walk = func(n *Node, d int) {
		if seen[n.ID] {
			return
		}
		seen[n.ID] = true
		n.Depth = d
		for _, c := range n.Children {
			walk(c, d+1)
		}
	}
	for _, r := range t.Roots {
		walk(r, 0)
	}
}

// ConversationRoots は「会話を含む根」だけを返す。
//
// 根がセッションに複数あるのは異常ではない。`/remote-control` を使うと、
// **ファイルの先頭ごとに** `system/bridge_status` の案内行が親を持たずに置かれる
// （実測: 親を持たない bridge_status 40件は**すべて**そのファイルの先頭行）。
// resume のサイドカーはほぼ空なので、その案内行だけが根として残る。
// 会話が何本あるかを見たいときは、これらを数に入れてはいけない。
func (t *Tree) ConversationRoots() []*Node {
	var out []*Node
	for _, r := range t.Roots {
		if hasConversation(r, map[int64]bool{}) {
			out = append(out, r)
		}
	}
	return out
}

func hasConversation(n *Node, seen map[int64]bool) bool {
	if seen[n.ID] {
		return false
	}
	seen[n.ID] = true
	if n.Type == "user" || n.Type == "assistant" {
		return true
	}
	for _, c := range n.Children {
		if hasConversation(c, seen) {
			return true
		}
	}
	return false
}

// FragmentsWithoutRepair は logicalParentUuid を無視した場合に
// 会話が何本に割れるかを返す。
//
// compact_boundary は必ず子を持つ（実測15件すべて）ので、繋ぎ直しを1つ捨てるたびに
// 断片が1つ増える。「この列が無いとスレッドが断片化する」を数字で言うための関数。
func (t *Tree) FragmentsWithoutRepair() int {
	return len(t.ConversationRoots()) + t.Repaired
}

// TimestampInversions は表示順で時刻が逆行する箇所を数える。
//
// 「タイムスタンプで並べ替えてはいけない」を数字で言うための関数。
// 実測では72ファイル中47本、計681箇所で逆行していた。
func (t *Tree) TimestampInversions() int {
	n, prev := 0, ""
	for _, node := range t.Order {
		if node.Timestamp == "" {
			continue
		}
		if prev != "" && node.Timestamp < prev {
			n++
		}
		prev = node.Timestamp
	}
	return n
}

// Describe は木の形を1行で言う。
func (t *Tree) Describe() string {
	return fmt.Sprintf("%d行 / 会話%d本（繋ぎ直さなければ%d本） / 根%d / 繋ぎ直し%d / 迷子%d / 時刻の逆行%d",
		len(t.Order), len(t.ConversationRoots()), t.FragmentsWithoutRepair(),
		len(t.Roots), t.Repaired, t.Orphans, t.TimestampInversions())
}
