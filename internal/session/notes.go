package session

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MoomA-0750/camp/internal/notes"
)

// campd が実行面にノートを書かせる（Phase 5 / M53）。

const (
	noteWriteDeadline  = 30 * time.Second
	noteCommitDeadline = 3 * time.Minute
	notePushDeadline   = 5 * time.Minute // 取り寄せ・取り込み・push で網を待つ
)

// ErrNoNoteVault は、繋がっている実行面が Vault を持っていない（古い実行面・-vault 無し）。
var ErrNoNoteVault = fmt.Errorf("実行面が Vault を持っていない（campd agent -vault）")

// ErrNoteBusy は、ほかの git が動いているので待ってやり直せばよい。
var ErrNoteBusy = fmt.Errorf("ほかの git が動いている")

// NoteVault は、繋がっている実行面が書く Vault の実パス。書けなければ空。
func (s *Supervisor) NoteVault() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agent == nil {
		return ""
	}
	return s.agent.noteVault
}

// noteAgentReady は、繋がっている実行面が vault（実パス）を書く者かを見る。
func (s *Supervisor) noteAgentReady(vault string) error {
	s.mu.Lock()
	a := s.agent
	s.mu.Unlock()
	switch {
	case a == nil:
		return ErrNoAgent
	case a.noteVault == "":
		return ErrNoNoteVault
	case a.noteVault != vault:
		return fmt.Errorf("%w: 実行面の Vault は %s、このノートは %s", ErrOtherVault, a.noteVault, vault)
	}
	return nil
}

// ErrOtherVault は、実行面が書く Vault とノートの Vault が違う。**同じ相対パスでも別物なので頼まない。**
var ErrOtherVault = fmt.Errorf("実行面の Vault が違う")

// NoteWrite は実行面に1本書かせる。
//
// **JSON にした1行が制御口の上限を超えるなら頼まない**——超えた行を受けた実行面の読み手は落ち、
// 制御口ごと切れて、走っているセッションの見張りまで切れる（Fable の設計レビュー 7）。
func (s *Supervisor) NoteWrite(vault, path, base, body string, create, reauthed bool) (notes.Result, error) {
	if err := s.noteAgentReady(vault); err != nil {
		return notes.Result{}, err
	}
	m := Msg{T: MsgNoteWrite, NotePath: path, NoteBase: base, NoteBody: body,
		NoteCreate: create, NoteReauth: reauthed, ReqID: strings.Repeat("0", 32)}
	if b, err := json.Marshal(m); err != nil {
		return notes.Result{}, err
	} else if len(b)+64 > maxLine {
		return notes.Result{}, fmt.Errorf("本文が大きすぎて実行面へ渡せない（%d バイト）", len(b))
	}
	got, err := s.recAsk(m, noteWriteDeadline)
	if err != nil {
		return notes.Result{}, err
	}
	if got.NoteWrote == nil {
		return notes.Result{}, fmt.Errorf("実行面の返事に結果が無い")
	}
	return *got.NoteWrote, nil
}

// NoteCommit は待ち行を commit させる。
func (s *Supervisor) NoteCommit(vault string, entries []notes.Entry) (notes.CommitResult, error) {
	if err := s.noteAgentReady(vault); err != nil {
		return notes.CommitResult{}, err
	}
	got, err := s.noteAsk(Msg{T: MsgNoteCommit, NoteEntries: entries}, noteCommitDeadline)
	if err != nil {
		return notes.CommitResult{}, err
	}
	if got.NoteCommit == nil {
		return notes.CommitResult{}, fmt.Errorf("実行面の返事に結果が無い")
	}
	return *got.NoteCommit, nil
}

// NotePush は push させる。
func (s *Supervisor) NotePush(vault string, known []string) (notes.PushResult, error) {
	if err := s.noteAgentReady(vault); err != nil {
		return notes.PushResult{}, err
	}
	got, err := s.noteAsk(Msg{T: MsgNotePush, NoteKnown: known}, notePushDeadline)
	if err != nil {
		return notes.PushResult{}, err
	}
	if got.NotePush == nil {
		return notes.PushResult{}, fmt.Errorf("実行面の返事に結果が無い")
	}
	return *got.NotePush, nil
}

// noteAsk は recAsk と同じ往復だが、「待ってやり直せばよい」失敗を ErrNoteBusy で返す。
func (s *Supervisor) noteAsk(m Msg, wait time.Duration) (Msg, error) {
	s.mu.Lock()
	agent := s.agent
	s.mu.Unlock()
	if agent == nil {
		return Msg{}, ErrNoAgent
	}
	req := newID()
	ch := make(chan Msg, 1)
	s.mu.Lock()
	s.waits[req] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.waits, req)
		s.mu.Unlock()
	}()
	m.ReqID = req
	if err := agent.send(m); err != nil {
		return Msg{}, err
	}
	select {
	case got := <-ch:
		switch {
		case got.NoteBusy:
			return Msg{}, fmt.Errorf("%w: %s", ErrNoteBusy, got.Error)
		case got.Error != "":
			return Msg{}, fmt.Errorf("%s", got.Error)
		}
		return got, nil
	case <-time.After(wait):
		return Msg{}, fmt.Errorf("実行面が返事をしない")
	}
}

// NoteTrash は実行面に1本を `.trash/` へ移させる（M55）。
func (s *Supervisor) NoteTrash(vault, path, base string, reauthed bool) (notes.TrashResult, error) {
	if err := s.noteAgentReady(vault); err != nil {
		return notes.TrashResult{}, err
	}
	got, err := s.recAsk(Msg{T: MsgNoteTrash, NotePath: path, NoteBase: base, NoteReauth: reauthed}, noteWriteDeadline)
	if err != nil {
		return notes.TrashResult{}, err
	}
	if got.NoteTrashed == nil {
		return notes.TrashResult{}, fmt.Errorf("実行面の返事に結果が無い")
	}
	return *got.NoteTrashed, nil
}
