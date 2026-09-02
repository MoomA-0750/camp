package ingest

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoomA-0750/camp/internal/store"
)

// deltaLine は version 1 のバックアップの記録。実データでは delta の
// backup.version は常に 1 で、2 以上は1件も出てこない。
func deltaLine(aUUID, trackingPath, name string, minute int) string {
	m := string(rune('0' + minute))
	return `{"type":"file-history-delta","messageId":"` + aUUID + `","trackingPath":"` + trackingPath + `",` +
		`"timestamp":"2026-09-02T00:0` + m + `:02.000Z",` +
		`"backup":{"backupFileName":"` + name + `","version":1,"backupTime":"2026-09-02T00:0` + m + `:02.000Z",` +
		`"realParentDir":"/nonexistent/proj/notes"}}` + "\n"
}

// snapshotLine は version 2 以上のバックアップの記録。名前が載るのは
// こちら側だけなので、delta しか読まない実装では実体の大半に名前が付かない。
func snapshotLine(aUUID, trackingPath, name string, version, minute int) string {
	m := string(rune('0' + minute))
	v := string(rune('0' + version))
	return `{"type":"file-history-snapshot","messageId":"` + aUUID + `","isSnapshotUpdate":false,` +
		`"timestamp":"2026-09-02T00:0` + m + `:03.000Z",` +
		`"snapshot":{"messageId":"` + aUUID + `","timestamp":"2026-09-02T00:0` + m + `:03.000Z",` +
		`"trackedFileBackups":{"` + trackingPath + `":{"backupFileName":"` + name + `","version":` + v + `,` +
		`"backupTime":"2026-09-02T00:0` + m + `:03.000Z","realParentDir":"/nonexistent/proj/notes"}}}}` + "\n"
}

// blobFile は ~/.claude/file-history/<sessionId>/<name> を作る。
func blobFile(t *testing.T, fh, sid, name, body string) string {
	t.Helper()
	dir := filepath.Join(fh, sid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// M9 の受け入れそのもの。CLI 側が実体を捨てても中身が読めること。
func TestBackupSurvivesDeletionOfTheOriginal(t *testing.T) {
	db := newTestDB(t)
	root, fh := t.TempDir(), t.TempDir()
	const sid = "dddddddd-2222-4333-8444-555555555555"

	writeSession(t, root, sid, convoLines(sid, 0, 1)+
		deltaLine("a1", "proj/notes/x.md", "aaaa@v1", 1))
	blob := blobFile(t, fh, sid, "aaaa@v1", "編集される前の中身\n")

	if _, err := Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	r, err := CaptureBackups(db, fh)
	if err != nil {
		t.Fatal(err)
	}
	if r.Captured != 1 || r.Scanned != 1 {
		t.Fatalf("%+v", r)
	}

	// CLI が GC した、の再現。
	if err := os.Remove(blob); err != nil {
		t.Fatal(err)
	}
	r2, err := CaptureBackups(db, fh)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Scanned != 0 || r2.Missing != 1 {
		t.Fatalf("消滅を見つけていない: %+v", r2)
	}

	var absPath, missing, codec string
	var content []byte
	if err := db.QueryRow(`
		select b.abs_path, coalesce(b.missing_at, ''), l.codec, l.content
		  from file_backups b join blobs l on l.sha256 = b.sha256`).
		Scan(&absPath, &missing, &codec, &content); err != nil {
		t.Fatal(err)
	}
	if absPath != "/nonexistent/proj/notes/x.md" {
		t.Fatalf("パスが復元できていない: %q", absPath)
	}
	if missing == "" {
		t.Fatal("missing_at が入っていない")
	}
	body, err := UnpackBlob(codec, content)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "編集される前の中身\n" {
		t.Fatalf("中身が読めない: %q", body)
	}

	// 消えたあとにもう一度回しても、行も中身も消えない。
	if _, err := CaptureBackups(db, fh); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `select count(*) from file_backups`); n != 1 {
		t.Fatalf("行が %d 件になった", n)
	}
}

// 保管の同一性は (session_id, backup_name)。名前だけでは足りない。
// 実測 809 個のブロブに対して名前は 618 種しかなく、同じ <hash>@v<N> が
// 別セッションの下に並んで存在する（ハッシュはパスから作られるので、
// 同じファイルを複数のセッションが触れば必ずぶつかる）。
//
// 名前だけを鍵にすると、素性が混ざり、片方の実体が消えても気付けなくなる。
func TestBackupNameCollidesAcrossSessions(t *testing.T) {
	db := newTestDB(t)
	root, fh := t.TempDir(), t.TempDir()
	const s1 = "e1111111-2222-4333-8444-555555555555"
	const s2 = "e2222222-2222-4333-8444-555555555555"

	// 同じ名前のブロブだが、指しているノートも中身も違う。
	notes := map[string]string{s1: "x.md", s2: "y.md"}
	for i, sid := range []string{s1, s2} {
		writeSession(t, root, sid, convoLines(sid, 0, 1)+
			deltaLine("a1", "proj/notes/"+notes[sid], "same@v1", i+1))
		blobFile(t, fh, sid, "same@v1", "セッション"+sid[:2]+"の中身\n")
	}

	if _, err := Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	if r, err := CaptureBackups(db, fh); err != nil {
		t.Fatal(err)
	} else if r.Captured != 2 {
		t.Fatalf("同名を1つに畳んでしまった: %+v", r)
	}
	if n := count(t, db, `select count(distinct sha256) from file_backups`); n != 2 {
		t.Fatalf("中身が %d 種しかない", n)
	}
	// 素性が混ざっていないこと。
	for sid, note := range notes {
		var abs string
		if err := db.QueryRow(`select abs_path from file_backups where session_id = ?`, sid).
			Scan(&abs); err != nil {
			t.Fatal(err)
		}
		if abs != "/nonexistent/proj/notes/"+note {
			t.Fatalf("%s のパスが %q（期待 %s）", sid[:2], abs, note)
		}
	}

	// 片方だけ消す。名前だけを鍵にしていると、もう片方が残っているせいで
	// 消えたことに気付けない。
	if err := os.Remove(filepath.Join(fh, s1, "same@v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureBackups(db, fh); err != nil {
		t.Fatal(err)
	}
	var gone string
	if err := db.QueryRow(`select session_id from file_backups where missing_at is not null`).
		Scan(&gone); err != nil {
		t.Fatalf("消滅を1件も見つけていない: %v", err)
	}
	if gone != s1 {
		t.Fatalf("消滅の印が %s に付いた", gone[:2])
	}
	if n := count(t, db, `select count(*) from file_backups where missing_at is not null`); n != 1 {
		t.Fatalf("消滅の印が %d 件", n)
	}
}

// version 2 以上の backupFileName は snapshot にしか出ない。
// delta だけ読む実装は、実体の半分以上を素性不明のまま取り込むことになる。
func TestSnapshotIsTheOnlySourceForLaterVersions(t *testing.T) {
	db := newTestDB(t)
	root, fh := t.TempDir(), t.TempDir()
	const sid = "ffffffff-2222-4333-8444-555555555555"

	writeSession(t, root, sid, convoLines(sid, 0, 1)+
		deltaLine("a1", "proj/notes/x.md", "aaaa@v1", 1)+
		snapshotLine("a2", "proj/notes/x.md", "aaaa@v2", 2, 2))
	blobFile(t, fh, sid, "aaaa@v1", "1版目\n")
	blobFile(t, fh, sid, "aaaa@v2", "2版目\n")

	if _, err := Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureBackups(db, fh); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Query(`select backup_name, coalesce(version, 0), origin, coalesce(abs_path,'')
	                         from file_backups order by backup_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var n, o, p string
		var v int64
		if err := rows.Scan(&n, &v, &o, &p); err != nil {
			t.Fatal(err)
		}
		got = append(got, n+" v"+string(rune('0'+v))+" "+o+" "+p)
	}
	want := []string{
		"aaaa@v1 v1 delta /nonexistent/proj/notes/x.md",
		"aaaa@v2 v2 snapshot /nonexistent/proj/notes/x.md",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// 何度回しても増えない。取り込みのたびに走らせるので、ここが崩れると
// ブロブが二重に積み上がる。
func TestCaptureIsIdempotentAndDedupsContent(t *testing.T) {
	db := newTestDB(t)
	root, fh := t.TempDir(), t.TempDir()
	const sid = "aaaaaaaa-9999-4333-8444-555555555555"

	writeSession(t, root, sid, convoLines(sid, 0, 1)+
		deltaLine("a1", "proj/notes/x.md", "aaaa@v1", 1)+
		snapshotLine("a2", "proj/notes/y.md", "bbbb@v2", 2, 2))
	blobFile(t, fh, sid, "aaaa@v1", "同じ中身\n")
	blobFile(t, fh, sid, "bbbb@v2", "同じ中身\n")

	if _, err := Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		r, err := CaptureBackups(db, fh)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if r.Captured != 2 || r.Deduped != 1 {
				t.Fatalf("初回: %+v", r)
			}
		} else if r.Captured != 0 || r.Known != 2 || r.Missing != 0 {
			t.Fatalf("%d 回目: %+v", i+1, r)
		}
	}
	// 中身が同じなら1本しか持たない。
	if n := count(t, db, `select count(*) from blobs`); n != 1 {
		t.Fatalf("blobs が %d 行", n)
	}
	if n := count(t, db, `select count(*) from file_backups`); n != 2 {
		t.Fatalf("file_backups が %d 行", n)
	}
}

// 取り込みが自分で捕獲まで済ませること。参照だけ残して中身を取り逃すと
// あとから拾い直せない（CLI 側が消してしまうため）。
func TestIngestCapturesBackups(t *testing.T) {
	db := newTestDB(t)
	home := t.TempDir()
	root := filepath.Join(home, "projects")
	fh := filepath.Join(home, "file-history")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	const sid = "bbbbbbbb-9999-4333-8444-555555555555"
	writeSession(t, root, sid, convoLines(sid, 0, 1)+
		deltaLine("a1", "proj/notes/x.md", "aaaa@v1", 1))
	blobFile(t, fh, sid, "aaaa@v1", "取り込みが拾うべき中身\n")

	res, err := Ingest(db, "testhost", root)
	if err != nil {
		t.Fatal(err)
	}
	if res.Backups == nil || res.Backups.Captured != 1 {
		t.Fatalf("取り込みが捕獲していない: %+v", res.Backups)
	}
}

// 圧縮は往復できること、そして縮まないものは生で置くこと。
func TestPackBlobRoundTrip(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(""),
		[]byte("短い"),
		bytes.Repeat([]byte("繰り返して縮むテキスト\n"), 500),
	} {
		codec, packed := packBlob(body)
		got, err := UnpackBlob(codec, packed)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("往復しない: %d bytes", len(body))
		}
		if len(packed) > len(body) {
			t.Fatalf("太っている: %d -> %d (%s)", len(body), len(packed), codec)
		}
	}
}

// 参照が見つからないブロブでも中身は拾う。JSONL が先に消えて実体だけ
// 残る順序はありうる（実コーパスでは 0 件だが、拾わない理由がない）。
func TestOrphanBlobIsStillCaptured(t *testing.T) {
	db := newTestDB(t)
	root, fh := t.TempDir(), t.TempDir()
	const sid = "cccccccc-9999-4333-8444-555555555555"

	writeSession(t, root, sid, convoLines(sid, 0, 1))
	blobFile(t, fh, sid, "orphan@v1", "参照は無いが中身はある\n")

	if _, err := Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureBackups(db, fh); err != nil {
		t.Fatal(err)
	}
	var origin string
	var abs any
	if err := db.QueryRow(`select origin, abs_path from file_backups`).Scan(&origin, &abs); err != nil {
		t.Fatal(err)
	}
	if origin != backupOrphan || abs != nil {
		t.Fatalf("origin=%q abs=%v", origin, abs)
	}
}

// blobs / file_backups の整合。外部キーが効いていること。
func TestBackupRequiresKnownSession(t *testing.T) {
	db := newTestDB(t)
	_, err := db.Exec(`insert into blobs(sha256,size,codec,content,stored_at) values('x',1,'raw',x'00','t')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`insert into file_backups(session_id,backup_name,sha256,origin,captured_at)
	                  values('居ないセッション','n@v1','x','delta','t')`)
	if err == nil {
		t.Fatal("知らないセッションの行が入ってしまった")
	}
}

// 素性は派生値なので、中身を読み直さずに作り直せなければならない（D-014）。
// パスの組み立てを直したとき、すでに捕獲した行にも当てられること。
func TestBackfillBackupMetaRestoresDerivedColumns(t *testing.T) {
	db := newTestDB(t)
	root, fh := t.TempDir(), t.TempDir()
	const sid = "dddddddd-9999-4333-8444-555555555555"

	writeSession(t, root, sid, convoLines(sid, 0, 1)+
		deltaLine("a1", "proj/notes/x.md", "aaaa@v1", 1)+
		snapshotLine("a2", "proj/notes/x.md", "aaaa@v2", 2, 2))
	blobFile(t, fh, sid, "aaaa@v1", "1版目\n")
	blobFile(t, fh, sid, "aaaa@v2", "2版目\n")

	if _, err := Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureBackups(db, fh); err != nil {
		t.Fatal(err)
	}
	before := dumpBackups(t, db)

	// 素性を壊す。中身（blobs）はそのまま。
	if _, err := db.Exec(`update file_backups
	                         set abs_path = null, rel_path = null, version = null, origin = 'orphan'`); err != nil {
		t.Fatal(err)
	}
	n, err := BackfillBackupMeta(db)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("作り直したのが %d 行", n)
	}
	after := dumpBackups(t, db)
	if strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("復元できていない:\n%s\n---\n%s",
			strings.Join(after, "\n"), strings.Join(before, "\n"))
	}
	// 2回目は1行も動かさない。
	if n, err := BackfillBackupMeta(db); err != nil || n != 0 {
		t.Fatalf("冪等でない: %d, %v", n, err)
	}
	// 中身は一度も読み直していないので blobs は増えない。
	if n := count(t, db, `select count(*) from blobs`); n != 2 {
		t.Fatalf("blobs が %d 行", n)
	}
}

func dumpBackups(t *testing.T, db *store.DB) []string {
	t.Helper()
	rows, err := db.Query(`
		select session_id || '|' || backup_name || '|' || coalesce(abs_path, '') || '|' ||
		       coalesce(rel_path, '') || '|' || coalesce(version, 0) || '|' || origin || '|' || sha256
		  from file_backups order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}
