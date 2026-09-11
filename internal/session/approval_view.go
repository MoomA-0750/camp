package session

import (
	"encoding/json"
	"strings"
)

// 承認の中身の共通の形。**画面はこの形だけを見て描き、エージェントを見ない**（D-031）。
// 駆動器が、そのエージェントの承認の要求をこの形に直し、中身（detail）の "view" に入れる。
// 元の要求も並べて残す（あとから読み返すため）。

// AskView は承認1つの中身。
type AskView struct {
	// What は何の承認か: command（コマンドを走らせる）/ file（ファイルを変える）/ tool（その他の道具）。
	What    string `json:"what"`
	Tool    string `json:"tool,omitempty"`
	Command string `json:"command,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	// Outside は起こした場所の外で走らせようとしているか（断らずに印を付けて見せる。D-030）。
	Outside bool        `json:"outside,omitempty"`
	Changes []AskChange `json:"changes,omitempty"`
	// Input は tool のときの入力そのまま。
	Input  json.RawMessage `json:"input,omitempty"`
	Reason string          `json:"reason,omitempty"` // エージェントが添えた理由
}

// AskChange はファイル1つの変更。Patch は差分（+/- の行）。**切り詰めない**——途中までの
// 中身で許させない（画面へ渡せない大きさのものは、駆動器が見せずに断る）。
type AskChange struct {
	Path  string `json:"path"`
	Kind  string `json:"kind,omitempty"`
	Patch string `json:"patch"`
}

// linesWith は各行の頭に p を付ける（差分の +/- の行を作る）。
func linesWith(p, s string) string {
	if s == "" {
		return ""
	}
	ls := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i := range ls {
		ls[i] = p + ls[i]
	}
	return strings.Join(ls, "\n") + "\n"
}
