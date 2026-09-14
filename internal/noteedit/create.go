package noteedit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/MoomA-0750/camp/internal/audit"
	"github.com/MoomA-0750/camp/internal/notes"
	"github.com/MoomA-0750/camp/internal/vault"
)

// 行き来（Phase 5 / M55）: 名前の一覧・新しいノート・日次ログ・`.trash/` へ移す。
// 設計は `dev/active/phase5-design.md` の「M55 の設計」。

const (
	opWrite = "write"
	opTrash = "trash"
)

// 日次ログの場所とテンプレート。rules.go の場所の一覧と同じく、この Vault の決まりを定数で持つ
// （Obsidian の daily-notes.json の folder: Human/Logs・format: YYYY-MM-DD と同じ）。
const (
	DailyDir      = "Human/Logs"
	DailyTemplate = "Meta/Templates/日次ログ.md"
)

// ErrExists は新しく作ろうとした名前が既にある。
var ErrExists = errors.New("その名前のノートは既にある")

// ErrBadName は名前が使えない。
var ErrBadName = errors.New("その名前では作れない")

// Name は名前の一覧の1件。
type Name struct {
	ID   int64  `json:"id"`
	Path string `json:"path"`
	// Link は wikilink に入れる形（`[[` と `]]` の中身）。書けない名前は空。
	Link     string `json:"link,omitempty"`
	Editable bool   `json:"editable,omitempty"`
}

// Names は名前の一覧と、その世代。
type Names struct {
	Gen   string `json:"gen"`
	Same  bool   `json:"same,omitempty"`
	Names []Name `json:"names,omitempty"`
}

// linkUnsafe は wikilink の中に書けない文字。
const linkUnsafe = "|#^[]\n"

// Names は Vault のノートの名前と、補完が入れるリンクの形を返す。gen が今と同じなら中身を返さない。
//
// **リンクの形はここで決める**（解決の規則を画面に二重に書かない）: ベース名が Vault の中で1つだけなら
// ベース名、そうでなければ `.md` を外したパス。**大文字小文字を畳んで**1つかを見る（Obsidian は畳んで
// 解決する）。決めた形は resolve.go の Resolve に通し、曖昧でなくその本人に届くものだけを出す。
func (s *Service) Names(vaultID int64, gen string) (*Names, error) {
	ix, ids, key, err := s.linkIndexKey(vaultID)
	if err != nil {
		return nil, err
	}
	if gen != "" && gen == key {
		return &Names{Gen: key, Same: true}, nil
	}
	folded := map[string]int{}
	for p := range ids {
		folded[notes.FoldName(stem(p))]++
	}
	out := &Names{Gen: key, Names: make([]Name, 0, len(ids))}
	for p, id := range ids {
		n := Name{ID: id, Path: p}
		// 自動保存で書ける場所か（新しいノートを置けるフォルダの元。指示の紙は含めない）。
		if c, _ := notes.Classify(p); c == notes.Editable {
			n.Editable = true
		}
		n.Link = linkFor(ix, p, folded[notes.FoldName(stem(p))] == 1)
		out.Names = append(out.Names, n)
	}
	return out, nil
}

// stem はベース名（`.md` は外す。ほかの拡張子は残す——`[[a.png]]` と書く）。
func stem(p string) string {
	return strings.TrimSuffix(path.Base(p), ".md")
}

func linkFor(ix *vault.LinkIndex, p string, unique bool) string {
	if strings.ContainsAny(p, linkUnsafe) {
		return ""
	}
	cands := []string{strings.TrimSuffix(p, ".md")}
	if unique {
		cands = append([]string{stem(p)}, cands...)
	}
	for _, c := range cands {
		// 解決は「どのノートから書くか」で変わりうる（同じフォルダが優先）。Vault の外から見て
		// 1つに決まる形なら、どこから書いても同じ先に決まる。
		if r := ix.Resolve("", c); r.To == p && !r.Ambiguous {
			return c
		}
	}
	return ""
}

// Created は作った（または既にあった）ノート。
type Created struct {
	NoteID  int64  `json:"note_id"`
	Path    string `json:"path"`
	Created bool   `json:"created"`
	// Warn は本人に知らせる一言（テンプレートの置き換えなかった式など）。
	Warn string `json:"warn,omitempty"`
}

// Create は新しいノートを作る。**Editable の場所だけ・親のフォルダは既にあるものだけ。**
func (s *Service) Create(vaultID int64, rel, body string) (*Created, error) {
	rel, err := notes.NameRel(rel)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrBadName, err)
	}
	if c, why := notes.Classify(rel); c != notes.Editable {
		if why == "" {
			why = "エージェントの指示の紙は新しく作らない"
		}
		s.audit("note.refused", rel, "新しいノート: "+why, audit.Denied)
		return nil, fmt.Errorf("%w: %s", ErrReadOnly, why)
	}
	if err := notes.CheckText([]byte(body)); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrReadOnly, err)
	}
	root, _, _, err := vault.VaultRoot(s.DB, vaultID)
	if err != nil {
		return nil, err
	}
	// フォルダは作らない。
	if fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path.Dir(rel)))); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%w: フォルダ %s が無い（Camp はフォルダを作らない）", ErrBadName, path.Dir(rel))
	}
	// 索引で先に照らす（画面にすぐ「ある」と出す）。最後の砦は実行面がフォルダを読んで照らす。
	if id, other, err := s.sameNameInIndex(vaultID, rel); err != nil {
		return nil, err
	} else if other != "" {
		return &Created{NoteID: id, Path: other}, ErrExists
	}
	res, err := s.write(vaultID, root, rel, "", body, true, false)
	if err != nil {
		return nil, err
	}
	switch res.Status {
	case notes.StatusWritten, notes.StatusSame:
		id, err := vault.NoteIDByPath(s.DB, vaultID, rel)
		if err != nil {
			return nil, err
		}
		return &Created{NoteID: id, Path: rel, Created: res.Status == notes.StatusWritten}, nil
	case notes.StatusExists:
		other := rel
		if res.Other != "" {
			other = res.Other
		}
		id, _ := vault.NoteIDByPath(s.DB, vaultID, other)
		return &Created{NoteID: id, Path: other}, ErrExists
	}
	return nil, fmt.Errorf("実行面の返事が分からない: %s", res.Status)
}

// sameNameInIndex は索引に、同じフォルダで畳むと同じ名前になるノートがあるかを見る。
func (s *Service) sameNameInIndex(vaultID int64, rel string) (int64, string, error) {
	q, err := s.DB.Query(`select id, path from notes where vault_id = ? and missing_at is null and path like ? escape '\'`,
		vaultID, likePrefix(path.Dir(rel)+"/")+"%")
	if err != nil {
		return 0, "", err
	}
	defer q.Close()
	want := notes.FoldName(rel)
	for q.Next() {
		var id int64
		var p string
		if err := q.Scan(&id, &p); err != nil {
			return 0, "", err
		}
		if path.Dir(p) == path.Dir(rel) && notes.FoldName(p) == want {
			return id, p, nil
		}
	}
	return 0, "", q.Err()
}

func likePrefix(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// templateVars はテンプレートで置き換える書き方。**Templater の式は実行しない**（任意の JS）。
var templateVars = []string{"<% tp.file.title %>", "{{title}}", "{{date}}"}

var leftoverRe = regexp.MustCompile(`<%[\s\S]*?%>|\{\{[^}\n]*\}\}`)

// Daily はその日の日次ログを返す。無ければテンプレートから作る。
func (s *Service) Daily(vaultID int64, date string) (*Created, error) {
	if !dateRe.MatchString(date) {
		return nil, fmt.Errorf("%w: 日付は YYYY-MM-DD", ErrBadName)
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return nil, fmt.Errorf("%w: 在る日付ではない", ErrBadName)
	}
	rel := DailyDir + "/" + date + ".md"
	root, _, _, err := vault.VaultRoot(s.DB, vaultID)
	if err != nil {
		return nil, err
	}
	if _, err := readPlain(root, rel); err == nil {
		id, err := vault.NoteIDByPath(s.DB, vaultID, rel)
		if err != nil {
			return nil, err
		}
		if id == 0 {
			// ディスクにはあるが索引がまだ。索引に入れてから返す。
			if err := s.touchFromDisk(vaultID, root, rel); err != nil {
				return nil, err
			}
			id, _ = vault.NoteIDByPath(s.DB, vaultID, rel)
		}
		return &Created{NoteID: id, Path: rel}, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	tpl, err := readPlain(root, DailyTemplate)
	if err != nil {
		return nil, fmt.Errorf("日次ログのテンプレート（%s）が読めない: %w", DailyTemplate, err)
	}
	body := string(tpl)
	for _, v := range templateVars {
		body = strings.ReplaceAll(body, v, date)
	}
	out, err := s.Create(vaultID, rel, body)
	if errors.Is(err, ErrExists) && out != nil && out.Path == rel {
		return &Created{NoteID: out.NoteID, Path: rel}, nil // 作る間にほかで作られた
	}
	if err != nil {
		return out, err
	}
	if left := leftoverRe.FindAllString(body, 5); len(left) > 0 {
		out.Warn = "テンプレートの式を置き換えなかった（Camp は実行しない）: " + strings.Join(left, " ")
	}
	return out, nil
}

func (s *Service) touchFromDisk(vaultID int64, root, rel string) error {
	b, err := readPlain(root, rel)
	if err != nil {
		return err
	}
	return vault.TouchNote(s.DB, vaultID, rel, b, time.Now())
}

// TrashWaiting は、書いた版の commit を待っているので、まだ移さない。
const TrashWaiting = "waiting"

// TrashOut は `.trash/` へ移した結果。
type TrashOut struct {
	// Status は trashed・changed（見ていた版と違う。移していない）・gone（もう無い）・kept（移したが
	// 途中で書かれていた。To に残した）・waiting（書いた版の commit を待っている。移していない）。
	Status  string `json:"status"`
	To      string `json:"to,omitempty"`
	DiskSHA string `json:"disk_sha256,omitempty"`
}

// Trash はノートを `.trash/` へ移す。base は画面で見ている版のハッシュ。
func (s *Service) Trash(noteID int64, base string, reauthed bool) (*TrashOut, error) {
	n, err := vault.OneNote(s.DB, noteID)
	if err != nil {
		return nil, err
	}
	if n == nil {
		return nil, fs.ErrNotExist
	}
	if n.Missing != "" {
		return &TrashOut{Status: notes.TrashGone}, nil
	}
	root, _, _, err := vault.VaultRoot(s.DB, n.VaultID)
	if err != nil {
		return nil, err
	}
	switch c, why := notes.Classify(n.Path); {
	case c == notes.ReadOnly:
		s.audit("note.refused", n.Path, "移す: "+why, audit.Denied)
		return nil, fmt.Errorf("%w: %s", ErrReadOnly, why)
	case c == notes.Instruction && !reauthed:
		s.audit("note.refused", n.Path, "指示の紙を再認証なしで移す", audit.Denied)
		return nil, ErrNeedReauth
	}
	// **書いた版の commit を待っているうちは移さない**（本人の決定、2026-09-14）。先に移すと、その版は git の
	// 履歴に入らず、このマシンの .trash/ と Camp の控えにしか残らない（outer gate の Fable 7）。
	var waiting int
	if err := s.DB.QueryRow(`select count(*) from note_writes where vault_id = ? and path = ? and op = 'write'
		and state in ('planned', 'pending')`, n.VaultID, n.Path).Scan(&waiting); err != nil {
		return nil, err
	}
	if waiting > 0 {
		return &TrashOut{Status: TrashWaiting}, nil
	}
	if b, err := vault.BlobBody(s.DB, base); err != nil {
		return nil, err
	} else if b == nil {
		// 見ていた版の控えが無い（開いた記録が無い）。移した中身を残せないので頼まない。
		return nil, fmt.Errorf("%w: 見ていた版の控えが無い。開き直してから移す", ErrBadName)
	}
	now := nowStr()
	r, err := s.DB.Exec(`insert into note_writes(vault_id, path, sha256, state, base_sha256, op, created_at, updated_at)
		values(?, ?, ?, 'planned', ?, 'trash', ?, ?)`, n.VaultID, n.Path, base, base, now, now)
	if err != nil {
		return nil, err
	}
	id, _ := r.LastInsertId()
	res, err := s.W.NoteTrash(root, n.Path, base, reauthed)
	if err != nil {
		// 「移す予定」のまま残す（返事が届かなかっただけで移せているかもしれない。定期の照合が決める）。
		s.setState(id, "planned", err.Error())
		s.audit("note.trash", n.Path, err.Error(), audit.Error)
		return nil, err
	}
	out := &TrashOut{Status: res.Status, To: res.To, DiskSHA: res.DiskSHA}
	switch res.Status {
	case notes.TrashDone:
		// 同じパスの commit を待つ書き込みは、もう commit しない（その版は .trash/ と blobs にある）。
		s.supersedeOlder(n.VaultID, n.Path, id)
		s.setState(id, "pending", "移した先 "+res.To)
		vault.MarkMissing(s.DB, n.VaultID, n.Path)
		s.audit("note.trash", n.Path, "→ "+res.To+" sha256="+short(base), audit.OK)
	case notes.TrashKept:
		s.setState(id, "dropped", "移す間に書かれた。"+res.To+" に残した")
		s.audit("note.trash", n.Path, "移す間に書かれ、元の場所にも作られていた。移した版は "+res.To, audit.Error)
	default:
		s.setState(id, "dropped", res.Status)
	}
	return out, nil
}
