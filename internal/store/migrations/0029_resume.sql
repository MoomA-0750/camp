-- 終わったセッションの続きから起こす（Phase 3.9 / M48、2026-09-13）。
--
-- **新しい行を作り、どこから続けたかを残す**（本人の決定 2026-09-13）。
-- 終わった行を生き返らせて使い回すと、そのとき何が起きたか（どう終わったか・いつ・
-- 待たせたまま終わった承認）を上書きすることになり、過去が消える。
-- プロセスとの 1 対 1（pid・起動時刻・boot_id・scope で所有権を見る）も崩れる。
--
-- 画面では「元のものが生き返った」ように見せる（本人の決定）。台帳は別の行のまま。
--
-- **エージェント側の id は再開しても同じ。** 実測（2026-09-13）:
--   Claude Code `-p --resume <id>`  → 同じ session_id。記録は同じ JSONL に追記
--   Codex `thread/resume {threadId}` → 同じ thread id。記録は同じ rollout に追記
-- つまり claude_id は元の行と同じ値が入る。**取り込み側は何も変えなくてよい**
-- （差分読みがそのまま効き、会話は 1 本のまま繋がる）。
ALTER TABLE runtime_sessions ADD COLUMN resumed_from TEXT REFERENCES runtime_sessions(id);

-- 「この行の続きは起きているか」を引く。画面が元の行に「続きが起きた」を出すのに使う。
CREATE INDEX ix_runtime_resumed ON runtime_sessions(resumed_from) WHERE resumed_from IS NOT NULL;
