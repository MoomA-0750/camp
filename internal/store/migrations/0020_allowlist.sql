-- Phase 3 / M29。**cwd の許可リスト。**
--
-- 実行専用OSユーザーもVM分離も採らないと決めた時点で、ここが Camp の API を
-- 通した誤用・暴走を止める唯一の場所になる。**VM侵入を止めるものではない**
-- ——シェルを取られた時点で守るものは無い。両者を混同しない。
--
-- 既定は deny。空なら何も起こせない。**「まだ設定していない」を
-- 「全部許す」と読ませない。**
CREATE TABLE allowed_cwd (
  id       INTEGER PRIMARY KEY,
  path     TEXT NOT NULL UNIQUE,   -- 実パス（symlink を解いたもの）
  note     TEXT,
  added_at TEXT NOT NULL,
  added_by TEXT NOT NULL
);
