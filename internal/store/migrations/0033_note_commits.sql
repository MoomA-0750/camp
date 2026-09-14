-- Phase 5 outer gate: Camp が作った commit の id を覚える（2026-09-13）。
--
-- **push してよいかは、この表にある commit かで決める。** commit の文の `Camp-Commit:` トレーラーでは決めない——
-- エージェントが Camp の commit を amend したり、同じトレーラーを書いたりすると、中身の違う commit が本人の確認なしに
-- 出てしまう（Fable と codex の outer gate）。取り込みの merge の commit もここに入れる。
--
-- kind: commit（待ち行の commit）・merge（GitHub の変更を取り込んだ commit）
CREATE TABLE note_commits (
  vault_id   INTEGER NOT NULL REFERENCES vaults(id),
  sha        TEXT NOT NULL,
  kind       TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (vault_id, sha)
);

-- これまでの Camp の commit（中身を照らして commit に入れた待ち行のもの）。
INSERT OR IGNORE INTO note_commits(vault_id, sha, kind, created_at)
  SELECT vault_id, commit_sha, 'commit', min(updated_at) FROM note_writes
   WHERE commit_sha IS NOT NULL GROUP BY vault_id, commit_sha;
