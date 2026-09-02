-- アプリ認証（D-011）。単一ユーザーなので鍵は1本、セッションはCookie1つ。
--
-- Tailscale だけでは境界にならない。tailnet には21台居て、その中には
-- 引き出しの iPhone X も119日オフラインの Quest 2 も居る。Phase 0 の
-- 時点で全トランスクリプトがそこに露出するので、最初のエンドポイントと
-- 同時に入れる。

-- 認証情報は1行だけ。id は常に 1。
CREATE TABLE auth_credential (
  id         INTEGER PRIMARY KEY CHECK (id = 1),
  algo       TEXT NOT NULL,     -- 'pbkdf2-sha256'
  iterations INTEGER NOT NULL,
  salt       BLOB NOT NULL,
  hash       BLOB NOT NULL,
  updated_at TEXT NOT NULL
);

-- ログインセッション。**トークンそのものは保存しない。**
-- DB を読めた者がそのままログインできてしまうのを避けるため、
-- Cookie に載せる乱数の SHA-256 だけを置く。
CREATE TABLE auth_sessions (
  token_hash   BLOB PRIMARY KEY,
  created_at   TEXT NOT NULL,
  expires_at   TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  user_agent   TEXT,
  remote_addr  TEXT
);
CREATE INDEX ix_auth_sessions_exp ON auth_sessions(expires_at);
