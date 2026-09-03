package retain_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoomA-0750/camp/internal/audit"
	"github.com/MoomA-0750/camp/internal/ingest"
	"github.com/MoomA-0750/camp/internal/retain"
	"github.com/MoomA-0750/camp/internal/search"
	"github.com/MoomA-0750/camp/internal/store"
)

// 実物と同じ形で仕込む。**主な経路は Bash のコマンドライン**で、
// 実測（2026-09-03）では 25 件のうち 22 件がそれだった。
func seedSecret(t *testing.T, secret string) (*store.DB, string, string) {
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
	head := `"sessionId":"` + sid + `","session_id":"r-` + sid + `","cwd":"/nonexistent/proj",`

	body := `{"type":"assistant","uuid":"a-1",` + head +
		`"timestamp":"2026-09-03T00:00:00.000Z","message":{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"sshpass -p ` + secret + ` ssh nas"}}]}}` + "\n" +
		`{"type":"user","uuid":"u-1",` + head +
		`"timestamp":"2026-09-03T00:01:00.000Z","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"t1","content":"接続できた ` + secret + `"}]}}` + "\n" +
		`{"type":"user","uuid":"u-2",` + head +
		`"timestamp":"2026-09-03T00:02:00.000Z","message":{"role":"user","content":"パスワードは ` + secret + ` です"}}` + "\n" +
		`{"type":"user","uuid":"u-3",` + head +
		`"timestamp":"2026-09-03T00:03:00.000Z","message":{"role":"user","content":"関係のない話"}}` + "\n"

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	return db, root, path
}

func surfaces(t *testing.T, db *store.DB, secret string) (int, int, int) {
	t.Helper()
	return int(scan(t, db, `select count(*) from messages where instr(raw_json,?)>0`, secret)),
		int(scan(t, db, `select count(*) from message_blocks where instr(coalesce(text,''),?)>0`, secret)),
		int(scan(t, db, `select count(*) from message_blocks where instr(coalesce(bigrams,''),?)>0`, secret))
}

// 写っているすべての場所から消える。**raw_json だけでは足りない。**
//
// 実データでは raw_json 25 / text 25 / bigrams 25 / blobs 1 に写っていた。
func TestTheSecretGoesFromEverySurface(t *testing.T) {
	const secret = "9zTESTpw4"
	db, _, _ := seedSecret(t, secret)

	raw, text, big := surfaces(t, db, secret)
	if raw == 0 || text == 0 || big == 0 {
		t.Fatalf("前提が崩れている: raw=%d text=%d bigrams=%d", raw, text, big)
	}

	out, err := retain.Secret(db, []byte(secret), retain.Op{
		Reason: "平文の認証情報", Actor: "secret_test"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Messages != raw {
		t.Errorf("伏せたのが %d 行（%d を期待）", out.Messages, raw)
	}

	raw, text, big = surfaces(t, db, secret)
	if raw != 0 || text != 0 || big != 0 {
		t.Errorf("まだ残っている: raw=%d text=%d bigrams=%d", raw, text, big)
	}

	// FTS の索引そのものも見る。検索経路を変えれば引けてしまうため。
	if n := scan(t, db, `select count(*) from messages_fts where messages_fts match ?`, secret); n != 0 {
		t.Errorf("FTS 索引に %d 件残っている", n)
	}
	hits, err := search.Query(db, secret, search.Opts{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("検索で %d 件引ける", len(hits))
	}
	if failed, detail := doctorFail(t, db, "messages_fts integrity"); failed {
		t.Errorf("FTS 索引が壊れた: %s", detail)
	}
}

// 周りの本文は残る。**行ごと消さない。**
func TestOnlyTheSecretGoesAway(t *testing.T) {
	const secret = "9zTESTpw4"
	db, _, _ := seedSecret(t, secret)
	before := scan(t, db, `select count(*) from messages`)

	if _, err := retain.Secret(db, []byte(secret), retain.Op{
		Reason: "平文の認証情報", Actor: "secret_test"}); err != nil {
		t.Fatal(err)
	}

	if n := scan(t, db, `select count(*) from messages`); n != before {
		t.Errorf("行数が %d → %d に変わった", before, n)
	}
	if n := scan(t, db, `select count(*) from messages where length(raw_json)=0`); n != 0 {
		t.Error("空になった行がある")
	}
	// 前後の文脈は読めるまま。
	for _, want := range []string{"sshpass", "ssh nas", "接続できた", "パスワードは", "関係のない話"} {
		if n := scan(t, db, `select count(*) from messages where instr(raw_json,?)>0`, want); n == 0 {
			t.Errorf("%q まで消えた", want)
		}
	}
	// 伏字が入っている。
	if n := scan(t, db, `select count(*) from messages where instr(raw_json,?)>0`,
		strings.Repeat("*", len(secret))); n == 0 {
		t.Error("伏字が入っていない")
	}
}

// 長さを変えないので、バイト位置で持っている所見が動かない。
func TestMaskingKeepsEveryByteOffsetInPlace(t *testing.T) {
	const secret = "9zTESTpw4"
	db, _, _ := seedSecret(t, secret)

	var id int64
	var raw []byte
	if err := db.QueryRow(`
		select id, raw_json from messages where instr(raw_json,?)>0 order by id limit 1`,
		secret).Scan(&id, &raw); err != nil {
		t.Fatal(err)
	}
	before := len(raw)
	marker := int64(strings.Index(string(raw), "sshpass"))
	if marker < 0 {
		t.Fatal("目印が無い")
	}
	if _, err := db.Exec(`
		insert into sensitive_findings(message_id, pattern, byte_offset, length, reviewed, verdict, found_at)
		values(?,'めじるし',?,7,1,'本人が確認済み','2026-09-03')`, id, marker); err != nil {
		t.Fatal(err)
	}

	if _, err := retain.Secret(db, []byte(secret), retain.Op{
		Reason: "平文の認証情報", Actor: "secret_test"}); err != nil {
		t.Fatal(err)
	}

	var after []byte
	var off int64
	if err := db.QueryRow(`
		select m.raw_json, f.byte_offset from sensitive_findings f
		  join messages m on m.id = f.message_id where f.pattern='めじるし'`).
		Scan(&after, &off); err != nil {
		t.Fatal(err)
	}
	if len(after) != before {
		t.Errorf("長さが %d → %d に変わった。伏字は同じ長さでなければならない", before, len(after))
	}
	if off != marker {
		t.Errorf("所見の位置が %d → %d に動いた", marker, off)
	}
	if !strings.HasPrefix(string(after[off:]), "sshpass") {
		t.Error("所見が別の場所を指している")
	}
	if failed, detail := doctorFail(t, db, "findings の位置"); failed {
		t.Errorf("所見が raw_json の外を指している: %s", detail)
	}
}

// 元の JSONL を書き換えないので、再取り込みで戻ってこないことを確かめる。
//
// **これが M25 の本当の受け入れ条件。** 消しても次の ingest で戻るなら
// 消したことにならない。
func TestTheSecretDoesNotComeBackOnReingest(t *testing.T) {
	const secret = "9zTESTpw4"
	db, root, path := seedSecret(t, secret)

	if _, err := retain.Secret(db, []byte(secret), retain.Op{
		Reason: "平文の認証情報", Actor: "secret_test"}); err != nil {
		t.Fatal(err)
	}

	// 元ファイルは今も持っている。ここが実データと同じ条件。
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), secret) {
		t.Fatal("前提が崩れている: 元ファイルから消えている")
	}

	if _, err := ingest.Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	if raw, text, big := surfaces(t, db, secret); raw != 0 || text != 0 || big != 0 {
		t.Errorf("通常の再取り込みで戻った: raw=%d text=%d bigrams=%d", raw, text, big)
	}

	// オフセットを戻して頭から読み直させる。tombstone が弾く経路。
	if _, err := db.Exec(`update source_files set ingested_offset = 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	if raw, text, big := surfaces(t, db, secret); raw != 0 || text != 0 || big != 0 {
		t.Errorf("読み直しで戻った: raw=%d text=%d bigrams=%d", raw, text, big)
	}

	// backfill で派生を作り直しても戻らない。
	if _, _, err := ingest.BackfillBlocks(db); err != nil {
		t.Fatal(err)
	}
	if _, text, big := surfaces(t, db, secret); text != 0 || big != 0 {
		t.Errorf("backfill で戻った: text=%d bigrams=%d", text, big)
	}
}

// 消したことが記録に残る。tombstone と監査ログの両方。
func TestRedactingASecretIsRecorded(t *testing.T) {
	const secret = "9zTESTpw4"
	db, _, _ := seedSecret(t, secret)

	out, err := retain.Secret(db, []byte(secret), retain.Op{
		Reason: "平文の認証情報が写っていた", Actor: "secret_test"})
	if err != nil {
		t.Fatal(err)
	}
	if n := scan(t, db, `select count(*) from tombstones where kind=?`, retain.KindSecret); int(n) != out.Messages {
		t.Errorf("tombstone が %d 件（%d を期待）", n, out.Messages)
	}
	rows, err := audit.List(db, audit.Opts{Action: "retain.secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("監査ログが %d 件", len(rows))
	}
	// **値そのものは記録に入れない。**
	for _, s := range []string{rows[0].Target, rows[0].Detail} {
		if strings.Contains(s, secret) {
			t.Errorf("監査ログに値そのものが入っている: %q", s)
		}
	}
	if n := scan(t, db, `select count(*) from tombstones where instr(coalesce(note,'')||reason,?)>0`, secret); n != 0 {
		t.Error("tombstone に値そのものが入っている")
	}
}

// 短すぎる値は受け付けない。関係ない場所まで潰す。
func TestItRefusesAValueTooShortToBeSafe(t *testing.T) {
	const secret = "9zTESTpw4"
	db, _, _ := seedSecret(t, secret)
	if _, err := retain.Secret(db, []byte("ab"), retain.Op{
		Reason: "x", Actor: "y"}); err == nil {
		t.Error("短すぎる値を受け付けた")
	}
}

// 伏字化は、派生した列に残った平文も消す。
//
// 2026-09-03 の outer gate の実測: `campd redact --apply` が
// 「走査し直して 0 件。」と表示して終了コード 0 を返す一方、
// `sessions.first_user_message`（`/api/sessions` が一覧に返す列）と
// `source_files.summary_json` に平文が残っていた。見ている表が3つだけで、
// 完了後の確認も同じ3表だったため。
func TestSecretIsGoneFromDerivedColumnsToo(t *testing.T) {
	const secret = "derivedleak8888"
	db := seedFirstMessage(t, secret)

	// 前提の確認: 派生列に本当に写っていること。写っていなければ試験にならない。
	before := scan(t, db, `select count(*) from sessions
		where instr(coalesce(first_user_message,''), ?) > 0`, secret)
	if before == 0 {
		t.Fatal("sessions.first_user_message に写っていない。この試験は穴を突けていない")
	}

	if _, err := retain.Secret(db, []byte(secret), retain.Op{
		Reason: "テスト", Actor: "secret_test",
	}); err != nil {
		t.Fatal(err)
	}

	hits, err := retain.Sweep(db, []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	if n := retain.Total(hits); n != 0 {
		for _, h := range hits {
			t.Errorf("%s.%s に %d 件残っている", h.Table, h.Column, h.Count)
		}
	}
}

// 総なめは、知っている置き場を数え上げる方式より広い。
func TestSweepSeesColumnsFindSecretDoesNot(t *testing.T) {
	const secret = "sweepwider7777"
	db := seedFirstMessage(t, secret)

	plan, err := retain.FindSecret(db, []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	hits, err := retain.Sweep(db, []byte(secret))
	if err != nil {
		t.Fatal(err)
	}

	known := map[string]bool{
		"messages.raw_json": true, "message_blocks.text": true,
		"message_blocks.bigrams": true, "blobs.content": true,
	}
	extra := 0
	for _, h := range hits {
		if !known[h.Table+"."+h.Column] && !retain.FTSShadow(h.Table) {
			extra++
			t.Logf("知っている置き場の外: %s.%s %d 件", h.Table, h.Column, h.Count)
		}
	}
	if extra == 0 {
		t.Fatalf("総なめが広くない。plan=%d 件", plan.Total())
	}
}

// seedFirstMessage は、最初の user 発言に値を入れて取り込む。
// sessions.first_user_message のような**派生列**へ値が流れる形を作るため。
func seedFirstMessage(t *testing.T, secret string) *store.DB {
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
	body := ""
	for i, text := range []string{secret, "続きの話"} {
		body += fmt.Sprintf(
			`{"type":"user","uuid":"f-%[1]d","sessionId":"%[2]s","session_id":"r-%[2]s",`+
				`"timestamp":"2026-09-03T00:0%[1]d:00.000Z","cwd":"/nonexistent/proj",`+
				`"message":{"role":"user","content":%[3]q}}`+"\n", i, sid, text)
	}
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	return db
}
