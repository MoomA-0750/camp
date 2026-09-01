-- camp:no-foreign-keys
-- 1つのパスに対して source_files の行を複数持てるようにする。
--
-- 元の設計は UNIQUE(host_id, path) で1パス1行だった。だが「(dev,inode) が
-- 変わった」「size < ingested_offset」を検出したとき、同じ行のオフセットを
-- 0に戻すと壊れる。新しい中身は古い中身と同じバイト位置に現れるので、
-- messages の UNIQUE(source_file_id, byte_offset) に当たって
-- ON CONFLICT DO NOTHING が新しい行を黙って捨てる。
-- 検出はできているのにデータが消える、という最悪の形になる。
--
-- incarnation を足して世代を分ける。古い世代は superseded_at を立てて残す
-- （D-001「元が消えても行は消さない」）。現行の世代だけが一意。
--
-- FK の張り替えを避けるため、新テーブルを作って中身を移し、旧テーブルを
-- 落としてから rename する。messages.source_file_id は文字列として
-- "source_files" を参照しているので、rename 後にそのまま解決される。
-- FK は camp:no-foreign-keys 指示で切る。defer_foreign_keys では、親テーブルを
-- DROP して同名で作り直したときに遅延カウンタが戻らずコミットで 787 になる。
-- 切った代わりに、コミット前に foreign_key_check が走る。


CREATE TABLE source_files_v2 (
  id              INTEGER PRIMARY KEY,
  host_id         INTEGER NOT NULL REFERENCES hosts(id),
  path            TEXT NOT NULL,
  incarnation     INTEGER NOT NULL DEFAULT 0,
  role            TEXT NOT NULL,
  session_id      TEXT REFERENCES sessions(id),
  agent_id        TEXT,
  dev             INTEGER,
  inode           INTEGER,
  size            INTEGER NOT NULL DEFAULT 0,
  mtime           TEXT,
  ingested_offset INTEGER NOT NULL DEFAULT 0,
  pending_tail    BLOB,
  summary_json    BLOB,
  first_seen_at   TEXT NOT NULL,
  missing_at      TEXT,
  superseded_at   TEXT
);

INSERT INTO source_files_v2(
  id, host_id, path, incarnation, role, session_id, agent_id,
  dev, inode, size, mtime, ingested_offset, pending_tail, first_seen_at, missing_at)
SELECT id, host_id, path, 0, role, session_id, agent_id,
       dev, inode, size, mtime, ingested_offset, pending_tail, first_seen_at, missing_at
  FROM source_files;

DROP TABLE source_files;
ALTER TABLE source_files_v2 RENAME TO source_files;

-- 現行の世代だけが (host_id, path) で一意。superseded は何本でも積める。
CREATE UNIQUE INDEX ux_sf_current ON source_files(host_id, path) WHERE superseded_at IS NULL;
CREATE INDEX ix_sf_session ON source_files(session_id);
CREATE INDEX ix_sf_scan    ON source_files(host_id, missing_at, mtime DESC);
CREATE INDEX ix_sf_path    ON source_files(host_id, path, incarnation);

-- 要約キャッシュの世代。パーサを直したら上げて全ファイルを読み直させる。
ALTER TABLE source_files ADD COLUMN summary_version INTEGER NOT NULL DEFAULT 0;
