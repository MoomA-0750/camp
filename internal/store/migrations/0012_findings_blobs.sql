-- 検出器が messages しか見ていなかった。Phase 1 でノート本文が blobs に
-- 入ったので、同じ秘密が**走査されない場所**にもう1つ増えた（実測: Vault の
-- 平文パスワードが messages 25件 + blobs 1個）。blobs も指せるようにする。
--
-- message_id の NOT NULL を外すにはテーブルを作り直すしかない。ここには
-- 人が付けた判定（reviewed / verdict）が乗っていて作り直せないので、
-- 中身を必ず引き継ぐ。id も維持する（外から参照されうるため）。
CREATE TABLE sensitive_findings_new (
  id           INTEGER PRIMARY KEY,
  message_id   INTEGER REFERENCES messages(id),
  blob_sha256  TEXT    REFERENCES blobs(sha256),
  block_id     INTEGER REFERENCES message_blocks(id),
  pattern      TEXT NOT NULL,
  byte_offset  INTEGER,
  length       INTEGER,
  reviewed     INTEGER NOT NULL DEFAULT 0,
  verdict      TEXT,
  found_at     TEXT NOT NULL,
  -- どちらか片方だけを指す。両方 NULL は「どこの話か分からない所見」で無意味、
  -- 両方入りは同じ所見を二重に数えることになる。
  CHECK ((message_id IS NULL) <> (blob_sha256 IS NULL))
);

INSERT INTO sensitive_findings_new
  (id, message_id, blob_sha256, block_id, pattern, byte_offset, length, reviewed, verdict, found_at)
SELECT id, message_id, NULL, block_id, pattern, byte_offset, length, reviewed, verdict, found_at
  FROM sensitive_findings;

DROP TABLE sensitive_findings;
ALTER TABLE sensitive_findings_new RENAME TO sensitive_findings;

CREATE INDEX ix_findings_msg  ON sensitive_findings(message_id);
CREATE INDEX ix_findings_blob ON sensitive_findings(blob_sha256);

-- 所見の同一性は「どの入れ物の、どのバイト位置か」。block_id を鍵にしては
-- いけない（backfill で振り直されるため・D-019）。
CREATE UNIQUE INDEX ux_findings_spot
  ON sensitive_findings(message_id, pattern, byte_offset) WHERE message_id IS NOT NULL;
CREATE UNIQUE INDEX ux_findings_blob_spot
  ON sensitive_findings(blob_sha256, pattern, byte_offset) WHERE blob_sha256 IS NOT NULL;
