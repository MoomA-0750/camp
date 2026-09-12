package store

import (
	"path/filepath"
	"testing"
)

// 移行 0027: エージェントの語彙を `claude` / `codex` に揃える（本人の決定 2026-09-12）。
//
// 取り込みは `sessions.agent` に `'claude-code'` を直に書いていて、走っているセッションの表
// （`runtime_sessions.agent`）と `docs/20-data-model.md` は `claude` / `codex` だった。既存の行も
// 直さないと、同じエージェントが2つの名前で台帳に並ぶ。
func TestTheAgentVocabMigrationRenamesOldRows(t *testing.T) {
	db := upTo(t, "0027")

	// 古い語彙の行を置く（移行前の取り込みが書いた形）。
	mustExec(t, db, `insert into hosts(name) values('h')`)
	mustExec(t, db, `insert into projects(host_id, name, repo_path) values(1, 'p', '/p')`)
	mustExec(t, db, `
		insert into sessions(id, host_id, project_id, agent, started_at, updated_at)
		values('s-old', 1, 1, 'claude-code', '2026-09-01T00:00:00Z', '2026-09-01T00:00:00Z')`)
	// ends_at が NULL の窓も混ぜる（IS で比べていないと落ちる）。
	mustExec(t, db, `
		insert into usage_windows(agent, kind, started_at, ends_at, used_pct, source, fetched_at)
		values('claude-code', 'five_hour', '2026-09-01T00:00:00Z', '2026-09-01T05:00:00Z', 12.5,
		       'statusline', '2026-09-01T00:00:00Z')`)
	mustExec(t, db, `
		insert into usage_windows(agent, kind, started_at, ends_at, used_pct, source, fetched_at)
		values('claude-code', 'spend_limit', NULL, NULL, 1.0, 'statusline', '2026-09-01T00:00:00Z')`)

	// 0027 を当てる（Migrate は未適用のものだけ当てる）。
	if _, err := db.Migrate(); err != nil {
		t.Fatal(err)
	}

	var agent string
	if err := db.QueryRow(`select agent from sessions where id='s-old'`).Scan(&agent); err != nil {
		t.Fatal(err)
	}
	if agent != "claude" {
		t.Fatalf("sessions.agent が %q のまま（claude のはず）", agent)
	}

	var old, now int
	if err := db.QueryRow(`select
		  (select count(*) from usage_windows where agent='claude-code'),
		  (select count(*) from usage_windows where agent='claude')`).Scan(&old, &now); err != nil {
		t.Fatal(err)
	}
	if old != 0 || now != 2 {
		t.Fatalf("usage_windows: 古い語彙 %d 件 / 新しい語彙 %d 件（0 と 2 のはず。ends_at が NULL の行も移る）",
			old, now)
	}
}

// **同じ窓が既に新しい語彙で入っていても、移行で落ちない。**
// usage_windows は UNIQUE(agent, kind, ends_at, source) を持つので、素の UPDATE だと衝突する。
func TestTheAgentVocabMigrationSurvivesADuplicateWindow(t *testing.T) {
	db := upTo(t, "0027")
	for _, a := range []string{"claude-code", "claude"} {
		mustExec(t, db, `
			insert into usage_windows(agent, kind, started_at, ends_at, used_pct, source, fetched_at)
			values(?, 'five_hour', '2026-09-01T00:00:00Z', '2026-09-01T05:00:00Z', 20.0,
			       'statusline', '2026-09-01T00:00:00Z')`, a)
	}
	if _, err := db.Migrate(); err != nil {
		t.Fatalf("同じ窓が両方の語彙で入っていると移行が落ちる: %v", err)
	}
	var n int
	if err := db.QueryRow(`select count(*) from usage_windows`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("窓が %d 件（1 件に畳むはず）", n)
	}
}

// upTo は name より前の移行だけを当てた DB を返す。**その移行の前の姿**を作るため。
func upTo(t *testing.T, name string) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "camp.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.Name >= name {
			break
		}
		if err := db.applyOne(m); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func mustExec(t *testing.T, db *DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}
