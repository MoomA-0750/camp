package ingest

import "testing"

// worktree の解決はファイルシステムを見ない。実測で2本は既に消えており、
// ディスクを頼りにすると親を見失う。文字列規則が壊れていないことを固定する。
func TestResolveWorktreeWithoutFilesystem(t *testing.T) {
	// 実在しないパスを使う。ここでディスクを触ったら意図が壊れている。
	const parent = "/nonexistent/git-cloned/Obsidian-Vault"
	cases := []struct {
		cwd      string
		wantRoot string
		wantName string
	}{
		{
			parent + "/.claude/worktrees/bridge-cse_019QyCmwPLtNeEHnuqfXHRby",
			parent + "/.claude/worktrees/bridge-cse_019QyCmwPLtNeEHnuqfXHRby",
			"bridge-cse_019QyCmwPLtNeEHnuqfXHRby",
		},
		{
			// worktree の中でさらに cd した場合も worktree のルートに畳む
			parent + "/.claude/worktrees/bridge-cse_01BSEgt4SG6oTUx7eU4ytPsu/Human/Logs",
			parent + "/.claude/worktrees/bridge-cse_01BSEgt4SG6oTUx7eU4ytPsu",
			"bridge-cse_01BSEgt4SG6oTUx7eU4ytPsu",
		},
	}

	for _, tc := range cases {
		assign, roots := ResolveProjects([]string{tc.cwd})
		got := assign[tc.cwd]
		if got != tc.wantRoot {
			t.Errorf("cwd %s\n got root %s\nwant root %s", tc.cwd, got, tc.wantRoot)
			continue
		}
		ref := roots[got]
		if !ref.IsWorktree || ref.WorktreeName != tc.wantName {
			t.Errorf("cwd %s: is_worktree=%v worktree_name=%q", tc.cwd, ref.IsWorktree, ref.WorktreeName)
		}
		if ref.ParentPath != parent {
			t.Errorf("cwd %s: parent %q, want %q", tc.cwd, ref.ParentPath, parent)
		}
		if ref.IsRepo {
			t.Errorf("cwd %s: 消えた worktree が is_repo=true になっている", tc.cwd)
		}
		if _, ok := roots[parent]; !ok {
			t.Errorf("cwd %s: 親リポジトリが projects に登録されていない", tc.cwd)
		}
	}
}

// 大文字と小文字だけが違うパスは別プロジェクト。
// 実際に Obsidian-Vault と Obsidian-vault が両方存在した。
func TestResolveIsCaseSensitive(t *testing.T) {
	upper := "/nonexistent/git-cloned/Obsidian-Vault"
	lower := "/nonexistent/git-cloned/Obsidian-vault"

	assign, roots := ResolveProjects([]string{upper, lower})
	if assign[upper] == assign[lower] {
		t.Fatalf("大文字小文字の違いが畳まれた: %s", assign[upper])
	}
	if len(roots) != 2 {
		t.Fatalf("projects が %d 件。2件であるべき", len(roots))
	}
}

// 解決は走査順に依存してはいけない。1段階でやると、リポジトリでない
// /tmp と /tmp/camp-test のどちらが親になるかが順序で変わる。
func TestResolveIsOrderIndependent(t *testing.T) {
	a := "/nonexistent/scratch"
	b := "/nonexistent/scratch/sub"

	forward, _ := ResolveProjects([]string{a, b})
	reverse, _ := ResolveProjects([]string{b, a})

	if forward[a] != reverse[a] || forward[b] != reverse[b] {
		t.Fatalf("順序で結果が変わった:\n  順方向 %v\n  逆方向 %v", forward, reverse)
	}
	if forward[a] != a || forward[b] != b {
		t.Fatalf("リポジトリでないディレクトリが畳まれた: %v", forward)
	}
}
