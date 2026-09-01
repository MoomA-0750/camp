-- runs の期間を messages から埋める更新が、run_id に索引が無いために
-- 全表走査を33回繰り返していた。messages は raw_json をインラインで持つので
-- 1ページあたりの行数が少なく、全表走査が特に高い。
-- timestamp まで含めた被覆索引にして、本体に触らず min/max を取れるようにする。
CREATE INDEX ix_msg_run_ts ON messages(run_id, timestamp);
