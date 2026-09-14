package notes

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Camp の commit を見分けるトレーラー。**題の頭だけでは見分けない**（人やエージェントが
// 同じ題を書きうる）。値は campd の待ち行の id。
const Trailer = "Camp-Commit"

// Entry は commit を待つ1ファイル。SHA は Camp が書いた中身のハッシュ。
type Entry struct {
	ID   int64  `json:"id"`
	Path string `json:"path"`
	SHA  string `json:"sha"`
	// Delete は `.trash/` へ移したノートの削除を commit する（M55）。SHA は移した中身。
	Delete bool `json:"delete,omitempty"`
}

// CommitResult は commit の結果。
type CommitResult struct {
	Commit string `json:"commit,omitempty"` // 作った commit。何も commit しなければ空
	// Done は片付いた待ち行（commit に入れた・もう HEAD と同じ中身だった）。
	Done []int64 `json:"done,omitempty"`
	// Overwritten は、Camp が書いたあとで別の書き手が中身を変えていた（または消した）もの。
	// **commit しない。** Camp の版は campd の blobs にある（Fable の設計レビュー 3）。
	Overwritten []Entry `json:"overwritten,omitempty"`
	// Mismatch は commit に入った中身が照合した中身と違った（照合と commit の間に書かれた）。
	// 取り消さずに知らせる。
	Mismatch []string `json:"mismatch,omitempty"`
}

// Push の結果の種類。
const (
	PushDone          = "pushed"         // 出した
	PushNothing       = "nothing"        // 出すものが無い
	PushAgentPending  = "agent_pending"  // Camp 以外の commit が push を待っている（本人の決定5）
	PushBehindDirty   = "behind_dirty"   // GitHub 側が進んでいて、書きかけと重なるので取り込めない
	PushConflict      = "merge_conflict" // 取り込むとぶつかる。戻した
	PushHook          = "hook_refused"   // pre-push が止めた。取り込み直さない
	PushRejected      = "rejected"       // 送る間に GitHub 側が進んだ。次の契機に
	PushBusy          = "busy"           // ほかの git が動いている（index.lock・merge の途中など）
	PushRemoteRefused = "remote_refused" // GitHub の規則が断った（push protection など）。取り込み直さない
	PushFailed        = "failed"         // それ以外（鍵・網）
)

// PushResult は push の結果。
type PushResult struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail,omitempty"`
	// Pending は Camp 以外の、push を待つ commit の数（agent_pending のとき）。
	Pending int `json:"pending,omitempty"`
	// Merged は取り込みの commit を作った。MergeSHA はその id（campd が Camp の commit として覚える）。
	Merged   bool   `json:"merged,omitempty"`
	MergeSHA string `json:"merge_sha,omitempty"`
}

// Git は Vault の作業コピーを commit・push する者。**実行面が持つ。**
type Git struct {
	Disk   *Disk
	Remote string // 既定 origin
	Branch string // 既定 master

	// Timeout は1つの git の長さ（fetch・push は網を待つ）。
	Timeout time.Duration
}

func (g *Git) remote() string {
	if g.Remote == "" {
		return "origin"
	}
	return g.Remote
}

func (g *Git) branch() string {
	if g.Branch == "" {
		return "master"
	}
	return g.Branch
}

// run は git を1つ走らせる。
//
//   - **`GIT_LITERAL_PATHSPECS=1`**: ノート名の `*` `?` `[` `:` をグロブや魔法として読ませない
//     （読むと別のファイルまで commit しうる。Fable の設計レビュー 10）
//   - **`SKIP_PUSH_GUARD` を外す**: pre-push の守りを Camp の経路で外さない（設計の制約9）
//   - 端末へ訊かない（`GIT_TERMINAL_PROMPT=0`）。訊かれたら失敗させる
func (g *Git) run(stdin []byte, args ...string) (string, string, error) {
	t := g.Timeout
	if t == 0 {
		t = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), t)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.Disk.Dir()}, args...)...)
	// git の知らせは英語で受ける（失敗の分け方を文字で見ているので、翻訳されると読み違える）。
	// 文字の扱い（LC_CTYPE）は本人の設定のまま残す。
	env := []string{"GIT_LITERAL_PATHSPECS=1", "GIT_TERMINAL_PROMPT=0", "LC_MESSAGES=C"}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "SKIP_PUSH_GUARD", "GIT_LITERAL_PATHSPECS", "GIT_TERMINAL_PROMPT",
			"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "LANGUAGE", "LC_MESSAGES":
			continue
		case "LC_ALL":
			if v != "" {
				env = append(env, "LC_CTYPE="+v)
			}
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// errBusy はほかの git が作業コピーを使っている。
var errBusy = errors.New("ほかの git が動いている")

// ready は、commit・取り込みをしてよい状態かを見る。
//
// **HEAD が決まったブランチを指し、merge・rebase・cherry-pick・revert の途中でない。**
// 途中のときに commit すると、エージェントの `rebase --abort` などでディスクから消える
// （Fable の設計レビュー 3）。
func (g *Git) ready() error {
	head, _, err := g.run(nil, "symbolic-ref", "-q", "HEAD")
	if err != nil || strings.TrimSpace(head) != "refs/heads/"+g.branch() {
		return fmt.Errorf("%w: HEAD が %s を指していない（%s）", errBusy, g.branch(), strings.TrimSpace(head))
	}
	for _, name := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply", "index.lock"} {
		p, _, err := g.run(nil, "rev-parse", "--git-path", name)
		if err != nil {
			return err
		}
		p = strings.TrimSpace(p)
		if !strings.HasPrefix(p, "/") {
			p = g.Disk.Dir() + "/" + p
		}
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("%w: %s がある（ほかの作業の途中）", errBusy, name)
		}
	}
	return nil
}

// IsBusy は「待ってやり直せばよい」失敗か。
func IsBusy(err error) bool { return errors.Is(err, errBusy) }

// gitBlobID は git がその中身に付ける blob の id（sha1）。commit に入った中身を照らすのに使う。
func gitBlobID(b []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(b))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// Commit は、待ち行のうち**ディスクの中身が Camp の書いたままのもの**だけを1つの commit にする。
//
// `git commit --only -- <paths>`: 渡したパスの作業ツリーの中身だけが入り、ほかの人が stage した
// ものは入らない（stage はそのまま残る）。
func (g *Git) Commit(entries []Entry) (CommitResult, error) {
	var res CommitResult
	if err := g.ready(); err != nil {
		return res, err
	}
	var paths []string
	var ids []string
	blobs := map[string]string{}
	var adds []string
	for _, e := range entries {
		if e.Delete {
			b, err := g.Disk.Read(e.Path)
			switch {
			case err == nil:
				// 移したあとで誰かが同じパスに作り直した。削除を commit しない。
				ov := e
				ov.SHA = Sum(b)
				res.Overwritten = append(res.Overwritten, ov)
				continue
			case !errors.Is(err, os.ErrNotExist):
				return res, err
			}
			out, _, err := g.run(nil, "ls-tree", "HEAD", "--", e.Path)
			if err != nil {
				return res, err
			}
			if strings.TrimSpace(out) == "" {
				res.Done = append(res.Done, e.ID) // HEAD に無い（commit される前に移した）
				continue
			}
			paths = append(paths, e.Path)
			ids = append(ids, strconv.FormatInt(e.ID, 10))
			blobs[e.Path] = ""
			res.Done = append(res.Done, e.ID)
			continue
		}
		b, err := g.Disk.Read(e.Path)
		switch {
		case err != nil && !errors.Is(err, os.ErrNotExist):
			return res, err
		case err != nil || Sum(b) != e.SHA:
			ov := e
			if err == nil {
				ov.SHA = Sum(b)
			} else {
				ov.SHA = ""
			}
			res.Overwritten = append(res.Overwritten, ov)
			continue
		}
		// もう HEAD と同じ中身なら commit するものが無い（エージェントが同じ中身を commit した等）。
		out, _, err := g.run(nil, "ls-tree", "HEAD", "--", e.Path)
		if err != nil {
			return res, err
		}
		if f := strings.Fields(out); len(f) >= 3 && f[2] == gitBlobID(b) {
			res.Done = append(res.Done, e.ID)
			continue
		}
		paths = append(paths, e.Path)
		adds = append(adds, e.Path)
		ids = append(ids, strconv.FormatInt(e.ID, 10))
		blobs[e.Path] = gitBlobID(b)
		res.Done = append(res.Done, e.ID)
	}
	if len(paths) == 0 {
		return res, nil
	}
	subject := "Camp: " + paths[0]
	if blobs[paths[0]] == "" {
		subject += " を .trash へ"
	}
	if len(paths) > 1 {
		subject = fmt.Sprintf("Camp: %d ファイル", len(paths))
	}
	msg := subject + "\n\n" + strings.Join(paths, "\n") + "\n\n" + Trailer + ": " + strings.Join(ids, ",") + "\n"
	args := append([]string{"commit", "--only", "--cleanup=verbatim", "-F", "-", "--"}, paths...)
	// --only は untracked のパス（新しいノート）を拒むので、先に intent-to-add で知らせる。
	// 削除するパスは作業ツリーに無いので渡さない。
	if len(adds) > 0 {
		add := append([]string{"add", "--intent-to-add", "--"}, adds...)
		if _, e, err := g.run(nil, add...); err != nil {
			return CommitResult{Overwritten: res.Overwritten}, g.gitErr("add", e, err)
		}
	}
	if _, e, err := g.run([]byte(msg), args...); err != nil {
		return CommitResult{Overwritten: res.Overwritten}, g.gitErr("commit", e, err)
	}
	head, _, err := g.run(nil, "rev-parse", "HEAD")
	if err != nil {
		return res, err
	}
	res.Commit = strings.TrimSpace(head)
	for _, p := range paths {
		out, _, err := g.run(nil, "ls-tree", "HEAD", "--", p)
		f := strings.Fields(out)
		switch {
		case err != nil:
			res.Mismatch = append(res.Mismatch, p)
		case blobs[p] == "" && len(f) != 0: // 削除のはずが残っている
			res.Mismatch = append(res.Mismatch, p)
		case blobs[p] != "" && (len(f) < 3 || f[2] != blobs[p]):
			res.Mismatch = append(res.Mismatch, p)
		}
	}
	return res, nil
}

func (g *Git) gitErr(what, stderr string, err error) error {
	if strings.Contains(stderr, "index.lock") {
		return fmt.Errorf("%w: %s（index.lock）", errBusy, what)
	}
	return fmt.Errorf("git %s: %v: %s", what, err, lastLines(stderr, 6))
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Push は GitHub へ出す。**push を待つ commit が全部 Camp の作ったものであるときだけ**（本人の決定5）。
//
// **Camp の commit かは、campd が覚えている commit の id（known）で見る。** トレーラーの文字では見ない——
// エージェントが Camp の commit を `--amend` したり、同じトレーラーを書いたりすると、中身の変わった commit が
// 本人の確認なしに出てしまう（Phase 5 outer gate、Fable 5・codex）。
//
// 出すのは**照らした時点の commit の id**（HEAD ではない）。照らしたあとでエージェントが commit しても、
// それは出さない（codex）。GitHub 側が進んでいれば merge で取り込む（本人の決定7）。
// 失敗は、進んでいた／フックが止めた／GitHub が断った／それ以外 に分ける（Fable の設計レビュー 2）。
func (g *Git) Push(known []string) (PushResult, error) {
	if err := g.ready(); err != nil {
		if IsBusy(err) {
			return PushResult{Kind: PushBusy, Detail: err.Error()}, nil
		}
		return PushResult{}, err
	}
	up := g.remote() + "/" + g.branch()
	if _, e, err := g.run(nil, "fetch", "--quiet", g.remote(), g.branch()); err != nil {
		return PushResult{Kind: PushFailed, Detail: "取り寄せられない: " + lastLines(e, 4)}, nil
	}
	target, err := g.revParse("HEAD")
	if err != nil {
		return PushResult{}, err
	}
	upSHA, err := g.revParse(up)
	if err != nil {
		return PushResult{}, err
	}
	out, _, err := g.run(nil, "rev-list", upSHA+".."+target)
	if err != nil {
		return PushResult{}, err
	}
	isKnown := make(map[string]bool, len(known))
	for _, k := range known {
		isKnown[k] = true
	}
	ahead, foreign := 0, 0
	for _, c := range strings.Fields(out) {
		ahead++
		if !isKnown[c] {
			foreign++
		}
	}
	if ahead == 0 {
		return PushResult{Kind: PushNothing}, nil
	}
	if foreign > 0 {
		return PushResult{Kind: PushAgentPending, Pending: foreign,
			Detail: fmt.Sprintf("Camp 以外の commit が %d 本 push を待っている", foreign)}, nil
	}

	res := PushResult{}
	behind, _, err := g.run(nil, "rev-list", "--count", target+".."+upSHA)
	if err != nil {
		return PushResult{}, err
	}
	if strings.TrimSpace(behind) != "0" {
		// **ほかの merge の途中なら触らない。** merge が失敗したときに戻すのは、Camp の merge がぶつかったときだけ
		// （エージェントの merge を `--abort` すると、手で解いた中身が消える。codex）。
		if g.inMerge() {
			return PushResult{Kind: PushBusy, Detail: "ほかの merge の途中"}, nil
		}
		msg := "Camp: GitHub の変更を取り込む\n\n" + Trailer + ": merge\n"
		_, e, err := g.run(nil, "merge", "--no-ff", "--no-edit", "-m", msg, upSHA)
		if err != nil {
			switch {
			case strings.Contains(e, "not concluded your merge") || strings.Contains(e, "MERGE_HEAD exists"):
				return PushResult{Kind: PushBusy, Detail: "ほかの merge の途中"}, nil
			case strings.Contains(e, "index.lock"):
				return PushResult{Kind: PushBusy, Detail: "index.lock"}, nil
			case g.inMerge() && g.mergeHeadIs(upSHA):
				g.run(nil, "merge", "--abort")
				return PushResult{Kind: PushConflict, Detail: "取り込むとぶつかるので戻した: " + lastLines(e, 6)}, nil
			}
			return PushResult{Kind: PushBehindDirty, Detail: "取り込めない（書きかけと重なる）: " + lastLines(e, 6)}, nil
		}
		merge, err := g.revParse("HEAD")
		if err != nil {
			return PushResult{}, err
		}
		res.Merged, res.MergeSHA = true, merge
		// merge の親が照らした2つでなければ（その間にエージェントが commit した）、出さない。
		parents, _, err := g.run(nil, "rev-list", "--parents", "-n", "1", merge)
		if err != nil {
			return PushResult{}, err
		}
		if f := strings.Fields(parents); len(f) != 3 || f[1] != target || f[2] != upSHA {
			res.Kind, res.Detail = PushAgentPending, "取り込む間にほかの commit が入った"
			return res, nil
		}
		target = merge
	}

	if beforePush != nil {
		beforePush()
	}
	o, e, err := g.run(nil, "push", "--porcelain", g.remote(), target+":refs/heads/"+g.branch())
	if err == nil {
		res.Kind = PushDone
		return res, nil
	}
	switch {
	case strings.Contains(o+e, "[remote rejected]"):
		// GitHub の規則（push protection など）が断った。取り込んでも通らない。
		res.Kind, res.Detail = PushRemoteRefused, lastLines(o+e, 6)
	case strings.Contains(o+e, "[rejected]"):
		res.Kind, res.Detail = PushRejected, "送る間に GitHub 側が進んだ"
	case strings.Contains(e, "fatal:"):
		res.Kind, res.Detail = PushFailed, lastLines(e, 4)
	default:
		// 取り寄せは通ったのに ref が断られていない失敗は、pre-push が止めたもの。
		// フックの出力はラベルだけ（値を出さない）なので、そのまま画面へ渡してよい。
		res.Kind, res.Detail = PushHook, lastLines(e, 8)
	}
	return res, nil
}

// beforePush は試験で「照らしたあとでエージェントが commit した」を作るための差し込み口。
var beforePush func()

func (g *Git) revParse(ref string) (string, error) {
	out, e, err := g.run(nil, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %v: %s", ref, err, lastLines(e, 3))
	}
	return strings.TrimSpace(out), nil
}

// mergeHeadIs は MERGE_HEAD が sha を指しているか（Camp が始めた merge か）。
func (g *Git) mergeHeadIs(sha string) bool {
	out, _, err := g.run(nil, "rev-parse", "--verify", "--quiet", "MERGE_HEAD")
	return err == nil && strings.TrimSpace(out) == sha
}

func (g *Git) inMerge() bool {
	p, _, err := g.run(nil, "rev-parse", "--git-path", "MERGE_HEAD")
	if err != nil {
		return false
	}
	p = strings.TrimSpace(p)
	if !strings.HasPrefix(p, "/") {
		p = g.Disk.Dir() + "/" + p
	}
	_, err = os.Lstat(p)
	return err == nil
}
