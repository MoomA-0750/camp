// Package notes は Vault のノートを書く口（Phase 5 / M53、2026-09-13）。
//
// **書くのは実行面（本人のユーザー）。** campd は `camp` ユーザーで Vault に書けない
// （D-024）。ここにある照合は campd（画面に「直せない」と出すため）と実行面（最後の砦）の
// 両方で同じものを使う。設計は `dev/active/phase5-design.md` の「書く口」。
package notes

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// Class はそのパスを編集面から書けるか。
type Class int

const (
	// ReadOnly は書けない（場所の一覧の外）。
	ReadOnly Class = iota
	// Editable は自動保存で書ける。
	Editable
	// Instruction はエージェントが指示として読む紙。**パスワードを入れ直したときだけ書ける**
	// （本人の決定8、2026-09-13）。自動保存しない。
	Instruction
)

// editableDirs は散文のノートを置く場所（本人の決定4: 散文のノートだけ）。
var editableDirs = []string{
	"Human/", "AI/", "Data/Notes/", "Inbox/",
	"Meta/Templates/", "Meta/References/", "Meta/Reviews/",
}

// generated は自動で集計されるので手で直さない（Vault の AGENTS.md）。
var generated = map[string]bool{
	"Home.md":                       true,
	"Human/Dashboards/Dashboard.md": true,
}

// instruction はエージェントの指示の紙。`Meta/Agent-Skills/` は
// `Meta/Scripts/sync_agent_skills.py` が `.claude/skills`・`.agents/skills` へ写す Skill の元。
var instruction = map[string]bool{"AGENTS.md": true, "CLAUDE.md": true}

const instructionDir = "Meta/Agent-Skills/"

// Classify は Vault の中の相対パス（`/` 区切り）を分ける。reason は書けないときの理由。
//
// **形の崩れたパスは全部 ReadOnly。** 実パスでの照合（symlink）は WriteFile が別にやる。
func Classify(rel string) (Class, string) {
	if err := cleanRel(rel); err != nil {
		return ReadOnly, err.Error()
	}
	if path.Ext(rel) != ".md" {
		return ReadOnly, "Markdown（.md）以外は直せない"
	}
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			// .git/.obsidian/.trash/.claude/.agents/.githooks とドット始まりのファイル
			return ReadOnly, "ドットで始まる場所は直せない"
		}
	}
	if generated[rel] {
		return ReadOnly, "自動で集計されるノートなので手で直さない"
	}
	if instruction[rel] || strings.HasPrefix(rel, instructionDir) {
		return Instruction, ""
	}
	for _, d := range editableDirs {
		if strings.HasPrefix(rel, d) {
			return Editable, ""
		}
	}
	return ReadOnly, "散文のノートを置く場所（Human/・AI/・Data/Notes/・Inbox/・Meta/ の一部）の外"
}

// cleanRel はパスの形だけを見る。
func cleanRel(rel string) error {
	switch {
	case rel == "":
		return fmt.Errorf("パスが空")
	case strings.HasPrefix(rel, "/"):
		return fmt.Errorf("絶対パスは受け取らない")
	case strings.ContainsAny(rel, "\\\x00"):
		return fmt.Errorf("パスに使えない文字がある")
	case !utf8.ValidString(rel):
		return fmt.Errorf("パスが UTF-8 でない")
	case path.Clean(rel) != rel:
		return fmt.Errorf("パスの形が崩れている（.. や // を含む）")
	case rel == ".." || strings.HasPrefix(rel, "../"):
		return fmt.Errorf("Vault の外は指せない")
	}
	return nil
}

// CheckText は編集面で扱える中身かを見る。
//
// **CR・BOM・不正な UTF-8 を含むものは読み取り専用にする。** CodeMirror は読み込みで CR を畳み、
// ブラウザは不正なバイトを置き換えるので、保存すると黙って書き換わる（Fable の設計レビュー 10）。
func CheckText(b []byte) error {
	switch {
	case !utf8.Valid(b):
		return fmt.Errorf("UTF-8 として読めない")
	case len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF:
		return fmt.Errorf("先頭に BOM がある")
	case strings.IndexByte(string(b), '\r') >= 0:
		return fmt.Errorf("改行に CR を含む")
	}
	return nil
}

// Sum は中身の SHA-256（16進）。
func Sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
