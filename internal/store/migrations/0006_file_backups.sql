-- file-history の実体（編集される直前のファイルの中身）の保管庫。
-- CLI はこれをいずれ捨てる。捨てられたあとも読めるようにするのが目的。

-- 内容でアドレスする保管庫。同じ中身は1回しか持たない。
-- codec は 'raw' か 'gzip'。実測では 809 個 15.5MiB のテキストが
-- gzip で 4.7MiB になる（30%）。ただし小さいファイルは gzip の
-- ヘッダ分だけ太るので、縮んだときだけ圧縮して入れる。
CREATE TABLE blobs (
  sha256    TEXT PRIMARY KEY,   -- 展開後の中身のハッシュ。codec に依らない
  size      INTEGER NOT NULL,   -- 展開後のバイト数
  codec     TEXT NOT NULL,
  content   BLOB NOT NULL,
  stored_at TEXT NOT NULL
);

-- バックアップ1件。
--
-- 同一性は (session_id, backup_name)。名前だけでは足りない。
-- 実測 809 個のブロブに対し名前は 618 種しかなく、同じ
-- <hash>@v<N> が別セッションの配下に並んで存在する。ハッシュは
-- パスから作られるので、同じファイルを複数セッションが触れば衝突する。
--
-- abs_path が NULL なのは、JSONL 側に参照が見つからなかったブロブ
-- （origin='orphan'）。実コーパスでは 0 件だが、JSONL が先に消えて
-- 実体だけ残る順序はありうるので、中身は拾って残す。
CREATE TABLE file_backups (
  id          INTEGER PRIMARY KEY,
  session_id  TEXT NOT NULL REFERENCES sessions(id),
  backup_name TEXT NOT NULL,
  version     INTEGER,
  abs_path    TEXT,
  rel_path    TEXT,
  backup_time TEXT,
  sha256      TEXT NOT NULL REFERENCES blobs(sha256),
  -- 参照元。'delta'（file-history-delta、version 1 のみ）/
  -- 'snapshot'（file-history-snapshot、version 2 以上）/ 'orphan'
  origin      TEXT NOT NULL,
  captured_at TEXT NOT NULL,
  -- 実体が消えたのを見つけた時刻。行は消さない。
  missing_at  TEXT,
  UNIQUE(session_id, backup_name)
);
CREATE INDEX ix_fbk_path ON file_backups(abs_path, backup_time DESC);
CREATE INDEX ix_fbk_sess ON file_backups(session_id, backup_time DESC);
CREATE INDEX ix_fbk_sha  ON file_backups(sha256);
