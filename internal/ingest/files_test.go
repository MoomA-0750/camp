package ingest

import (
	"encoding/json"
	"testing"

	"github.com/MoomA-0750/camp/internal/store"
)

func parseLine(t *testing.T, s string) *Line {
	t.Helper()
	var l Line
	if err := json.Unmarshal([]byte(s), &l); err != nil {
		t.Fatal(err)
	}
	l.Raw = []byte(s)
	return &l
}

// Read の結果だけ filePath が1段深い。filePath 直下しか見ない実装だと
// 「読んだだけのノート」が丸ごと落ちる（実コーパスで 617件・53パスが消える）。
// 消えるのは Inbox のように読まれたあと .trash へ移されたノートで、
// そういうノートこそ Camp にしか残っていない。
func TestReadPathIsOneLevelDeeper(t *testing.T) {
	l := parseLine(t, `{"type":"user","uuid":"u1","timestamp":"2026-09-02T00:00:00.000Z",
		"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"x"}]},
		"toolUseResult":{"type":"text","file":{"filePath":"/vault/Inbox/消えたメモ.md","content":"..."}}}`)

	if w, r := l.ToolResultPaths(); w != "" || r != "/vault/Inbox/消えたメモ.md" {
		t.Fatalf("written=%q read=%q", w, r)
	}
	refs := extractFileRefs(l)
	if len(refs) != 1 || refs[0].Op != opRead || refs[0].AbsPath != "/vault/Inbox/消えたメモ.md" {
		t.Fatalf("%+v", refs)
	}
	if refs[0].ToolUseID != "tu_1" {
		t.Fatalf("tool_use_id が取れていない: %+v", refs[0])
	}
}

// file-history 行は自分の絶対パスを持たない。trackingPath は
// プロジェクト相対だったり絶対だったりする（実コーパスで両方ある）ので、
// realParentDir + basename で組み立てる。
func TestFileHistoryBuildsAbsPathFromParentDir(t *testing.T) {
	for _, tp := range []string{
		"Obsidian-Vault/Data/Todo/x.md",
		"/home/me/Documents/Obsidian-Vault/Data/Todo/x.md",
		"x.md",
	} {
		l := parseLine(t, `{"type":"file-history-delta","messageId":"a1",
			"trackingPath":"`+tp+`","timestamp":"2026-09-02T00:00:00.000Z",
			"backup":{"backupFileName":"deadbeef@v1","version":1,
			"realParentDir":"/home/me/Documents/Obsidian-Vault/Data/Todo"}}`)
		refs := extractFileRefs(l)
		if len(refs) != 1 {
			t.Fatalf("%s: %+v", tp, refs)
		}
		if got := refs[0].AbsPath; got != "/home/me/Documents/Obsidian-Vault/Data/Todo/x.md" {
			t.Fatalf("%s -> %s", tp, got)
		}
		if refs[0].TargetUUID != "a1" || refs[0].BackupName != "deadbeef@v1" || refs[0].BackupVersion != 1 {
			t.Fatalf("%+v", refs[0])
		}
	}
}

// 大文字小文字を畳んではいけない。実コーパスには Obsidian-Vault と
// Obsidian-vault の両方があり（改名の前後）、5つのノートが名前だけ同じで
// 別々のパスに残っている。畳むと、触っていない側を触ったことにする。
func TestRelToDoesNotCaseFold(t *testing.T) {
	const root = "/home/me/Documents/git-cloned/Obsidian-Vault"
	if got := relTo(root, root+"/Human/Projects/ExampleProject.md"); got != "Human/Projects/ExampleProject.md" {
		t.Fatalf("同じ綴りで相対化できていない: %q", got)
	}
	lower := "/home/me/Documents/git-cloned/Obsidian-vault/Human/Projects/ExampleProject.md"
	if got := relTo(root, lower); got != "" {
		t.Fatalf("小文字vのパスを畳んでしまった: %q", got)
	}
}

// 相対パスは採らない。cwd はセッションの途中で変わるので、
// 当て推量で絶対化すると存在しないノートへのリンクが静かに増える。
func TestRelativePathsAreDropped(t *testing.T) {
	l := parseLine(t, `{"type":"user","uuid":"u1","timestamp":"2026-09-02T00:00:00.000Z",
		"toolUseResult":{"filePath":"docs/10-decisions.md"},
		"attachment":{"type":"file","filename":"notes/x.md"}}`)
	if refs := extractFileRefs(l); len(refs) != 0 {
		t.Fatalf("相対パスを拾ってしまった: %+v", refs)
	}
}

// attachment は種別ごとに意味が違う。索引の対象にならない種別
// （task_reminder, skill_listing など）を op なしで取り込まない。
func TestAttachmentOpVocabulary(t *testing.T) {
	for kind, want := range map[string]string{
		"edited_text_file":       opExternal,
		"file":                   opAttach,
		"compact_file_reference": opMention,
		"task_reminder":          "",
		"skill_listing":          "",
	} {
		if got := attachmentOp(kind); got != want {
			t.Fatalf("%s -> %q（期待 %q）", kind, got, want)
		}
	}
}

// 1ターンぶんの Edit。tool_use・tool_result・file-history-delta が
// 別々の行に散らばっているところまで含めて再現する。
//
// historyFirst は実コーパスの並び。403件中372件で、バックアップの記録が
// それを発行したアシスタント行より先に書かれている。
func editTurn(sid, aUUID, tuid, tool, path string, minute int, historyFirst bool) string {
	m := string(rune('0' + minute))
	history := `{"type":"file-history-delta","messageId":"` + aUUID + `","trackingPath":"proj/` + base(path) + `",` +
		`"timestamp":"2026-09-02T00:0` + m + `:02.000Z",` +
		`"backup":{"backupFileName":"cafe@v1","version":1,"realParentDir":"/nonexistent/proj/notes"}}` + "\n"
	turn := `{"type":"assistant","uuid":"` + aUUID + `","sessionId":"` + sid + `","session_id":"r-` + sid + `",` +
		`"timestamp":"2026-09-02T00:0` + m + `:00.000Z","cwd":"/nonexistent/proj",` +
		`"message":{"id":"m_` + aUUID + `","role":"assistant","model":"claude-opus-5",` +
		`"content":[{"type":"tool_use","id":"` + tuid + `","name":"` + tool + `","input":{"file_path":"` + path + `"}}]}}` + "\n" +
		`{"type":"user","uuid":"u_` + aUUID + `","parentUuid":"` + aUUID + `","sessionId":"` + sid + `","session_id":"r-` + sid + `",` +
		`"timestamp":"2026-09-02T00:0` + m + `:01.000Z","cwd":"/nonexistent/proj",` +
		`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + tuid + `","content":"ok"}]},` +
		`"toolUseResult":{"filePath":"` + path + `","oldString":"a","newString":"b"}}` + "\n"
	if historyFirst {
		return history + turn
	}
	return turn + history
}

// base は末尾のファイル名。テストの中でだけ使う。
func base(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

// 結合の受け入れ。file-history 行は uuid も sessionId も持たないのに、
// 「そのノートを触ったターン」まで辿れなければ意味がない。
func TestSessionFilesLinkBackToTheTurn(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	const sid = "bbbbbbbb-2222-4333-8444-555555555555"

	// x.md はバックアップが先（実コーパスの多数派）、y.md はあと。
	body := convoLines(sid, 0, 1) +
		editTurn(sid, "a1", "tu_1", "Edit", "/nonexistent/proj/notes/x.md", 1, true) +
		editTurn(sid, "a2", "tu_2", "Write", "/nonexistent/proj/notes/y.md", 2, false)
	writeSession(t, root, sid, body)

	if _, err := Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}

	// file-history は自分の行ではなく、編集を発行したアシスタント行に繋ぐ。
	// 並び順（先か後か）で結果が変わってはいけない。
	for path, want := range map[string]string{
		"/nonexistent/proj/notes/x.md": "a1",
		"/nonexistent/proj/notes/y.md": "a2",
	} {
		var uuid, rel string
		if err := db.QueryRow(`
			select coalesce(m.uuid, ''), coalesce(f.rel_path, '') from session_files f
			  left join messages m on m.id = f.message_id
			 where f.origin = 'file-history' and f.abs_path = ?`, path).Scan(&uuid, &rel); err != nil {
			t.Fatal(err)
		}
		if uuid != want {
			t.Fatalf("%s の繋ぎ先が %q（期待 %q）", path, uuid, want)
		}
		if rel != "notes/"+base(path) {
			t.Fatalf("rel_path が出ていない: %q", rel)
		}
	}

	// op はツール名から引く。Edit と Write を取り違えない。
	for path, want := range map[string]string{
		"/nonexistent/proj/notes/x.md": "edit",
		"/nonexistent/proj/notes/y.md": "write",
	} {
		var op string
		if err := db.QueryRow(`
			select op from session_files where origin = 'tool-result' and abs_path = ?`,
			path).Scan(&op); err != nil {
			t.Fatal(err)
		}
		if op != want {
			t.Fatalf("%s の op が %s（期待 %s）", path, op, want)
		}
	}
}

// 派生表はディスクを読み直さずに作り直せなければならない（D-014）。
// 取り込みで書いたものと、raw_json から作り直したものが一致すること。
func TestBackfillSessionFilesMatchesIngest(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	const sid = "cccccccc-2222-4333-8444-555555555555"

	body := convoLines(sid, 0, 1) +
		editTurn(sid, "a1", "tu_1", "Edit", "/nonexistent/proj/notes/x.md", 1, true) +
		editTurn(sid, "a2", "tu_2", "Write", "/nonexistent/proj/notes/y.md", 2, false)
	writeSession(t, root, sid, body)
	if _, err := Ingest(db, "testhost", root); err != nil {
		t.Fatal(err)
	}

	before := dumpFiles(t, db)
	if len(before) == 0 {
		t.Fatal("取り込みで1件も繋がっていない")
	}

	for i := 0; i < 2; i++ { // 2回回しても増えない
		if _, _, err := BackfillSessionFiles(db); err != nil {
			t.Fatal(err)
		}
		after := dumpFiles(t, db)
		if len(after) != len(before) {
			t.Fatalf("%d 回目: %d 件 -> %d 件", i+1, len(before), len(after))
		}
		for j := range after {
			if after[j] != before[j] {
				t.Fatalf("%d 回目: %q != %q", i+1, after[j], before[j])
			}
		}
	}
}

func dumpFiles(t *testing.T, db *store.DB) []string {
	t.Helper()
	rows, err := db.Query(`
		select f.session_id || '|' || coalesce(m.uuid, '') || '|' || f.abs_path || '|' ||
		       coalesce(f.rel_path, '') || '|' || f.op || '|' || f.origin || '|' ||
		       coalesce(f.backup_name, '') || '|' || f.at
		  from session_files f left join messages m on m.id = f.message_id
		 order by 1`)
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
