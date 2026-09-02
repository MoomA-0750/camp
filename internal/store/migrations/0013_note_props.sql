-- ノートのfrontmatterを列として持つ。1プロパティ1行。
--
-- 列を固定したテーブルにしないのは、実測でVault全体に136種のプロパティが
-- あり、Health だけで102種あるため。しかも増え続ける（Apple Health が
-- 指標を足すたびに増える）。列を固定すると、そのたびにマイグレーションが要る。
--
-- 値は text に寄せて、数値として読めるものだけ num にも入れる。
-- 並べ替えと Sum は num を見る（"10" と "9" を文字列で比べない）。
CREATE TABLE note_props (
  note_id  INTEGER NOT NULL REFERENCES notes(id) ON DELETE CASCADE,
  key      TEXT NOT NULL,
  seq      INTEGER NOT NULL DEFAULT 0,  -- リスト値の並び。スカラは0
  text     TEXT,
  num      REAL,
  PRIMARY KEY(note_id, key, seq)
);
CREATE INDEX ix_props_key  ON note_props(key, num);
CREATE INDEX ix_props_text ON note_props(key, text);
