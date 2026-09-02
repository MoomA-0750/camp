package mcp

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

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

// talk は行区切りの JSON-RPC を1往復させる。
func talk(t *testing.T, db *store.DB, msgs ...string) []map[string]any {
	t.Helper()
	in := strings.NewReader(strings.Join(msgs, "\n") + "\n")
	var out bytes.Buffer
	if err := New(db, in, &out, "test").Serve(); err != nil {
		t.Fatal(err)
	}
	var res []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("返事が JSON でない: %q", line)
		}
		res = append(res, m)
	}
	return res
}

func TestInitializeAndListTools(t *testing.T) {
	db := newDB(t)
	res := talk(t, db,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if len(res) != 2 {
		t.Fatalf("2件返るはずが %d", len(res))
	}
	r0 := res[0]["result"].(map[string]any)
	if r0["protocolVersion"] != protocolVersion {
		t.Errorf("protocolVersion=%v", r0["protocolVersion"])
	}
	tools := res[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) < 7 {
		t.Fatalf("道具が %d 個しかない", len(tools))
	}
	// 全部に説明とスキーマが要る。無いとモデルが使い方を推測することになる。
	for _, x := range tools {
		tm := x.(map[string]any)
		if tm["description"] == "" || tm["inputSchema"] == nil {
			t.Errorf("%v に説明かスキーマが無い", tm["name"])
		}
	}
}

// 通知（id 無し）には返事を書かない。書くと相手のパーサが壊れる。
func TestNotificationsGetNoReply(t *testing.T) {
	db := newDB(t)
	res := talk(t, db, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if len(res) != 0 {
		t.Fatalf("通知に返事をしている: %v", res)
	}
}

// 道具の失敗はプロトコルの失敗ではない。isError で返し、接続は続ける。
func TestToolFailureIsNotAProtocolError(t *testing.T) {
	db := newDB(t)
	res := talk(t, db,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_sessions","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if len(res) != 2 {
		t.Fatalf("失敗のあと接続が切れている: %d件", len(res))
	}
	if _, bad := res[0]["error"]; bad {
		t.Error("道具の失敗を JSON-RPC のエラーで返している")
	}
	out := res[0]["result"].(map[string]any)
	if out["isError"] != true {
		t.Errorf("isError が立っていない: %v", out)
	}
}

// 知らないメソッドはエラーで返し、接続は続ける。
func TestUnknownMethodKeepsGoing(t *testing.T) {
	db := newDB(t)
	res := talk(t, db,
		`{"jsonrpc":"2.0","id":1,"method":"nope/nope"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if len(res) != 2 {
		t.Fatalf("接続が切れている: %d件", len(res))
	}
	if _, bad := res[0]["error"]; !bad {
		t.Error("知らないメソッドを通している")
	}
}

// 壊れた行で落ちない。
func TestBadJSONDoesNotKillTheConnection(t *testing.T) {
	db := newDB(t)
	res := talk(t, db, `{ not json`, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if len(res) != 2 {
		t.Fatalf("壊れた行で止まった: %d件", len(res))
	}
}

// **書き込む道具を1つも出さない。** Phase 3 まで読み取り専用。
func TestNoWriteTools(t *testing.T) {
	db := newDB(t)
	res := talk(t, db, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools := res[0]["result"].(map[string]any)["tools"].([]any)
	for _, x := range tools {
		name := x.(map[string]any)["name"].(string)
		for _, verb := range []string{"write", "create", "update", "delete", "set_", "put_", "run_", "exec"} {
			if strings.Contains(name, verb) {
				t.Errorf("書き込みらしい道具がある: %s", name)
			}
		}
	}
}
