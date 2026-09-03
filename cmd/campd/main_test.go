package main

import (
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
		err := checkNotAGhostDB(cmd, missing)
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

// 作ってよいものは通す。
func TestTheCommandsThatMayCreateADBStillPass(t *testing.T) {
	t.Setenv("CAMP_DB", "")
	missing := filepath.Join(t.TempDir(), "camp.sqlite")
	for _, cmd := range []string{"migrate", "passwd", "version"} {
		if err := checkNotAGhostDB(cmd, missing); err != nil {
			t.Errorf("%s が止められた: %v", cmd, err)
		}
	}
}

// 明示的に -db で指した場所には口を出さない。
func TestAnExplicitPathIsNotSecondGuessed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "camp.sqlite")
	t.Setenv("CAMP_DB", missing)
	if err := checkNotAGhostDB("doctor", missing); err != nil {
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
	if err := checkNotAGhostDB("doctor", p); err != nil {
		t.Errorf("既にあるのに止められた: %v", err)
	}
}
