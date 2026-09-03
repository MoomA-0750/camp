package retain

import (
	"encoding/json"
	"strings"
	"testing"
)

// 触らない部分は1バイトも変えない。
func TestTrimLeavesEverythingElseByteForByte(t *testing.T) {
	raw := []byte(`{"type":"assistant","uuid":"u-1","n":1.10,"z":"あ","message":{"content":[` +
		`{"type":"thinking","thinking":"","signature":"AAAABBBBCCCC"},` +
		`{"type":"text","text":"本文はのこる"}]}}`)

	out, removed, changed, err := Trim(raw, []Rule{RuleThinkingSignature})
	if err != nil {
		t.Fatal(err)
	}
	if !changed || removed != len("AAAABBBBCCCC") {
		t.Fatalf("落とした量がおかしい: removed=%d changed=%v", removed, changed)
	}
	if strings.Contains(string(out), "AAAABBBBCCCC") {
		t.Error("signature が残っている")
	}
	// 数値の表記もキーの順序も変えない。読み直して書き戻す実装だと 1.10 が 1.1 になる。
	if !strings.Contains(string(out), `"n":1.10`) {
		t.Errorf("触っていない部分が書き換わった: %s", out)
	}
	if !strings.Contains(string(out), `"本文はのこる"`) {
		t.Error("本文まで消えた")
	}
	if want := strings.Replace(string(raw), `"AAAABBBBCCCC"`, `""`, 1); string(out) != want {
		t.Errorf("差し替え以外の変化がある:\n出力 %s\n期待 %s", out, want)
	}
	if !json.Valid(out) {
		t.Error("JSON として壊れた")
	}
}

// 画像は toolUseResult 側だけ落とす。message.content 側は残す。
func TestTrimDropsOnlyTheDuplicateImage(t *testing.T) {
	const data = "iVBORw0KGgoAAAANSUhEUg"
	raw := []byte(`{"type":"user",` +
		`"message":{"content":[{"type":"tool_result","content":[` +
		`{"type":"image","source":{"type":"base64","data":"` + data + `"}}]}]},` +
		`"toolUseResult":{"content":[` +
		`{"type":"image","source":{"type":"base64","data":"` + data + `"}}]}}`)

	out, removed, changed, err := Trim(raw, []Rule{RuleDuplicateImage})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("二重の画像を落とせていない")
	}
	if removed != len(data) {
		t.Errorf("落とした量が %d（%d を期待）", removed, len(data))
	}

	// message.content 側は残っていること。**画像そのものは失わない。**
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	kept := imageData(doc["message"], nil)
	if len(kept) != 1 || kept[0].data != data {
		t.Errorf("message.content 側の画像まで消えた: %v", kept)
	}
	if got := imageData(doc["toolUseResult"], nil); len(got) != 0 {
		t.Errorf("toolUseResult 側に画像が %d 個残っている", len(got))
	}
}

// 片方にしか無い画像は「重複」ではないので落とさない。
func TestTrimKeepsAnImageThatIsNotDuplicated(t *testing.T) {
	raw := []byte(`{"type":"user","toolUseResult":{"content":[` +
		`{"type":"image","source":{"type":"base64","data":"onlyhere"}}]}}`)
	out, removed, changed, err := Trim(raw, []Rule{RuleDuplicateImage})
	if err != nil {
		t.Fatal(err)
	}
	if changed || removed != 0 {
		t.Errorf("重複していない画像を落とした: removed=%d", removed)
	}
	if string(out) != string(raw) {
		t.Error("触っていないはずの行が変わった")
	}
}

// 落とすものが無ければ何もしない。
func TestTrimIsANoopWhenThereIsNothingToDrop(t *testing.T) {
	raw := []byte(`{"type":"user","message":{"content":"ただの本文"}}`)
	out, removed, changed, err := Trim(raw, []Rule{RuleThinkingSignature, RuleDuplicateImage})
	if err != nil {
		t.Fatal(err)
	}
	if changed || removed != 0 || string(out) != string(raw) {
		t.Errorf("何も無いのに触った: removed=%d changed=%v", removed, changed)
	}
}

// 読めない行は触らない。壊れた行を壊し直しても得がない。
func TestTrimRefusesBrokenJSON(t *testing.T) {
	raw := []byte(`{"type":"user","message":`)
	out, removed, changed, err := Trim(raw, []Rule{RuleThinkingSignature})
	if err == nil {
		t.Error("壊れた行を黙って通した")
	}
	if changed || removed != 0 || string(out) != string(raw) {
		t.Error("壊れた行を書き換えた")
	}
}

// エスケープの要る文字が入っていても、元のバイト列と一致しなければ触らない。
func TestTrimHandlesEscapedValues(t *testing.T) {
	// 元の行は非ASCIIを \u エスケープで書いているが、Go はそのまま書く。
	// 位置で指しているので**そもそも中身の一致に依存しない**が、
	// 差し替えた範囲が文字列でなければ触らないことは確かめておく。
	raw := []byte(`{"message":{"content":[{"type":"thinking","signature":"\u3042\u3044"}]}}`)
	out, removed, changed, err := Trim(raw, []Rule{RuleThinkingSignature})
	if err != nil {
		t.Fatal(err)
	}
	// エスケープされていても位置は求まるので、ちゃんと落ちる。
	if !changed || removed == 0 {
		t.Error("エスケープされた値を落とせていない")
	}
	if !json.Valid(out) {
		t.Error("JSON として壊れた")
	}
	if strings.Contains(string(out), "u3042") {
		t.Error("signature が残っている")
	}
}

// 1行に複数の対象があっても、それぞれ正しい位置を指す。
//
// 位置は `append(path, key)` で作るので、共有された配列を書き潰すと
// 兄弟どうしで位置が入れ替わる。**別の値を消しかねない。**
func TestTrimHandlesManyTargetsInOneLine(t *testing.T) {
	const a, b, c = "AAAAAAAA", "BBBBBBBB", "CCCCCCCC"
	raw := []byte(`{"message":{"content":[` +
		`{"type":"thinking","signature":"` + a + `"},` +
		`{"type":"text","text":"のこす"},` +
		`{"type":"thinking","signature":"` + b + `"},` +
		`{"type":"thinking","signature":"` + c + `"}]}}`)

	out, removed, changed, err := Trim(raw, []Rule{RuleThinkingSignature})
	if err != nil {
		t.Fatal(err)
	}
	if !changed || removed != len(a)+len(b)+len(c) {
		t.Fatalf("落とした量が %d（%d を期待）", removed, len(a)+len(b)+len(c))
	}
	for _, s := range []string{a, b, c} {
		if strings.Contains(string(out), s) {
			t.Errorf("%s が残っている", s)
		}
	}
	if !strings.Contains(string(out), `"のこす"`) {
		t.Error("残すはずの本文が消えた")
	}
	if !json.Valid(out) {
		t.Errorf("JSON として壊れた: %s", out)
	}
	// 構造も保つ。ブロックは4つのまま。
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	msg := doc["message"].(map[string]any)
	if n := len(msg["content"].([]any)); n != 4 {
		t.Errorf("ブロックが %d 個になった", n)
	}
}

// 画像が複数あっても、二重のものだけを toolUseResult 側から落とす。
func TestTrimHandlesManyImages(t *testing.T) {
	const d1, d2, only = "IMAGEONE1111", "IMAGETWO2222", "ONLYINRESULT"
	img := func(d string) string {
		return `{"type":"image","source":{"type":"base64","data":"` + d + `"}}`
	}
	raw := []byte(`{"type":"user","message":{"content":[{"type":"tool_result","content":[` +
		img(d1) + `,` + img(d2) + `]}]},` +
		`"toolUseResult":{"content":[` + img(d1) + `,` + img(d2) + `,` + img(only) + `]}}`)

	out, removed, changed, err := Trim(raw, []Rule{RuleDuplicateImage})
	if err != nil {
		t.Fatal(err)
	}
	if !changed || removed != len(d1)+len(d2) {
		t.Fatalf("落とした量が %d（%d を期待）", removed, len(d1)+len(d2))
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	for _, h := range imageData(doc["message"], nil) {
		kept[h.data] = true
	}
	if !kept[d1] || !kept[d2] {
		t.Error("message.content 側の画像が消えた")
	}
	left := map[string]bool{}
	for _, h := range imageData(doc["toolUseResult"], nil) {
		left[h.data] = true
	}
	if left[d1] || left[d2] {
		t.Error("二重のぶんが toolUseResult に残っている")
	}
	if !left[only] {
		t.Error("片方にしか無い画像まで落とした")
	}
}
