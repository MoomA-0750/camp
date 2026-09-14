-- Phase 5 / M55: 待ち行に「何をしたか」を足す（2026-09-13）。
--
--   write  ノートを書いた（M53）。sha256 は書いた中身
--   trash  ノートを .trash/ へ移した。sha256 は移した中身。commit では削除として入れる
ALTER TABLE note_writes ADD COLUMN op TEXT NOT NULL DEFAULT 'write';
