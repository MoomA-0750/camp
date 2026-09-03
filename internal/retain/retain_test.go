package retain_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/MoomA-0750/camp/internal/ingest"
	"github.com/MoomA-0750/camp/internal/query"
	"github.com/MoomA-0750/camp/internal/retain"
	"github.com/MoomA-0750/camp/internal/search"
	"github.com/MoomA-0750/camp/internal/store"
)

const sid = "11111111-2222-4333-8444-555555555555"

// 会話を1本作って取り込む。secret は1行だけに入れる。
func seed(t *testing.T, secret string) (*store.DB, string, string) {
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

	body := ""
	for i, text := range []string{"最初の話", secret, "最後の話"} {
		body += fmt.Sprintf(
			`{"type":"user","uuid":"u-%[1]d","sessionId":"%[2]s","session_id":"r-%[2]s",`+
				`"timestamp":"2026-09-03T00:0%[1]d:00.000Z","cwd":"/nonexistent/proj",`+
				`"message":{"role":"user","content":%[3]q}}`+"\n", i, sid, text)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	return db, root, path
}

func scan(t *testing.T, db *store.DB, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func findMessage(t *testing.T, db *store.DB, secret string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(
		`select id from messages where instr(raw_json, ?) > 0`, secret).Scan(&id); err != nil {
		t.Fatalf("secret を含む行が見つからない: %v", err)
	}
	return id
}

func doctorFail(t *testing.T, db *store.DB, name string) (bool, string) {
	t.Helper()
	checks, err := db.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range checks {
		if c.Name == name {
			return !c.OK, c.Detail
		}
	}
	t.Fatalf("点検 %q が無い", name)
	return false, ""
}

// 消したら、写っている場所すべてから消え、消したという記録が1本残る。
//
// raw_json を落とすだけでは message_blocks と FTS に残る。M25 の平文25件は
// まさにこの形で写っていた（raw_json 25 / text 25 / bigrams 25）。
func TestRedactRemovesEverySurfaceAndLeavesATombstone(t *testing.T) {
	const secret = "9zzQQnotarealsecret"
	db, _, _ := seed(t, secret)
	id := findMessage(t, db, secret)

	before := map[string]int64{
		"raw_json": scan(t, db, `select count(*) from messages where instr(raw_json,?)>0`, secret),
		"text":     scan(t, db, `select count(*) from message_blocks where instr(coalesce(text,''),?)>0`, secret),
		"bigrams":  scan(t, db, `select count(*) from message_blocks where instr(coalesce(bigrams,''),?)>0`, secret),
	}
	for k, v := range before {
		if v == 0 {
			t.Fatalf("前提が崩れている: %s に secret が無い", k)
		}
	}

	out, err := retain.Message(db, id, retain.Op{
		Reason: "平文の認証情報が写っていた", Actor: "retain_test"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Messages != 1 || out.Blocks == 0 {
		t.Errorf("消した件数がおかしい: %+v", out)
	}
	if out.Unrecoverable != 0 {
		t.Errorf("元ファイルがあるのに「戻せない」と記録している: %+v", out)
	}

	for _, q := range []struct {
		name, sql string
	}{
		{"raw_json", `select count(*) from messages where instr(raw_json,?)>0`},
		{"text", `select count(*) from message_blocks where instr(coalesce(text,''),?)>0`},
		{"bigrams", `select count(*) from message_blocks where instr(coalesce(bigrams,''),?)>0`},
	} {
		if n := scan(t, db, q.sql, secret); n != 0 {
			t.Errorf("%s にまだ %d 件残っている", q.name, n)
		}
	}

	// 行そのものは残る。「ここに何かあった」と言えなくなるため。
	if n := scan(t, db, `select count(*) from messages where id=?`, id); n != 1 {
		t.Error("行ごと消えている。行は残さないといけない")
	}

	// 記録が1本。
	if n := scan(t, db, `select count(*) from tombstones where kind=? and ref=?`,
		retain.KindRawJSON, fmt.Sprint(id)); n != 1 {
		t.Errorf("tombstone が %d 件（1件を期待）", n)
	}
	var reason string
	if err := db.QueryRow(`select reason from tombstones where ref=?`, fmt.Sprint(id)).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason == "" {
		t.Error("理由が残っていない")
	}
}

// FTS は external-content なので、行を消すだけでは索引に残る。
// 検索から引けなくなることと、doctor の integrity-check が通ることの両方を見る。
func TestRedactedTextIsGoneFromSearchAndTheIndexStaysSound(t *testing.T) {
	const secret = "zzsecretzz"
	db, _, _ := seed(t, secret)

	hits, err := search.Query(db, secret, search.Opts{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("前提が崩れている: 消す前に検索で引けない")
	}

	id := findMessage(t, db, secret)
	if _, err := retain.Message(db, id, retain.Op{Reason: "テスト", Actor: "retain_test"}); err != nil {
		t.Fatal(err)
	}

	hits, err = search.Query(db, secret, search.Opts{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("消したのに検索で %d 件引ける", len(hits))
	}

	// **索引そのものを直接見る。**
	// search.Query は message_blocks と結合するので、本体を消しただけでも
	// 0件に見えてしまう。それでは「索引に語が残っている」を見逃す
	// （実測: 本体だけ消すと messages_fts MATCH は返し続ける）。
	if n := scan(t, db, `select count(*) from messages_fts where messages_fts match ?`, secret); n != 0 {
		t.Errorf("FTS 索引に %d 件残っている。検索経路を変えれば引けてしまう", n)
	}

	if failed, detail := doctorFail(t, db, "messages_fts integrity"); failed {
		t.Errorf("FTS 索引が壊れた: %s", detail)
	}
}

// 消したものが次の取り込みで戻ってこない。
//
// 元の JSONL は書き換えないので、行を消す実装だと必ず戻る。
// 行を残し、さらに tombstone を見て弾く。
func TestRedactedMessageDoesNotComeBackOnReingest(t *testing.T) {
	const secret = "comeback9999"
	db, root, _ := seed(t, secret)
	id := findMessage(t, db, secret)
	if _, err := retain.Message(db, id, retain.Op{Reason: "テスト", Actor: "retain_test"}); err != nil {
		t.Fatal(err)
	}

	// 追記ぶんだけ読む通常経路。
	if _, err := ingest.Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	if n := scan(t, db, `select count(*) from messages where instr(raw_json,?)>0`, secret); n != 0 {
		t.Errorf("通常の再取り込みで %d 件戻った", n)
	}

	// オフセットを戻して、ファイルを頭から読み直させる。
	// **これは世代交代ではない**（source_files の行は同じ）。
	// 世代が変わる経路は TestRedactedMessageDoesNotComeBackAfterTheFileIsRecreated。
	if _, err := db.Exec(`update source_files set ingested_offset = 0`); err != nil {
		t.Fatal(err)
	}
	res, err := ingest.Ingest(db, "testhost", root)
	if err != nil {
		t.Fatal(err)
	}
	if n := scan(t, db, `select count(*) from messages where instr(raw_json,?)>0`, secret); n != 0 {
		t.Errorf("読み直しで %d 件戻った", n)
	}
	if res.Suppressed == 0 {
		t.Error("tombstone で弾いた件数が記録されていない")
	}
	if n := scan(t, db, `select count(*) from message_blocks where instr(coalesce(text,''),?)>0`, secret); n != 0 {
		t.Errorf("読み直しで message_blocks に %d 件戻った", n)
	}
}

// 消した記録の無い空 raw_json を doctor が拾う。
// このパッケージを通さずに消した痕跡がここに出る。
func TestDoctorCatchesADeletionWithNoRecord(t *testing.T) {
	db, _, _ := seed(t, "何でもよい")
	if failed, detail := doctorFail(t, db, "tombstones"); failed {
		t.Fatalf("何もしていないのに落ちた: %s", detail)
	}

	// retain を通さず、直接 raw_json を空にする。
	if _, err := db.Exec(`update messages set raw_json = x'' where id = (select min(id) from messages)`); err != nil {
		t.Fatal(err)
	}
	failed, detail := doctorFail(t, db, "tombstones")
	if !failed {
		t.Error("記録の無い削除を doctor が見逃した")
	}
	if detail == "" {
		t.Error("何が起きたか説明がない")
	}
}

// doctor は列ではなく実体を見る。
//
// source_files.missing_at は前回の走査時点の話でしかない。実測（2026-09-03）で
// 83本中1本が既に消えていたのに、DBは83本すべてあると思っていた。
func TestDoctorLooksAtTheDiskNotTheColumn(t *testing.T) {
	db, _, path := seed(t, "何でもよい")

	st, err := db.SourceFileStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.Gone != 0 {
		t.Fatalf("前提が崩れている: 消す前に %d 本欠けている", st.Gone)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	st, err = db.SourceFileStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.Gone != 1 {
		t.Errorf("実体が消えたのに Gone=%d", st.Gone)
	}
	if st.Stale != 1 {
		t.Errorf("missing_at が NULL のままなのに Stale=%d", st.Stale)
	}
	if st.OrphanMessages == 0 || st.OrphanBytes == 0 {
		t.Errorf("ここにしか無い行を数えていない: %+v", st)
	}
	if failed, _ := doctorFail(t, db, "source_files 実体"); !failed {
		t.Error("doctor が「全部ある」と言い続けている")
	}

	n, err := db.MarkMissingSourceFiles()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("missing_at を入れたのが %d 本", n)
	}
	st, err = db.SourceFileStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.Stale != 0 {
		t.Errorf("直したのに Stale=%d", st.Stale)
	}
	// 実体は無いままなので Gone は残る。行は消さない。
	if st.Gone != 1 {
		t.Errorf("行まで消えている: Gone=%d", st.Gone)
	}
}

// 元ファイルが無い状態での削除は「戻せない」と記録される。
func TestRedactionWithoutASourceIsMarkedUnrecoverable(t *testing.T) {
	const secret = "unrecoverable42"
	db, _, path := seed(t, secret)
	id := findMessage(t, db, secret)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	out, err := retain.Message(db, id, retain.Op{Reason: "テスト", Actor: "retain_test"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Unrecoverable != 1 {
		t.Errorf("戻せない削除なのに %+v", out)
	}
	var rec int
	if err := db.QueryRow(`select recoverable from tombstones where ref=?`, fmt.Sprint(id)).Scan(&rec); err != nil {
		t.Fatal(err)
	}
	if rec != 0 {
		t.Error("tombstone が「戻せる」と言っている")
	}
	if failed, detail := doctorFail(t, db, "tombstones"); failed {
		t.Errorf("正しく消したのに落ちた: %s", detail)
	}
}

// 新しく取り込んだ行には parser_version が入る。0 は「分からない」の予約値。
func TestNewRowsCarryTheirParserVersion(t *testing.T) {
	db, _, _ := seed(t, "何でもよい")
	pv, err := db.ParserVersions()
	if err != nil {
		t.Fatal(err)
	}
	if pv[0] != 0 {
		t.Errorf("いま取り込んだ行が %d 件も「不明」になっている", pv[0])
	}
	if pv[ingest.ParserVersion] == 0 {
		t.Errorf("parser_version=%d の行が無い: %v", ingest.ParserVersion, pv)
	}
}

// 消した行は、API から見ても空行と区別が付く。
//
// ブロックが0本の行は他にもあるので、印が無いと画面で見分けられない。
func TestTheAPICanTellARedactionFromAnEmptyRow(t *testing.T) {
	const secret = "apilevel1234"
	db, _, _ := seed(t, secret)
	id := findMessage(t, db, secret)
	if _, err := retain.Message(db, id, retain.Op{
		Reason: "平文の認証情報が写っていた", Actor: "retain_test"}); err != nil {
		t.Fatal(err)
	}

	msgs, err := query.Messages(db, sid, 0, 100, true)
	if err != nil {
		t.Fatal(err)
	}
	var found *query.Message
	for i := range msgs {
		if msgs[i].ID == id {
			found = &msgs[i]
		}
	}
	if found == nil {
		t.Fatal("消した行が一覧から落ちている。行は残さないといけない")
	}
	if len(found.Blocks) != 0 {
		t.Errorf("本文が残っている: %+v", found.Blocks)
	}
	if found.Redacted == nil {
		t.Fatal("消した印が付いていない。空行と区別が付かない")
	}
	if found.Redacted.Reason == "" || found.Redacted.At == "" {
		t.Errorf("理由か日時が空: %+v", found.Redacted)
	}
	if !found.Redacted.Recoverable {
		t.Error("元ファイルがあるのに「戻せない」と言っている")
	}

	// 消していない行には印を付けない。
	for i := range msgs {
		if msgs[i].ID != id && msgs[i].Redacted != nil {
			t.Errorf("消していない行 %d に印が付いている", msgs[i].ID)
		}
	}
}

// 元ファイルが同じ内容のまま作り直されても、何も起きない。
//
// 2026-09-03 の outer gate で見つけた穴。ファイルの身元に inode を使っていたので、
// 中身が1バイトも同じでも inode が変われば世代が上がり、
//
//   - 全行がもう一度 messages へ入り（一意制約は (source_file_id, byte_offset)）
//   - 古い世代に紐づいた tombstone が効かなくなって消したものが戻る
//
// の2つが同時に起きていた。rsync（既定で一時ファイル＋rename）、
// バックアップからの復元、別マシンへの移動が全部この経路。
// **身元は中身で決める**ようにしたので、そもそも世代が上がらない。
func TestRecreatingTheFileWithTheSameBytesChangesNothing(t *testing.T) {
	const secret = "reincarnate7777"
	db, root, path := seed(t, secret)
	id := findMessage(t, db, secret)
	if _, err := retain.Message(db, id, retain.Op{Reason: "テスト", Actor: "retain_test"}); err != nil {
		t.Fatal(err)
	}
	before := scan(t, db, `select count(*) from messages`)

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil { // 新しい inode になる
		t.Fatal(err)
	}

	if _, err := ingest.Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}

	if n := scan(t, db, `select count(*) from source_files where superseded_at is not null`); n != 0 {
		t.Errorf("中身は同じなのに %d 本が旧世代に落ちた", n)
	}
	if after := scan(t, db, `select count(*) from messages`); after != before {
		t.Errorf("メッセージが %d → %d に増えた（二重取り込み）", before, after)
	}
	if n := scan(t, db, `select count(*) from messages where instr(raw_json,?)>0`, secret); n != 0 {
		t.Errorf("消したものが %d 件戻った", n)
	}
}

// 本当に世代が変わった場合でも、消したものは戻らない。
//
// 上の修正は「世代を上げない」ことで守るが、切り詰めて書き直された場合は
// 世代が上がるのが正しい。そのときの保険が tombstone の行ハッシュ。
// **位置でも世代でもなく、行そのものの sha256 で弾く。**
func TestRedactedLineStaysGoneEvenWhenTheFileTrulyRotates(t *testing.T) {
	const secret = "trulyrotated5555"
	db, root, path := seed(t, secret)
	id := findMessage(t, db, secret)
	if _, err := retain.Message(db, id, retain.Op{Reason: "テスト", Actor: "retain_test"}); err != nil {
		t.Fatal(err)
	}

	// 先頭に1行足して書き直す。再開点の中身が変わるので世代が上がる。
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	head := `{"type":"user","uuid":"u-prepended","sessionId":"` + sid +
		`","timestamp":"2026-09-01T00:00:00.000Z","cwd":"/nonexistent/proj",` +
		`"message":{"role":"user","content":"先頭に足した行"}}` + "\n"
	if err := os.WriteFile(path, append([]byte(head), body...), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ingest.Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}

	if n := scan(t, db, `select count(*) from source_files where superseded_at is not null`); n == 0 {
		t.Fatal("世代交代が起きていない。この試験は保険を突けていない")
	}
	if n := scan(t, db, `select count(*) from messages where instr(raw_json,?)>0`, secret); n != 0 {
		t.Errorf("世代が変わったら消したものが %d 件戻った", n)
	}
	if n := scan(t, db, `select count(*) from message_blocks where instr(coalesce(text,''),?)>0`, secret); n != 0 {
		t.Errorf("世代が変わったら message_blocks に %d 件戻った", n)
	}
}
