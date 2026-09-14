// Package noteedit は campd の側の編集面（Phase 5 / M53、2026-09-13）。
//
// 開く（ディスクを直に読む）・保存（実行面に書かせる）・ぶつかったら合わせる・commit と push を
// 回す、を持つ。**Vault のファイルには書かない**——書くのは実行面（internal/notes、D-024）。
// 設計は `dev/active/phase5-design.md` の「書く口」。
package noteedit

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoomA-0750/camp/internal/audit"
	"github.com/MoomA-0750/camp/internal/notes"
	"github.com/MoomA-0750/camp/internal/store"
	"github.com/MoomA-0750/camp/internal/vault"
)

// Writer は実行面へ頼む口（session.Supervisor）。試験で差し替える。
type Writer interface {
	NoteWrite(vault, path, base, body string, create, reauthed bool) (notes.Result, error)
	NoteCommit(vault string, entries []notes.Entry) (notes.CommitResult, error)
	NotePush(vault string, known []string) (notes.PushResult, error)
	NoteTrash(vault, path, base string, reauthed bool) (notes.TrashResult, error)
}

// Service は campd の側の編集面。
type Service struct {
	DB *store.DB
	W  Writer
	// IsBusy は「待ってやり直せばよい」失敗か（session.ErrNoteBusy）。
	IsBusy func(error) bool

	// CommitAfter は、そのパスが書かれなくなってから commit するまで（本人の決定2。初期値 1 分）。
	CommitAfter time.Duration
	// PushRetry は、出せなかったときにもう一度試すまで（取り寄せで網を使うので短くしない）。
	PushRetry time.Duration

	mu       sync.Mutex
	push     notes.PushResult
	pushAt   time.Time
	pushWant bool // Camp の commit が出ていないかもしれない

	links linkCache // プレビューの wikilink を解決する索引（resolve.go）
}

// Source は開いたノート。
type Source struct {
	NoteID int64  `json:"note_id"`
	Path   string `json:"path"`
	Body   string `json:"body"`
	SHA    string `json:"sha256"`
	// Editable は編集面から書けるか。Instruction はパスワードを入れ直せば書ける。
	Editable    bool   `json:"editable"`
	Instruction bool   `json:"instruction,omitempty"`
	ReadOnly    string `json:"read_only,omitempty"` // 書けない理由
}

// Open はノートを**ディスクから直に**読む。
//
// DB の索引は遅れるので、書き始めた中身にしない（Fable の設計レビュー 4: 遅れた中身を base に
// すると、エージェントが触ったノートが毎回「変わっていた」になる）。読んだ中身は blobs に入れる
// ——ぶつかったときに合わせる共通の祖先になり、Camp の控えにもなる。
func (s *Service) Open(noteID int64) (*Source, error) {
	n, err := vault.OneNote(s.DB, noteID)
	if err != nil {
		return nil, err
	}
	if n == nil {
		return nil, fs.ErrNotExist
	}
	root, _, _, err := vault.VaultRoot(s.DB, n.VaultID)
	if err != nil {
		return nil, err
	}
	body, err := readPlain(root, n.Path)
	if err != nil {
		return nil, err
	}
	sha, err := vault.PutBlob(s.DB, body)
	if err != nil {
		return nil, err
	}
	src := &Source{NoteID: noteID, Path: n.Path, Body: string(body), SHA: sha}
	switch c, why := notes.Classify(n.Path); c {
	case notes.Editable:
		src.Editable = true
	case notes.Instruction:
		src.Editable, src.Instruction = true, true
	default:
		src.ReadOnly = why
	}
	if err := notes.CheckText(body); err != nil {
		src.Editable, src.Instruction = false, false
		src.ReadOnly = "編集面では直せない中身: " + err.Error()
	}
	return src, nil
}

// DiskSHA はディスクの今の中身のハッシュだけを返す（開いている間に変わったかを見る。blobs には入れない）。
func (s *Service) DiskSHA(noteID int64) (string, error) {
	n, err := vault.OneNote(s.DB, noteID)
	if err != nil {
		return "", err
	}
	if n == nil {
		return "", fs.ErrNotExist
	}
	root, _, _, err := vault.VaultRoot(s.DB, n.VaultID)
	if err != nil {
		return "", err
	}
	b, err := readPlain(root, n.Path)
	if err != nil {
		return "", err
	}
	return notes.Sum(b), nil
}

// readPlain は Vault の中の1本を読む（symlink を通さない）。campd は ACL で読める。
func readPlain(root, rel string) ([]byte, error) {
	d, err := notes.OpenDisk(root)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.Read(rel)
}

// SaveResult は保存の結果。
type SaveResult struct {
	// Status は saved（書いた）・merged（ぶつかったが重ならないので合わせて書いた）・
	// conflict（同じところを直していた。書いていない）。
	Status string `json:"status"`
	SHA    string `json:"sha256"`
	// SentSHA は画面が送った本文のハッシュ。**merged のあと画面はこれを次の base にする**
	// ——合わせた版の sha を base にすると、次の保存が照合を通って直接書き、相手の編集を消す
	// （Fable の M54 設計レビュー 1）。送った版は blobs にあるので、次の保存で合わせられる。
	SentSHA string `json:"sent_sha256"`
	// Body は merged のときの合わせた本文。画面はこれに置き換える。
	Body string `json:"body,omitempty"`
	// Disk・DiskSHA は conflict のときのディスクの今の中身。画面は本人の版を捨てずに並べる。
	Disk    string `json:"disk,omitempty"`
	DiskSHA string `json:"disk_sha256,omitempty"`
}

// ErrReadOnly は編集面から書けないノート。
var ErrReadOnly = errors.New("直せないノート")

// ErrNeedReauth は、指示の紙なのにパスワードを入れ直していない。
var ErrNeedReauth = errors.New("パスワードを入れ直さないと直せない")

// Save はノートを保存する。base は書き始めた中身のハッシュ、reauthed はパスワードを入れ直したか。
func (s *Service) Save(noteID int64, base, body string, reauthed bool) (*SaveResult, error) {
	n, err := vault.OneNote(s.DB, noteID)
	if err != nil {
		return nil, err
	}
	if n == nil {
		return nil, fs.ErrNotExist
	}
	root, _, _, err := vault.VaultRoot(s.DB, n.VaultID)
	if err != nil {
		return nil, err
	}
	switch c, why := notes.Classify(n.Path); {
	case c == notes.ReadOnly:
		s.audit("note.refused", n.Path, why, audit.Denied)
		return nil, fmt.Errorf("%w: %s", ErrReadOnly, why)
	case c == notes.Instruction && !reauthed:
		s.audit("note.refused", n.Path, "指示の紙を再認証なしで", audit.Denied)
		return nil, ErrNeedReauth
	}
	if err := notes.CheckText([]byte(body)); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrReadOnly, err)
	}

	res, err := s.write(n.VaultID, root, n.Path, base, body, false, reauthed)
	if err != nil {
		return nil, err
	}
	sent := notes.Sum([]byte(body))
	if res.Status != notes.StatusChanged {
		return &SaveResult{Status: "saved", SHA: res.DiskSHA, SentSHA: sent}, nil
	}
	out, err := s.merge(n.VaultID, root, n.Path, base, body, res.DiskSHA, reauthed)
	if out != nil {
		out.SentSHA = sent
	}
	return out, err
}

// write は待ち行に「書く予定」を入れてから実行面に書かせる。
func (s *Service) write(vaultID int64, root, rel, base, body string, create, reauthed bool) (notes.Result, error) {
	// **本文は先に blobs へ。** 書けなくても、上書きされても、Camp の版は残る（復旧の土台）。
	sha, err := vault.PutBlob(s.DB, []byte(body))
	if err != nil {
		return notes.Result{}, err
	}
	now := nowStr()
	r, err := s.DB.Exec(`insert into note_writes(vault_id, path, sha256, state, base_sha256, created_at, updated_at)
		values(?, ?, ?, 'planned', ?, ?, ?)`, vaultID, rel, sha, nullStr(base), now, now)
	if err != nil {
		return notes.Result{}, err
	}
	id, _ := r.LastInsertId()

	res, err := s.W.NoteWrite(root, rel, base, body, create, reauthed)
	if err != nil {
		// **「書く予定」のまま残す。** 返事が届かなかっただけで、実行面は書けているかもしれない。
		// 定期の照合（Tick → reconcile）がディスクと照らして決める（outer gate の Fable 3）。
		s.setState(id, "planned", err.Error())
		s.audit("note.write", rel, err.Error(), audit.Error)
		return notes.Result{}, err
	}
	switch res.Status {
	case notes.StatusWritten, notes.StatusSame:
		sup, err := s.DB.Exec(`update note_writes set state = 'superseded', updated_at = ?
			where vault_id = ? and path = ? and state = 'pending' and op = 'write' and id <> ?`, now, vaultID, rel, id)
		if err != nil {
			return res, err
		}
		s.setState(id, "pending", "")
		mtime := time.Now()
		if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err == nil {
			mtime = fi.ModTime()
		}
		vault.TouchNote(s.DB, vaultID, rel, []byte(body), mtime)
		// **監査は commit を待つ1回ぶんに1行。** 自動保存は書くのをやめるたびに走るので、
		// 保存ごとに書くと監査ログ（保持の対象外）が積み上がる。中身の版は note_writes に全部ある。
		if n, _ := sup.RowsAffected(); n == 0 && res.Status == notes.StatusWritten {
			s.audit("note.write", rel, "sha256="+short(sha), audit.OK)
		}
	case notes.StatusExists:
		s.setState(id, "dropped", "新しく作ろうとしたが既にあった")
	default:
		s.setState(id, "dropped", "書き始めたあとで変わっていた")
	}
	return res, nil
}

// merge は、ディスクの今の中身・書き始めた版・本人の版を git merge-file で合わせる（本人の決定6）。
//
// 重ならなければ合わせた本文を書く。重なれば書かない（**何も捨てない**——本人の版は画面と blobs に、
// ディスクの版はディスクにある）。
func (s *Service) merge(vaultID int64, root, rel, base, body, diskSHA string, reauthed bool) (*SaveResult, error) {
	disk, err := readPlain(root, rel)
	if err != nil {
		return nil, err
	}
	// **ディスクの版も blobs に控える。** 本人が「自分の版で上書き」を選ぶと、相手の未 commit の編集は
	// ほかのどこにも残らない（Fable の M54 設計レビュー 5）。
	diskBlob, err := vault.PutBlob(s.DB, disk)
	if err != nil {
		return nil, err
	}
	conflict := &SaveResult{Status: "conflict", Disk: string(disk), DiskSHA: diskBlob}
	baseBody, err := vault.BlobBody(s.DB, base)
	if err != nil {
		return nil, err
	}
	if baseBody == nil {
		// 共通の祖先が無い（開いた記録が無い）。合わせずに見せる。
		s.audit("note.conflict", rel, "書き始めた版の控えが無い。ディスクの版 sha256="+short(diskBlob), audit.Denied)
		return conflict, nil
	}
	// 合わせるのは**いま読んだ**ディスクの中身（実行面が返したハッシュのあとでまた変わっていても、
	// 読んだものと合わせて、それを土台に書く。その間にまた変われば書く口の照合で止まる）。
	diskSHA = notes.Sum(disk)
	merged, clean, err := Merge3(baseBody, disk, []byte(body))
	if err != nil {
		return nil, err
	}
	if !clean {
		s.audit("note.conflict", rel, "同じところを直していた。ディスクの版 sha256="+short(diskBlob), audit.Denied)
		return conflict, nil
	}
	res, err := s.write(vaultID, root, rel, diskSHA, string(merged), false, reauthed)
	if err != nil {
		return nil, err
	}
	if res.Status == notes.StatusChanged {
		s.audit("note.conflict", rel, "合わせている間にまた変わった。ディスクの版 sha256="+short(diskBlob), audit.Denied)
		return conflict, nil
	}
	s.audit("note.merge", rel, "重ならない編集を合わせた", audit.OK)
	return &SaveResult{Status: "merged", SHA: res.DiskSHA, Body: string(merged)}, nil
}

// Merge3 は3つの版を合わせる。clean は重なりが無かったか。
//
// `git merge-file` を**本人の設定を読まずに**呼ぶ（campd の PrivateTmp の中の一時ファイル）。
func Merge3(base, ours, theirs []byte) (merged []byte, clean bool, err error) {
	dir, err := os.MkdirTemp("", "camp-merge-")
	if err != nil {
		return nil, false, err
	}
	defer os.RemoveAll(dir)
	names := []string{"ours", "base", "theirs"}
	for i, b := range [][]byte{ours, base, theirs} {
		if err := os.WriteFile(filepath.Join(dir, names[i]), b, 0o600); err != nil {
			return nil, false, err
		}
	}
	cmd := exec.Command("git", "merge-file", "-p", "-L", "ディスク", "-L", "書き始め", "-L", "Camp",
		filepath.Join(dir, "ours"), filepath.Join(dir, "base"), filepath.Join(dir, "theirs"))
	cmd.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=" + dir, "PATH=" + os.Getenv("PATH")}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return out.Bytes(), true, nil
	case errors.As(err, &ee) && ee.ExitCode() > 0 && ee.ExitCode() < 127:
		return out.Bytes(), false, nil
	}
	return nil, false, fmt.Errorf("git merge-file: %v: %s", err, errb.String())
}

// Reconcile は起き抜けに「書く予定」のまま残った行をディスクと照らす（Fable の設計レビュー 9）。
func (s *Service) Reconcile() (pending, dropped int, err error) {
	return s.reconcile(time.Now().Add(time.Second), true)
}

// staleAfter は、返事の届かなかった「書く予定」の行を定期に照らすまで（実行面の返事の待ちより長く）。
const staleAfter = 2 * time.Minute

// reconcile は before より前に更新された「書く予定」の行をディスクと照らす。
//
//   - 同じパスに**より新しい行**（書く予定・commit を待つ・commit 済み）があれば、この行は superseded
//     （古い行を commit を待つに戻すと、新しい行の commit のあとで「上書きされた」と誤って浮かぶ。outer gate の Fable 4）
//   - write: ディスクがこの行の中身なら pending（同じパスの古い pending は superseded）、違えば dropped
//   - trash: 元の場所に無ければ pending（索引で消えた印・同じパスの古い行は superseded）、在れば dropped
func (s *Service) reconcile(before time.Time, startup bool) (pending, dropped int, err error) {
	type row struct {
		id      int64
		vaultID int64
		path    string
		sha     string
		op      string
		newer   bool
	}
	var rows []row
	q, err := s.DB.Query(`select w.id, w.vault_id, w.path, w.sha256, w.op,
		exists (select 1 from note_writes x where x.vault_id = w.vault_id and x.path = w.path and x.id > w.id
		        and x.state in ('planned', 'pending', 'committed'))
		from note_writes w where w.state = 'planned' and w.updated_at <= ? order by w.id`, before.UTC().Format(timeFmt))
	if err != nil {
		return 0, 0, err
	}
	for q.Next() {
		var r row
		if err := q.Scan(&r.id, &r.vaultID, &r.path, &r.sha, &r.op, &r.newer); err != nil {
			q.Close()
			return 0, 0, err
		}
		rows = append(rows, r)
	}
	q.Close()
	when := "起動時に"
	if !startup {
		when = "返事が届かなかったので"
	}
	for _, r := range rows {
		if r.newer {
			s.setState(r.id, "superseded", when+"照らしたが、同じパスに新しい行がある")
			continue
		}
		root, _, _, err := vault.VaultRoot(s.DB, r.vaultID)
		if err != nil {
			return pending, dropped, err
		}
		b, rerr := readPlain(root, r.path)
		if r.op == opTrash {
			// 移したかどうかは「元の場所に無い」で見る。
			if errors.Is(rerr, fs.ErrNotExist) {
				s.supersedeOlder(r.vaultID, r.path, r.id)
				s.setState(r.id, "pending", when+"ディスクと照らして移せていた")
				vault.MarkMissing(s.DB, r.vaultID, r.path)
				pending++
				continue
			}
			s.setState(r.id, "dropped", when+"ディスクと照らして移せていなかった")
			s.audit("note.trash", r.path, "移せたか分からなかった。元の場所に残っている", audit.Error)
			dropped++
			continue
		}
		if rerr == nil && notes.Sum(b) == r.sha {
			s.supersedeOlder(r.vaultID, r.path, r.id)
			s.setState(r.id, "pending", when+"ディスクと照らして書けていた")
			pending++
			continue
		}
		s.setState(r.id, "dropped", when+"ディスクと照らして書けていなかった")
		if startup {
			s.audit("note.write", r.path, "書けたか分からないまま落ちた。ディスクは Camp の版ではない", audit.Error)
		}
		dropped++
	}
	return pending, dropped, nil
}

// supersedeOlder は同じパスの、id より古い commit を待つ行を superseded にする。
func (s *Service) supersedeOlder(vaultID int64, path string, id int64) {
	s.DB.Exec(`update note_writes set state = 'superseded', updated_at = ?
		where vault_id = ? and path = ? and state in ('planned', 'pending') and id < ?`, nowStr(), vaultID, path, id)
}

// Tick は commit を待つ行を commit し、出す。serve の中から定期に呼ぶ。
func (s *Service) Tick(now time.Time) {
	after := s.CommitAfter
	if after == 0 {
		after = time.Minute
	}
	s.reconcile(now.Add(-staleAfter), false)
	byVault, err := s.due(now.Add(-after))
	if err != nil {
		return
	}
	for vaultID, entries := range byVault {
		root, _, _, err := vault.VaultRoot(s.DB, vaultID)
		if err != nil {
			continue
		}
		s.commit(vaultID, root, entries)
	}
	s.maybePush(now)
}

// due は、パスごとの最新の行が cutoff より前に書かれた pending の行（パスごとに1つ）。
func (s *Service) due(cutoff time.Time) (map[int64][]notes.Entry, error) {
	q, err := s.DB.Query(`
		select w.id, w.vault_id, w.path, w.sha256, w.op, w.updated_at from note_writes w
		 where w.state = 'pending'
		   and not exists (select 1 from note_writes x where x.vault_id = w.vault_id and x.path = w.path
		                   and x.id > w.id and x.state in ('planned', 'pending'))
		 order by w.id`)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	out := map[int64][]notes.Entry{}
	for q.Next() {
		var e notes.Entry
		var vaultID int64
		var at, op string
		if err := q.Scan(&e.ID, &vaultID, &e.Path, &e.SHA, &op, &at); err != nil {
			return nil, err
		}
		e.Delete = op == opTrash
		if t, err := time.Parse(timeFmt, at); err == nil && t.After(cutoff) {
			continue
		}
		out[vaultID] = append(out[vaultID], e)
	}
	return out, q.Err()
}

func (s *Service) commit(vaultID int64, root string, entries []notes.Entry) {
	res, err := s.W.NoteCommit(root, entries)
	if err != nil {
		if s.IsBusy == nil || !s.IsBusy(err) {
			s.audit("note.commit", root, err.Error(), audit.Error)
		}
		return // 次の契機にやり直す
	}
	now := nowStr()
	// commit に照らした中身と違うもの（照らしてから commit までの間にほかの書き手が書いた）が入ったパス。
	// **その commit は Camp の commit として覚えない**——覚えないので push は「Camp 以外の commit が待っている」で
	// 止まり、本人が確かめてから出す（本人の決定5。outer gate の codex）。待ち行は「上書きされた」に。
	mismatch := map[int64]bool{}
	for _, p := range res.Mismatch {
		for _, e := range entries {
			if e.Path == p {
				mismatch[e.ID] = true
			}
		}
	}
	for _, id := range res.Done {
		if mismatch[id] {
			s.DB.Exec(`update note_writes set state = 'overwritten', commit_sha = ?, updated_at = ?,
				detail = 'commit の直前にほかの書き手が書き、その中身が commit に入った（Camp の commit として出さない）' where id = ?`,
				nullStr(res.Commit), now, id)
			continue
		}
		s.DB.Exec(`update note_writes set state = 'committed', commit_sha = ?, updated_at = ? where id = ?`,
			nullStr(res.Commit), now, id)
	}
	if res.Commit != "" && len(res.Mismatch) == 0 {
		s.rememberCommit(vaultID, res.Commit, "commit")
	}
	for _, ov := range res.Overwritten {
		if ov.Delete {
			s.DB.Exec(`update note_writes set state = 'overwritten', disk_sha256 = ?, updated_at = ?,
				detail = '.trash へ移したあと、commit の前に同じパスへ作り直された（削除は commit していない）' where id = ?`,
				nullStr(ov.SHA), now, ov.ID)
			s.audit("note.overwritten", ov.Path, ".trash へ移したあとで作り直された。削除を commit していない", audit.Denied)
			continue
		}
		s.DB.Exec(`update note_writes set state = 'overwritten', disk_sha256 = ?, updated_at = ?,
			detail = '別の書き手が commit の前に中身を変えた（Camp の版は blobs にある）' where id = ?`,
			nullStr(ov.SHA), now, ov.ID)
		s.audit("note.overwritten", ov.Path, "commit の前に別の書き手が変えた。commit していない", audit.Error)
	}
	if len(res.Mismatch) > 0 {
		s.audit("note.commit", strings.Join(res.Mismatch, ","),
			"commit に入った中身が照合した中身と違う。commit "+short(res.Commit)+" は Camp の commit として出さない", audit.Error)
	}
	if res.Commit != "" {
		ids := make([]string, 0, len(res.Done))
		for _, id := range res.Done {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
		s.audit("note.commit", res.Commit, "待ち行 "+strings.Join(ids, ","), audit.OK)
		s.mu.Lock()
		s.pushWant, s.pushAt = true, time.Time{}
		s.mu.Unlock()
	}
}

// maybePush は Camp の commit が出ていないかもしれないときに push を頼む。
func (s *Service) maybePush(now time.Time) {
	retry := s.PushRetry
	if retry == 0 {
		retry = 5 * time.Minute
	}
	s.mu.Lock()
	want := s.pushWant && (s.pushAt.IsZero() || now.Sub(s.pushAt) >= retry)
	if want {
		s.pushAt = now
	}
	s.mu.Unlock()
	if !want {
		return
	}
	vaults, err := s.committedVaults()
	if err != nil {
		return
	}
	for _, v := range vaults {
		root := v.root
		known, err := s.knownCommits(v.id)
		if err != nil {
			continue
		}
		res, err := s.W.NotePush(root, known)
		if err != nil {
			res = notes.PushResult{Kind: notes.PushFailed, Detail: err.Error()}
		}
		if res.MergeSHA != "" {
			s.rememberCommit(v.id, res.MergeSHA, "merge")
		}
		s.mu.Lock()
		changed := res.Kind != s.push.Kind
		s.push = res
		if res.Kind == notes.PushDone || res.Kind == notes.PushNothing {
			s.pushWant = false
		}
		s.mu.Unlock()
		out := audit.OK
		if res.Kind != notes.PushDone && res.Kind != notes.PushNothing {
			out = audit.Denied
		}
		if res.Kind == notes.PushDone || changed {
			s.audit("note.push", root, res.Kind+" "+res.Detail, out)
		}
	}
}

type vaultRef struct {
	id   int64
	root string
}

func (s *Service) committedVaults() ([]vaultRef, error) {
	// 覚えていない commit（食い違いのあった commit）だけの Vault も訊く——push は見送られ、画面に「確認待ち」が出る。
	q, err := s.DB.Query(`select distinct v.id, v.root from vaults v where v.id in
		(select vault_id from note_commits union select vault_id from note_writes where commit_sha is not null)`)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	var out []vaultRef
	for q.Next() {
		var r vaultRef
		if err := q.Scan(&r.id, &r.root); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].root < out[j].root })
	return out, q.Err()
}

// rememberCommit は Camp が作った commit の id を覚える（push してよい commit の一覧）。
func (s *Service) rememberCommit(vaultID int64, sha, kind string) {
	if _, err := s.DB.Exec(`insert or ignore into note_commits(vault_id, sha, kind, created_at) values(?, ?, ?, ?)`,
		vaultID, sha, kind, nowStr()); err != nil {
		s.audit("note.commit", sha, "Camp の commit を覚えられない（push は本人の確認待ちになる）: "+err.Error(), audit.Error)
	}
}

// maxKnown は push に渡す覚えた commit の数。まだ出ていない commit を覆えればよい。
const maxKnown = 2000

func (s *Service) knownCommits(vaultID int64) ([]string, error) {
	q, err := s.DB.Query(`select sha from note_commits where vault_id = ? order by created_at desc limit ?`, vaultID, maxKnown)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	var out []string
	for q.Next() {
		var sha string
		if err := q.Scan(&sha); err != nil {
			return nil, err
		}
		out = append(out, sha)
	}
	return out, q.Err()
}

// WantPush は起動時など「出ていない commit があるかもしれない」ときに呼ぶ。
func (s *Service) WantPush() {
	s.mu.Lock()
	s.pushWant = true
	s.mu.Unlock()
}

// WriteBody は待ち行の1件の本文（Camp の版）を返す。上書きされたものを見る・書き戻すため。
type WriteBody struct {
	ID     int64  `json:"id"`
	NoteID int64  `json:"note_id"`
	Path   string `json:"path"`
	SHA    string `json:"sha256"`
	State  string `json:"state"`
	Body   string `json:"body"`
}

// Write は待ち行の1件と、その本文を返す。
func (s *Service) Write(id int64) (*WriteBody, error) {
	var w WriteBody
	err := s.DB.QueryRow(`select w.id, coalesce(n.id, 0), w.path, w.sha256, w.state from note_writes w
		left join notes n on n.vault_id = w.vault_id and n.path = w.path where w.id = ?`, id).
		Scan(&w.ID, &w.NoteID, &w.Path, &w.SHA, &w.State)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fs.ErrNotExist
		}
		return nil, err
	}
	b, err := vault.BlobBody(s.DB, w.SHA)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fs.ErrNotExist
	}
	w.Body = string(b)
	return &w, nil
}

// WriteRow は Camp が書いた版の1件（中身は Write で引く）。
type WriteRow struct {
	ID     int64  `json:"id"`
	SHA    string `json:"sha256"`
	State  string `json:"state"`
	Op     string `json:"op"`
	At     string `json:"at"`
	Detail string `json:"detail,omitempty"`
}

// Writes はそのノートのパスに Camp が書いた版（新しい順）。**blobs に残る版へ画面から辿れるように**
// （合わせたときに消えた段落・上書きされた版を「blobs から戻せる」を画面で成り立たせる。outer gate の Fable 2）。
func (s *Service) Writes(noteID int64) ([]WriteRow, error) {
	n, err := vault.OneNote(s.DB, noteID)
	if err != nil {
		return nil, err
	}
	if n == nil {
		return nil, fs.ErrNotExist
	}
	q, err := s.DB.Query(`select id, sha256, state, op, updated_at, coalesce(detail, '') from note_writes
		where vault_id = ? and path = ? and state <> 'dropped' order by id desc limit 50`, n.VaultID, n.Path)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	out := []WriteRow{}
	for q.Next() {
		var w WriteRow
		if err := q.Scan(&w.ID, &w.SHA, &w.State, &w.Op, &w.At, &w.Detail); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, q.Err()
}

// Sync は画面に出す同期の様子。
type Sync struct {
	Pending     int              `json:"pending"`     // commit を待つファイル
	Overwritten []Overwritten    `json:"overwritten"` // 上書きされて commit しなかったもの（新しい順）
	Push        notes.PushResult `json:"push"`
	PushAt      string           `json:"push_at,omitempty"`
}

// Overwritten は上書きされた1件。Camp の版は SHA で blobs から引ける。
type Overwritten struct {
	ID      int64  `json:"id"`
	Path    string `json:"path"`
	SHA     string `json:"sha256"`
	DiskSHA string `json:"disk_sha256,omitempty"`
	At      string `json:"at"`
}

// Status は同期の様子を返す。
func (s *Service) Status() (*Sync, error) {
	out := &Sync{Overwritten: []Overwritten{}}
	if err := s.DB.QueryRow(`select count(distinct vault_id || ':' || path) from note_writes where state in ('planned', 'pending')`).
		Scan(&out.Pending); err != nil {
		return nil, err
	}
	q, err := s.DB.Query(`select id, path, sha256, coalesce(disk_sha256, ''), updated_at from note_writes
		where state = 'overwritten' and op = 'write' order by id desc limit 20`)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	for q.Next() {
		var o Overwritten
		if err := q.Scan(&o.ID, &o.Path, &o.SHA, &o.DiskSHA, &o.At); err != nil {
			return nil, err
		}
		out.Overwritten = append(out.Overwritten, o)
	}
	s.mu.Lock()
	out.Push = s.push
	if !s.pushAt.IsZero() {
		out.PushAt = s.pushAt.UTC().Format(timeFmt)
	}
	s.mu.Unlock()
	return out, q.Err()
}

func (s *Service) setState(id int64, state, detail string) {
	s.DB.Exec(`update note_writes set state = ?, detail = ?, updated_at = ? where id = ?`,
		state, nullStr(detail), nowStr(), id)
}

func (s *Service) audit(action, target, detail, outcome string) {
	audit.Append(s.DB, audit.Entry{Actor: "user", Action: action, Target: target, Detail: detail, Outcome: outcome})
}

const timeFmt = "2006-01-02T15:04:05Z"

func nowStr() string { return time.Now().UTC().Format(timeFmt) }

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
