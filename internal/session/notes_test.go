package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoomA-0750/camp/internal/notes"
)

// campd → 実行面 → ディスク の往復（Phase 5 / M53）。

func withVault(t *testing.T) (func(*Agent), string) {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "Human/Logs"), 0o755)
	os.WriteFile(filepath.Join(dir, "Human/Logs/a.md"), []byte("a\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("指示\n"), 0o644)
	d, err := notes.OpenDisk(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	real, _ := filepath.EvalSymlinks(dir)
	return func(a *Agent) { a.Notes = &notes.Git{Disk: d} }, real
}

func TestNoteWriteGoesThroughTheExecutionSide(t *testing.T) {
	s := New(newDB(t))
	opt, dir := withVault(t)
	attach(t, s, fakeClaude(t), opt)

	r, err := s.NoteWrite(dir, "Human/Logs/a.md", notes.Sum([]byte("a\n")), "a\n本人\n", false, false)
	if err != nil || r.Status != notes.StatusWritten {
		t.Fatalf("%+v %v", r, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "Human/Logs/a.md")); string(b) != "a\n本人\n" {
		t.Fatalf("%q", b)
	}
	// 実行面は campd の照合を信じずにもう一度照らす。
	if _, err := s.NoteWrite(dir, "AGENTS.md", notes.Sum([]byte("指示\n")), "乗っ取り\n", false, false); err == nil ||
		!strings.Contains(err.Error(), "パスワード") {
		t.Fatalf("再認証なしで指示の紙を書いた: %v", err)
	}
	if _, err := s.NoteWrite(dir, "../outside.md", "", "x\n", true, false); err == nil {
		t.Fatal("Vault の外を書いた")
	}
}

func TestTooLargeNoteIsRefusedWithoutCuttingTheControlLine(t *testing.T) {
	s := New(newDB(t))
	opt, dir := withVault(t)
	attach(t, s, fakeClaude(t), opt)
	// JSON で伸びる本文（改行は2バイトになる）。本文そのものは MaxBody の下。
	body := strings.Repeat("\n", notes.MaxBody-1)
	if _, err := s.NoteWrite(dir, "Human/Logs/big.md", "", body, true, false); err == nil ||
		!strings.Contains(err.Error(), "大きすぎて") {
		t.Fatalf("%v", err)
	}
	if _, err := s.NoteWrite("/somewhere/else", "Human/Logs/a.md", "", "x\n", true, false); !errors.Is(err, ErrOtherVault) {
		t.Fatalf("別の Vault のノートを頼んだ: %v", err)
	}
	if !s.AgentConnected() {
		t.Fatal("制御口が切れた")
	}
	if r, err := s.NoteWrite(dir, "Human/Logs/small.md", "", "x\n", true, false); err != nil || r.Status != notes.StatusWritten {
		t.Fatalf("そのあと書けない: %+v %v", r, err)
	}
}

func TestOldExecutionSideCannotWriteNotes(t *testing.T) {
	s := New(newDB(t))
	attach(t, s, fakeClaude(t)) // Vault を持たない（古い実行面・-vault 無し）
	if _, err := s.NoteWrite("/v", "Human/Logs/a.md", "", "x\n", true, false); !errors.Is(err, ErrNoNoteVault) {
		t.Fatalf("%v", err)
	}
	if _, err := s.NotePush("/v", nil); !errors.Is(err, ErrNoNoteVault) {
		t.Fatalf("%v", err)
	}
}
