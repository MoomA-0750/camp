-- Phase 4 / M50: ビュー定義を Camp の DB に持つ（2026-09-13）。
--
-- **本人の決定**: 定義は Camp の DB に持ち、Vault には書かない（「当分 Camp は Vault に
-- 書かない」を崩さない）。`.base` は読み取りのまま残し、しばらく併読して見比べる。
--
-- **単位は「台紙1枚 = 1行」。** ビューはその中の配列（YAML の `views:`）。
-- ビュー1つ1行にすると、`Payments.base` の `formulas` 3本と台紙の `filters` が
-- **14ビューに複製される**——1つ直すと残り13が静かにずれ、併読の突き合わせにも出ない
-- （設計レビューの指摘2、2026-09-13）。`.base` と同じ単位に揃えておけば複製が生まれない。
--
-- **定義は YAML の文字列のまま持つ。** 構造を列に割ると、将来 Vault のファイルへ
-- 書き出したくなったときに形が食い違う。読み手は1つ。
CREATE TABLE views (
  id            INTEGER PRIMARY KEY,
  vault_id      INTEGER NOT NULL REFERENCES vaults(id),
  -- base は viewID（`base/name`）の前半。**画面の URL とモデルが持ち回すハンドルが
  -- これで出来ているので、変換しても変えない**（設計の動かせない制約1）。
  base          TEXT NOT NULL,
  def           TEXT NOT NULL,
  -- origin は変換元の `.base` のパス（手で書いたものは NULL）。
  origin        TEXT,
  -- origin_sha256 は**変換したときの `.base` の中身**のハッシュ。
  -- 併読中に Obsidian 側で `.base` が変わると結果が食い違うが、それを
  -- 「変換の誤り」と誤診しないため（同レビューの指摘1）。
  origin_sha256 TEXT,
  converted_at  TEXT,
  updated_at    TEXT NOT NULL,
  UNIQUE(vault_id, base)
);

-- 書き換えの履歴。`.base` を DB へ移すと git の履歴が効かなくなるので、その代わり
-- （本人の決定6、2026-09-13）。**変換も1つの書き手として残す**（by = convert）ので、
-- 「いつ変換され、そのあと誰が直したか」が追える。
--
-- **履歴は定義の行と寿命を分ける**（実装後レビュー、2026-09-13。codex の指摘10）。
-- 初めは `view_id REFERENCES views(id) ON DELETE CASCADE` にしていて、定義の行を消すと
-- 履歴も全版消えた。「git 履歴の代わり」なら、誤って消したときの戻り先でなければならない。
-- だから台紙を (vault_id, base) で指し、外部キーを張らない。追記だけの表。
-- （この移行はまだ本番に入る前に直した。本番の campd は M50 より前に作ったもの。）
CREATE TABLE view_history (
  id       INTEGER PRIMARY KEY,
  vault_id INTEGER NOT NULL,
  base     TEXT NOT NULL,
  def      TEXT NOT NULL,
  at       TEXT NOT NULL,
  by       TEXT NOT NULL
);
CREATE INDEX ix_view_history ON view_history(vault_id, base, id);
