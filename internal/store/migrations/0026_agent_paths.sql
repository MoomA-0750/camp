-- Phase 3.7 の M42。向こうのホストでの、エージェントごとの実体の場所（行が無ければ向こうで探す）。
--
-- **行にする**（JSON の列にしない）。エージェントの追加・削除が行の追加・削除で済む（D-031。Fable の
-- M42 の設計レビュー）。0023 の claude_path の値は写す。列は SQLite で落としにくいので残すが、もう読まない。
CREATE TABLE ssh_agent_paths (
  host  TEXT NOT NULL,   -- ssh_hosts.alias
  agent TEXT NOT NULL,   -- 駆動器の名前
  path  TEXT NOT NULL,
  UNIQUE (host, agent)
);
INSERT INTO ssh_agent_paths(host, agent, path)
  SELECT alias, 'claude', claude_path FROM ssh_hosts
  WHERE claude_path IS NOT NULL AND claude_path <> '';
