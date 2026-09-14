-- Phase 5 / M53: Camp が Vault に書いたノートの控えと、commit を待つ行（2026-09-13）。
--
-- **書くのは実行面、覚えるのは campd。** 保存（ファイルへ書く）と commit・push は別の契機
-- （本人の決定1・2: 自動で保存し、そのファイルが 1 分書かれていなければまとめて commit）。
-- その間に campd や実行面が落ちても忘れないよう、待ち行を DB に持つ。
--
-- **行は「書いた中身のハッシュ」を持つ。** commit の直前にディスクと照らし、違えば commit しない
-- （別の書き手が上書きした。Fable の設計レビュー 3）。DB を過去の版から戻しても、
-- ハッシュが合わない行は commit されない（同 9）。中身そのものは blobs にある。
--
-- state:
--   planned     書く前に入れた（書けたか分からないまま落ちたら、起動時にディスクと照らす）
--   pending     書けた。commit を待つ
--   superseded  同じパスに新しい保存が来た（commit するのは最後の行だけ）
--   committed   commit に入った（または、もう HEAD と同じ中身だった）
--   overwritten commit の前に別の書き手が中身を変えた・消した。commit していない
--   dropped     書けなかった・ぶつかった
CREATE TABLE note_writes (
  id          INTEGER PRIMARY KEY,
  vault_id    INTEGER NOT NULL REFERENCES vaults(id),
  path        TEXT NOT NULL,
  sha256      TEXT NOT NULL REFERENCES blobs(sha256),
  state       TEXT NOT NULL,
  -- base_sha256 は書き始めた中身（新しいノートなら NULL）。合わせるときの共通の祖先。
  base_sha256 TEXT,
  commit_sha  TEXT,
  -- disk_sha256 は overwritten のときのディスクの中身（消えていたら NULL）。
  disk_sha256 TEXT,
  detail      TEXT,
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

CREATE INDEX ix_note_writes_state ON note_writes(vault_id, state, path);
