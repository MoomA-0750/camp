-- Phase 3 / M30。**SSH接続先の台帳。**
--
-- 0001 の `hosts` とは別にする。あちらは「そのセッションがどのホストで
-- 動いたか」で、`projects` と `sessions` が参照している。こちらは
-- 「Camp がこれから繋いでよい先」——同じ表に混ぜると、記録と権限が
-- 同じ行に乗ってしまう。
--
-- **`~/.ssh/config` は読むだけ。書き戻さない。** Camp のバグで端末の SSH が
-- 壊れてはいけない。取り込みは上書きではなく突き合わせで、**allowed は
-- 取り込みで変えない**（許可は人が決めたことで、設定ファイルが決めることではない）。
CREATE TABLE ssh_hosts (
  id           INTEGER PRIMARY KEY,
  alias        TEXT NOT NULL UNIQUE,   -- ssh の Host 名
  hostname     TEXT,                   -- HostName
  user         TEXT,                   -- User
  port         INTEGER,
  identity     TEXT,                   -- IdentityFile
  tailscale_ip TEXT,
  note         TEXT,

  -- **既定は deny。** 取り込んだだけでは繋げない。
  allowed      INTEGER NOT NULL DEFAULT 0,

  source       TEXT NOT NULL,          -- 'ssh_config' / 'manual'
  seen_at      TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);

CREATE INDEX ix_ssh_allowed ON ssh_hosts(allowed, alias);
