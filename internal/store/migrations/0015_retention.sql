-- 何を残して何を消すかの規則。
--
-- **入れただけでは何も消えない。** enabled は既定で 0 で、`campd retain --apply`
-- を明示的に叩かない限り1バイトも減らない。消す判断はいつも人が行う。
--
-- applies_from があるので「遡及しない」がデータで表現できる。NULL は
-- 「全部が対象」で、日時が入っていればそれ以降に取り込んだぶんだけになる。
CREATE TABLE retention (
  id           INTEGER PRIMARY KEY,
  name         TEXT NOT NULL UNIQUE,

  -- 何を落とすか。internal/retain の Rule と対応する
  kind         TEXT NOT NULL,
  -- どこまでを対象にするか。'all' / 'session:<id>' / 'project:<id>'
  scope        TEXT NOT NULL DEFAULT 'all',

  -- これより古いものだけを対象にする。NULL は「古さを問わない」
  keep_days    INTEGER,
  -- この日時以降に取り込んだぶんだけを対象にする。NULL は「全部」
  applies_from TEXT,

  enabled      INTEGER NOT NULL DEFAULT 0,
  note         TEXT,
  created_at   TEXT NOT NULL
);

-- 最初の2つ。どちらも**索引に入っていない＝画面にも検索にも出ない**もので、
-- 落としても見え方が変わらない。元ファイルの有無にも依らない。
-- 2026-09-03 の実測に基づく（dev/active/phase2.5-baseline.md）。
INSERT INTO retention(name, kind, scope, keep_days, applies_from, enabled, note, created_at)
VALUES
  ('読めない thinking 署名', 'thinking-signature', 'all', NULL, NULL, 0,
   '本文は元から空。signature は Anthropic 側の鍵で誰も復号できない。実測 3,099 ブロック 6.4 MB',
   '2026-09-03'),
  ('二重に入っている画像', 'duplicate-image', 'all', NULL, NULL, 0,
   'message.content と toolUseResult に同じ base64 が2回。実測 105 枚 19.7 MB。message.content 側は残す',
   '2026-09-03');
