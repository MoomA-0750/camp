package retain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// Rule は raw_json の中から落とす部分の決め方。
//
// **行ごとでも列ごとでもなく、行の中の一部分を落とす。** 対象にしているのは
// 「誰も読めないもの」と「同じ行の中で二重になっているもの」で、どちらも
// 消しても画面と検索の見え方が変わらない（索引に入っていない）。
type Rule string

const (
	// RuleThinkingSignature は thinking の signature を落とす。
	//
	// 本文（thinking フィールド）は元から空で、中身は暗号化された signature に
	// 入っている。Anthropic 側の鍵なので Camp も本人も復号できない。
	// 実測（2026-09-03）で 3,099 ブロック・6.4 MB、本文は合計 4.7 KB しかない。
	RuleThinkingSignature Rule = "thinking-signature"

	// RuleDuplicateImage は toolUseResult 側の画像だけを落とす。
	//
	// スクリーンショットは message.content と toolUseResult に**まるごと2回**
	// 入っている。実測で 105 枚・base64 19.7 MB が二重。message.content 側は
	// 残すので、画像そのものは失われない。
	RuleDuplicateImage Rule = "duplicate-image"
)

// Trim は raw_json から rule の対象を落とした新しいバイト列を返す。
//
// **触らない部分は1バイトも変えない。** JSON を読み直して書き戻す実装だと、
// キーの順序も数値の表記も変わる。元のバイト列をそのまま持っているのが
// Camp の値打ちなので、落とす値の**位置**を求めて、そこだけ差し替える。
//
// 中身で探して置換する実装は使えない。同じ画像が2箇所にある行で、
// 残すべき message.content 側まで一緒に消えるため（テストで確認済み）。
func Trim(raw []byte, rules []Rule) (out []byte, removed int, changed bool, err error) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		// 読めない行は触らない。壊れた行を壊し直しても得がない。
		return raw, 0, false, fmt.Errorf("JSON として読めない: %w", err)
	}

	var paths [][]any
	for _, r := range rules {
		paths = append(paths, collect(doc, r)...)
	}
	if len(paths) == 0 {
		return raw, 0, false, nil
	}

	type span struct{ start, end int }
	var spans []span
	for _, p := range paths {
		s, e, ok := spanOf(raw, p)
		if !ok {
			// 位置が求まらない値は触らない。**当てずっぽうで消さない。**
			continue
		}
		if raw[s] != '"' || raw[e-1] != '"' {
			continue // 文字列以外は対象にしない
		}
		spans = append(spans, span{s, e})
	}
	if len(spans) == 0 {
		return raw, 0, false, nil
	}

	// 後ろから差し替える。前から詰めると以降の位置がずれる。
	sort.Slice(spans, func(i, j int) bool { return spans[i].start > spans[j].start })
	out = append([]byte(nil), raw...)
	for _, sp := range spans {
		removed += sp.end - sp.start - 2
		out = append(out[:sp.start], append([]byte(`""`), out[sp.end:]...)...)
	}
	if removed == 0 {
		return raw, 0, false, nil
	}
	// 差し替えた結果が JSON として読めることを必ず確かめる。
	if !json.Valid(out) {
		return raw, 0, false, fmt.Errorf("差し替えた結果が JSON として壊れた")
	}
	return out, removed, true, nil
}

// collect は rule の対象になる値の**位置**を集める。
// 位置はキー名と配列の添字の並びで表す。中身ではなく位置で指すのが要点。
func collect(doc any, r Rule) [][]any {
	switch r {
	case RuleThinkingSignature:
		var out [][]any
		walk(doc, nil, func(m map[string]any, path []any) {
			if m["type"] != "thinking" {
				return
			}
			if s, ok := m["signature"].(string); ok && s != "" {
				out = append(out, append(clone(path), "signature"))
			}
		})
		return out

	case RuleDuplicateImage:
		// **toolUseResult の下だけ**を見る。message.content 側は残す。
		top, ok := doc.(map[string]any)
		if !ok {
			return nil
		}
		sub, ok := top["toolUseResult"]
		if !ok {
			return nil
		}
		// message.content 側に同じ画像があることを確かめてから落とす。
		// 片方にしか無い画像を「重複」として消さないため。
		keep := map[string]bool{}
		for _, hit := range imageData(top["message"], nil) {
			keep[hit.data] = true
		}
		var out [][]any
		for _, hit := range imageData(sub, []any{"toolUseResult"}) {
			if keep[hit.data] {
				out = append(out, hit.path)
			}
		}
		return out
	}
	return nil
}

type imageHit struct {
	data string
	path []any
}

// imageData は部分木にある image ブロックの base64 と、その位置を集める。
func imageData(v any, prefix []any) []imageHit {
	var out []imageHit
	walk(v, prefix, func(m map[string]any, path []any) {
		if m["type"] != "image" {
			return
		}
		src, ok := m["source"].(map[string]any)
		if !ok {
			return
		}
		if s, ok := src["data"].(string); ok && s != "" {
			out = append(out, imageHit{data: s, path: append(clone(path), "source", "data")})
		}
	})
	return out
}

// walk は部分木のすべてのオブジェクトに、その位置つきで fn を当てる。
func walk(v any, path []any, fn func(map[string]any, []any)) {
	switch t := v.(type) {
	case map[string]any:
		fn(t, path)
		for k, val := range t {
			walk(val, append(path, k), fn)
		}
	case []any:
		for i, val := range t {
			walk(val, append(path, i), fn)
		}
	}
}

func clone(p []any) []any { return append([]any(nil), p...) }

// spanOf は path が指す値の、元のバイト列での範囲を返す。
func spanOf(raw []byte, path []any) (int, int, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	return navigate(dec, raw, path)
}

func navigate(dec *json.Decoder, raw []byte, path []any) (int, int, bool) {
	if len(path) == 0 {
		start := dec.InputOffset()
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return 0, 0, false
		}
		end := dec.InputOffset()
		// InputOffset はキーを読んだ直後（`:` の手前）を指す。配列なら `,` の手前。
		// 値そのものの範囲にするため、空白と区切りを外す。
		for start < end && (isSpace(raw[start]) || raw[start] == ':' || raw[start] == ',') {
			start++
		}
		return int(start), int(end), true
	}

	tok, err := dec.Token()
	if err != nil {
		return 0, 0, false
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return 0, 0, false
	}

	switch key := path[0].(type) {
	case string:
		if d != '{' {
			return 0, 0, false
		}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return 0, 0, false
			}
			k, _ := kt.(string)
			if k == key {
				return navigate(dec, raw, path[1:])
			}
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return 0, 0, false
			}
		}
	case int:
		if d != '[' {
			return 0, 0, false
		}
		for i := 0; dec.More(); i++ {
			if i == key {
				return navigate(dec, raw, path[1:])
			}
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return 0, 0, false
			}
		}
	}
	return 0, 0, false
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
