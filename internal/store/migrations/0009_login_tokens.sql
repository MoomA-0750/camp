-- 使い捨てのログイン用トークン（開発中の入口）。
--
-- 発行には data/camp.sqlite への書き込みが要る。そこに書ける者は
-- すでに messages も auth_credential も好きにできるので、これは
-- 「シェルが取れる者だけが入れる」という既にある事実を使うだけで、
-- 権限を増やさない。パスワードを別経路で流すのとは違う。
--
-- token そのものは保存しない（auth_sessions と同じ）。1回使ったら
-- used_at が入り、二度目は通らない。
CREATE TABLE auth_login_tokens (
  token_hash BLOB PRIMARY KEY,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  used_at    TEXT
);
CREATE INDEX ix_login_tokens_exp ON auth_login_tokens(expires_at);
