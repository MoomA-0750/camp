package ingest

import (
	"encoding/json"
	"strings"
	"testing"
)

// bigram の刻み方。索引と問い合わせで同じ関数を通す前提なので、
// ここが変わると既存の索引は全部作り直しになる。
func TestBigramsShape(t *testing.T) {
	cases := map[string]string{
		"Redmineで進捗報告":   "Redmine で進 進捗 捗報 報告",
		"設定":             "設定",
		"認証を通す":          "認証 証を を通 通す",
		"管":              "管", // 1文字は前方一致で拾う
		"hello world":    "hello world",
		"vSphere と RHEL": "vSphere と RHEL",
		"":               "",
	}
	for in, want := range cases {
		if got := Bigrams(in); got != want {
			t.Errorf("Bigrams(%q) = %q, want %q", in, got, want)
		}
	}
}

// ASCII だけの文字列は触らない。unicode61 がそのまま切れるので、
// 加工すると索引が無駄に太るだけ。
func TestBigramsLeavesASCIIAlone(t *testing.T) {
	const s = "SELECT * FROM messages WHERE run_id IS NOT NULL"
	if got := Bigrams(s); got != s {
		t.Fatalf("ASCII を書き換えている: %q", got)
	}
}

func blocksOf(t *testing.T, raw string) []blockRow {
	t.Helper()
	var l Line
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		t.Fatal(err)
	}
	return extractBlocks(&l)
}

// tool_use の入力はキー名ごと索引する。パスを憶えていなくても
// ツール名や引数の値で引けるようにする。
func TestExtractToolUseIndexesInput(t *testing.T) {
	bs := blocksOf(t, `{"type":"assistant","message":{"role":"assistant","content":[
		{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"pgpool -n","description":"接続確認"}}]}}`)
	if len(bs) != 1 {
		t.Fatalf("ブロック %d 個", len(bs))
	}
	b := bs[0]
	if b.Kind != "tool_use" || b.ToolName != "Bash" || b.ToolUseID != "toolu_1" {
		t.Fatalf("%+v", b)
	}
	for _, want := range []string{"command", "pgpool -n", "接続確認"} {
		if !strings.Contains(b.Text, want) {
			t.Errorf("入力に %q が入っていない: %q", want, b.Text)
		}
	}
}

// tool_result の content は文字列のことも配列のこともある。
// 画像しか入っていないものは索引しない（base64 は引けない）。
func TestExtractToolResultShapes(t *testing.T) {
	str := blocksOf(t, `{"type":"user","message":{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"toolu_1","content":"設定を書き換えた"}]}}`)
	if len(str) != 1 || str[0].Text != "設定を書き換えた" {
		t.Fatalf("文字列の content: %+v", str)
	}

	arr := blocksOf(t, `{"type":"user","message":{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"text","text":"一行目"},{"type":"text","text":"二行目"}]}]}}`)
	if len(arr) != 1 || !strings.Contains(arr[0].Text, "一行目") || !strings.Contains(arr[0].Text, "二行目") {
		t.Fatalf("配列の content: %+v", arr)
	}

	img := blocksOf(t, `{"type":"user","message":{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"toolu_3","content":[{"type":"image","source":{"type":"base64","data":"iVBOR"}}]}]}}`)
	if len(img) != 0 {
		t.Fatalf("画像だけの結果は索引しない: %+v", img)
	}
}

// thinking は本文が空で signature だけ、という行が大半だった（実測2,992件）。
// 空文字を索引に入れない。
func TestExtractSkipsSignatureOnlyThinking(t *testing.T) {
	bs := blocksOf(t, `{"type":"assistant","message":{"role":"assistant","content":[
		{"type":"thinking","thinking":"","signature":"Eu0eCqgBCBEYAipA"}]}}`)
	if len(bs) != 0 {
		t.Fatalf("署名だけの thinking を索引している: %+v", bs)
	}

	withText := blocksOf(t, `{"type":"assistant","message":{"role":"assistant","content":[
		{"type":"thinking","thinking":"どう直すか考える","signature":"x"}]}}`)
	if len(withText) != 1 || withText[0].Kind != "thinking" {
		t.Fatalf("本文のある thinking は索引する: %+v", withText)
	}
}
