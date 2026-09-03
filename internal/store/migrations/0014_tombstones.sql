-- 消したことを消さない。
--
-- Camp の存在理由は「元が消えても読める」ことなので、削除は例外的な操作になる。
-- 例外を黙って通すと、あとから「ここに何かあったのか、元から無かったのか」が
-- 区別できなくなる。だから削除は必ず行を1本足す形にする。
--
-- messages の行そのものは消さない。raw_json を空にして、ここに1行足す。
-- 行を残すのは UNIQUE(source_file_id, byte_offset) を生かすため——
-- 行ごと消すと、元ファイルが手元にある限り次の取り込みで戻ってくる。
CREATE TABLE tombstones (
  id             INTEGER PRIMARY KEY,

  -- 何を消したか。'message.raw_json' / 'message_blocks' / 'blob'
  kind           TEXT NOT NULL,
  -- kind ごとの識別子。message_id、blobs.sha256 など
  ref            TEXT NOT NULL,

  -- 元ファイルのどこだったか。messages 以外では NULL。
  -- 再取り込みを止めるのに使うので、message_id ではなくこちらで引く。
  source_file_id INTEGER REFERENCES source_files(id),
  byte_offset    INTEGER,

  reason         TEXT NOT NULL,   -- なぜ消したか。人が読む文
  actor          TEXT NOT NULL,   -- 誰が消したか。'campd retain' / 'manual' など
  redacted_at    TEXT NOT NULL,
  bytes_removed  INTEGER NOT NULL DEFAULT 0,

  -- 消した時点で「元ファイルから作り直せる」と判断できたか。
  -- 0 なら、この削除は取り返しがつかない。
  recoverable    INTEGER NOT NULL DEFAULT 0,

  note           TEXT
);

CREATE INDEX ix_tomb_src  ON tombstones(source_file_id, byte_offset);
CREATE INDEX ix_tomb_ref  ON tombstones(kind, ref);
CREATE INDEX ix_tomb_when ON tombstones(redacted_at DESC);

-- どの版のパーサが作った派生行か。
--
-- 0 は「分からない」。M21 より前に取り込んだ行が該当する。嘘をつくより
-- 分からないと言う。派生を作り直せるかの判定でここを見る。
ALTER TABLE messages ADD COLUMN parser_version INTEGER NOT NULL DEFAULT 0;
