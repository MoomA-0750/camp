package vault

import (
	"testing"

	"github.com/MoomA-0750/camp/internal/store"
)

// seedSession は最小の session_files を作る。
func seedSession(t *testing.T, db *store.DB, absPaths ...string) {
	t.Helper()
	if _, err := db.Exec(`
		insert or ignore into hosts(id, name) values(1, 'h');
		insert or ignore into projects(id, host_id, repo_path, name) values(1, 1, '/p', 'p');
		insert or ignore into sessions(id, host_id, project_id, agent, started_at, updated_at)
		values('s1', 1, 1, 'claude', 't', 't')`); err != nil {
		t.Fatal(err)
	}
	for i, p := range absPaths {
		if _, err := db.Exec(`
			insert into session_files(session_id, abs_path, op, origin, at)
			values('s1', ?, 'edit', 'test', ?)`, p, "2026-09-0"+string(rune('1'+i))); err != nil {
			t.Fatal(err)
		}
	}
}

func vaultID(t *testing.T, db *store.DB) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`select id from vaults limit 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func noteID(t *testing.T, db *store.DB, path string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`select id from notes where path = ?`, path).Scan(&id); err != nil {
		t.Fatalf("note %s: %v", path, err)
	}
	return id
}

// 突き合わせは完全一致。**大文字小文字を畳まない。**
//
// 畳むと、もう存在しないディレクトリ（改名前の Obsidian-vault）への操作を、
// 現存するノートへの操作として表示することになる。実測で18パス・85行ある。
func TestTouchMatchingIsCaseSensitive(t *testing.T) {
	db := newDB(t)
	root := mkVault(t, map[string]string{"Human/Projects/Camp.md": "本文"})
	index(t, db, root)

	// 同じ相対パスだが、ルートの表記が違う（改名前を模す）
	seedSession(t, db,
		root+"/Human/Projects/Camp.md",
		lowerLast(root)+"/Human/Projects/Camp.md")

	ts, err := NoteTouches(db, noteID(t, db, "Human/Projects/Camp.md"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 {
		t.Fatalf("表記が一致する1件だけのはずが %d: %+v", len(ts), ts)
	}
}

// 触った記録はあるが実体が無いパスは ghost として出す。分類を付ける。
func TestGhostsAreClassified(t *testing.T) {
	db := newDB(t)
	root := mkVault(t, map[string]string{"生きてる.md": "本文"})
	index(t, db, root)

	seedSession(t, db,
		root+"/生きてる.md",                          // 索引にある → ghost ではない
		root+"/Human/Learning/消えたフォルダ.md",        // gone
		root+"/.claude/worktrees/wt/Data/コピー.md", // worktree
		root+"/.githooks/README.md",              // hidden
		lowerLast(root)+"/Human/Projects/改名前.md", // other-case
		"/tmp/まったく別の場所.md")                       // Vault と無関係 → 出さない

	gs, err := Ghosts(db, vaultID(t, db))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, g := range gs {
		got[g.Reason] = g.Path
	}
	for _, want := range []string{GhostGone, GhostWorktree, GhostHidden, GhostOtherCase} {
		if _, ok := got[want]; !ok {
			t.Errorf("%s が出ていない: %+v", want, gs)
		}
	}
	if len(gs) != 4 {
		t.Errorf("4件のはずが %d（Vault 外や索引済みが混ざっている）: %+v", len(gs), gs)
	}
}

// セッション側からノートを引ける。
func TestSessionNotes(t *testing.T) {
	db := newDB(t)
	root := mkVault(t, map[string]string{"a.md": "A", "b/c.md": "C"})
	index(t, db, root)
	seedSession(t, db, root+"/a.md", root+"/b/c.md", root+"/無い.md")

	ns, err := SessionNotes(db, "s1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 2 {
		t.Fatalf("索引にある2件のはずが %d: %+v", len(ns), ns)
	}
}

// 消えたノートでも、行が残っている限りバックリンクは辿れる。
func TestTouchesSurviveNoteDeletion(t *testing.T) {
	db := newDB(t)
	root := mkVault(t, map[string]string{"消える.md": "本文"})
	index(t, db, root)
	seedSession(t, db, root+"/消える.md")
	id := noteID(t, db, "消える.md")

	if err := removeFile(root, "消える.md"); err != nil {
		t.Fatal(err)
	}
	index(t, db, root)

	ts, err := NoteTouches(db, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 {
		t.Fatalf("消えたあとも触り跡は残るはず: %+v", ts)
	}
}

// 中身の版数は file_backups.abs_path から数える。
// session_files の backup_name は file-history 由来の行にしか入っておらず
// （実測 2,418行中 2,246行が NULL）、そちら経由で数えると空振りする。
func TestGhostVersionsComeFromBackupPath(t *testing.T) {
	db := newDB(t)
	root := mkVault(t, map[string]string{"生きてる.md": "本文"})
	index(t, db, root)

	gone := root + "/Human/Learning/消えた.md"
	// backup_name の無い触り跡（実データの大半がこの形）
	seedSession(t, db, gone)

	if _, err := db.Exec(`
		insert into blobs(sha256, size, codec, content, stored_at)
		values('aa', 5, 'raw', x'68656c6c6f', 't');
		insert into file_backups(session_id, backup_name, version, abs_path, rel_path,
		                         backup_time, sha256, origin, captured_at)
		values('s1', 'h@v1', 1, ?, 'Human/Learning/消えた.md', 't', 'aa', 'delta', 't'),
		      ('s1', 'h@v2', 2, ?, 'Human/Learning/消えた.md', 't', 'aa', 'delta', 't')`,
		gone, gone); err != nil {
		t.Fatal(err)
	}

	gs, err := Ghosts(db, vaultID(t, db))
	if err != nil {
		t.Fatal(err)
	}
	var g *Ghost
	for i := range gs {
		if gs[i].Reason == GhostGone {
			g = &gs[i]
		}
	}
	if g == nil {
		t.Fatalf("gone が出ていない: %+v", gs)
	}
	if g.Backups != 2 {
		t.Errorf("版数=%d、2 のはず（backup_name 経由で数えると 0 になる）", g.Backups)
	}
	if len(g.Versions) != 2 {
		t.Fatalf("読める版を2つ返すはず: %+v", g.Versions)
	}
	if g.Versions[0].Version != 2 {
		t.Errorf("新しい版が先頭のはず: %+v", g.Versions)
	}
	if g.Versions[0].BackupID == 0 {
		t.Error("中身を引く id が入っていない")
	}
}

// 版の並びは時刻順。版番号はセッション内でしか意味を持たないので、
// 版順に並べると複数セッションの記録が混ざって時系列が壊れる。
func TestGhostVersionsAreOrderedByTime(t *testing.T) {
	db := newDB(t)
	root := mkVault(t, map[string]string{"生きてる.md": "本文"})
	index(t, db, root)
	gone := root + "/Human/Learning/消えた.md"
	seedSession(t, db, gone)

	if _, err := db.Exec(`
		insert into blobs(sha256, size, codec, content, stored_at)
		values('aa', 5, 'raw', x'68656c6c6f', 't');
		insert into sessions(id, host_id, project_id, agent, started_at, updated_at)
		values('s2', 1, 1, 'claude', 't', 't');
		insert into file_backups(session_id, backup_name, version, abs_path, rel_path,
		                         backup_time, sha256, origin, captured_at)
		values('s1', 'h@v3', 3, ?, 'r', '2026-08-11T12:04:00Z', 'aa', 'delta', 't'),
		      ('s2', 'h@v1', 1, ?, 'r', '2026-08-17T03:12:00Z', 'aa', 'delta', 't')`,
		gone, gone); err != nil {
		t.Fatal(err)
	}

	gs, err := Ghosts(db, vaultID(t, db))
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range gs {
		if g.Reason != GhostGone {
			continue
		}
		if len(g.Versions) != 2 {
			t.Fatalf("2版のはず: %+v", g.Versions)
		}
		// 版番号だと v3 が先に来るが、時刻では 08-17 の v1 が新しい。
		if g.Versions[0].Version != 1 {
			t.Errorf("時刻順になっていない（版順に並べている）: %+v", g.Versions)
		}
	}
}
