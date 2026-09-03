package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 空のDBを黙って作って、それについて報告してはいけない。
//
// 2026-09-04 実測: 本番DBを /var/lib/camp へ移したあと、リポジトリで
// `campd doctor` を引数なしに叩くと data/camp.sqlite が新しく作られ、
// 空のDBに対する点検結果が出た。**中身が無いから ok なのを、
// 中身が正しいから ok と読み違える。**
func TestABareCommandDoesNotConjureAnEmptyDB(t *testing.T) {
	t.Setenv("CAMP_DB", "")
	missing := filepath.Join(t.TempDir(), "camp.sqlite")

	for _, cmd := range []string{"doctor", "search", "audit", "ingest", "retain"} {
		err := checkNotAGhostDB(cmd, "", missing)
		if err == nil {
			t.Errorf("%s が、無いDBをそのまま開こうとしている", cmd)
			continue
		}
		// どちらの道も出す。境界が効いていると /var/lib/camp の有無は
		// このプロセスからは確かめられないので、両方案内する。
		if !strings.Contains(err.Error(), systemDBPath) {
			t.Errorf("%s: 常駐しているほうへの案内が無い: %v", cmd, err)
		}
		if !strings.Contains(err.Error(), "migrate") {
			t.Errorf("%s: 新しく作る道の案内が無い: %v", cmd, err)
		}
	}
}

// DB を開かないもの、自分で報告口へ回すものは止めない。
// **止める理由が無いのに止めると、機能が黙って死ぬ。**
// 2026-09-04 に campd report と statusLine のフックを実際に止めてしまった。
func TestCommandsThatNeverTouchTheDBAreNotBlocked(t *testing.T) {
	t.Setenv("CAMP_DB", "")
	missing := filepath.Join(t.TempDir(), "camp.sqlite")
	for _, tc := range []struct{ cmd, sub string }{
		{"report", ""},       // socket にしか触らない
		{"scan", ""},         // 会話記録を歩くだけ
		{"limits", "record"}, // DBを開けなければ報告口へ回す
		{"vault", "scan"},    // Vault を歩くだけ
	} {
		if err := checkNotAGhostDB(tc.cmd, tc.sub, missing); err != nil {
			t.Errorf("%s %s が止められた: %v", tc.cmd, tc.sub, err)
		}
	}
	// 同じ入口でも DB を読むほうは止める。
	for _, tc := range []struct{ cmd, sub string }{
		{"limits", "show"}, {"vault", "index"}, {"vault", "ghosts"},
	} {
		if err := checkNotAGhostDB(tc.cmd, tc.sub, missing); err == nil {
			t.Errorf("%s %s が素通りしている", tc.cmd, tc.sub)
		}
	}
}

// 作ってよいものは通す。
func TestTheCommandsThatMayCreateADBStillPass(t *testing.T) {
	t.Setenv("CAMP_DB", "")
	missing := filepath.Join(t.TempDir(), "camp.sqlite")
	for _, cmd := range []string{"migrate", "passwd", "version"} {
		if err := checkNotAGhostDB(cmd, "", missing); err != nil {
			t.Errorf("%s が止められた: %v", cmd, err)
		}
	}
}

// 明示的に -db で指した場所には口を出さない。
func TestAnExplicitPathIsNotSecondGuessed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "camp.sqlite")
	t.Setenv("CAMP_DB", missing)
	if err := checkNotAGhostDB("doctor", "", missing); err != nil {
		t.Errorf("CAMP_DB で指したのに止められた: %v", err)
	}
	if !hasFlag([]string{"-db", "/x"}, "-db") || !hasFlag([]string{"-db=/x"}, "-db") {
		t.Error("-db を見落としている")
	}
	if hasFlag([]string{"-v", "-fix"}, "-db") {
		t.Error("-db が無いのに有ると言っている")
	}
}

// 既にあるDBは当然通す。
func TestAnExistingDBIsFine(t *testing.T) {
	t.Setenv("CAMP_DB", "")
	p := filepath.Join(t.TempDir(), "camp.sqlite")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkNotAGhostDB("doctor", "", p); err != nil {
		t.Errorf("既にあるのに止められた: %v", err)
	}
}

// 残量の渡し先は、cwd にある古い DB より報告口を優先する。
//
// 2026-09-04: statusLine のフックが古いパスへ書き続けて、本番へ1件も
// 届いていなかった。**届いていないのに届いたつもりになる**のが一番まずい。
func TestLimitsPrefersTheDaemonOverAStrayLocalDB(t *testing.T) {
	dir := t.TempDir()
	stray := filepath.Join(dir, "camp.sqlite")
	if err := os.WriteFile(stray, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "report.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	if got := limitsTarget(false, sock, stray); got != targetSocket {
		t.Errorf("報告口があるのに %s を選んだ。本番へ届かない", got)
	}
	if got := limitsTarget(true, sock, stray); got != targetDB {
		t.Errorf("-db で指したのに %s を選んだ", got)
	}
	if got := limitsTarget(false, "", stray); got != targetDB {
		t.Errorf("報告口が無いのに %s を選んだ", got)
	}
	if got := limitsTarget(false, filepath.Join(dir, "none.sock"),
		filepath.Join(dir, "none.sqlite")); got != targetSocket {
		t.Errorf("どちらも無いときは報告口を試すべきだが %s", got)
	}
}
