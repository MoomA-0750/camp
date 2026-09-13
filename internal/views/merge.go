package views

// 回す材料を集める口（Phase 4 / M50、2026-09-13）。画面・MCP・CLI はここだけを見る。

import (
	"sort"

	"github.com/MoomA-0750/camp/internal/store"
)

// Load は台紙を全部集める。**DB の独自定義を優先し、定義の無い台紙だけ `.base` に落ちる。**
//
// **落ちるのは台紙単位。ビュー単位にしない。** ビュー単位だと、独自定義から**消したビューが
// `.base` から復活する**（設計レビューの指摘6、2026-09-13）。台紙が DB にあるなら、その台紙は
// DB だけを見る——消したことが消したままになる。
//
// **併読の間も、見えているのは常にどちらか一方。** 突き合わせ（`-compare`）は
// `LoadBases` と `LoadNativeDefs` を別々に呼んで比べる。ここで混ぜると、画面が
// 「どちらを描いたか」を言えなくなる。
//
// read は `.base` の中身を読む関数（Vault のファイル）。
func Load(db *store.DB, vaultID int64, read func(rel string) ([]byte, error)) ([]*Base, error) {
	native, err := LoadNativeDefs(db, vaultID)
	if err != nil {
		return nil, err
	}
	have := make(map[string]bool, len(native))
	out := make([]*Base, 0, len(native))
	for _, b := range native {
		have[b.Name] = true
		out = append(out, b)
	}

	bases, err := LoadBases(db, vaultID, read)
	if err != nil {
		return nil, err
	}
	for _, b := range bases {
		if have[b.Name] {
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// FromDB は、その台紙が独自定義から来たか（画面が「どちらを描いたか」を言うため）。
func FromDB(b *Base) bool { return len(b.Path) > 3 && b.Path[:3] == "db:" }
