-- Phase 3.8 の M47。向こうのホストの記録を読んでよいか（ホストとエージェントごと）。
--
-- **行が無ければ読まない。** 許した接続先でも、記録を読むのは別に選ぶ（起こしてよい場所と、
-- 記録を読んでよいかは別物）。0026 と同じ「行で持つ」作法（D-031）。
--
-- **パスは持たない**（本人の決定 2026-09-12、Fable の指摘5）。置き場は向こうの sh が
-- $HOME と環境変数から解決して名乗る。自由なパスを打ち込む口を作らないので、Cookie を
-- 盗られてもできるのは「知っている記録の読みを on/off」だけになる。
--
-- last_ok_at・last_error・fail_count は**静かな失敗に本人が気づけるように**画面へ出す。
-- 失敗のたびに監査へ残すと、寝ている携帯で1日ぶんが積もる（Fable の指摘6）。
CREATE TABLE ssh_record_roots (
  host       TEXT NOT NULL,   -- ssh_hosts.alias
  agent      TEXT NOT NULL,   -- 取り込み器の名前（claude / codex）
  enabled    INTEGER NOT NULL DEFAULT 1,
  last_ok_at TEXT,            -- 最後に読めた時刻
  last_error TEXT,            -- 最後の失敗の理由
  fail_count INTEGER NOT NULL DEFAULT 0,  -- 続けて失敗した回数（間隔を伸ばす材料）
  UNIQUE (host, agent)
);
