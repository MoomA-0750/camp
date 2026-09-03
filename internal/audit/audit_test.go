package audit_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoomA-0750/camp/internal/audit"
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

func add(t *testing.T, db *store.DB, action, target string) int64 {
	t.Helper()
	id, err := audit.Append(db, audit.Entry{
		Actor: "user", Action: action, Target: target,
		SessionID: "s-1", Outcome: audit.OK})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// 書き換えも削除もできない。**消せる監査ログは監査にならない。**
func TestItCannotBeChangedOrDeleted(t *testing.T) {
	db := newDB(t)
	id := add(t, db, "tool.approve", "rm -rf /tmp/x")

	if _, err := db.Exec(`update audit set outcome = 'ok' where id = ?`, id); err == nil {
		t.Error("監査ログを書き換えられた")
	} else if !strings.Contains(err.Error(), "書き換えられない") {
		t.Errorf("理由が伝わらない: %v", err)
	}

	if _, err := db.Exec(`delete from audit where id = ?`, id); err == nil {
		t.Error("監査ログを消せた")
	} else if !strings.Contains(err.Error(), "消せない") {
		t.Errorf("理由が伝わらない: %v", err)
	}

	// 行はそのまま残っている。
	var n int
	if err := db.QueryRow(`select count(*) from audit where id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("行が消えている")
	}
}

// 何が起きたか分からない記録は残さない。
func TestItRefusesIncompleteEntries(t *testing.T) {
	db := newDB(t)
	for _, e := range []audit.Entry{
		{Action: "x", Outcome: audit.OK},   // actor が無い
		{Actor: "user", Outcome: audit.OK}, // action が無い
		{Actor: "user", Action: "x"},       // outcome が無い
	} {
		if _, err := audit.Append(db, e); err == nil {
			t.Errorf("欠けた記録を通した: %+v", e)
		}
	}
}

// トリガを外しても、連鎖で気づける。
//
// トリガは DROP TRIGGER で外せるし、このファイルを持っている者は sqlite3 で
// 何でもできる。**止められないので、気づけるようにする。**
func TestTamperingIsCaughtEvenWithoutTheTriggers(t *testing.T) {
	db := newDB(t)
	add(t, db, "session.start", "/home/x/proj")
	mid := add(t, db, "tool.approve", "rm -rf /tmp/x")
	add(t, db, "session.stop", "")

	if n, err := audit.Verify(db); err != nil {
		t.Fatalf("何もしていないのに連鎖が壊れている: %v（%d 行）", err, n)
	}

	// トリガを外して書き換える。
	if _, err := db.Exec(`drop trigger audit_no_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`update audit set target = 'ls' where id = ?`, mid); err != nil {
		t.Fatal(err)
	}
	_, err := audit.Verify(db)
	if err == nil {
		t.Fatal("書き換えに気づかなかった")
	}
	if !strings.Contains(err.Error(), "書き換わっている") {
		t.Errorf("理由が伝わらない: %v", err)
	}
}

// 途中の行を消しても、連鎖で気づける。
func TestADeletedRowBreaksTheChain(t *testing.T) {
	db := newDB(t)
	add(t, db, "session.start", "a")
	mid := add(t, db, "tool.approve", "b")
	add(t, db, "session.stop", "c")

	if _, err := db.Exec(`drop trigger audit_no_delete`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`delete from audit where id = ?`, mid); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.Verify(db); err == nil {
		t.Fatal("消されたことに気づかなかった")
	}
}

// トリガが外れていること自体を検出できる。
func TestMissingTriggersAreVisible(t *testing.T) {
	db := newDB(t)
	have, err := audit.TriggersInPlace(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit.Missing(have)) != 0 {
		t.Errorf("最初からトリガが足りない: %v", have)
	}

	if _, err := db.Exec(`drop trigger audit_no_delete`); err != nil {
		t.Fatal(err)
	}
	have, err = audit.TriggersInPlace(db)
	if err != nil {
		t.Fatal(err)
	}
	miss := audit.Missing(have)
	if len(miss) != 1 || miss[0] != "audit_no_delete" {
		t.Errorf("外れたトリガを見つけられない: %v", miss)
	}
}

// 時系列で読めて、セッションで絞れる。
func TestItReadsBackInOrderAndFiltersBySession(t *testing.T) {
	db := newDB(t)
	for i, a := range []string{"session.start", "tool.approve", "tool.deny", "session.stop"} {
		if _, err := audit.Append(db, audit.Entry{
			Actor: "user", Action: a, SessionID: "s-1", Outcome: audit.OK,
			At: "2026-09-03T00:0" + string(rune('0'+i)) + ":00Z"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := audit.Append(db, audit.Entry{
		Actor: "user", Action: "session.start", SessionID: "s-2", Outcome: audit.OK}); err != nil {
		t.Fatal(err)
	}

	all, err := audit.List(db, audit.Opts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 6 { // 5 + マイグレーションが入れた1行
		t.Errorf("%d 行（6 を期待）", len(all))
	}
	// 新しい順。
	for i := 1; i < len(all); i++ {
		if all[i-1].ID < all[i].ID {
			t.Fatal("新しい順になっていない")
		}
	}

	one, err := audit.List(db, audit.Opts{Session: "s-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 4 {
		t.Errorf("s-1 が %d 行（4 を期待）", len(one))
	}
	for _, e := range one {
		if e.SessionID != "s-1" {
			t.Errorf("よそのセッションが混ざっている: %+v", e)
		}
	}

	byAction, err := audit.List(db, audit.Opts{Action: "tool.approve"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byAction) != 1 {
		t.Errorf("action で絞れていない: %d 行", len(byAction))
	}
}

// 保持ポリシーは監査ログに触らない。消せる監査ログは監査にならない。
func TestRetentionDoesNotTouchTheAuditLog(t *testing.T) {
	db := newDB(t)
	add(t, db, "tool.approve", "何か")

	rows, err := db.Query(`select kind from retention`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(kind, "audit") {
			t.Errorf("監査ログを対象にする規則がある: %s", kind)
		}
	}
}

// doctor 側の連鎖計算と、このパッケージの計算が食い違っていないこと。
//
// store が audit を参照すると import が逆流するので、同じ式が2箇所にある。
// **片方だけ直したら、ここで落ちる。**
func TestTheDoctorAgreesWithTheAuditPackage(t *testing.T) {
	db := newDB(t)
	add(t, db, "session.start", "a")
	add(t, db, "tool.approve", "b")

	mine, err := audit.Verify(db)
	if err != nil {
		t.Fatalf("audit 側が落ちた: %v", err)
	}
	theirs, err := db.AuditChain()
	if err != nil {
		t.Fatalf("doctor 側が落ちた: %v", err)
	}
	if mine != theirs {
		t.Errorf("数えた行数が違う: audit=%d doctor=%d", mine, theirs)
	}

	// 壊したときも両方が気づくこと。
	if _, err := db.Exec(`drop trigger audit_no_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`update audit set actor = 'べつのだれか' where id = 2`); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.Verify(db); err == nil {
		t.Error("audit 側が見逃した")
	}
	if _, err := db.AuditChain(); err == nil {
		t.Error("doctor 側が見逃した")
	}
}
