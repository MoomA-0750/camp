-- tombstone を「元ファイルの世代」から切り離す。
--
-- 2026-09-03 の outer gate で、`(source_file_id, byte_offset)` による再取り込み抑止が
-- **inode が変わるだけで丸ごと外れる**ことが分かった（rsync・バックアップ復元・
-- 別マシンへの移動が全部これに当たる）。ingest は同じパスでも新しい世代の
-- source_files 行を作るので、新世代には tombstone が無い。
--
-- `raw_json` は元の JSONL の行と**バイト単位で一致する**（実測済み）ので、
-- 行そのものの sha256 を持てば世代にもバイト位置にも依存しなくなる。
-- JSONL の行は uuid と timestamp を含むので、別内容と衝突することは実質ない。
--
-- 既存の行の埋め戻しは `campd doctor -fix` が元ファイルを読んで行う
-- （ここでは SQL からファイルを読めない）。

ALTER TABLE tombstones ADD COLUMN line_sha256 TEXT;

CREATE INDEX ix_tombstones_line ON tombstones(line_sha256)
  WHERE line_sha256 IS NOT NULL;
