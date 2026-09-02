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
