-- Vault は「ホスト上の1つのルートディレクトリ」。複数Vault・複数ホストを想定する。
CREATE TABLE vaults (
  id         INTEGER PRIMARY KEY,
  host_id    INTEGER NOT NULL REFERENCES hosts(id),
  name       TEXT NOT NULL,
  root       TEXT NOT NULL,               -- 絶対パス。大文字小文字を区別する
  scanned_at TEXT,
  UNIQUE(host_id, root)
);

-- notes は 0001 で作ってあるが、索引に必要な列が足りない。
--   kind    : 本文を読むのは markdown だけ。.base や .pdf もリンク先になるので行は持つ
--   sha256  : 中身は blobs に寄せる（file_backups と同じ置き場）。ノートが消えても中身は残る
--   missing : 実体が消えたことを残す。Campの存在理由そのもの
ALTER TABLE notes ADD COLUMN kind          TEXT;
ALTER TABLE notes ADD COLUMN ext           TEXT;
ALTER TABLE notes ADD COLUMN sha256        TEXT REFERENCES blobs(sha256);
ALTER TABLE notes ADD COLUMN first_seen_at TEXT;
ALTER TABLE notes ADD COLUMN missing_at    TEXT;

CREATE INDEX ix_notes_base    ON notes(vault_id, title);
CREATE INDEX ix_notes_missing ON notes(missing_at);

-- note_links も 0001 のままでは足りない。
--   ambiguous  : 候補が複数あった。黙って1つ選ばない（曖昧26件・実測）
--   candidates : そのときの候補一覧（改行区切り）。後から人が判断できるように
--   alias/frag : [[target|alias]] と [[target#heading]] を分解して残す
--   embed      : ![[..]] は埋め込み。表示の意味が違う
ALTER TABLE note_links ADD COLUMN alias      TEXT;
ALTER TABLE note_links ADD COLUMN frag       TEXT;
ALTER TABLE note_links ADD COLUMN embed      INTEGER NOT NULL DEFAULT 0;
ALTER TABLE note_links ADD COLUMN ambiguous  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE note_links ADD COLUMN candidates TEXT;
ALTER TABLE note_links ADD COLUMN line       INTEGER;
