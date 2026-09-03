-- 実行の監査ログ。**Phase 3 が書き込む先を、書く側より先に作る。**
--
-- Phase 3 の中で作ると、書く側と器を同時に設計することになって、器のほうが
-- 妥協される。承認・実行・終了状態を残す先は、承認や実行より前に決まっている
-- べきもの。
--
-- 実行専用OSユーザーもVM分離も採らないと決めた以上、何が起きたかを後から
-- 辿れることが最後の砦になる。だから消せないようにする。
CREATE TABLE audit (
  id          INTEGER PRIMARY KEY,
  at          TEXT NOT NULL,          -- RFC3339 UTC
  actor       TEXT NOT NULL,          -- 誰が。'user' / 'campd' / 'session:<id>'
  action      TEXT NOT NULL,          -- 何をしたか。'session.start' / 'tool.approve' など
  target      TEXT,                   -- 何に対して。コマンド・パス・ホストなど
  session_id  TEXT,                   -- セッションで絞れるように。外部キーは張らない
  detail_json TEXT,                   -- 追加の文脈。形は action ごと
  outcome     TEXT NOT NULL,          -- 'ok' / 'denied' / 'error' / 'timeout'

  -- 改竄に気づくための連鎖。prev_hash は1つ前の行の hash。
  --
  -- **トリガだけでは足りない。** トリガは DROP TRIGGER で外せるし、この
  -- ファイルを持っている者は sqlite3 で何でもできる。消したこと・書き換えた
  -- ことに**気づける**ようにするのが、ここで現実に守れる線。
  prev_hash   TEXT NOT NULL,
  hash        TEXT NOT NULL
);

CREATE INDEX ix_audit_at      ON audit(at DESC);
CREATE INDEX ix_audit_session ON audit(session_id, at DESC);
CREATE INDEX ix_audit_action  ON audit(action, at DESC);

-- 追記専用。書き換えも削除も拒む。
CREATE TRIGGER audit_no_update BEFORE UPDATE ON audit
BEGIN
  SELECT RAISE(ABORT, '監査ログは書き換えられない');
END;

CREATE TRIGGER audit_no_delete BEFORE DELETE ON audit
BEGIN
  SELECT RAISE(ABORT, '監査ログは消せない');
END;

-- 保持ポリシーの対象外であることを、消せない形で残しておく。
-- 消せる監査ログは監査にならない。
INSERT INTO audit(at, actor, action, target, outcome, prev_hash, hash)
VALUES ('2026-09-03T00:00:00Z', 'campd', 'audit.init',
        '監査ログを作った。保持ポリシーの対象外', 'ok', '', 'genesis');
