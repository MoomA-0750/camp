-- 再開点の検証。ingested_offset の直前256バイトのハッシュを持つ。
--
-- (dev, inode) と size だけでは、同じ inode のまま前より長く書き直された
-- ファイルを見逃す。そのとき差分追尾は新しい中身の途中から読み始め、
-- 気づかないまま壊れたレコードを作る。追記専用ならこの256バイトは
-- 不変なので、変わっていたら書き直されたと判定できる。
ALTER TABLE source_files ADD COLUMN resume_sha TEXT;
