package search

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/MoomA-0750/camp/internal/ingest"
	"github.com/MoomA-0750/camp/internal/store"
)

// 索引した文字列と、それを引くための問い合わせ式が対になっていることを見る。
// ここが食い違うと、エラーも警告も出ないまま常に0件になる。
func TestBuildMatch(t *testing.T) {
	cases := []struct{ in, want string }{
		{"設定", `"設定"`},
		{"全文検索", `"全文 文検 検索"`},
		{"管", `"管"*`},                  // 1文字は前方一致
		{"pgpool", `"pgpool"`},         // ASCII はそのまま
		{"pgpool 設定", `"pgpool" "設定"`}, // 空白区切りは AND
		// " は FTS5 の文字列リテラルの中で重ねて逃がす。bigram 側から見ると
		// " は CJK の連なりを切るので、引用 と 付き の2トークンになる。
		{`引用"付き`, `"引用 "" 付き"`},
	}
	for _, c := range cases {
		if got, _ := BuildMatch(c.in); got != c.want {
			t.Errorf("BuildMatch(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got, _ := BuildMatch("   "); got != "" {
		t.Errorf("空白だけは空の式であるべき: %q", got)
	}
}

func newDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "camp.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	return db
}

// 日本語の文を入れて、2文字の語で引けることを端から端まで確かめる。
// これが M6 の回帰テストの本体。既定トークナイザに戻すとここが落ちる。
func TestJapaneseTwoCharacterQuery(t *testing.T) {
	db := newDB(t)
	root := t.TempDir()
	dir := filepath.Join(root, "-nonexistent-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const sid = "12345678-2222-4333-8444-555555555555"

	line := func(uuid, min, text string) string {
		return fmt.Sprintf(
			`{"type":"user","uuid":"%s","sessionId":"%s","session_id":"r-%s",`+
				`"timestamp":"2026-09-02T00:%s:00.000Z","cwd":"/nonexistent/proj",`+
				`"message":{"role":"user","content":[{"type":"text","text":%q}]}}`+"\n",
			uuid, sid, sid, min, text)
	}
	body := line("u1", "01", "pgpool-IIの設定を見直して、認証の方式を変えた") +
		line("u2", "02", "サーバーの管理台帳を更新する") +
		line("u3", "03", "セッションの履歴をあとから引けるようにしたい") +
		// ツール結果も索引対象。ここが抜けると本文の数%しか引けない。
		`{"type":"user","uuid":"u4","sessionId":"` + sid + `","session_id":"r-` + sid + `",` +
		`"timestamp":"2026-09-02T00:04:00.000Z","cwd":"/nonexistent/proj",` +
		`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1",` +
		`"content":"認証に失敗しました"}]}}` + "\n"

	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}

	for _, w := range []string{"設定", "管理", "履歴", "認証"} {
		hits, err := Query(db, w, Opts{})
		if err != nil {
			t.Fatalf("%s: %v", w, err)
		}
		if len(hits) == 0 {
			t.Errorf("%q が0件。日本語の索引が効いていない", w)
		}
	}

	// 語をまたぐ並びは当たらない。bigram をフレーズとして問うているので
	// 「認証の方式」は当たり、「方式の認証」は当たらない。
	if hits, _ := Query(db, "認証の方式", Opts{}); len(hits) != 1 {
		t.Errorf("「認証の方式」が %d 件。1件であるべき", len(hits))
	}
	if hits, _ := Query(db, "方式の認証", Opts{}); len(hits) != 0 {
		t.Errorf("「方式の認証」が %d 件。0件であるべき", len(hits))
	}

	// kind で絞れる
	all, _ := Query(db, "認証", Opts{})
	only, _ := Query(db, "認証", Opts{Kind: "tool_result"})
	if len(all) != 2 || len(only) != 1 || only[0].Kind != "tool_result" {
		t.Fatalf("kind で絞れていない: 全部 %d 件 / tool_result %d 件", len(all), len(only))
	}

	// 抜粋は bigram 列ではなく元のテキストから作る
	if hits, _ := Query(db, "管理", Opts{}); len(hits) == 1 {
		if hits[0].Snippet != "サーバーの管理台帳を更新する" {
			t.Errorf("抜粋が元のテキストになっていない: %q", hits[0].Snippet)
		}
	}
}

func TestExcerptWindowsAroundTheHit(t *testing.T) {
	long := "前置き"
	for i := 0; i < 60; i++ {
		long += "あ"
	}
	long += "認証"
	for i := 0; i < 60; i++ {
		long += "い"
	}
	got := Excerpt(long, []string{"認証"}, 20)
	if len([]rune(got)) > 22 {
		t.Fatalf("抜粋が長すぎる: %d 文字", len([]rune(got)))
	}
	if !contains(got, "認証") {
		t.Fatalf("当たった語が抜粋に入っていない: %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
