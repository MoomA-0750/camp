package views

import (
	"fmt"
	"testing"

	"github.com/MoomA-0750/camp/internal/store"
)

// 合流の読み手（M50、2026-09-13）。**DB の独自定義を優先し、定義の無い台紙だけ `.base` に落ちる。**
//
// **落ちるのは台紙単位。ビュー単位にしない。** ビュー単位だと、独自定義から**消したビューが
// `.base` から復活する**（設計レビューの指摘6）。ここはレビューが名指しで警告した規則なので、
// 行動で縛る。

// seedBaseNotes は `.base` のノートの行を作る（`LoadBases` は `notes` を引くので、
// 行が無いとファイルを読みに行かない）。
func seedBaseNotes(t *testing.T, db *store.DB, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := db.Exec(
			`insert into notes(vault_id, path, kind) values(1, ?, 'base')`, p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadPrefersTheDatabaseDefinitionPerBase(t *testing.T) {
	db := defDB(t)
	seedBaseNotes(t, db, "D/A.base", "D/B.base")

	read := func(rel string) ([]byte, error) {
		switch rel {
		case "D/A.base":
			// `.base` 側には2本ある。うち1本は独自定義から消してある。
			return []byte("views:\n  - type: table\n    name: keep\n" +
				"  - type: table\n    name: deleted\n"), nil
		case "D/B.base":
			return []byte("views:\n  - type: table\n    name: onlyBase\n"), nil
		}
		return nil, fmt.Errorf("無い: %s", rel)
	}

	// A だけ独自定義を入れる（`deleted` は意図的に含めない）。
	if err := SaveDef(db, 1, &Def{Base: "A",
		Body: "base: A\nviews:\n  - name: keep\n    kind: table\n"}, ByUser, false); err != nil {
		t.Fatal(err)
	}

	bs, err := Load(db, 1, read)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 2 {
		t.Fatalf("台紙が %d 枚（A と B の2枚のはず）: %+v", len(bs), bs)
	}
	byName := map[string]*Base{}
	for _, b := range bs {
		byName[b.Name] = b
	}
	a, b := byName["A"], byName["B"]
	if a == nil || b == nil {
		t.Fatalf("台紙が揃わない: %v", byName)
	}

	// **A は DB 側だけを見る。** 消したビューが `.base` から復活しない。
	if !FromDB(a) {
		t.Fatalf("A が DB の定義から来ていない: %q", a.Path)
	}
	if len(a.Views) != 1 || a.Views[0].Name != "keep" {
		t.Fatalf("A のビューが DB の定義どおりでない（消したものが復活した？）: %+v", a.Views)
	}

	// **B は定義が無いので `.base` に落ちる。**
	if FromDB(b) {
		t.Fatalf("B が DB から来ている（定義は無いはず）: %q", b.Path)
	}
	if len(b.Views) != 1 || b.Views[0].Name != "onlyBase" {
		t.Fatalf("B が `.base` どおりでない: %+v", b.Views)
	}

	// 台紙は名前順（画面の並びが実行ごとに変わらない）。
	if bs[0].Name != "A" || bs[1].Name != "B" {
		t.Fatalf("並びが名前順でない: %s, %s", bs[0].Name, bs[1].Name)
	}
}

// 定義が1枚も無ければ、全部 `.base` から来る（併読を始める前の状態）。
func TestLoadFallsBackEntirelyWhenNothingIsConverted(t *testing.T) {
	db := defDB(t)
	seedBaseNotes(t, db, "D/A.base")
	read := func(string) ([]byte, error) {
		return []byte("views:\n  - type: table\n    name: v\n"), nil
	}
	bs, err := Load(db, 1, read)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 1 || FromDB(bs[0]) {
		t.Fatalf("`.base` に落ちていない: %+v", bs)
	}
}

// 読めない定義が入っていても、その台紙は `.base` へ落ちない。
//
// **落としてはいけない。** 落とすと、壊れた定義を直す前に `.base` の古い姿が出て、
// 「直したのに変わらない」ように見える。読めないことを見せるほうが正しい。
func TestAnUnreadableDefinitionDoesNotSilentlyFallBack(t *testing.T) {
	db := defDB(t)
	seedBaseNotes(t, db, "D/A.base")
	if err := SaveDef(db, 1, &Def{Base: "A",
		Body: "base: A\nviews:\n  - name: v\n    kind: table\n"}, ByUser, false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`update views set def = 'views: [' where base = 'A'`); err != nil {
		t.Fatal(err)
	}
	read := func(string) ([]byte, error) {
		return []byte("views:\n  - type: table\n    name: fromBase\n"), nil
	}
	bs, err := Load(db, 1, read)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 1 {
		t.Fatalf("台紙が %d 枚: %+v", len(bs), bs)
	}
	if bs[0].ParseError == "" {
		t.Fatalf("読めない定義が `.base` に置き換わった: %+v", bs[0])
	}
}
