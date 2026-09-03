package retain_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/MoomA-0750/camp/internal/ingest"
	"github.com/MoomA-0750/camp/internal/retain"
	"github.com/MoomA-0750/camp/internal/store"
)

// thinking の署名と、二重の画像を1本ずつ持つ会話を作る。
func seedPolicy(t *testing.T) (*store.DB, string, string) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "camp.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "-nonexistent-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sid+".jsonl")

	const sig = "SIGNATURESIGNATURESIGNATURE"
	const img = "IMAGEIMAGEIMAGEIMAGEIMAGEIMAGE"
	head := `"sessionId":"` + sid + `","session_id":"r-` + sid + `","cwd":"/nonexistent/proj",`
	body := `{"type":"assistant","uuid":"a-1",` + head +
		`"timestamp":"2026-09-03T00:00:00.000Z",` +
		`"message":{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"","signature":"` + sig + `"},` +
		`{"type":"text","text":"のこる本文"}]}}` + "\n" +
		`{"type":"user","uuid":"u-1",` + head +
		`"timestamp":"2026-09-03T00:01:00.000Z",` +
		`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[` +
		`{"type":"image","source":{"type":"base64","data":"` + img + `"}}]}]},` +
		`"toolUseResult":{"content":[{"type":"image","source":{"type":"base64","data":"` + img + `"}}]}}` + "\n" +
		`{"type":"user","uuid":"u-2",` + head +
		`"timestamp":"2026-09-03T00:02:00.000Z",` +
		`"message":{"role":"user","content":"ふつうの発言"}}` + "\n"

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	return db, root, path
}

func enableAll(t *testing.T, db *store.DB) {
	t.Helper()
	pols, err := retain.Policies(db, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pols {
		if err := retain.SetEnabled(db, p.Name, true); err != nil {
			t.Fatal(err)
		}
	}
}

// 入れただけでは何も消えない。規則は既定で無効。
func TestPoliciesDoNothingUntilEnabled(t *testing.T) {
	db, _, _ := seedPolicy(t)

	pols, err := retain.Policies(db, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pols) == 0 {
		t.Fatal("既定の規則が入っていない")
	}
	for _, p := range pols {
		if p.Enabled {
			t.Errorf("規則 %q が最初から有効になっている", p.Name)
		}
	}

	plan, err := retain.Plan(db, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 0 {
		t.Errorf("無効なのに %d 行が対象になっている", len(plan))
	}
}

// 見積りの件数と、実際に落ちた件数が一致する。
//
// --dry-run と --apply が別の道を通ると、見せた数と消える数が食い違う。
func TestTheDryRunMatchesWhatActuallyHappens(t *testing.T) {
	db, _, _ := seedPolicy(t)
	enableAll(t, db)

	plan, err := retain.Plan(db, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 2 {
		t.Fatalf("対象が %d 行（thinking 1 + 画像 1 を期待）: %+v", len(plan), plan)
	}
	var planned int64
	for _, r := range plan {
		planned += int64(r.Bytes)
		if r.Bytes == 0 {
			t.Errorf("落とせる量が 0 の行が混ざっている: %+v", r)
		}
	}

	out, err := retain.Apply(db, plan, "policy_test")
	if err != nil {
		t.Fatal(err)
	}
	if out.Messages != len(plan) {
		t.Errorf("見積り %d 行に対して実際は %d 行", len(plan), out.Messages)
	}
	if out.BytesRemoved != planned {
		t.Errorf("見積り %d バイトに対して実際は %d バイト", planned, out.BytesRemoved)
	}

	// 落としたものは消え、残すものは残る。
	if n := scan(t, db, `select count(*) from messages where instr(raw_json, 'SIGNATURESIGNATURE') > 0`); n != 0 {
		t.Errorf("署名が %d 行に残っている", n)
	}
	if n := scan(t, db, `select count(*) from messages where instr(raw_json, 'IMAGEIMAGEIMAGE') > 0`); n != 1 {
		t.Errorf("画像を持つ行が %d（message.content 側の 1 を期待）", n)
	}
	if n := scan(t, db, `select count(*) from messages where instr(raw_json, 'のこる本文') > 0`); n != 1 {
		t.Error("残すはずの本文が消えた")
	}

	// 記録が残る。
	if n := scan(t, db, `select count(*) from tombstones where kind = ?`, retain.KindTrim); n != 2 {
		t.Errorf("tombstone が %d 件（2 件を期待）", n)
	}

	// 2度目は何も残っていないので対象が消える。
	plan2, err := retain.Plan(db, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan2) != 0 {
		t.Errorf("落としたあとにまだ %d 行が対象になっている", len(plan2))
	}
}

// 落としても派生行と検索の見え方が変わらない。
//
// 対象にしたのは索引に入っていないものだけなので、ここが変わったら
// 選び方が間違っている。
func TestTrimmingChangesNothingVisible(t *testing.T) {
	db, _, _ := seedPolicy(t)
	enableAll(t, db)

	before := scan(t, db, `select count(*) from message_blocks`)
	beforeText := scan(t, db, `select sum(length(coalesce(text,''))) from message_blocks`)

	plan, err := retain.Plan(db, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retain.Apply(db, plan, "policy_test"); err != nil {
		t.Fatal(err)
	}

	if n := scan(t, db, `select count(*) from message_blocks`); n != before {
		t.Errorf("ブロックが %d → %d に変わった", before, n)
	}
	if n := scan(t, db, `select sum(length(coalesce(text,''))) from message_blocks`); n != beforeText {
		t.Errorf("本文の量が %d → %d に変わった", beforeText, n)
	}
	if failed, detail := doctorFail(t, db, "messages_fts integrity"); failed {
		t.Errorf("FTS 索引が壊れた: %s", detail)
	}
	// 行はすべて残る。
	if n := scan(t, db, `select count(*) from messages where length(raw_json) = 0`); n != 0 {
		t.Errorf("空になった行が %d 件ある。行ごと消してはいけない", n)
	}
}

// 元ファイルが無い行は、明示しない限り触らない。
func TestUnrecoverableRowsAreLeftAloneByDefault(t *testing.T) {
	db, _, path := seedPolicy(t)
	enableAll(t, db)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	plan, err := retain.Plan(db, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 0 {
		t.Errorf("戻せない行 %d 件が既定で対象になっている", len(plan))
	}

	all, err := retain.Plan(db, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("--include-unrecoverable で %d 行（2 を期待）", len(all))
	}
	for _, r := range all {
		if r.Recoverable {
			t.Errorf("元ファイルが無いのに「戻せる」と言っている: %+v", r)
		}
	}

	out, err := retain.Apply(db, all, "policy_test")
	if err != nil {
		t.Fatal(err)
	}
	if out.Unrecoverable != 2 {
		t.Errorf("戻せない削除が %d 件と記録された", out.Unrecoverable)
	}
	var rec int
	if err := db.QueryRow(`select min(recoverable) from tombstones`).Scan(&rec); err != nil {
		t.Fatal(err)
	}
	if rec != 0 {
		t.Error("tombstone が「戻せる」と言っている")
	}
	_ = fmt.Sprint()
}

// 落としたあとでも、raw_json から派生行を作り直せる。
//
// これが保持ポリシーの本当の受け入れ条件。「見え方が変わらない」だけでは
// 足りない——**元から作り直したときに同じものが出る**必要がある。
// 実データでも確かめた（18,760 ブロック / 15,523,752 バイトが完全一致）。
func TestDerivedRowsCanStillBeRebuiltFromWhatIsLeft(t *testing.T) {
	db, _, _ := seedPolicy(t)
	enableAll(t, db)

	beforeBlocks := scan(t, db, `select count(*) from message_blocks`)
	beforeText := scan(t, db, `select sum(length(coalesce(text,''))) from message_blocks`)

	plan, err := retain.Plan(db, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retain.Apply(db, plan, "policy_test"); err != nil {
		t.Fatal(err)
	}

	// 派生を捨てて、残った raw_json から作り直す。
	blocks, msgs, err := ingest.BackfillBlocks(db)
	if err != nil {
		t.Fatal(err)
	}
	if msgs == 0 {
		t.Fatal("作り直しが走っていない")
	}
	if int64(blocks) != beforeBlocks {
		t.Errorf("作り直したら %d ブロック（落とす前は %d）", blocks, beforeBlocks)
	}
	if n := scan(t, db, `select sum(length(coalesce(text,''))) from message_blocks`); n != beforeText {
		t.Errorf("作り直したら本文が %d バイト（落とす前は %d）", n, beforeText)
	}
	if failed, detail := doctorFail(t, db, "messages_fts integrity"); failed {
		t.Errorf("作り直した索引が壊れている: %s", detail)
	}
}
