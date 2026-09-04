-- Phase 3 / M26。**Camp が起こしたセッション**の台帳。
--
-- 会話記録側の `sessions` とは別物。あちらは JSONL を読んで作る「記録」で、
-- こちらは Camp が自分で起こした「プロセス」。両方が揃って初めて
-- 「Camp が起こしたものが、あとで記録として戻ってくる」ことを確かめられる。
--
-- 0001 の `live_sessions` は Phase 0 の見込みで置いた器で、一度も使っていない。
-- pid しか持っておらず、**pid は使い回される**ので所有権の判定に足りない。
-- 消すのは破壊なので残すが、こちらが正本。live_sessions は書かない。
CREATE TABLE runtime_sessions (
  id           TEXT PRIMARY KEY,      -- campd が採番する。子が名乗る id とは別
  claude_id    TEXT,                  -- 子が system/init で名乗った session_id
  cwd          TEXT NOT NULL,         -- 実パス（symlink を解いたもの）
  state        TEXT NOT NULL,         -- starting/idle/running/stopping/exited/orphaned
  requested_by TEXT NOT NULL,         -- 誰が起こしたか。監査ログの actor と揃える
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL,

  -- **所有権は (pid, 起動時刻, boot_id) の3つで見る。**
  -- pid だけだと、死んだあとに同じ番号を取った他人のプロセスを掴む。
  -- 起動時刻は /proc/<pid>/stat の22番目（起動からの clock ticks）。
  -- 再起動を跨ぐとその基準が変わるので boot_id も要る。
  pid          INTEGER,
  proc_started INTEGER,
  boot_id      TEXT,
  scope        TEXT,                  -- systemd の transient scope 名（孫ごと止めるため）

  exit_code    INTEGER,
  exit_reason  TEXT,
  ended_at     TEXT
);

CREATE INDEX ix_runtime_state ON runtime_sessions(state, updated_at DESC);
CREATE INDEX ix_runtime_claude ON runtime_sessions(claude_id);
