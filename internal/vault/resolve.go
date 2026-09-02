package vault

import (
	"path"
	"sort"
	"strings"
)

// Index は解決のための索引。パスの大文字小文字は畳まない。
type LinkIndex struct {
	byPath map[string]string   // 正規化した相対パス -> 実パス
	byBase map[string][]string // ベース名（拡張子あり・なし） -> 実パス
}

// NewLinkIndex は Vault のファイル一覧から解決用の索引を作る。
func NewLinkIndex(paths []string) *LinkIndex {
	ix := &LinkIndex{
		byPath: make(map[string]string, len(paths)*2),
		byBase: make(map[string][]string, len(paths)*2),
	}
	for _, p := range paths {
		ix.byPath[p] = p
		base := path.Base(p)
		ix.byBase[base] = append(ix.byBase[base], p)
		if ext := path.Ext(p); ext == ".md" {
			// `.md` は省略できる。パスでもベース名でも。
			ix.byPath[strings.TrimSuffix(p, ext)] = p
			ix.byBase[strings.TrimSuffix(base, ext)] = append(
				ix.byBase[strings.TrimSuffix(base, ext)], p)
		}
	}
	for k := range ix.byBase {
		sort.Strings(ix.byBase[k])
	}
	return ix
}

// Resolution は1本のリンクの解決結果。
type Resolution struct {
	To         string   // 解決先の実パス。空なら宙吊り
	Ambiguous  bool     // 候補が複数あって選びきれなかった
	Candidates []string // 曖昧だったときの候補（To を含む）
}

// Resolve は `[[target]]` の指す先を決める。
//
// **候補が複数残ったときに黙って1つ選ばない。** 選んだ先は返すが
// `Ambiguous` を立てて候補も返す。理由:
//
// Obsidian の解決規則を突き合わせて検証する手段が今は無い（メタデータ
// キャッシュはディスクではなく IndexedDB の中）。このVaultには実測で
// 26本の曖昧なリンクがあり、全部 `Data/Health/YYYY-MM-DD.md` と
// `Human/Logs/YYYY-MM-DD.md` の衝突で、文脈上はどれも日記を指している。
// だがパスの短さで決めると `Data/Health/` が勝つ。**検証できない規則に
// 一致したふりをするのが一番危ない**ので、曖昧さそのものを記録する。
func (ix *LinkIndex) Resolve(from, target string) Resolution {
	target = strings.TrimSpace(strings.Trim(target, "/"))
	if target == "" {
		return Resolution{}
	}

	// パスを含む、または拡張子付きなら完全一致で引く。
	if p, ok := ix.byPath[target]; ok && (strings.Contains(target, "/") || path.Ext(target) != "") {
		return Resolution{To: p}
	}

	cands := uniq(ix.byBase[target])
	if len(cands) == 0 {
		// ベース名で当たらなくても、パス指定なら拾えることがある。
		if p, ok := ix.byPath[target]; ok {
			return Resolution{To: p}
		}
		return Resolution{}
	}
	if len(cands) == 1 {
		return Resolution{To: cands[0]}
	}

	// 同じフォルダにあるものが最優先。ここで1つに決まれば曖昧ではない。
	dir := path.Dir(from)
	var same []string
	for _, c := range cands {
		if path.Dir(c) == dir {
			same = append(same, c)
		}
	}
	if len(same) == 1 {
		return Resolution{To: same[0]}
	}
	if len(same) > 1 {
		cands = same
	}

	// 決まらない。パスの短い順・辞書順で1つ選ぶが、曖昧だったことを残す。
	sort.Slice(cands, func(i, j int) bool {
		if len(cands[i]) != len(cands[j]) {
			return len(cands[i]) < len(cands[j])
		}
		return cands[i] < cands[j]
	})
	return Resolution{To: cands[0], Ambiguous: true, Candidates: cands}
}

func uniq(xs []string) []string {
	if len(xs) < 2 {
		return xs
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	return out
}
