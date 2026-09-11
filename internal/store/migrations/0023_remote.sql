-- 2026-09-11。**SSH で他のホストにセッションを起こす。**
--
-- 向こうに常駐するものは置かない（D-003）。実行面が `ssh <alias> <小さな sh>` を
-- 起こし、その sh が場所を確かめてから `claude` に成り代わる。
--
-- runtime_sessions の pid / proc_started / boot_id は、これまでどおり
-- **手元のプロセス**（リモートなら ssh）を指す。campd が /proc で確かめられるのは
-- 手元だけなので、所有権の判定はそのまま効く。向こうの子は別の列に置く
-- ——**campd はそれを確かめられない**（camp ユーザーは鍵を持たない）。
-- 実行面が向こうを見に行って報告したものを記録するだけ。
ALTER TABLE runtime_sessions ADD COLUMN host TEXT;            -- ssh の Host 名。NULL はこのマシン
ALTER TABLE runtime_sessions ADD COLUMN remote_pid INTEGER;
ALTER TABLE runtime_sessions ADD COLUMN remote_started INTEGER;
ALTER TABLE runtime_sessions ADD COLUMN remote_boot_id TEXT;
ALTER TABLE runtime_sessions ADD COLUMN remote_scope TEXT;    -- 向こうの systemd scope（あれば）

-- **許したときの行き先を固定する。**
--
-- D-026 は「`~/.ssh/config` を編集しただけで繋げる先が増えることはない」と約束した。
-- だがエイリアスだけを許すと、許したあとで config の HostName を書き換えれば、
-- 同じ名前のまま別の先へ繋がる。許したときに `ssh -G` で見た行き先
-- （hostname / user / port / proxyjump / proxycommand）を JSON で残し、
-- 起こすたびに照らす。違えば起こさない。
--
-- 9/11 より前に許した行は NULL。**起こす前に許し直してもらう**（推し量って固定しない）。
ALTER TABLE ssh_hosts ADD COLUMN pinned TEXT;
-- 向こうの `claude` の実体。空なら向こうで探す（PATH、~/.local/bin、Homebrew）。
-- 非対話の ssh では PATH に Homebrew が入らないホストがある（mac で実測済み）。
ALTER TABLE ssh_hosts ADD COLUMN claude_path TEXT;

-- 向こうで起こしてよい場所。**ホストごと。**
--
-- 手元の allowed_cwd とは表を分ける。あちらは path だけで UNIQUE で、
-- 同じパスが別のホストでは別の場所を指す。
-- campd は向こうのパスを実パスに直せないので、ここには**書かれたとおり**入れる
-- （正規化はするが symlink は解かない）。実パスへは起こすときに向こうで直し、
-- 許した行も同じく向こうで直してから比べる。
CREATE TABLE allowed_remote_cwd (
  id       INTEGER PRIMARY KEY,
  host     TEXT NOT NULL,          -- ssh_hosts.alias
  path     TEXT NOT NULL,
  note     TEXT,
  added_at TEXT NOT NULL,
  added_by TEXT NOT NULL,
  UNIQUE (host, path)
);
