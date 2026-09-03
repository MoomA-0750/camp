package snapshot

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
)

// testIter は鍵の導出回数。形式の検証に 600,000 回は要らない。
// 本番の経路（Create / Restore）は必ず iterations を通る。
const testIter = 100

func roundtrip(t *testing.T, plain []byte, key string) {
	t.Helper()
	var enc bytes.Buffer
	if _, err := encryptWith(bytes.NewReader(plain), &enc, []byte(key), testIter); err != nil {
		t.Fatal(err)
	}
	var dec bytes.Buffer
	n, err := decrypt(bytes.NewReader(enc.Bytes()), &dec, []byte(key))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(plain)) {
		t.Errorf("戻したバイト数が %d（%d を期待）", n, len(plain))
	}
	if !bytes.Equal(dec.Bytes(), plain) {
		t.Error("戻したものが元と違う")
	}
}

// 大きさによらず、取って戻したら元に戻る。
// かたまりの境目ちょうどが一番怪しいので、そこを厚めに見る。
func TestItComesBackTheSameAtEverySize(t *testing.T) {
	for _, n := range []int{0, 1, 100, chunkSize - 1, chunkSize, chunkSize + 1, chunkSize*2 + 7} {
		buf := make([]byte, n)
		if _, err := rand.Read(buf); err != nil {
			t.Fatal(err)
		}
		t.Run(strings.TrimSpace(byteLabel(n)), func(t *testing.T) { roundtrip(t, buf, "とても長い鍵の文字列") })
	}
}

func byteLabel(n int) string {
	switch {
	case n == chunkSize:
		return "chunk ちょうど"
	case n == chunkSize-1:
		return "chunk-1"
	case n == chunkSize+1:
		return "chunk+1"
	}
	return string(rune('a'+n%26)) + "_" + itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// 鍵が違えば開かない。
func TestTheWrongKeyDoesNotOpenIt(t *testing.T) {
	plain := bytes.Repeat([]byte("なかみ"), 1000)
	var enc bytes.Buffer
	if _, err := encryptWith(bytes.NewReader(plain), &enc, []byte("ただしい鍵"), testIter); err != nil {
		t.Fatal(err)
	}
	var dec bytes.Buffer
	if _, err := decrypt(bytes.NewReader(enc.Bytes()), &dec, []byte("ちがう鍵")); err == nil {
		t.Fatal("違う鍵で開いた")
	}
	if dec.Len() != 0 {
		t.Error("開けていないのに中身を書き出した")
	}
}

// **後ろを切り落としたファイルを、正しい復元として通さない。**
//
// 静かに欠けたバックアップは、取れていないバックアップより悪い。
// 取れているつもりで、戻したときに初めて足りないと分かる。
func TestATruncatedFileIsRefused(t *testing.T) {
	plain := make([]byte, chunkSize*2+50)
	if _, err := rand.Read(plain); err != nil {
		t.Fatal(err)
	}
	var enc bytes.Buffer
	if _, err := encryptWith(bytes.NewReader(plain), &enc, []byte("かぎ"), testIter); err != nil {
		t.Fatal(err)
	}
	// 最後のかたまりを丸ごと落とす。
	cut := enc.Bytes()[:headerLen+(chunkSize+16)*2]
	var dec bytes.Buffer
	_, err := decrypt(bytes.NewReader(cut), &dec, []byte("かぎ"))
	if err == nil {
		t.Fatal("切り落としたファイルを正しいものとして通した")
	}
	if !strings.Contains(err.Error(), "切れた") {
		t.Errorf("理由が伝わらない: %v", err)
	}
}

// 1バイトでも書き換わっていたら開かない。
func TestAFlippedByteIsCaught(t *testing.T) {
	plain := bytes.Repeat([]byte("x"), 5000)
	var enc bytes.Buffer
	if _, err := encryptWith(bytes.NewReader(plain), &enc, []byte("かぎ"), testIter); err != nil {
		t.Fatal(err)
	}
	b := enc.Bytes()
	b[headerLen+100] ^= 0x01
	var dec bytes.Buffer
	if _, err := decrypt(bytes.NewReader(b), &dec, []byte("かぎ")); err == nil {
		t.Fatal("書き換わったファイルを通した")
	}
}

// Camp のものでないファイルは、はっきり断る。
func TestItRefusesSomethingElse(t *testing.T) {
	var dec bytes.Buffer
	_, err := decrypt(strings.NewReader("これはただのテキストファイルです。長さだけはある。"), &dec, []byte("かぎ"))
	if err == nil {
		t.Fatal("よそのファイルを開こうとした")
	}
	if !strings.Contains(err.Error(), "Camp") {
		t.Errorf("理由が伝わらない: %v", err)
	}
}

// 同じ中身を2回取っても、暗号文は毎回違う（salt と nonce が毎回変わる）。
func TestTwoBackupsOfTheSameThingDiffer(t *testing.T) {
	plain := bytes.Repeat([]byte("おなじなかみ"), 500)
	var a, b bytes.Buffer
	if _, err := encryptWith(bytes.NewReader(plain), &a, []byte("かぎ"), testIter); err != nil {
		t.Fatal(err)
	}
	if _, err := encryptWith(bytes.NewReader(plain), &b, []byte("かぎ"), testIter); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Error("2回とも同じ暗号文。salt か nonce を使い回している")
	}
}
