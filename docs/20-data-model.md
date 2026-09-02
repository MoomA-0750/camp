# データモデルと取り込み仕様

2026-09-01の実証（`dev/active/review-ingest.md`）を反映した確定版。**当初案は複数の点で壊れていた**ので、根拠は必ずそちらを参照すること。

## 設計を決めた13の事実

| # | 事実 | 帰結 |
|---|---|---|
| 1 | `sessionId`（会話の同一性）と `session_id`（実行ごとの同一性）は別物。resume は元ファイルに追記しつつ、新IDのほぼ空なサイドカーを作る | `runs` テーブルが要る。1ファイル1セッションにすると幽霊が約15件出る |
| 2 | assistant はコンテンツブロックごとに1行書き、どの行も同じ `api_message_id` と usage を持つ | usage は `api_message_id` を主キーに。`messages` に対して `SUM` すると出力トークンが実測で約2倍（9,360,046 対 4,700,098） |
| 3 | ディレクトリ名のマングルは `[/._] → -` で4方向に曖昧。逆変換不能。cwd は1セッション内で変わる（最大11個） | ディレクトリ名を parse しない。行ごとの `cwd` を使い、`/.claude/worktrees/` で分割 |
| 4 | サブエージェントは `<sessionId>/subagents/agent-<id>.jsonl` の別ファイル。親からの前方リンクは無い | パスとファイル名だけが結合手段 |
| 5 | compaction でツリーが切断され、`logicalParentUuid` が修復リンク | この列が無いとスレッドが断片化する |
| 6 | **`uuid` はファイルを跨ぐと重複する。** `--fork-session` が親の履歴を uuid ごと新ファイルへ複製する（実測26件） | `messages` の主キーは `uuid` ではなく **`(source_file_id, byte_offset)`** |
| 7 | **行タイプは15種。** 当初14種としていたが `frame-link` を見落としていた | パーサは未知の型で落ちてはいけない |
| 8 | **`toolUseResult` と `attachment` は形が一定しない。** オブジェクト・文字列・配列のいずれもある（厳密な型で受けると実コーパスで26行が落ちた） | `json.RawMessage` で受けて遅延デコードする |
| 9 | **run とセッションは多対多。** `--resume` は1セッションに多runを、`/clear` は1runに多セッションを作る（実測: run `bf4ff50f` が6日間で8セッションを生成） | `runs.session_id` を持たせず `session_runs` で対応を張る（D-012） |
| 10 | **会話行が無いファイルは2種類ある。** 他ファイルから run として参照されるサイドカー（14件）と、参照もされない「起動しただけ」（9件）。後者は remote-control を開いて何も送らなかった痕跡 | `role` を5種にし、`sessions.conversation_count` で一覧から外せるようにする |
| 11 | **同じ `api_message_id` の行は usage が同一とは限らない。** ストリーミングの途中経過が並ぶため、出力側だけが伸びる（実測: 7,101 id 中48件。入力側が食い違う id はゼロ、時刻順で減る箇所もゼロ） | 「最初の1行を採る」は使えない。列ごとに `max` を取る（D-013） |
| 12 | **`file-history-delta` は、それを出したアシスタント行より「先」に書かれる。** 403件中372件（あとに来るのは31件だけ）。自分の `uuid` も `sessionId` も持たず `messageId` で相手を指す | 取り込みの最中には繋ぎ先が存在しない。読み終えてから繋ぎ直す（D-017） |
| 13 | **`file-history` の実体はディスクにしか無く、名前はセッションを跨いで衝突する。** 809個のブロブに対して `<hash>@v<N>` は618種（ハッシュはパスから作られるため、同じファイルを複数セッションが触れば必ずぶつかる）。しかも `backupFileName` は version 1 が `delta`、version 2 以上が `snapshot` にしか出ない | 保管の同一性は **`(session_id, backup_name)`**。片方の記録だけ読むと809個中637個の素性が付かない（D-018） |

## 全文検索は日本語で無言に失敗する（重要）

**英語では通るのでスモークテストを抜けて本番で無言に失敗する。** これが Camp で最初に潰した罠。

### 対策の効き方（実コーパス16,535ブロックで実測、2026-09-02）

`instr(text, 語)` で数えた「本当は当たるべきブロック数」を正解として、各トークナイザのヒット数と並べたもの。

| 語 | unicode61（既定） | trigram | **bigram（採用）** | 正解 |
|---|---|---|---|---|
| `管理` | 38 | 0 | **382** | 382 |
| `設定` | 108 | 0 | **699** | 699 |
| `履歴` | 108 | 0 | **557** | 557 |
| `認証` | 39 | 0 | **219** | 219 |

**trigram は2文字クエリで必ず0件になる**（3文字必要）。`管理` `設定` `履歴` `認証` のような2文字語は日本語に無数にある。

**unicode61 のほうが質が悪い。** 「0件になる」なら壊れていると気づけるが、実際は**正解の1〜2割だけ返す**。前後がASCIIや記号で偶然区切られたぶんだけ当たるからで、**それらしい結果が出るので気づけない**。

bigram は4語すべてで**正解と完全一致**した。取りこぼしも余計なヒットも無い。3文字以上の語（`全文検索` `セッション` `トークン` `日本語` `取り込み` `バックアップ`）でも同様に完全一致を確認済み。

### やり方

`ingest.Bigrams` が **CJK の連なりだけ**を2文字ずつ重ねて切り、それ以外はそのまま通す。

```
"Redmineで進捗報告"  →  "Redmine で進 進捗 捗報 報告"
```

ASCII だけの文字列は一切触らない（unicode61 がそのまま切れるので、加工しても索引が太るだけ）。これを `message_blocks.bigrams` に保存し、FTS5 は external-content でその列だけを索引する。

問い合わせ側（`search.BuildMatch`）も**同じ関数を通す**。bigram の並びを**フレーズ**として問うので、「認証の方式」は当たり「方式の認証」は当たらない。**片方だけ変えるとエラーも警告も出ないまま常に0件になる**ので、索引と問い合わせは必ず対で変更する。

- 1文字のCJKは索引に bigram しか無いので前方一致（`"管"*`）で拾う。語の途中や末尾にある1文字（保**管** の 管）は取りこぼす
- 抜粋は `bigrams` 列ではなく元の `text` から作る。FTS5 の `snippet()` は「設定 定を 変え」のような分かち書き済み文字列を返すので人間には見せられない

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
CREATE TABLE source_files (      -- 1パス複数世代。現行世代だけが一意（部分索引 ux_sf_current）
  id              INTEGER PRIMARY KEY,
  host_id         INTEGER NOT NULL REFERENCES hosts(id),
  path            TEXT NOT NULL,
  incarnation     INTEGER NOT NULL DEFAULT 0,  -- 書き直されるたびに進む世代
  role            TEXT NOT NULL,          -- main | resume-sidecar | subagent | stub | empty
  session_id      TEXT REFERENCES sessions(id),
  agent_id        TEXT,                   -- subagent: ファイル名 agent-<id>.jsonl から
  dev             INTEGER, inode INTEGER, -- 別の実体になっていないかのガード
  size            INTEGER NOT NULL DEFAULT 0,
  mtime           TEXT,
  ingested_offset INTEGER NOT NULL DEFAULT 0,  -- 必ず \n 境界に着地させる
  pending_tail    BLOB,                        -- 最後の \n 以降のバイト（診断用）
  resume_sha      TEXT,                        -- offset 直前256バイト。書き直しの検出
  summary_json    BLOB,                        -- 分類に要る要約。差分だけ吸い上げる
  summary_version INTEGER NOT NULL DEFAULT 0,
  first_seen_at   TEXT NOT NULL,
  missing_at      TEXT,                        -- ★独立保持: 行は決して消さない
  superseded_at   TEXT                         -- 世代が進んだ古い行。これも消さない
);
CREATE UNIQUE INDEX ux_sf_current ON source_files(host_id, path) WHERE superseded_at IS NULL;
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
  conversation_count   INTEGER NOT NULL DEFAULT 0,  -- user/assistant だけ。stub を一覧から外すのに使う
  total_cost_usd       REAL,              -- cost-state 行がタダでくれる
  archived             INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX ix_sessions_recent  ON sessions(host_id, updated_at DESC);
CREATE INDEX ix_sessions_project ON sessions(project_id, updated_at DESC);
CREATE INDEX ix_sessions_agent   ON sessions(agent, updated_at DESC);
CREATE INDEX ix_sessions_parent  ON sessions(parent_session_id);

-- ── runs: --resume ごとに1行。当初案に欠けていたテーブル ────────────────────
CREATE TABLE runs (              -- CLI の1実行。session とは多対多（D-012）
  id              TEXT PRIMARY KEY,
  sidecar_file_id INTEGER REFERENCES source_files(id),
  cli_version     TEXT,
  cwd             TEXT,
  mode            TEXT,
  permission_mode TEXT,
  started_at      TEXT,
  ended_at        TEXT
);

CREATE TABLE session_runs (      -- seq = そのセッション内での実行順（0 = 初回）
  session_id TEXT NOT NULL REFERENCES sessions(id),
  run_id     TEXT NOT NULL REFERENCES runs(id),
  seq        INTEGER NOT NULL,
  PRIMARY KEY(session_id, run_id)
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

-- ── file-history の実体 ─────────────────────────────────────────────────────
-- CLI がいずれ捨てるバックアップの中身そのもの。捨てられたあとも読めること。
CREATE TABLE blobs (
  sha256    TEXT PRIMARY KEY,             -- 展開後の中身のハッシュ。codec に依らない
  size      INTEGER NOT NULL,             -- 展開後のバイト数
  codec     TEXT NOT NULL,                -- raw|gzip。縮まなければ raw で置く
  content   BLOB NOT NULL,
  stored_at TEXT NOT NULL
);

CREATE TABLE file_backups (
  id          INTEGER PRIMARY KEY,
  session_id  TEXT NOT NULL REFERENCES sessions(id),
  backup_name TEXT NOT NULL,              -- <hash>@v<N>。セッションを跨ぐと衝突する
  version     INTEGER,
  abs_path    TEXT,                       -- 参照が無いブロブは NULL（origin='orphan'）
  rel_path    TEXT,
  backup_time TEXT,
  sha256      TEXT NOT NULL REFERENCES blobs(sha256),
  origin      TEXT NOT NULL,              -- delta|snapshot|orphan
  captured_at TEXT NOT NULL,
  missing_at  TEXT,                       -- 実体が消えたのを見つけた時刻。行は消さない
  UNIQUE(session_id, backup_name)
);
CREATE INDEX ix_fbk_path ON file_backups(abs_path, backup_time DESC);
CREATE INDEX ix_fbk_sess ON file_backups(session_id, backup_time DESC);
CREATE INDEX ix_fbk_sha  ON file_backups(sha256);

-- ── 認証（D-011 / D-020）─────────────────────────────────────────────────────
-- 鍵は1本、セッションはCookie1つ。単一ユーザーなのでこれで足りる。
CREATE TABLE auth_credential (
  id         INTEGER PRIMARY KEY CHECK (id = 1),
  algo       TEXT NOT NULL,                  -- pbkdf2-sha256
  iterations INTEGER NOT NULL,
  salt       BLOB NOT NULL,
  hash       BLOB NOT NULL,
  updated_at TEXT NOT NULL
);

-- **トークンそのものは保存しない。** Cookie に載せる乱数の SHA-256 だけ。
CREATE TABLE auth_sessions (
  token_hash   BLOB PRIMARY KEY,
  created_at   TEXT NOT NULL,
  expires_at   TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  user_agent   TEXT, remote_addr TEXT
);
CREATE INDEX ix_auth_sessions_exp ON auth_sessions(expires_at);

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

1. **プロジェクトは行ごとの `cwd` をリポジトリのルートへ畳んで導く。** マングルされたディレクトリ名を絶対にパースしない。順に試す:

   1. `/.claude/worktrees/` があればそこで割る（**ファイルシステムを見ない**。実測2本は既に消えている）。左が親、右の第1セグメントが `worktree_name`
   2. `.git` を上に辿る。worktree では `.git` がファイルなのでディレクトリ限定にしない。`git` プロセスは起動しない。cwd 自身が消えていても祖先が残っていれば解決できる
   3. 1・2で決まらなかったものだけ、**確定済みルートへの最長前方一致**で寄せる。決まらなければ cwd 自身

   3を独立した段にすること。1段階だと、リポジトリでない `/tmp` と `/tmp/camp-test` のどちらが親になるかが走査順で変わる。

   `git remote get-url origin` はルートが実在するときだけ日和見的に引く。`is_repo` は「取り込み時点でルートとして実在した」で、消えた worktree・消えたクローンは 0 になるが**行は消さない**。実測: cwd 35個 → projects 21個
2. **役割は中身から毎回決め直す。** 一度決めて固定してはいけない。稼働中のセッションは生まれた直後 `stub` と見分けが付かず、最初のプロンプトが書かれた瞬間 `main` に変わる

   | role | 判定 | 実測 | sessions を作るか |
   |---|---|---|---|
   | `subagent` | `<sessionId>/subagents/agent-*.jsonl` | 3 | 作る（親に紐付け、`is_sidechain`） |
   | `empty` | 0バイト、または1行も無い | 10 | 作らない |
   | `main` | `user`/`assistant` 行がある | 46 | 作る |
   | `resume-sidecar` | 会話行が無く、**他ファイルの `session_id` として参照されている** | 14 | **作らない**（`runs` になる） |
   | `stub` | 会話行が無く、参照もされていない | 9 | 作る（`conversation_count = 0`） |

   当初の規則は「`{mode, permission-mode, bridge-session, system}` しか含まないもの」としていたが、**サイドカーには `cost-state` や `last-prompt` も混じる**ので型の集合では判定できない。会話行の有無と参照の有無で決める。

   分類は全ファイルを見終わるまで確定しない（「参照されているか」が他ファイルの中身に依存する）。したがって取り込みは必ず2パスになる。
3. **`<sessionId>/subagents/agent-*.jsonl` はパスで親セッションに結び付ける。** subagent 行の `sessionId` は**親の値**なので、そのままでは主キーにできない。`sessions.id` は `<親sessionId>.<agentId>` を合成する。`is_sidechain` と `agent_id` を立て、`parent_session_id` で親を指す
3b. **run の照合はセッション横断で行う。** `/clear` で生まれたセッションの行が別セッション由来の run を指すのは正しい。セッション単位で絞ると実測で7,173件の `run_id` を落とす
4. **usage は `api_message_id` を主キーに、列ごとの `max` で upsert する。** `messages` に対して `SUM` しない（実測で約2倍になる）。

   `DO NOTHING` では足りない。同じ id が複数行に現れる理由は2つあり、**ストリーミングの途中経過**では出力側だけが伸びる（実測48件、最初の1行を採ると36,406トークンの過少計上）。`max` は順序に依らず冪等なので、途中経過と確定値が別々の取り込みパスに分かれても結果が変わらない。**入力側・キャッシュ側が食い違う id はゼロ**なので `max` は実質的に恒等写像として働く。

   `<synthetic>` は `usage` に**入れない**（規則10）。`ts` はローカル日付に落として `day` に入れる（D-013）。`iterations` はオブジェクト配列の**本数**。同じ id が複数セッションに現れたら最初に取り込んだほうに帰属させる（トークンは1回しか払っていない）。

   `cost-state` 行はセッションの累計コストを丸ごとくれる。タイムスタンプが無いので順序では選べず、累計なので**最大値**を採って `sessions.total_cost_usd` に入れる
5. **最後の `\n` までしかコミットしない。** 残りは `pending_tail` に退避（診断用。再開点は `ingested_offset` なので未完了の末尾は自然に読み直される）。

   **世代が変わったと判定する条件は3つ**（1つでも当たれば `incarnation` を進めた**別の行**を作り、古い行は `superseded_at` を立てて残す）:

   1. `(dev, inode)` が変わった — 別の実体になった
   2. `size < ingested_offset` — 切り詰められた
   3. `ingested_offset` 直前256バイトのハッシュ（`resume_sha`）が変わった — **同じ inode のまま前より長く書き直された**

   3が無いと1と2をすり抜ける書き直しを見逃し、新しい中身の途中から読み始めて黙って壊れる。

   **同じ行のオフセットを0に戻してはいけない。** 新しい中身が古い中身と同じバイト位置に現れ、`messages` の `UNIQUE(source_file_id, byte_offset)` に当たって `ON CONFLICT DO NOTHING` が新しい行を捨てる。
5b. **走査で見えなかったファイルは `missing_at` を立てるだけ。行は消さない**（D-001）。既に立っている `missing_at` は上書きしない
5c. **要約は `summary_json` に永続化して差分だけ吸い上げる。** 役割の分類に `RunIDs` と会話行の有無が要るので、保存しないと追記2行のために165MBを読み直すことになる（実測 6.2秒 → 66ms）。`summary_version` を上げると全ファイルが読み直される
6. **オフセット更新とレコード挿入は1トランザクション**
7. **`sessions.updated_at = max(メッセージのtimestamp, ファイルのmtime)`**。5種類の行にタイムスタンプが無い
8. **表示順は `(source_file_id, byte_offset)`。** タイムスタンプでソートしない（実測: 72ファイル中**47本**、計**681箇所**で時刻が逆行する。最も多いセッションで105箇所）
8b. **木に参加するのは15種のうち4種だけ**（`user` `assistant` `attachment` `system`）。残り11種は uuid も parentUuid も持たない付帯記録で、木の外にある。**実効の親 = `logicalParentUuid` があればそれ、無ければ `parentUuid`**。`logicalParentUuid` は `system/compact_boundary` にしか付かず（実測15件全部）、その行は `parentUuid` を持たない

   **親の照合は必ず `session_id` で絞る。** uuid はセッション内では一意（重複0件）だが、セッションを跨ぐと fork で重複する。絞らないと他人の履歴に繋がる。

   **根が1つになるとは限らない。それが正常。** 親を持たない `system/bridge_status` 行40件は**すべてそのファイルの先頭行**で、`/remote-control is active` の案内である。resume のサイドカーはほぼ空なので案内行だけが根として残る。会話の本数を数えるときは「子孫に user/assistant を含む根」だけを数える（実測: 49セッションすべてが1本）
9. **`~/.claude/file-history/<sessionId>/` のバックアップ実体も捕獲対象**（現在18セッション/18MB）。ノートの編集前の中身そのもので、CLIがいずれGCする
10. **`<synthetic>` は `usage` に入れない。** CLIがローカルで作る擬似アシスタント行で、`usage` は常にゼロ、課金も発生していない。入れておいて集計のたびに除外するより、最初から入れないほうが「除外を忘れる」経路が消える。`messages` には残す（レート制限履歴として有用。実測16行）
11. **Vaultの索引は `.claude/` `.git/` `.trash/` を必ず除外する。** `.claude/worktrees/` に本体の11倍（45,125ファイル/442MB）の古いコピーがある
12. **Codex は木構造ではなくフラットな `ordinal` 列。トークンは累積値なのでSUMすると多重計上する**（Claudeとは逆向きの失敗モード）
13. **1行が壊れていてもファイル全体の読み取りを止めない。** 厳密なデコードに失敗した行は最小限の共通フィールドだけ拾って `degraded` を立て、`raw_json` は必ず保存する。「独立保持」を謳う以上、形が想定外だからという理由で記録を失ってはいけない。パーサを直せば後から作り直せる
14. **末尾が改行で終わっていない行は消費しない。** `pending_tail` に退避してオフセットを進めない
15. **派生テーブルの作り直しはディスクではなく `messages` から行う。** 新しい派生テーブル（`usage` など）を足しても、取り込み済みファイルは `ingested_offset` が終端なので埋まらない。ここで `~/.claude/projects` から取り込み直すと、**CLI 側で既に消えている記録を失う**。`raw_json` は無加工で持っている（D-010）ので `campd backfill` がDBの中だけで作り直す。取り込み経路で作った結果と一致することを7,119行の全列突き合わせで確認済み（D-014）
16. **ノートへの結合は4経路の和を採り、パスは大文字小文字を畳まない。** `file-history-delta`（`realParentDir` + `basename(trackingPath)`）／`toolUseResult.filePath`（Edit・Write）／**`toolUseResult.file.filePath`（Read だけ1段深い）**／`attachment.filename`。実測 2,414件・370パス。

    - **`Read` を落とすと 617件・53パスが消える。** 消えるのは Inbox のように読まれたあと `.trash/` へ移されたノートで、そういうノートこそ Camp にしか残っていない
    - **`edit` と `write` の区別は `tool_use_id` からツール名を引く。** 形（`oldString` の有無など）から当てると `Write` の2件を取り違える
    - **相対パスは採らない。** cwd はセッションの途中で変わる（事実3）ので、当て推量で絶対化すると存在しないノートへのリンクが静かに増える
    - **`Obsidian-Vault` と `Obsidian-vault` は別物として残す。** 小文字v は 2026-08-09〜08-10 の6セッションで使われ、いまディレクトリは存在しない（改名された）。名前だけ同じノートが5つある。畳むと「触っていない側を触ったことにする」（D-016）
    - **`file-history-snapshot` は使わない。** 展開すると4,754行になるが新しいパスは1つも増えない（286パスは370に完全に含まれる）。version 2以上の `backupFileName` を持つのはこちらだけなので M9 では読む
17. **`file-history` の繋ぎ先は取り込みの最後に1本の UPDATE で入れる。** その場で引くと372件が繋がらず、`backfill` と結果が食い違う。いったん `file-history` 行自身を指しておき、`LinkFileHistory` が繋ぎ直す。繋ぎ直したあとの指し先は `assistant` なので二度目以降は1行も動かない（D-017）
18. **`file-history` の実体は取り込みのたびに捕獲する。** 参照（JSONL）は永久に持てるが中身はディスクにしかなく、CLI がいずれ GC する。保管の鍵は `(session_id, backup_name)`。名前は809個中618種しか無くセッションを跨いで衝突するので、名前を鍵にすると素性が混ざり、片方が消えても気付けない。素性は `delta`（version 1、172個・92パス）と `snapshot`（version 2以上、637個・283パス）の**両方**から採る。片方では足りない（D-018）
19. **中身は内容でアドレスし、縮んだときだけ圧縮する。** `blobs.sha256` は展開後の中身のハッシュなので `codec` に依らない。実測809個15.5MiBが、重複除去（751本）と gzip で 4.7MiB。小さいファイルは gzip ヘッダのぶん太るので、縮まなかったものは `raw` のまま入れる。実体が消えた行には `missing_at` を立てるだけで、行も中身も消さない
20. **検出器は記録するだけで、`raw_json` を1バイトも変えない**（D-010）。位置は `sensitive_findings.block_id` ではなく `(message_id, raw_json のバイト位置)` で持つ。`message_blocks.id` は `backfill` のたびに振り直されるので、人の判断（`verdict`）を載せる表をそこに繋いではいけない。同じ場所は二度記録しない（`ux_findings_spot` ＋ `ON CONFLICT DO NOTHING`）ので、何度走査しても判断は残る
21. **高信頼のパターンだけを見る。** 発行元が決めた接頭辞と長さを持つものに限る。実測でこのコーパスは偽陽性100%・偽陰性100%（D-019）なので、パターンで見つからない実在の秘密は `known-secrets.txt` に登録して突き合わせる。その中身はリポジトリに置かない
22. **HTTP は `/healthz` 以外を1つも素通ししない**（D-011 / D-020）。画面の殻もバンドルも認証の内側。ログインは組み込みの `/login` から。未認証は `/api/*` が 401 JSON、それ以外は `/login` へ 302
23. **未マッチの GET は殻（`index.html`）を返す。ただし `/api/` は 404 JSON。** 殻が無いと `/sessions/<id>` の直接オープンとリロードが404になる。逆に `/api/` に殻を返すと、JSON を待っている相手が原因の分からない壊れ方をする
