-- Camp 初期スキーマ。根拠は docs/20-data-model.md。
-- 作成順は外部キーの依存順（hosts → projects → sessions → source_files → runs → messages …）。

CREATE TABLE hosts (
  id           INTEGER PRIMARY KEY,
  name         TEXT NOT NULL UNIQUE,
  ssh_alias    TEXT,
  tailscale_ip TEXT,
  os           TEXT,
  last_seen_at TEXT,
  enabled      INTEGER NOT NULL DEFAULT 1
);

-- repo_path は絶対パス・大文字小文字を区別する。
-- Obsidian-Vault と Obsidian-vault は実際に別プロジェクトとして存在する。
CREATE TABLE projects (
  id                INTEGER PRIMARY KEY,
  host_id           INTEGER NOT NULL REFERENCES hosts(id),
  repo_path         TEXT NOT NULL,
  name              TEXT NOT NULL,
  git_origin        TEXT,
  is_worktree       INTEGER NOT NULL DEFAULT 0,
  worktree_name     TEXT,
  parent_project_id INTEGER REFERENCES projects(id),
  UNIQUE(host_id, repo_path)
);
CREATE INDEX ix_projects_parent ON projects(parent_project_id);

-- セッション = 会話の同一性（sessionId）。ファイルとは 1:1 ではない。
CREATE TABLE sessions (
  id                   TEXT PRIMARY KEY,
  host_id              INTEGER NOT NULL REFERENCES hosts(id),
  project_id           INTEGER NOT NULL REFERENCES projects(id),
  agent                TEXT NOT NULL,
  parent_session_id    TEXT REFERENCES sessions(id),
  parent_agent_id      TEXT,
  ai_title             TEXT,
  user_title           TEXT,
  first_user_message   TEXT,
  last_prompt          TEXT,
  last_prompt_leaf     TEXT,
  git_branch           TEXT,
  last_cwd             TEXT,
  last_cli_version     TEXT,
  last_model           TEXT,
  last_mode            TEXT,
  last_permission_mode TEXT,
  bridge_session_id    TEXT,
  is_sidechain         INTEGER NOT NULL DEFAULT 0,
  started_at           TEXT NOT NULL,
  updated_at           TEXT NOT NULL,
  message_count        INTEGER NOT NULL DEFAULT 0,
  total_cost_usd       REAL,
  archived             INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX ix_sessions_recent  ON sessions(host_id, updated_at DESC);
CREATE INDEX ix_sessions_project ON sessions(project_id, updated_at DESC);
CREATE INDEX ix_sessions_agent   ON sessions(agent, updated_at DESC);
CREATE INDEX ix_sessions_parent  ON sessions(parent_session_id);

-- 物理ファイル層。main / resume-sidecar / subagent / codex-rollout をすべてここで持つ。
-- missing_at が「独立保持」の核心。元が消えても行は削除しない。
CREATE TABLE source_files (
  id              INTEGER PRIMARY KEY,
  host_id         INTEGER NOT NULL REFERENCES hosts(id),
  path            TEXT NOT NULL,
  role            TEXT NOT NULL,
  session_id      TEXT REFERENCES sessions(id),
  agent_id        TEXT,
  dev             INTEGER,
  inode           INTEGER,
  size            INTEGER NOT NULL DEFAULT 0,
  mtime           TEXT,
  ingested_offset INTEGER NOT NULL DEFAULT 0,
  pending_tail    BLOB,
  first_seen_at   TEXT NOT NULL,
  missing_at      TEXT,
  UNIQUE(host_id, path)
);
CREATE INDEX ix_sf_session ON source_files(session_id);
CREATE INDEX ix_sf_scan    ON source_files(host_id, missing_at, mtime DESC);

-- --resume ごとに1行。claude の session_id（snake_case）がここに入る。
CREATE TABLE runs (
  id              TEXT PRIMARY KEY,
  session_id      TEXT NOT NULL REFERENCES sessions(id),
  seq             INTEGER NOT NULL,
  sidecar_file_id INTEGER REFERENCES source_files(id),
  cli_version     TEXT,
  cwd             TEXT,
  mode            TEXT,
  permission_mode TEXT,
  started_at      TEXT,
  ended_at        TEXT,
  UNIQUE(session_id, seq)
);
CREATE INDEX ix_runs_session ON runs(session_id, seq);

-- 主キーは (source_file_id, byte_offset)。uuid ではない。
-- --fork-session は親の履歴を uuid ごと新しいファイルへ複製するため、
-- uuid はファイルを跨ぐと重複しうる（実コーパスで26件確認）。
-- ファイルが保管の単位なので、ファイル内の位置で同一性を決める。
CREATE TABLE messages (
  id                  INTEGER PRIMARY KEY,
  uuid                TEXT,
  session_id          TEXT NOT NULL REFERENCES sessions(id),
  run_id              TEXT REFERENCES runs(id),
  source_file_id      INTEGER NOT NULL REFERENCES source_files(id),
  byte_offset         INTEGER NOT NULL,
  parent_uuid         TEXT,
  logical_parent_uuid TEXT,
  type                TEXT NOT NULL,
  subtype             TEXT,
  role                TEXT,
  timestamp           TEXT,
  cwd                 TEXT,
  cli_version         TEXT,
  is_sidechain        INTEGER NOT NULL DEFAULT 0,
  agent_id            TEXT,
  is_meta             INTEGER NOT NULL DEFAULT 0,
  is_compact_summary  INTEGER NOT NULL DEFAULT 0,
  is_api_error        INTEGER NOT NULL DEFAULT 0,
  api_message_id      TEXT,
  request_id          TEXT,
  model               TEXT,
  service_tier        TEXT,
  effort              TEXT,
  degraded            INTEGER NOT NULL DEFAULT 0,
  raw_json            BLOB NOT NULL,
  UNIQUE(source_file_id, byte_offset)
);
CREATE INDEX ix_msg_uuid    ON messages(uuid);
CREATE INDEX ix_msg_thread  ON messages(session_id, source_file_id, byte_offset);
CREATE INDEX ix_msg_parent  ON messages(parent_uuid);
CREATE INDEX ix_msg_logical ON messages(logical_parent_uuid);
CREATE INDEX ix_msg_apimsg  ON messages(api_message_id);
CREATE INDEX ix_msg_time    ON messages(timestamp);

-- 検索対象はブロック単位。tool_result が全体165MB中56MBを占め、価値の大半がそこにある。
-- idx は bigram 分かち書き済みの列。FTS5 はこちらを索引する。
CREATE TABLE message_blocks (
  id           INTEGER PRIMARY KEY,
  message_id   INTEGER NOT NULL REFERENCES messages(id),
  idx          INTEGER NOT NULL,
  kind         TEXT NOT NULL,
  tool_name    TEXT,
  tool_use_id  TEXT,
  text         TEXT,
  bigrams      TEXT
);
CREATE INDEX ix_blk_msg  ON message_blocks(message_id, idx);
CREATE INDEX ix_blk_tool ON message_blocks(tool_use_id);

-- external-content。本文の3重複製を避ける。
-- 日本語は unicode61 では分割されないため bigrams 列を索引する（docs/20-data-model.md 参照）。
CREATE VIRTUAL TABLE messages_fts USING fts5(
  bigrams,
  content='message_blocks',
  content_rowid='id',
  tokenize='unicode61 remove_diacritics 2'
);

-- usage は API リクエスト単位。メッセージ行単位にすると約99%過大計上する。
CREATE TABLE usage (
  api_message_id              TEXT PRIMARY KEY,
  request_id                  TEXT,
  session_id                  TEXT NOT NULL REFERENCES sessions(id),
  run_id                      TEXT REFERENCES runs(id),
  project_id                  INTEGER NOT NULL REFERENCES projects(id),
  ts                          TEXT NOT NULL,
  day                         TEXT NOT NULL,
  model                       TEXT NOT NULL,
  service_tier                TEXT,
  speed                       TEXT,
  effort                      TEXT,
  iterations                  INTEGER,
  input_tokens                INTEGER NOT NULL DEFAULT 0,
  output_tokens               INTEGER NOT NULL DEFAULT 0,
  cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_input_tokens     INTEGER NOT NULL DEFAULT 0,
  cache_creation_1h_tokens    INTEGER NOT NULL DEFAULT 0,
  cache_creation_5m_tokens    INTEGER NOT NULL DEFAULT 0,
  thinking_tokens             INTEGER NOT NULL DEFAULT 0,
  web_search_requests         INTEGER NOT NULL DEFAULT 0,
  web_fetch_requests          INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX ix_usage_day     ON usage(day, model);
CREATE INDEX ix_usage_session ON usage(session_id, ts);
CREATE INDEX ix_usage_project ON usage(project_id, day);

-- ノート↔セッションの結合。abs_path は case-fold しない。
CREATE TABLE session_files (
  id             INTEGER PRIMARY KEY,
  session_id     TEXT NOT NULL REFERENCES sessions(id),
  message_id     INTEGER REFERENCES messages(id),
  abs_path       TEXT NOT NULL,
  rel_path       TEXT,
  op             TEXT NOT NULL,
  origin         TEXT NOT NULL,
  backup_name    TEXT,
  backup_version INTEGER,
  at             TEXT NOT NULL,
  UNIQUE(session_id, abs_path, at, op)
);
CREATE INDEX ix_sfiles_path    ON session_files(abs_path, at DESC);
CREATE INDEX ix_sfiles_session ON session_files(session_id, at DESC);

CREATE TABLE notes (
  id           INTEGER PRIMARY KEY,
  vault_id     INTEGER NOT NULL,
  path         TEXT NOT NULL,
  title        TEXT,
  mtime        TEXT,
  size         INTEGER,
  content_hash TEXT,
  UNIQUE(vault_id, path)
);

CREATE TABLE note_links (
  from_note_id INTEGER NOT NULL REFERENCES notes(id),
  raw_target   TEXT NOT NULL,
  to_note_id   INTEGER REFERENCES notes(id),
  resolved     INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(from_note_id, raw_target)
);
CREATE INDEX ix_links_to ON note_links(to_note_id);

CREATE TABLE usage_windows (
  id         INTEGER PRIMARY KEY,
  agent      TEXT NOT NULL,
  account    TEXT,
  kind       TEXT NOT NULL,
  started_at TEXT,
  ends_at    TEXT,
  used_pct   REAL,
  tokens     INTEGER,
  source     TEXT NOT NULL,
  fetched_at TEXT NOT NULL,
  UNIQUE(agent, kind, ends_at, source)
);

CREATE TABLE live_sessions (
  id         INTEGER PRIMARY KEY,
  session_id TEXT REFERENCES sessions(id),
  host_id    INTEGER NOT NULL REFERENCES hosts(id),
  pid        INTEGER,
  started_at TEXT NOT NULL,
  status     TEXT NOT NULL
);

-- 非破壊の検出器（D-010）。記録するだけで raw_json は変更しない。
CREATE TABLE sensitive_findings (
  id           INTEGER PRIMARY KEY,
  message_id   INTEGER NOT NULL REFERENCES messages(id),
  block_id     INTEGER REFERENCES message_blocks(id),
  pattern      TEXT NOT NULL,
  byte_offset  INTEGER,
  length       INTEGER,
  reviewed     INTEGER NOT NULL DEFAULT 0,
  verdict      TEXT,
  found_at     TEXT NOT NULL
);
CREATE INDEX ix_findings_msg ON sensitive_findings(message_id);
CREATE INDEX ix_findings_new ON sensitive_findings(reviewed, found_at DESC);
