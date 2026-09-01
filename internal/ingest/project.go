package ingest

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// worktreeMarker は Claude Code が作る worktree の置き場。
// 実測の値は `.claude/worktrees/bridge-cse_XXXX`（ディレクトリ、区切りはアンダースコア）。
const worktreeMarker = "/.claude/worktrees/"

// ProjectRef は cwd を解決した結果。
type ProjectRef struct {
	Path         string // リポジトリのルート。大文字小文字はそのまま
	Name         string // 表示名
	IsWorktree   bool
	WorktreeName string
	ParentPath   string // worktree の親リポジトリ
	// IsRepo は取り込み時点でディスク上にリポジトリのルートとして
	// 存在したか。消えた worktree や消えたクローンは 0 になる。
	IsRepo    bool
	GitOrigin string
}

// ResolveProjects は cwd の集合をプロジェクトのルートへ畳む。
//
// 解決は2段階に分ける。1段目でパス規則とファイルシステムから決まるものを
// 確定させ、2段目で「どちらでも決まらなかったもの」を1段目の結果に対して
// 前方一致で寄せる。1段階でやると、走査順によって /tmp と /tmp/camp-test の
// どちらが親になるかが変わってしまう。
//
// 返り値は cwd -> ルート、およびルート -> 定義。
func ResolveProjects(cwds []string) (map[string]string, map[string]*ProjectRef) {
	roots := map[string]*ProjectRef{}
	assign := map[string]string{}
	var unresolved []string

	for _, cwd := range cwds {
		ref := resolveKnown(cwd)
		if ref == nil {
			unresolved = append(unresolved, cwd)
			continue
		}
		addRoot(roots, ref)
		assign[cwd] = ref.Path
	}

	// 2段目。1段目で確定したルートのうち、最も長い前方一致に寄せる。
	// worktree が消えていても、親が残っていればここで拾える。
	known := make([]string, 0, len(roots))
	for p := range roots {
		known = append(known, p)
	}
	sort.Slice(known, func(i, j int) bool { return len(known[i]) > len(known[j]) })

	for _, cwd := range unresolved {
		matched := ""
		for _, p := range known {
			if cwd == p || strings.HasPrefix(cwd, p+"/") {
				matched = p
				break
			}
		}
		if matched == "" {
			// リポジトリでもなく、既知のどれの下でもない。cwd 自身を
			// プロジェクトとして残す（/tmp や、既に消えたクローンなど）。
			ref := &ProjectRef{Path: cwd, Name: filepath.Base(cwd)}
			addRoot(roots, ref)
			matched = cwd
		}
		assign[cwd] = matched
	}
	return assign, roots
}

// resolveKnown はパス規則とファイルシステムだけで決まる解決を行う。
// 決まらなければ nil を返す。
func resolveKnown(cwd string) *ProjectRef {
	// worktree はファイルシステムを見ない。実測で2本は既に消えているが、
	// 文字列を割るだけなら親リポジトリまで確実に辿れる。
	if i := strings.Index(cwd, worktreeMarker); i >= 0 {
		parent := cwd[:i]
		rest := cwd[i+len(worktreeMarker):]
		name := rest
		if j := strings.Index(rest, "/"); j >= 0 {
			name = rest[:j]
		}
		if name == "" {
			return nil
		}
		root := parent + worktreeMarker + name
		return &ProjectRef{
			Path:         root,
			Name:         filepath.Base(parent),
			IsWorktree:   true,
			WorktreeName: name,
			ParentPath:   parent,
			IsRepo:       gitRoot(root) == root,
			GitOrigin:    gitOrigin(root),
		}
	}

	if root := gitRoot(cwd); root != "" {
		return &ProjectRef{
			Path:      root,
			Name:      filepath.Base(root),
			IsRepo:    true,
			GitOrigin: gitOrigin(root),
		}
	}
	return nil
}

// gitRoot は cwd から上に辿って .git を持つ最初のディレクトリを返す。
// worktree では .git がファイルなので、ディレクトリ限定にしない。
//
// git を起動しないのは、cwd が多いホストで無駄にプロセスを生やさないため。
// cwd 自身が消えていても、祖先が残っていれば解決できる。
func gitRoot(cwd string) string {
	if !filepath.IsAbs(cwd) {
		return ""
	}
	d := filepath.Clean(cwd)
	for {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// gitOrigin は origin の URL を日和見的に取る。取れなくても構わない。
func gitOrigin(root string) string {
	if _, err := os.Stat(root); err != nil {
		return ""
	}
	out, err := exec.Command("git", "-C", root, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func addRoot(roots map[string]*ProjectRef, ref *ProjectRef) {
	if cur, ok := roots[ref.Path]; ok {
		// 同じルートに二度到達した。情報の多いほうを残す。
		if cur.GitOrigin == "" && ref.GitOrigin != "" {
			cur.GitOrigin = ref.GitOrigin
		}
		return
	}
	roots[ref.Path] = ref
	// worktree の親も必ずプロジェクトとして登録する。
	// 親の cwd で作業した記録が1件も無くても、畳んで表示するのに要る。
	if ref.ParentPath != "" {
		if _, ok := roots[ref.ParentPath]; !ok {
			roots[ref.ParentPath] = &ProjectRef{
				Path:      ref.ParentPath,
				Name:      filepath.Base(ref.ParentPath),
				IsRepo:    gitRoot(ref.ParentPath) == ref.ParentPath,
				GitOrigin: gitOrigin(ref.ParentPath),
			}
		}
	}
}
