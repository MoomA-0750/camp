package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
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

// seed は sessions → messages の最小の骨組みを作る。
func seed(t *testing.T, db *store.DB, raws ...string) []int64 {
	t.Helper()
	if _, err := db.Exec(`
		insert into hosts(id, name) values(1, 'h');
		insert into projects(id, host_id, repo_path, name) values(1, 1, '/p', 'p');
		insert into sessions(id, host_id, project_id, agent, started_at, updated_at)
		values('s1', 1, 1, 'claude', 't', 't');
		insert into source_files(id, host_id, path, role, first_seen_at)
		values(1, 1, '/p/s1.jsonl', 'main', 't')`); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for i, raw := range raws {
		r, err := db.Exec(`insert into messages(uuid, session_id, source_file_id, byte_offset,
		                                        type, timestamp, raw_json)
		                   values(?, 's1', 1, ?, 'assistant', '2026-09-02T00:00:00.000Z', ?)`,
			"u", i, []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := r.LastInsertId()
		ids = append(ids, id)
	}
	return ids
}

func rawSum(t *testing.T, db *store.DB) string {
	t.Helper()
	rows, err := db.Query(`select raw_json from messages order by id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	h := sha256.New()
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// M9 の受け入れそのもの。検出しても raw_json は1バイトも変わらない（D-010）。
func TestScanNeverTouchesMessages(t *testing.T) {
	db := newDB(t)
	seed(t, db,
		`{"text":"token = ghp_`+strings.Repeat("A", 36)+`"}`,
		`{"text":"printf -- '-----BEGIN OPENSSH PRIVATE KEY-----'"}`,
		`{"text":"なんでもない行"}`)
	before := rawSum(t, db)

	r, err := Scan(db, []Known{{Label: "vault", Value: "なんでもない行"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Found != 3 || r.New != 3 || r.Known != 1 {
		t.Fatalf("%+v", r)
	}
	if after := rawSum(t, db); after != before {
		t.Fatalf("raw_json が変わった\n%s\n%s", before, after)
	}
	if n := count(t, db, `select count(*) from messages`); n != 3 {
		t.Fatalf("メッセージが %d 件", n)
	}
}

// 何度回しても増えず、人が付けた判断も消えない。
// findings は人の判断を載せる唯一の派生表なので、消して作り直してはいけない。
func TestScanIsIdempotentAndKeepsVerdicts(t *testing.T) {
	db := newDB(t)
	seed(t, db, `{"text":"token = ghp_`+strings.Repeat("B", 36)+`"}`)
	if _, err := Scan(db, nil); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := db.QueryRow(`select id from sensitive_findings`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := Review(db, id, "false-positive"); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		r, err := Scan(db, nil)
		if err != nil {
			t.Fatal(err)
		}
		if r.Found != 1 || r.New != 0 {
			t.Fatalf("%d 回目: %+v", i+2, r)
		}
	}
	var reviewed int
	var verdict string
	if err := db.QueryRow(`select reviewed, coalesce(verdict, '') from sensitive_findings`).
		Scan(&reviewed, &verdict); err != nil {
		t.Fatal(err)
	}
	if reviewed != 1 || verdict != "false-positive" {
		t.Fatalf("判断が消えた: reviewed=%d verdict=%q", reviewed, verdict)
	}
	if n := count(t, db, `select count(*) from sensitive_findings`); n != 1 {
		t.Fatalf("%d 行に増えた", n)
	}
}

// 位置は raw_json のバイト位置で持つ。block_id に繋がないこと。
// message_blocks は backfill のたびに消して入れ直され id が振り直されるので、
// 繋ぐと人の判断ごと宙に浮く。
func TestFindingsDoNotDependOnBlockIDs(t *testing.T) {
	db := newDB(t)
	seed(t, db, `{"text":"AKIA`+strings.Repeat("Z", 16)+`"}`)
	if _, err := Scan(db, nil); err != nil {
		t.Fatal(err)
	}
	var blockID any
	var off, length int
	if err := db.QueryRow(`select block_id, byte_offset, length from sensitive_findings`).
		Scan(&blockID, &off, &length); err != nil {
		t.Fatal(err)
	}
	if blockID != nil {
		t.Fatalf("block_id を持ってしまっている: %v", blockID)
	}
	var raw []byte
	if err := db.QueryRow(`select raw_json from messages`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if got := string(raw[off : off+length]); got != "AKIA"+strings.Repeat("Z", 16) {
		t.Fatalf("位置がずれている: %q", got)
	}
}

// 既知の秘密はパターンに引っかからない。だから別経路で突き合わせる。
// 実コーパスでは、当たった14箇所すべてが偽陽性で、実在する平文の
// 認証情報は1件も当たらなかった（偽陽性100%・偽陰性100%）。
func TestKnownSecretsCatchWhatPatternsCannot(t *testing.T) {
	db := newDB(t)
	seed(t, db,
		`{"text":"ssh mooma@host, パスワードは hunter2xy を使う"}`,
		`{"text":"別の行にも hunter2xy が出てくる"}`)

	// パターンだけでは1件も当たらない。
	r, err := Scan(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Found != 0 {
		t.Fatalf("パターンが当たってしまった: %+v", r)
	}

	r, err = Scan(db, []Known{{Label: "homelab", Value: "hunter2xy"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Found != 2 || r.Known != 2 {
		t.Fatalf("%+v", r)
	}
	var pattern string
	if err := db.QueryRow(`select pattern from sensitive_findings limit 1`).Scan(&pattern); err != nil {
		t.Fatal(err)
	}
	if pattern != "known:homelab" {
		t.Fatalf("pattern が %q", pattern)
	}
}

// 一覧は既定で当たりを伏せる。判断に必要なのは前後の文脈であって、
// 端末に本物を書き出すことではない。
func TestListMasksTheMatchByDefault(t *testing.T) {
	db := newDB(t)
	const tok = "ghp_" + "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
	seed(t, db, `{"text":"UPDATE keys SET v = '`+tok+`'"}`)
	if _, err := Scan(db, nil); err != nil {
		t.Fatal(err)
	}

	masked, err := List(db, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(masked) != 1 || strings.Contains(masked[0].Context, tok) {
		t.Fatalf("伏せていない: %q", masked[0].Context)
	}
	if !strings.Contains(masked[0].Context, "UPDATE keys SET") {
		t.Fatalf("文脈が出ていない: %q", masked[0].Context)
	}

	shown, err := List(db, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(shown[0].Context, tok) {
		t.Fatalf("-reveal で出ていない: %q", shown[0].Context)
	}
}

// 既知の秘密の一覧はリポジトリに置かない。無ければ静かに空で動く。
func TestLoadKnownIsOptional(t *testing.T) {
	got, err := LoadKnown(filepath.Join(t.TempDir(), "無い.txt"))
	if err != nil || got != nil {
		t.Fatalf("%v %v", got, err)
	}

	p := filepath.Join(t.TempDir(), "known.txt")
	os.WriteFile(p, []byte("# コメント\n\nhomelab\thunter2xy\nnas\tsecretpw\n"), 0o600)
	got, err = LoadKnown(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Label != "homelab" || got[1].Value != "secretpw" {
		t.Fatalf("%+v", got)
	}
}

func count(t *testing.T, db *store.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
