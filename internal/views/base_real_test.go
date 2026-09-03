package views

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoomA-0750/camp/internal/store"
)

// realVault は実在の Vault があればそのパスを返す。無ければテストを飛ばす。
func realVault(t *testing.T) string {
	t.Helper()
	p := os.Getenv("CAMP_VAULT")
	if p == "" {
		p = filepath.Join(os.Getenv("HOME"), "Documents/git-cloned/Obsidian-Vault")
	}
	if _, err := os.Stat(p); err != nil {
		t.Skip("実 Vault が無い環境")
	}
	return p
}

// 実在する7ファイル・30ビューを全部読める。1つでも落ちたら失敗。
func TestParsesEveryRealBase(t *testing.T) {
	root := realVault(t)
	var files []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if strings.HasSuffix(p, ".base") {
			files = append(files, p)
		}
		return nil
	})
	if len(files) != 7 {
		t.Fatalf(".base は7つのはずが %d: %v", len(files), files)
	}

	views, kinds := 0, map[string]int{}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		b, err := ParseBase(f, body)
		if err != nil {
			t.Fatalf("%s を読めない: %v", f, err)
		}
		for _, v := range b.Views {
			views++
			kinds[v.Kind]++
			if v.Name == "" {
				t.Errorf("%s: 名前の無いビュー", f)
			}
			if v.Kind == "" {
				t.Errorf("%s/%s: 種別が無い", f, v.Name)
			}
		}
	}
	if views != 30 {
		t.Fatalf("ビューは30のはずが %d", views)
	}
	if kinds[KindTable] != 25 || kinds[KindLifeTracker] != 4 || kinds[KindCards] != 1 {
		t.Fatalf("種別の内訳が違う: %v", kinds)
	}
}

// 解釈しないキーを捨てない。捨てると Obsidian が書いた設定が
// Camp を経由しただけで消える。
func TestPassthroughKeysSurvive(t *testing.T) {
	root := realVault(t)
	body, err := os.ReadFile(filepath.Join(root, "Data/Health/Health.base"))
	if err != nil {
		t.Skip("Health.base が無い")
	}
	b, err := ParseBase("Health.base", body)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range b.Views {
		if v.Kind == KindLifeTracker && len(v.Extra) > 0 {
			found = true
		}
	}
	if !found {
		t.Error("life-tracker の設定（timeFrame・columnConfigs 等）を落としている")
	}
}

// 実在の30ビューが全部、実データで回る。
// **列の反転が効いていることを実測値で確かめる。**
func TestEveryRealViewRunsWithAllColumns(t *testing.T) {
	root := realVault(t)
	db := openRealDB(t)
	if db == nil {
		t.Skip("実 DB が無い")
	}
	bases, err := LoadBases(db, 1, func(rel string) ([]byte, error) {
		return os.ReadFile(filepath.Join(root, rel))
	})
	if err != nil {
		t.Fatal(err)
	}
	recs, err := LoadRecords(db, 1)
	if err != nil {
		t.Fatal(err)
	}

	views := 0
	var health *Result
	for _, b := range bases {
		for i := range b.Views {
			r, err := Run(b, &b.Views[i], recs)
			if err != nil {
				t.Fatalf("%s/%s: %v", b.Name, b.Views[i].Name, err)
			}
			views++
			if b.Name == "Health" && b.Views[i].Kind == KindTable {
				health = r
			}
		}
	}
	if views != 30 {
		t.Fatalf("30ビュー回るはずが %d", views)
	}

	if health == nil {
		t.Fatal("Health のテーブルが見つからない")
	}
	// Bases の許可リストだと11列。反転すると実在する102列が出る。
	if len(health.Columns) < 100 {
		t.Fatalf("Health の列が %d 列しかない（許可リストとして読んでいる）", len(health.Columns))
	}
	pinned := 0
	for _, c := range health.Columns {
		if c.Pinned {
			pinned++
		}
	}
	if pinned >= len(health.Columns) {
		t.Fatal("全部が pinned＝order: 以外が出ていない")
	}
	t.Logf("Health: %d 列（定義が挙げたのは %d、自動が %d）",
		len(health.Columns), pinned, len(health.Columns)-pinned)
}

// openRealDB は実 DB を開く。無ければ nil。
func openRealDB(t *testing.T) *store.DB {
	t.Helper()
	p := os.Getenv("CAMP_DB")
	if p == "" {
		p = filepath.Join(os.Getenv("HOME"), "Documents/git-cloned/camp/data/camp.sqlite")
	}
	if _, err := os.Stat(p); err != nil {
		return nil
	}
	db, err := store.Open(p)
	if err != nil {
		return nil
	}
	t.Cleanup(func() { db.Close() })

	// **ファイルがあることと、中身があることは別。**
	// M25.5 で本番DBは /var/lib/camp へ移った。リポジトリ側に空の
	// camp.sqlite が残っていると、Stat は通るのに中身が無く、
	// この試験が「実データで回らない」ではなく「表が無い」で落ちる。
	// 空なら実データ扱いしない（＝skip させる）。
	var n int
	if err := db.QueryRow(
		`select count(*) from sqlite_master where type='table' and name='notes'`,
	).Scan(&n); err != nil || n == 0 {
		return nil
	}
	return db
}
