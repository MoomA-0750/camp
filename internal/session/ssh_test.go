package session

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConf(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSSHConfigIsReadIncludingIncludes(t *testing.T) {
	dir := t.TempDir()
	writeConf(t, dir, "conf.d/extra", `
Host nas
  HostName 192.168.1.10
  User admin
  Port 2222
`)
	main := writeConf(t, dir, "config", `
# コメント
Host *
  ServerAliveInterval 30

Host tower
  HostName=tower.tail1234.ts.net
  User mooma
  IdentityFile ~/.ssh/id_ed25519

Host build ci
  HostName build.example

Include conf.d/*
`)
	hosts, err := ReadSSHConfig(main)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]SSHHost{}
	for _, h := range hosts {
		got[h.Alias] = h
	}
	if _, ok := got["*"]; ok {
		t.Error("ワイルドカードを接続先として取り込んでいる")
	}
	if h := got["tower"]; h.HostName != "tower.tail1234.ts.net" || h.User != "mooma" {
		t.Errorf("tower が読めていない: %+v", h)
	}
	if h := got["nas"]; h.HostName != "192.168.1.10" || h.Port != 2222 {
		t.Errorf("Include の先が読めていない: %+v", h)
	}
	// 1行に複数書いた Host は両方に効く。
	for _, a := range []string{"build", "ci"} {
		if got[a].HostName != "build.example" {
			t.Errorf("%s が読めていない: %+v", a, got[a])
		}
	}
}

// **`~/.ssh/config` は1バイトも変わらない。**（inner gate の名指し）
//
// Camp のバグで端末の SSH 設定が壊れると、直すのに Camp が要る、という
// 一番まずい形になる。
func TestReadingTheSSHConfigNeverChangesIt(t *testing.T) {
	dir := t.TempDir()
	body := "Host a\n  HostName a.example\n"
	p := writeConf(t, dir, "config", body)
	before := sha256.Sum256([]byte(body))
	st1, _ := os.Stat(p)

	db := newDB(t)
	s := New(db)
	a := attach(t, s, fakeClaude(t))
	a.SSHConfig = p

	added, updated, err := s.ScanSSH()
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || updated != 0 {
		t.Fatalf("取り込み結果が違う: added=%d updated=%d", added, updated)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != before {
		t.Fatal("読んだだけで中身が変わっている")
	}
	st2, _ := os.Stat(p)
	if st1.ModTime() != st2.ModTime() || st1.Mode() != st2.Mode() {
		t.Fatal("読んだだけで mtime か mode が変わっている")
	}
}

// **書き戻す経路をコードとして持たない。**
func TestNothingInTheSSHReaderCanWrite(t *testing.T) {
	b, err := os.ReadFile("sshconf.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"os.WriteFile", "os.Create", "O_WRONLY", "O_RDWR", "os.Remove", "os.Rename"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("sshconf.go に %s がある。**読むだけにする**", bad)
		}
	}
}

// 取り込み直しても、人が決めた許可は変わらない。
func TestImportingAgainDoesNotChangeWhatWasAllowed(t *testing.T) {
	db := newDB(t)
	if _, _, err := ImportSSH(db, []SSHHost{{Alias: "tower", HostName: "old"}}); err != nil {
		t.Fatal(err)
	}
	if err := SetDestinationAllowed(db, "tower", true); err != nil {
		t.Fatal(err)
	}
	// 設定ファイルが変わった体で取り込み直す。
	if _, updated, err := ImportSSH(db, []SSHHost{{Alias: "tower", HostName: "new"}}); err != nil || updated != 1 {
		t.Fatalf("updated=%d err=%v", updated, err)
	}
	list, err := ListDestinations(db)
	if err != nil || len(list) != 1 {
		t.Fatalf("台帳が読めない: %+v %v", list, err)
	}
	if list[0].HostName != "new" {
		t.Fatal("HostName が更新されていない")
	}
	if !list[0].Allowed {
		t.Fatal("取り込みで許可が落ちた。**許可は人が決めたこと**")
	}
}

// 取り込んだだけでは繋げない（既定は deny）。
func TestAnImportedDestinationIsNotAllowedYet(t *testing.T) {
	db := newDB(t)
	if _, _, err := ImportSSH(db, []SSHHost{{Alias: "x"}}); err != nil {
		t.Fatal(err)
	}
	list, _ := ListDestinations(db)
	if len(list) != 1 || list[0].Allowed {
		t.Fatalf("取り込んだだけで許可されている: %+v", list)
	}
	_ = time.Now
}
