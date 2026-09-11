package store

import (
	"path/filepath"
	"testing"
)

// 移行 0025（確認の度合い）: それより前の行は cli、**ただし Phase 3.6 の Codex の行は legacy**
// （専用の置き場で approvalPolicy=untrusted を渡して起こしていた。cli と書くと、本人の設定のまま
// 起こしたことになってしまう）。
func TestThePermMigrationMarksOldCodexRowsAsLegacy(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "camp.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.Name >= "0025" {
			break
		}
		if err := db.applyOne(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range [][2]string{{"c1", "claude"}, {"x1", "codex"}} {
		if _, err := db.Exec(`insert into runtime_sessions(id, cwd, state, requested_by, created_at,
			updated_at, agent) values(?, '/w', 'exited', 'test', '2026-09-11T00:00:00Z',
			'2026-09-11T00:00:00Z', ?)`, row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`insert into ssh_hosts(alias, allowed, source, seen_at, updated_at, claude_path)
		values('far', 1, 'ssh_config', '2026-09-11T00:00:00Z', '2026-09-11T00:00:00Z', '/opt/claude')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	// 移行 0026: claude の場所は、エージェントごとの場所の行へ写る。
	var path string
	if err := db.QueryRow(`select path from ssh_agent_paths where host='far' and agent='claude'`).
		Scan(&path); err != nil || path != "/opt/claude" {
		t.Fatalf("claude の場所が写っていない: %q %v", path, err)
	}
	for id, want := range map[string]string{"c1": "cli", "x1": "legacy"} {
		var got string
		if err := db.QueryRow(`select perm from runtime_sessions where id=?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s の確認の度合いが %q（%q のはず）", id, got, want)
		}
	}
}
