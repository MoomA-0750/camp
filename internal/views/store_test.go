package views

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoomA-0750/camp/internal/store"
)

// 定義を DB に持つ（Phase 4 / M50、2026-09-13。移行 0030）。

func defDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	// `vaults.host_id` は `hosts` を参照する（移行 0011）。ほかのテストと同じ順で入れる。
	for _, q := range []string{
		`insert into hosts(id, name) values(1, 'h')`,
		`insert into vaults(id, host_id, name, root) values(1, 1, 'v', '/v')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

const oneView = "base: T\nviews:\n  - name: v\n    kind: table\n"

// 書いて読める。履歴が残る。
func TestADefinitionIsSavedWithItsHistory(t *testing.T) {
	db := defDB(t)
	d := &Def{Base: "T", Body: oneView, Origin: "Human/Dashboards/T.base",
		OriginSHA256: SHA256([]byte("x"))}
	if err := SaveDef(db, 1, d, ByConvert, false); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDef(db, 1, "T")
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != oneView || got.Origin != d.Origin || got.OriginSHA256 != d.OriginSHA256 {
		t.Fatalf("読み戻せない: %+v", got)
	}
	if got.ConvertedAt == "" || got.UpdatedAt == "" {
		t.Fatalf("時刻が入らない: %+v", got)
	}
	h, err := History(db, 1, "T", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 1 || h[0].By != ByConvert {
		t.Fatalf("履歴が残らない: %+v", h)
	}
	// 回せる形で読める。
	bs, err := LoadNativeDefs(db, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 1 || bs[0].Name != "T" || len(bs[0].Views) != 1 {
		t.Fatalf("回せる形にならない: %+v", bs)
	}
}

// **再変換は手編集を黙って踏み潰さない**（設計レビューの指摘6）。
func TestConvertingAgainDoesNotOverwriteAHandEdit(t *testing.T) {
	db := defDB(t)
	if err := SaveDef(db, 1, &Def{Base: "T", Body: oneView}, ByConvert, false); err != nil {
		t.Fatal(err)
	}
	edited := "base: T\nviews:\n  - name: v\n    kind: cards\n"
	if err := SaveDef(db, 1, &Def{Base: "T", Body: edited, BaseSHA256: SHA256([]byte(oneView))}, ByUser, false); err != nil {
		t.Fatal(err)
	}
	err := SaveDef(db, 1, &Def{Base: "T", Body: oneView}, ByConvert, false)
	if !errors.Is(err, HandEdited) {
		t.Fatalf("手編集を上書きした: %v", err)
	}
	if got, _ := LoadDef(db, 1, "T"); got.Body != edited {
		t.Fatal("手編集が消えた")
	}
	// force なら書ける（本人が明示したとき）。
	if err := SaveDef(db, 1, &Def{Base: "T", Body: oneView}, ByConvert, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadDef(db, 1, "T"); got.Body != oneView {
		t.Fatal("force でも書けない")
	}
	h, _ := History(db, 1, "T", 10)
	if len(h) != 3 {
		t.Fatalf("履歴が %d 件（3件のはず）", len(h))
	}
}

// **手で直しても、どこから変換したかは消えない**（2026-09-13、実ブラウザで見つけた不具合）。
//
// 消えると画面は「変換元なし」と嘘を言い、`-compare` は `origin_sha256` を失って
// 「`.base` が変換後に変わったか」を見分けられなくなる——最初の手編集で守りが黙って外れる。
func TestAHandSaveKeepsWhereTheDefinitionCameFrom(t *testing.T) {
	db := defDB(t)
	sha := SHA256([]byte("元の .base"))
	if err := SaveDef(db, 1, &Def{Base: "T", Body: oneView, Origin: "D/T.base",
		OriginSHA256: sha}, ByConvert, false); err != nil {
		t.Fatal(err)
	}
	edited := "base: T\nviews:\n  - name: v\n    kind: cards\n"
	if err := SaveDef(db, 1, &Def{Base: "T", Body: edited, BaseSHA256: SHA256([]byte(oneView))}, ByUser, false); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDef(db, 1, "T")
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != edited {
		t.Fatalf("手の保存が効いていない: %q", got.Body)
	}
	if got.Origin != "D/T.base" || got.OriginSHA256 != sha {
		t.Fatalf("手の保存で変換元が消えた: origin=%q sha=%q", got.Origin, got.OriginSHA256)
	}
	if got.ConvertedAt == "" {
		t.Fatal("手の保存で変換の時刻が消えた")
	}
}

// **読めない定義は保存しない。** 入れてしまうと画面から直せなくなる。
func TestABrokenDefinitionIsRefused(t *testing.T) {
	db := defDB(t)
	for _, bad := range []string{
		"base: U\nviews: []\n",               // 名前の食い違い
		"base: T\nviews:\n  - kind: table\n", // name が無い
		"views: [",                           // YAML が壊れている
	} {
		if err := SaveDef(db, 1, &Def{Base: "T", Body: bad}, ByUser, false); err == nil {
			t.Fatalf("壊れた定義を保存した: %q", bad)
		}
	}
	if _, err := LoadDef(db, 1, "T"); !errors.Is(err, ErrNoDef) {
		t.Fatalf("行ができてしまった: %v", err)
	}
}

// 読めない定義が DB に入っていても、他の台紙は読める（1枚で全部を落とさない）。
func TestOneUnreadableDefinitionDoesNotSinkTheRest(t *testing.T) {
	db := defDB(t)
	if err := SaveDef(db, 1, &Def{Base: "T", Body: oneView}, ByUser, false); err != nil {
		t.Fatal(err)
	}
	// 検査を通さずに直に壊す（外から書き換わった状況）。
	if _, err := db.Exec(`update views set def = 'views: [' where base = 'T'`); err != nil {
		t.Fatal(err)
	}
	if err := SaveDef(db, 1, &Def{Base: "U", Body: strings.ReplaceAll(oneView, "T", "U")},
		ByUser, false); err != nil {
		t.Fatal(err)
	}
	bs, err := LoadNativeDefs(db, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 2 {
		t.Fatalf("台紙が %d 枚（2枚のはず）", len(bs))
	}
	var broken, ok int
	for _, b := range bs {
		if b.ParseError != "" {
			broken++
		} else {
			ok++
		}
	}
	if broken != 1 || ok != 1 {
		t.Fatalf("読めない1枚で全部落ちた: broken=%d ok=%d", broken, ok)
	}
}

// **読んだ版の上にしか書かない**（実装後レビュー、2026-09-13。codex の指摘2）。2つのタブが同じ版を
// 読んで順に保存すると、後の保存が先の保存を黙って消していた。
func TestAStaleSaveIsRefused(t *testing.T) {
	db := defDB(t)
	if err := SaveDef(db, 1, &Def{Base: "T", Body: oneView}, ByConvert, false); err != nil {
		t.Fatal(err)
	}
	read := SHA256([]byte(oneView)) // 2つのタブが同じ版を読む
	first := "base: T\nviews:\n  - name: v\n    kind: cards\n"
	if err := SaveDef(db, 1, &Def{Base: "T", Body: first, BaseSHA256: read}, ByUser, false); err != nil {
		t.Fatal(err)
	}
	second := "base: T\nviews:\n  - name: w\n    kind: table\n"
	if err := SaveDef(db, 1, &Def{Base: "T", Body: second, BaseSHA256: read}, ByUser, false); !IsConflict(err) {
		t.Fatalf("古い版の上に書けた: %v", err)
	}
	if got, _ := LoadDef(db, 1, "T"); got.Body != first {
		t.Fatalf("先の保存が消えた: %q", got.Body)
	}
	// 版を持たずに既にある定義へ書くのも断る（読まずに書いている）。
	if err := SaveDef(db, 1, &Def{Base: "T", Body: second}, ByUser, false); !IsConflict(err) {
		t.Fatalf("版を持たない保存が通った: %v", err)
	}
}

// **再変換が守るのは `.base` で言える部分の手編集だけ**（本人の決定、2026-09-13）。
// 時間軸・集約を足しただけの台紙は再変換で飛ばさない（引き継ぎは CarryOver が受け持つ）。
func TestOnlyEditsTheBaseCanExpressBlockReconversion(t *testing.T) {
	db := defDB(t)
	conv := "base: T\nviews:\n  - name: v\n    kind: chart\n    shape:\n      measures: {x: only}\n" +
		"    emit:\n      human:\n        - {kind: chart, values: [x], chart: line}\n"
	if err := SaveDef(db, 1, &Def{Base: "T", Body: conv}, ByConvert, false); err != nil {
		t.Fatal(err)
	}
	timed := strings.Replace(conv, "measures: {x: only}", "time: {axis: date, bucket: day}\n      measures: {x: last}", 1)
	if err := SaveDef(db, 1, &Def{Base: "T", Body: timed, BaseSHA256: SHA256([]byte(conv))}, ByUser, false); err != nil {
		t.Fatal(err)
	}
	if edited, _ := HandEditedDef(db, 1, "T"); edited {
		t.Fatal("時間軸と集約を足しただけで手編集扱いになった")
	}
	// 描き方（.base の columnConfigs で言える）を直すと手編集。
	barred := strings.Replace(timed, "chart: line", "chart: bar", 1)
	if err := SaveDef(db, 1, &Def{Base: "T", Body: barred, BaseSHA256: SHA256([]byte(timed))}, ByUser, false); err != nil {
		t.Fatal(err)
	}
	if edited, _ := HandEditedDef(db, 1, "T"); !edited {
		t.Fatal("線を棒に直したのに手編集扱いにならない")
	}
	if err := SaveDef(db, 1, &Def{Base: "T", Body: conv}, ByConvert, false); !errors.Is(err, HandEdited) {
		t.Fatalf("描き方の手編集を再変換が上書きした: %v", err)
	}
	// 変換が一度も書いていない（手で書いた）台紙は守る。
	if err := SaveDef(db, 1, &Def{Base: "U", Body: strings.ReplaceAll(oneView, "T", "U")}, ByUser, false); err != nil {
		t.Fatal(err)
	}
	if edited, _ := HandEditedDef(db, 1, "U"); !edited {
		t.Fatal("手で書いた台紙が手編集扱いにならない")
	}
}

// 履歴は定義の行と寿命を分ける（codex の指摘10）。行を消しても戻り先が残る。
// 定義の版（控えの鍵）は書くたびに進む（codex の指摘8）。
func TestHistoryOutlivesTheDefinitionAndRevisionMoves(t *testing.T) {
	db := defDB(t)
	r0, _ := Revision(db, 1)
	if err := SaveDef(db, 1, &Def{Base: "T", Body: oneView}, ByConvert, false); err != nil {
		t.Fatal(err)
	}
	r1, _ := Revision(db, 1)
	if r1 <= r0 {
		t.Fatalf("書いても版が進まない: %d → %d", r0, r1)
	}
	// 変わらない再変換は書かない（履歴を埋めない）。版も進まない。
	if err := SaveDef(db, 1, &Def{Base: "T", Body: oneView}, ByConvert, false); err != nil {
		t.Fatal(err)
	}
	if r2, _ := Revision(db, 1); r2 != r1 {
		t.Fatalf("何も変わらない再変換で履歴が増えた: %d → %d", r1, r2)
	}
	if _, err := db.Exec(`delete from views where base = 'T'`); err != nil {
		t.Fatal(err)
	}
	if h, _ := History(db, 1, "T", 10); len(h) != 1 {
		t.Fatalf("定義の行を消したら履歴も消えた: %d 件", len(h))
	}
}
