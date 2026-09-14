package session

import (
	"path/filepath"

	"github.com/MoomA-0750/camp/internal/notes"
)

// noteVaultDir は名乗る Vault の実パス。書けないなら空。
func (a *Agent) noteVaultDir() string {
	if a.Notes == nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(a.Notes.Disk.Dir()); err == nil {
		return real
	}
	return a.Notes.Disk.Dir()
}

// 実行面が Vault のノートを書く（Phase 5 / M53）。
//
// **書くのは実行面。** campd は `camp` ユーザーで Vault に書けない（D-024）。campd に書き込み権を
// 足すと、campd を乗っ取った者が Vault（エージェントの読む指示の紙を含む）を書き換えられる向きの
// 穴が開く。実行面はすでに本人のユーザーで何でもできるので、増える力が無い。
//
// 照合（場所の一覧・symlink・文字・大きさ・指示の紙の再認証・書き始めた中身）は notes が持ち、
// campd も同じものを使う。**ここは最後の砦として、campd の照合を信じずにもう一度照らす。**

func (a *Agent) noteWrite(m Msg) {
	out := Msg{T: MsgNoteRes, ReqID: m.ReqID}
	if a.Notes == nil {
		out.Error = "この実行面は Vault を持っていない（campd agent -vault）"
		a.send(out)
		return
	}
	r, err := a.Notes.Disk.Write(m.NotePath, m.NoteBase, []byte(m.NoteBody), m.NoteCreate, m.NoteReauth)
	if err != nil {
		out.Error = err.Error()
	} else {
		out.NoteWrote = &r
	}
	a.send(out)
}

func (a *Agent) noteCommit(m Msg) {
	out := Msg{T: MsgNoteRes, ReqID: m.ReqID}
	if a.Notes == nil {
		out.Error = "この実行面は Vault を持っていない（campd agent -vault）"
		a.send(out)
		return
	}
	// 待ち行のパスも照らす（campd の取り違えで一覧の外を commit しない）。
	for _, e := range m.NoteEntries {
		if c, why := notes.Classify(e.Path); c == notes.ReadOnly {
			out.Error = e.Path + " は Camp が書く場所ではない: " + why
			a.send(out)
			return
		}
	}
	r, err := a.Notes.Commit(m.NoteEntries)
	switch {
	case notes.IsBusy(err):
		out.NoteBusy, out.Error = true, err.Error()
	case err != nil:
		out.Error = err.Error()
	default:
		out.NoteCommit = &r
	}
	a.send(out)
}

func (a *Agent) notePush(m Msg) {
	out := Msg{T: MsgNoteRes, ReqID: m.ReqID}
	if a.Notes == nil {
		out.Error = "この実行面は Vault を持っていない（campd agent -vault）"
		a.send(out)
		return
	}
	r, err := a.Notes.Push(m.NoteKnown)
	if err != nil {
		out.Error = err.Error()
	} else {
		out.NotePush = &r
	}
	a.send(out)
}

func (a *Agent) noteTrash(m Msg) {
	out := Msg{T: MsgNoteRes, ReqID: m.ReqID}
	if a.Notes == nil {
		out.Error = "この実行面は Vault を持っていない（campd agent -vault）"
		a.send(out)
		return
	}
	r, err := a.Notes.Disk.Trash(m.NotePath, m.NoteBase, m.NoteReauth)
	if err != nil {
		out.Error = err.Error()
	}
	if r.Status != "" {
		out.NoteTrashed = &r
	}
	a.send(out)
}
