package notes

import (
	"fmt"
	"path"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// 新しいノートの名前（Phase 5 / M55）。
//
// **名前は NFC に揃え、大文字小文字を畳んで照らす。** macOS・iOS の端末は大文字小文字を区別しない
// ファイルシステムで checkout するので、`Todo.md` と `todo.md` が並ぶと片方が壊れる。NFD の名前
// （macOS が作りがち）も、見た目が同じ別のファイルになる。

// MaxName は1つの名前の長さの上限（ext4 のファイル名の上限）。
const MaxName = 255

// badNameChars は名前に使わない文字。前半は端末のファイルシステムで使えないもの、
// 後半は wikilink に書けないもの（`[[a#b]]` は見出し、`[[a|b]]` は別名になる）。
const badNameChars = `\/:*?"<>|` + `#^[]`

// NameRel は新しいノートのパスを揃えて照らす。返すのは NFC に揃えたパス。
//
// 照らすのは**最後の名前**だけ（フォルダは既にあるものだけに作る。Disk.Write が確かめる）。
func NameRel(rel string) (string, error) {
	rel = norm.NFC.String(rel)
	if err := cleanRel(rel); err != nil {
		return "", err
	}
	if path.Ext(rel) != ".md" {
		return "", fmt.Errorf("新しいノートは .md で終わる名前にする")
	}
	name := path.Base(rel)
	stem := strings.TrimSuffix(name, ".md")
	switch {
	case len(name) > MaxName:
		return "", fmt.Errorf("名前が長すぎる（%d バイトまで）", MaxName)
	case strings.TrimSpace(stem) == "":
		return "", fmt.Errorf("名前が空")
	case strings.HasPrefix(name, "."):
		return "", fmt.Errorf("ドットで始まる名前は作らない")
	case strings.HasSuffix(stem, " ") || strings.HasSuffix(stem, "."):
		return "", fmt.Errorf("名前の終わりに空白や . を置かない")
	case strings.ContainsAny(name, badNameChars):
		return "", fmt.Errorf("名前に使えない文字がある（%s）", badNameChars)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("名前に制御文字がある")
		}
	}
	return rel, nil
}

var folder = cases.Fold()

// FoldName はぶつかりを照らすための形（NFC に揃えて大文字小文字を畳む）。
func FoldName(s string) string {
	return folder.String(norm.NFC.String(s))
}
