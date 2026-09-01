# データモデルと取り込み仕様

2026-09-01の実証（`dev/active/review-ingest.md`）を反映した確定版。**当初案は複数の点で壊れていた**ので、根拠は必ずそちらを参照すること。

## 設計を決めた5つの事実

| # | 事実 | 帰結 |
|---|---|---|
| 1 | `sessionId`（会話の同一性）と `session_id`（実行ごとの同一性）は別物。resume は元ファイルに追記しつつ、新IDのほぼ空なサイドカーを作る | `runs` テーブルが要る。1ファイル1セッションにすると幽霊が約15件出る |
| 2 | assistant はコンテンツブロックごとに1行書き、全行が同一の usage を持つ | usage は `api_message_id` を主キーに。実測で出力トークン99%過大 |
| 3 | ディレクトリ名のマングルは `[/._] → -` で4方向に曖昧。逆変換不能。cwd は1セッション内で変わる（最大11個） | ディレクトリ名を parse しない。行ごとの `cwd` を使い、`/.claude/worktrees/` で分割 |
| 4 | サブエージェントは `<sessionId>/subagents/agent-<id>.jsonl` の別ファイル。親からの前方リンクは無い | パスとファイル名だけが結合手段 |
| 5 | compaction でツリーが切断され、`logicalParentUuid` が修復リンク | この列が無いとスレッドが断片化する |
| 6 | **`uuid` はファイルを跨ぐと重複する。** `--fork-session` が親の履歴を uuid ごと新ファイルへ複製する（実測26件） | `messages` の主キーは `uuid` ではなく **`(source_file_id, byte_offset)`** |
| 7 | **行タイプは15種。** 当初14種としていたが `frame-link` を見落としていた | パーサは未知の型で落ちてはいけない |
| 8 | **`toolUseResult` と `attachment` は形が一定しない。** オブジェクト・文字列・配列のいずれもある（厳密な型で受けると実コーパスで26行が落ちた） | `json.RawMessage` で受けて遅延デコードする |

## 全文検索は日本語で無言に失敗する（重要）

sqlite 3.51.2 で実測:

| トークナイザ | `トークン` | `管理` |
|---|---|---|
| 既定（unicode61） | **0件** | **0件** |
| trigram | 1件 | **0件** |
| icu | ビルドに含まれず | |

既定は日本語を分割しない。**trigram も2文字クエリで機能しない**（3文字必要）。`管理` `設定` `履歴` `認証` のような2文字語は日本語に無数にある。

**英語では通るのでスモークテストを抜けて本番で無言に失敗する。** インデックス時に形態素解析して分かち書きを保存し `unicode61` を使うか、bigram を自前で作る。`messages_fts` と Vault の `notes` の両方に効く。

## DDL

```sql
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

-- ── トポロジ ────────────────────────────────────────────────────────────────
CREATE TABLE hosts (
  id            INTEGER PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  ssh_alias     TEXT, tailscale_ip TEXT, os TEXT,
  last_seen_at  TEXT, enabled INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE projects (
  id                INTEGER PRIMARY KEY,
  host_id           INTEGER NOT NULL REFERENCES hosts(id),
  repo_path         TEXT NOT NULL,        -- 絶対パス・大文字小文字を区別・worktreeは剥がし済み
  name              TEXT NOT NULL,
  git_origin        TEXT,
  is_worktree       INTEGER NOT NULL DEFAULT 0,
  worktree_name     TEXT,                 -- 例 'bridge-cse_012NqFhCsVGgRKsxQioEfF7N'
  parent_project_id INTEGER REFERENCES projects(id),
  UNIQUE(host_id, repo_path)              -- Obsidian-Vault と Obsidian-vault は別物
);
CREATE INDEX ix_projects_parent ON projects(parent_project_id);

-- ── ファイル層をセッション層から分離（事実1・4への対処）────────────────────
CREATE TABLE source_files (
  id              INTEGER PRIMARY KEY,
  host_id         INTEGER NOT NULL REFERENCES hosts(id),
  path            TEXT NOT NULL,
  role            TEXT NOT NULL,          -- main | resume-sidecar | subagent | codex-rollout
  session_id      TEXT REFERENCES sessions(id),
  agent_id        TEXT,                   -- subagent: ファイル名 agent-<id>.jsonl から
  dev             INTEGER, inode INTEGER, -- ローテーション/切り詰めのガード
  size            INTEGER NOT NULL DEFAULT 0,
  mtime           TEXT,
  ingested_offset INTEGER NOT NULL DEFAULT 0,  -- 必ず \n 境界に着地させる
  pending_tail    BLOB,                        -- 最後の \n 以降のバイト
  first_seen_at   TEXT NOT NULL,
  missing_at      TEXT,                        -- ★独立保持: 行は決して消さない
  UNIQUE(host_id, path)
);
CREATE INDEX ix_sf_session ON source_files(session_id);
CREATE INDEX ix_sf_scan    ON source_files(host_id, missing_at, mtime DESC);

-- ── セッション = 会話の同一性（sessionId）。ファイルではない ────────────────
CREATE TABLE sessions (
  id                   TEXT PRIMARY KEY,  -- sessionId。resumeを跨いで不変
  host_id              INTEGER NOT NULL REFERENCES hosts(id),
  project_id           INTEGER NOT NULL REFERENCES projects(id),
  agent                TEXT NOT NULL,     -- claude | codex
  parent_session_id    TEXT REFERENCES sessions(id),  -- fork / codex の spawn edge
  parent_agent_id      TEXT,
  ai_title             TEXT,              -- ai-title の最新勝ち。LLM呼び出し不要
  user_title           TEXT,
  first_user_message   TEXT,
  last_prompt          TEXT,
  last_prompt_leaf     TEXT,
  git_branch           TEXT,              -- 唯一セッション内で安定していた属性
  last_cwd             TEXT,              -- 非正規化。正はmessages側
  last_cli_version     TEXT,
  last_model           TEXT,
  last_mode            TEXT,
  last_permission_mode TEXT,
  bridge_session_id    TEXT,              -- claude remote-control との結合キー
  is_sidechain         INTEGER NOT NULL DEFAULT 0,
  started_at           TEXT NOT NULL,
  updated_at           TEXT NOT NULL,     -- max(msg ts, file mtime)。5種類の行にtsが無い
  message_count        INTEGER NOT NULL DEFAULT 0,
  total_cost_usd       REAL,              -- cost-state 行がタダでくれる
  archived             INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX ix_sessions_recent  ON sessions(host_id, updated_at DESC);
CREATE INDEX ix_sessions_project ON sessions(project_id, updated_at DESC);
CREATE INDEX ix_sessions_agent   ON sessions(agent, updated_at DESC);
CREATE INDEX ix_sessions_parent  ON sessions(parent_session_id);

-- ── runs: --resume ごとに1行。当初案に欠けていたテーブル ────────────────────
CREATE TABLE runs (
  id              TEXT PRIMARY KEY,       -- claude の session_id（snake_case）
  session_id      TEXT NOT NULL REFERENCES sessions(id),
  seq             INTEGER NOT NULL,
  sidecar_file_id INTEGER REFERENCES source_files(id),
  cli_version     TEXT, cwd TEXT, mode TEXT, permission_mode TEXT,
  started_at      TEXT, ended_at TEXT,
  UNIQUE(session_id, seq)
);
CREATE INDEX ix_runs_session ON runs(session_id, seq);

-- ── メッセージ ──────────────────────────────────────────────────────────────
CREATE TABLE messages (
  uuid                TEXT PRIMARY KEY,   -- 全体で一意（22039/22039 実測）
  session_id          TEXT NOT NULL REFERENCES sessions(id),
  run_id              TEXT REFERENCES runs(id),
  source_file_id      INTEGER NOT NULL REFERENCES source_files(id),
  byte_offset         INTEGER NOT NULL,   -- 表示順 かつ tailer の再開点
  parent_uuid         TEXT,
  logical_parent_uuid TEXT,               -- ★compact_boundary の修復リンク
  type                TEXT NOT NULL,      -- 14種（atis-latch と cost-state を含む）
  subtype             TEXT,
  role                TEXT,
  timestamp           TEXT,               -- ai-title/mode/permission-mode/bridge-session/atis-latch は NULL
  cwd                 TEXT,               -- 行ごと。セッション内で変わる
  cli_version         TEXT,
  is_sidechain        INTEGER NOT NULL DEFAULT 0,
  agent_id            TEXT,
  is_meta             INTEGER NOT NULL DEFAULT 0,
  is_compact_summary  INTEGER NOT NULL DEFAULT 0,
  is_api_error        INTEGER NOT NULL DEFAULT 0,
  api_message_id      TEXT,               -- message.id — usage の粒度
  request_id          TEXT,               -- api_message_id と 1:1（6936/6936）
  model               TEXT,
  service_tier        TEXT,
  effort              TEXT,
  raw_json            BLOB NOT NULL       -- zstd圧縮した元の行
);
CREATE INDEX ix_msg_thread  ON messages(session_id, byte_offset);
CREATE INDEX ix_msg_parent  ON messages(parent_uuid);
CREATE INDEX ix_msg_logical ON messages(logical_parent_uuid);
CREATE INDEX ix_msg_apimsg  ON messages(api_message_id);
CREATE INDEX ix_msg_time    ON messages(timestamp);

-- 検索対象テキストはブロック単位（事実: tool_result が 56MB / 全 165MB）
CREATE TABLE message_blocks (
  id           INTEGER PRIMARY KEY,
  message_uuid TEXT NOT NULL REFERENCES messages(uuid),
  idx          INTEGER NOT NULL,
  kind         TEXT NOT NULL,             -- text|thinking|tool_use|tool_result|image
  tool_name    TEXT,
  tool_use_id  TEXT,
  text         TEXT
);
CREATE INDEX ix_blk_msg  ON message_blocks(message_uuid, idx);
CREATE INDEX ix_blk_tool ON message_blocks(tool_use_id);

-- external-content FTS: テキストの3重複製を避ける
-- ★tokenize は日本語対応が要る（上記「無言に失敗する」参照）。
--   分かち書き済みの列を別に持ち unicode61 を当てるのが第一候補。
CREATE VIRTUAL TABLE messages_fts USING fts5(
  text, kind UNINDEXED, message_uuid UNINDEXED,
  content='message_blocks', content_rowid='id',
  tokenize="unicode61 remove_diacritics 2"
);

-- ── usage: メッセージ行ではなくAPIリクエスト単位（事実2）────────────────────
CREATE TABLE usage (
  api_message_id  TEXT PRIMARY KEY,       -- 11337行 → 6936行に重複排除
  request_id      TEXT,
  session_id      TEXT NOT NULL REFERENCES sessions(id),
  run_id          TEXT REFERENCES runs(id),
  project_id      INTEGER NOT NULL REFERENCES projects(id),
  ts              TEXT NOT NULL,
  day             TEXT NOT NULL,
  model           TEXT NOT NULL,          -- '<synthetic>' はモデル別集計から除外
  service_tier    TEXT, speed TEXT, effort TEXT, iterations INTEGER,
  input_tokens                INTEGER NOT NULL DEFAULT 0,
  output_tokens               INTEGER NOT NULL DEFAULT 0,
  cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_input_tokens     INTEGER NOT NULL DEFAULT 0,
  cache_creation_1h_tokens    INTEGER NOT NULL DEFAULT 0,  -- 1hと5mで価格が違う
  cache_creation_5m_tokens    INTEGER NOT NULL DEFAULT 0,
  thinking_tokens             INTEGER NOT NULL DEFAULT 0,
  web_search_requests         INTEGER NOT NULL DEFAULT 0,
  web_fetch_requests          INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX ix_usage_day     ON usage(day, model);
CREATE INDEX ix_usage_session ON usage(session_id, ts);
CREATE INDEX ix_usage_project ON usage(project_id, day);

-- ── ファイル接触 = ノート↔セッションの結合 ─────────────────────────────────
CREATE TABLE session_files (
  id             INTEGER PRIMARY KEY,
  session_id     TEXT NOT NULL REFERENCES sessions(id),
  message_uuid   TEXT REFERENCES messages(uuid),
  abs_path       TEXT NOT NULL,           -- case-fold しないこと
  rel_path       TEXT,
  op             TEXT NOT NULL,           -- read|edit|write|snapshot
  origin         TEXT NOT NULL,           -- file-history|toolUseResult|attachment
  backup_name    TEXT, backup_version INTEGER,
  at             TEXT NOT NULL,
  UNIQUE(session_id, abs_path, at, op)
);
CREATE INDEX ix_sfiles_path    ON session_files(abs_path, at DESC);
CREATE INDEX ix_sfiles_session ON session_files(session_id, at DESC);

-- ── Vault ───────────────────────────────────────────────────────────────────
CREATE TABLE notes (
  id       INTEGER PRIMARY KEY,
  vault_id INTEGER NOT NULL,
  path     TEXT NOT NULL,                 -- 大文字小文字を区別
  title    TEXT, mtime TEXT, size INTEGER, content_hash TEXT,
  UNIQUE(vault_id, path)
);
CREATE TABLE note_links (
  from_note_id INTEGER NOT NULL REFERENCES notes(id),
  raw_target   TEXT NOT NULL,
  to_note_id   INTEGER REFERENCES notes(id),
  resolved     INTEGER NOT NULL DEFAULT 0,  -- 宙吊りと未索引を区別する
  PRIMARY KEY(from_note_id, raw_target)
);
CREATE INDEX ix_links_to ON note_links(to_note_id);

CREATE TABLE usage_windows (
  id         INTEGER PRIMARY KEY,
  agent      TEXT NOT NULL,               -- codex は会話記録に埋め込んでいる
  account    TEXT,
  kind       TEXT NOT NULL,               -- five_hour|seven_day|spend_limit
  started_at TEXT, ends_at TEXT,
  used_pct   REAL, tokens INTEGER,
  source     TEXT NOT NULL,               -- control-protocol|codex-rollout|statusline
  fetched_at TEXT NOT NULL,
  UNIQUE(agent, kind, ends_at, source)
);
```

## 取り込み規則

1. **プロジェクトは行ごとの `cwd` から導く。** マングルされたディレクトリ名を絶対にパースしない。`/.claude/worktrees/` で分割し左半分を親リポジトリとする。**結果を永続化する**（worktreeは消える）。`git rev-parse --git-common-dir` は worktree が生きている間だけ日和見的に併用する
2. **`{mode, permission-mode, bridge-session, system}` しか含まず、他ファイルの `session_id` として現れるファイルは resume サイドカー。** `sessions` ではなく `runs` を作る
3. **`<sessionId>/subagents/agent-*.jsonl` はパスで親セッションに結び付ける。** `is_sidechain` と `agent_id` を立て、トップレベルのセッションを作らない
4. **usage は `INSERT … ON CONFLICT(api_message_id) DO NOTHING`。** `messages` に対して `SUM` しない
5. **最後の `\n` までしかコミットしない。** 残りは `pending_tail` に退避。`size < ingested_offset` または `(dev,inode)` が変わったら、新しい `source_files` 行としてオフセット0から取り直す
6. **オフセット更新とレコード挿入は1トランザクション**
7. **`sessions.updated_at = max(メッセージのtimestamp, ファイルのmtime)`**。5種類の行にタイムスタンプが無い
8. **表示順は `(source_file_id, byte_offset)`。** タイムスタンプでソートしない（69ファイル中44本で逆順が発生）
9. **`~/.claude/file-history/<sessionId>/` のバックアップ実体も捕獲対象**（現在18セッション/18MB）。ノートの編集前の中身そのもので、CLIがいずれGCする
10. **`<synthetic>` はモデル別集計から除外する。** 行自体は残す（レート制限履歴として有用）
11. **Vaultの索引は `.claude/` `.git/` `.trash/` を必ず除外する。** `.claude/worktrees/` に本体の11倍（45,125ファイル/442MB）の古いコピーがある
12. **Codex は木構造ではなくフラットな `ordinal` 列。トークンは累積値なのでSUMすると多重計上する**（Claudeとは逆向きの失敗モード）
13. **1行が壊れていてもファイル全体の読み取りを止めない。** 厳密なデコードに失敗した行は最小限の共通フィールドだけ拾って `degraded` を立て、`raw_json` は必ず保存する。「独立保持」を謳う以上、形が想定外だからという理由で記録を失ってはいけない。パーサを直せば後から作り直せる
14. **末尾が改行で終わっていない行は消費しない。** `pending_tail` に退避してオフセットを進めない
