package thread

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MoomA-0750/camp/internal/ingest"
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

// jsonl を1本書いて取り込む。
func ingestOne(t *testing.T, db *store.DB, sid, body string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "-nonexistent-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
}

const sid = "9182f0fd-06f7-4e05-966d-7c1f8ac2ffe9"

func msg(uuid, parent, typ, ts string) string {
	p := `"parentUuid":null`
	if parent != "" {
		p = `"parentUuid":"` + parent + `"`
	}
	role := "user"
	if typ == "assistant" {
		role = "assistant"
	}
	return `{"type":"` + typ + `","uuid":"` + uuid + `",` + p +
		`,"sessionId":"` + sid + `","session_id":"r-` + sid + `","timestamp":"` + ts +
		`","cwd":"/nonexistent/proj","message":{"role":"` + role + `","content":"x"}}` + "\n"
}

// compact_boundary は parentUuid を持たず logicalParentUuid だけを持つ。
// これを使わないと、要約のたびに会話が別スレッドに割れる。
func TestCompactBoundaryRejoinsTheThread(t *testing.T) {
	db := newDB(t)
	body := msg("u1", "", "user", "2026-09-02T00:01:00.000Z") +
		msg("a1", "u1", "assistant", "2026-09-02T00:02:00.000Z") +
		// 要約の境目。親は空、logicalParentUuid が a1 を指す
		`{"type":"system","subtype":"compact_boundary","uuid":"b1","parentUuid":null,` +
		`"logicalParentUuid":"a1","sessionId":"` + sid + `","session_id":"r-` + sid + `",` +
		`"timestamp":"2026-09-02T00:03:00.000Z","cwd":"/nonexistent/proj","content":"要約した"}` + "\n" +
		msg("u2", "b1", "user", "2026-09-02T00:04:00.000Z") +
		msg("a2", "u2", "assistant", "2026-09-02T00:05:00.000Z")
	ingestOne(t, db, sid, body)

	tr, err := Build(db, sid)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(tr.ConversationRoots()); got != 1 {
		t.Fatalf("会話が %d 本。繋ぎ直せば1本であるべき（%s）", got, tr.Describe())
	}
	if got := tr.FragmentsWithoutRepair(); got != 2 {
		t.Fatalf("繋ぎ直さなければ %d 本。2本であるべき", got)
	}
	if tr.Repaired != 1 || tr.Orphans != 0 {
		t.Fatalf("繋ぎ直し %d / 迷子 %d", tr.Repaired, tr.Orphans)
	}
	// 境目の行は logical 経由で繋がっていることが分かるようにする
	b1 := tr.Index["b1"]
	if b1 == nil || !b1.ViaLogical || b1.Parent != "a1" {
		t.Fatalf("境目の親が繋ぎ直されていない: %+v", b1)
	}
	// 深さは a1 の続きになる
	if tr.Index["a2"].Depth != 4 {
		t.Fatalf("a2 の深さが %d。u1→a1→b1→u2→a2 で4であるべき", tr.Index["a2"].Depth)
	}
}

// 表示順は書かれた順。時刻で並べ替えない。
// 実コーパスでは72ファイル中47本、計681箇所で時刻が逆行している。
func TestOrderIsByteOffsetNotTimestamp(t *testing.T) {
	db := newDB(t)
	// 3行目の時刻が2行目より前になっている
	body := msg("u1", "", "user", "2026-09-02T00:01:00.000Z") +
		msg("a1", "u1", "assistant", "2026-09-02T00:05:00.000Z") +
		msg("u2", "a1", "user", "2026-09-02T00:03:00.000Z")
	ingestOne(t, db, sid, body)

	tr, err := Build(db, sid)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"u1", "a1", "u2"}
	for i, w := range want {
		if tr.Order[i].UUID != w {
			t.Fatalf("%d 番目が %s。書かれた順（%v）であるべき", i, tr.Order[i].UUID, want)
		}
	}
	if tr.TimestampInversions() != 1 {
		t.Fatalf("時刻の逆行が %d 箇所。1箇所あるはず", tr.TimestampInversions())
	}
}

// uuid はセッションを跨ぐと重複する（fork が親の履歴を uuid ごと複製する）。
// 親を他セッションから拾ってきてはいけない。見つからなければ迷子として数える。
func TestParentLookupDoesNotCrossSessions(t *testing.T) {
	db := newDB(t)
	const other = "0f7900cc-866d-4e50-a56d-2f8b70063003"

	root := t.TempDir()
	dir := filepath.Join(root, "-nonexistent-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 別セッションに uuid "shared" が居る
	otherBody := `{"type":"user","uuid":"shared","parentUuid":null,"sessionId":"` + other +
		`","session_id":"r-` + other + `","timestamp":"2026-09-02T00:00:00.000Z",` +
		`"cwd":"/nonexistent/proj","message":{"role":"user","content":"別のセッション"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, other+".jsonl"), []byte(otherBody), 0o644); err != nil {
		t.Fatal(err)
	}
	// こちらの u1 は "shared" を親に指すが、自分のセッションには居ない
	body := msg("u1", "shared", "user", "2026-09-02T00:01:00.000Z")
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}

	tr, err := Build(db, sid)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Orphans != 1 {
		t.Fatalf("迷子が %d。他セッションの uuid を拾ってはいけない", tr.Orphans)
	}
	if len(tr.Roots) != 1 || tr.Roots[0].UUID != "u1" {
		t.Fatalf("迷子は根として扱う: %+v", tr.Roots)
	}
}

// /remote-control の案内行はファイルの先頭に親を持たずに置かれる。
// 子が付かないものは「会話の根」に数えない。
func TestBridgeBannerIsNotAConversationRoot(t *testing.T) {
	db := newDB(t)
	body := `{"type":"system","subtype":"bridge_status","uuid":"bs1","parentUuid":null,` +
		`"sessionId":"` + sid + `","session_id":"r-` + sid + `",` +
		`"timestamp":"2026-09-02T00:00:00.000Z","cwd":"/nonexistent/proj",` +
		`"content":"/remote-control is active"}` + "\n" +
		msg("u1", "", "user", "2026-09-02T00:01:00.000Z") +
		msg("a1", "u1", "assistant", "2026-09-02T00:02:00.000Z")
	ingestOne(t, db, sid, body)

	tr, err := Build(db, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Roots) != 2 {
		t.Fatalf("根が %d。案内行と会話で2つあるはず", len(tr.Roots))
	}
	if got := len(tr.ConversationRoots()); got != 1 {
		t.Fatalf("会話の根が %d。案内行は数えない", got)
	}
	if tr.ConversationRoots()[0].UUID != "u1" {
		t.Fatalf("会話の根が %s", tr.ConversationRoots()[0].UUID)
	}
}
