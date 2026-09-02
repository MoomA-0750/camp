package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MoomA-0750/camp/internal/store"
)

// ビューの材料を持つ Vault を1つ作る。
func seedVault(t *testing.T, db *store.DB, bases map[string]string, notes int) {
	t.Helper()
	root := t.TempDir()
	if _, err := db.Exec(
		`insert into vaults(id,host_id,name,root,scanned_at) values(1,1,'v',?,'s')`,
		root); err != nil {
		t.Fatal(err)
	}
	id := int64(1)
	for name, body := range bases {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`insert into notes(id,vault_id,path,title,kind,ext,mtime)
			values(?,1,?,?,'base','.base','2026-01-01T00:00:00Z')`,
			id, name, strings.TrimSuffix(name, ".base")); err != nil {
			t.Fatal(err)
		}
		id++
	}
	for i := 0; i < notes; i++ {
		if _, err := db.Exec(`insert into notes(id,vault_id,path,title,kind,ext,mtime)
			values(?,1,?,?,'markdown','.md',?)`,
			id, "n/"+itoa(i)+".md", itoa(i),
			"2026-01-"+pad(i%28+1)+"T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(
			`insert into note_props(note_id,key,seq,text,num) values(?,'a',0,?,?)`,
			id, itoa(i), i); err != nil {
			t.Fatal(err)
		}
		id++
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
func pad(i int) string {
	if i < 10 {
		return "0" + itoa(i)
	}
	return itoa(i)
}

const okBase = "views:\n  - type: table\n    name: t\n    order: [file.name, a]\n"

// **同時に叩いても campd が死なないこと。**
//
// 共有の Record を書き換えていたころは、`/api/views` を数本同時に投げると
// Go ランタイムの `fatal error: concurrent map writes` が出た。fatal error は
// recover できないので、プロセス全体が落ちる。Phase 3 では子プロセスと
// 承認待ちを抱えたまま落ちることになる。
func TestViewsSurviveConcurrentRequests(t *testing.T) {
	srv, db := newServer(t)
	seedVault(t, db, map[string]string{"A.base": okBase, "B.base": okBase}, 400)
	c := loggedIn(t, srv)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 15; j++ {
				url := srv.URL + "/api/views"
				if (i+j)%2 == 0 {
					url = srv.URL + "/api/views/A/t"
				}
				res, err := c.Get(url)
				if err != nil {
					t.Error(err)
					return
				}
				io.Copy(io.Discard, res.Body)
				res.Body.Close()
				if res.StatusCode != http.StatusOK {
					t.Errorf("%s: %d", url, res.StatusCode)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

// **読めない `.base` が1つあっても、他のビューは出ること。**
//
// 旧実装は1ファイルのパース失敗で LoadBases 全体を error にしていたので、
// Obsidian 側でフィルタに NOT を1つ足すだけで、画面もMCPも全滅した。
func TestOneBrokenBaseDoesNotKillTheRest(t *testing.T) {
	srv, db := newServer(t)
	seedVault(t, db, map[string]string{
		"Good.base": okBase,
		"Bad.base":  "filters:\n  なにこれ:\n    - a == 1\nviews:\n  - type: table\n    name: t\n",
	}, 10)
	c := loggedIn(t, srv)

	res, err := c.Get(srv.URL + "/api/views")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var got []struct {
		Base, Name, Error string
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	var good, bad bool
	for _, v := range got {
		if v.Base == "Good" && v.Error == "" {
			good = true
		}
		if v.Base == "Bad" && v.Error != "" {
			bad = true
		}
	}
	if !good {
		t.Error("読めない .base が1つあると、読める方まで出なくなっている")
	}
	if !bad {
		t.Error("読めなかったことを黙って隠している")
	}
}

// 大きな応答は畳む。ビューは1本で数MBになる。
func TestLargeResponsesAreCompressed(t *testing.T) {
	srv, db := newServer(t)
	seedVault(t, db, map[string]string{"A.base": okBase}, 500)
	c := loggedIn(t, srv)

	req, _ := http.NewRequest("GET", srv.URL+"/api/views/A/t", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("畳んでいない: %q", res.Header.Get("Content-Encoding"))
	}
	n, _ := io.Copy(io.Discard, res.Body)
	if n == 0 {
		t.Error("中身が空")
	}
}
