package report_test

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoomA-0750/camp/internal/audit"
	"github.com/MoomA-0750/camp/internal/report"
	"github.com/MoomA-0750/camp/internal/store"
)

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

func listen(t *testing.T, db *store.DB) *report.Listener {
	t.Helper()
	l, err := report.Listen(db, filepath.Join(t.TempDir(), "report.sock"), "")
	if err != nil {
		t.Fatal(err)
	}
	go l.Serve()
	t.Cleanup(func() { l.Close() })
	return l
}

// 送って、返ってきた1行を読む。
func send(t *testing.T, l *report.Listener, raw string) report.Reply {
	t.Helper()
	c, err := net.Dial("unix", l.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte(raw + "\n")); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(c)
	if !sc.Scan() {
		t.Fatal("返事が無い")
	}
	var r report.Reply
	if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
		t.Fatalf("返事を読めない: %q", sc.Text())
	}
	return r
}

// 境界の外から追記できる。
func TestTheOutsideCanAppend(t *testing.T) {
	db := newDB(t)
	l := listen(t, db)

	r := send(t, l, `{"action":"session.start","target":"/home/x/proj","outcome":"ok"}`)
	if !r.OK || r.ID == 0 {
		t.Fatalf("追記できていない: %+v", r)
	}

	es, err := audit.List(db, audit.Opts{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range es {
		if e.Action == "session.start" {
			found = true
		}
	}
	if !found {
		t.Error("監査ログに出ていない")
	}
}

// **名乗りは信じない。** actor はカーネルが答えた身元で上書きされる。
func TestTheCallerCannotChooseItsOwnName(t *testing.T) {
	db := newDB(t)
	l := listen(t, db)

	// actor を詐称しようとしても、そもそも受け取る欄が無い。
	send(t, l, `{"action":"session.start","actor":"campd","outcome":"ok"}`)

	es, err := audit.List(db, audit.Opts{Action: "session.start", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(es) == 0 {
		t.Fatal("追記されていない")
	}
	if es[0].Actor == "campd" {
		t.Error("名乗った名前がそのまま入っている")
	}
	if !strings.HasPrefix(es[0].Actor, "uid:") {
		t.Errorf("actor が %q。カーネルから取った身元であるべき", es[0].Actor)
	}
}

// **読み出しも更新も削除も、命令そのものが無い。**
func TestTheSocketHasNoWayToReadOrChangeAnything(t *testing.T) {
	db := newDB(t)
	l := listen(t, db)
	if _, err := audit.Append(db, audit.Entry{
		Actor: "campd", Action: "secret.thing", Target: "見せてはいけない", Outcome: audit.OK,
	}); err != nil {
		t.Fatal(err)
	}

	for _, attempt := range []string{
		`{"action":"list"}`,
		`{"op":"select","table":"audit"}`,
		`{"op":"delete","id":1}`,
		`{"op":"update","id":1,"outcome":"denied"}`,
		`{"action":"","target":"空の action"}`,
	} {
		r := send(t, l, attempt)
		// 通ってしまう場合でも、それは「追記」以上のことはしていない。
		if strings.Contains(r.Error, "見せてはいけない") {
			t.Errorf("%s で中身が漏れた", attempt)
		}
	}

	// 元の行は1つも変わっていない。
	es, err := audit.List(db, audit.Opts{Action: "secret.thing", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 || es[0].Outcome != audit.OK || es[0].Target != "見せてはいけない" {
		t.Errorf("既存の行が変わった: %+v", es)
	}
	if n, err := audit.Verify(db); err != nil {
		t.Errorf("連鎖が壊れた（%d 行目まで）: %v", n, err)
	}
}

// socket の mode は 0660。他人は書けない。
func TestTheSocketIsNotOpenToEveryone(t *testing.T) {
	db := newDB(t)
	l := listen(t, db)
	fi, err := os.Stat(l.Addr())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o007 != 0 {
		t.Errorf("mode %04o。その他のユーザーに開いている", fi.Mode().Perm())
	}
}

// 外は信用しない側なので、無制限には受けない。
func TestTheOutsideCannotFloodOrStuffTheLog(t *testing.T) {
	db := newDB(t)
	l := listen(t, db)

	long := strings.Repeat("あ", 20000)
	for _, tc := range []struct{ name, msg string }{
		{"長すぎる target", `{"action":"a","target":"` + long + `"}`},
		{"知らない outcome", `{"action":"a","outcome":"すごい"}`},
		{"長すぎる detail", `{"action":"a","detail":""+strings.Repeat("あ", 30000)+""}`},
	} {
		if r := send(t, l, tc.msg); r.OK {
			t.Errorf("%s が通った", tc.name)
		}
	}

	// 1接続で速く送りすぎると断られる。
	c, err := net.Dial("unix", l.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	refused := false
	sc := bufio.NewScanner(c)
	for i := 0; i < 300; i++ {
		if _, err := c.Write([]byte(`{"action":"flood"}` + "\n")); err != nil {
			break
		}
		if !sc.Scan() {
			refused = true
			break
		}
		var r report.Reply
		json.Unmarshal(sc.Bytes(), &r)
		if !r.OK {
			refused = true
			break
		}
	}
	if !refused {
		t.Error("いくらでも書き込めてしまう")
	}
}
