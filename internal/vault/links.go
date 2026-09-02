package vault

import (
	"regexp"
	"strings"
)

// Link は本文から取り出した1本のwikilink。
type Link struct {
	Raw    string // [[..]] の中身そのもの
	Target string // 解決に使う部分（エイリアス・見出し・ブロックを外したもの）
	Alias  string
	Frag   string // #見出し または ^ブロックID
	Embed  bool   // ![[..]]
	Line   int    // 1始まり

	// SelfFrag は [[#見出し]] のように自ノート内を指すリンク。
	// Obsidian では有効なリンクなので捨てない（実測4本）。
	SelfFrag bool
}

// linkRe は wikilink を拾う。
//
// 改行を跨がせないのは `[^\]\n]` ではなく、**ExtractLinks が先に行で切っている**
// ことのほうが本体。文字クラスの `\n` は、この正規表現を本文全体に当てる書き方に
// 後から変えられたときのための保険で、いま効いているわけではない
// （テストで区別できないことを確認済み）。
//
// 跨がせるとどうなるかは実測してある: 表の行にある `[[A` から遥か下の `]]` までを
// 1つのターゲットとして飲み込む（1件・約4,000字）。
var linkRe = regexp.MustCompile(`(!?)\[\[([^\]\n]+)\]\]`)

var fenceRe = regexp.MustCompile("^\\s*(```+|~~~+)")

// ExtractLinks は本文から wikilink を取り出す。
//
// **コードは除き、frontmatter は除かない。**
//   - `[[ ]]` は bash の test 構文と衝突する。実測で素朴に数えた703本のうち40本が
//     フェンス内の `[[ -n "$var" ]]` や、“ `[[wikilink]]` “ という構文の説明そのもの
//   - 一方このVaultは frontmatter の `source:` に `- "[[ノート名]]"` の形で
//     To-Do の由来を持っている。frontmatter を飛ばすと Data/Todo の相互リンクが丸ごと落ちる
func ExtractLinks(body []byte) []Link {
	var out []Link
	inFence, fence := false, ""

	for i, line := range strings.Split(string(body), "\n") {
		if m := fenceRe.FindStringSubmatch(line); m != nil {
			if !inFence {
				inFence, fence = true, m[1]
			} else if strings.HasPrefix(strings.TrimSpace(line), fence) {
				inFence = false
			}
			continue
		}
		if inFence {
			continue
		}
		for _, m := range linkRe.FindAllStringSubmatchIndex(maskInlineCode(line), -1) {
			raw := line[m[4]:m[5]]
			l := Link{Raw: raw, Embed: line[m[2]:m[3]] == "!", Line: i + 1}
			l.Target, l.Alias, l.Frag = splitTarget(raw)
			if l.Target == "" && l.Frag == "" {
				continue
			}
			// `[[#見出し]]` は「このノートのこの見出し」。Obsidian では有効な
			// リンクなので捨てない。解決先は自分自身になる（実測4本）。
			l.SelfFrag = l.Target == ""
			out = append(out, l)
		}
	}
	return out
}

// maskInlineCode はバッククォート内を同じ長さの空白に潰す。
// 位置をずらさないので、元の行から中身を切り出せる。
func maskInlineCode(line string) string {
	b := []byte(line)
	out := make([]byte, len(b))
	copy(out, b)
	open := -1
	for i := 0; i < len(b); i++ {
		if b[i] != '`' {
			continue
		}
		if open < 0 {
			open = i
			continue
		}
		for j := open; j <= i; j++ {
			out[j] = ' '
		}
		open = -1
	}
	return string(out)
}

// splitTarget は `パス|別名` と `パス#見出し` `パス^ブロック` を分解する。
// Obsidian の順序に合わせ、まず `|` で切ってから `#` / `^` を見る。
func splitTarget(raw string) (target, alias, frag string) {
	if i := strings.Index(raw, "|"); i >= 0 {
		raw, alias = raw[:i], strings.TrimSpace(raw[i+1:])
	}
	// `#` のほうが先に来る形しか実在しないが、両方見て早いほうで切る。
	cut := -1
	for _, c := range []string{"#", "^"} {
		if i := strings.Index(raw, c); i >= 0 && (cut < 0 || i < cut) {
			cut = i
		}
	}
	if cut >= 0 {
		frag, raw = strings.TrimSpace(raw[cut:]), raw[:cut]
	}
	return strings.TrimSpace(raw), alias, frag
}
