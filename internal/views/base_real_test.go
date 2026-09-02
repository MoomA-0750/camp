package views

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
