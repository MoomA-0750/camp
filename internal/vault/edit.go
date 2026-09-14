package vault

import (
	"database/sql"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/MoomA-0750/camp/internal/ingest"
	"github.com/MoomA-0750/camp/internal/notes"
	"github.com/MoomA-0750/camp/internal/store"
)

// 編集面のための小さな読み書き（Phase 5 / M53）。**Vault のファイルには書かない**——書くのは実行面。

// VaultRoot は Vault の場所と名前。
func VaultRoot(db *store.DB, id int64) (root, name, host string, err error) {
	err = db.QueryRow(`select v.root, v.name, h.name from vaults v join hosts h on h.id = v.host_id where v.id = ?`,
		id).Scan(&root, &name, &host)
	if err == sql.ErrNoRows {
		err = fmt.Errorf("Vault %d が無い", id)
	}
	return
}

// VaultByRoot は場所から Vault を引く。無ければ 0。
func VaultByRoot(db *store.DB, root string) (int64, error) {
	var id int64
	err := db.QueryRow(`select id from vaults where root = ? order by id limit 1`, root).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// PutBlob は中身を blobs に入れ、ハッシュを返す。
func PutBlob(db *store.DB, body []byte) (string, error) {
	hash := notes.Sum(body)
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := putBlob(tx, hash, body, time.Now().UTC().Format(timeFmt)); err != nil {
		return "", err
	}
	return hash, tx.Commit()
}

// BlobBody は blobs から中身を返す。無ければ nil, nil。
func BlobBody(db *store.DB, hash string) ([]byte, error) {
	var codec string
	var content []byte
	err := db.QueryRow(`select codec, content from blobs where sha256 = ?`, hash).Scan(&codec, &content)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ingest.UnpackBlob(codec, content)
}

// TouchNote は書いた1本を索引に反映する（中身・大きさ・更新時刻）。リンクとプロパティは次の
// 全体の索引で直る（1本だけ直すと、ほかのノートからの解決がずれるため）。
func TouchNote(db *store.DB, vaultID int64, rel string, body []byte, mtime time.Time) error {
	hash, err := PutBlob(db, body)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(timeFmt)
	ext := path.Ext(rel)
	title := strings.TrimSuffix(path.Base(rel), ext)
	_, err = db.Exec(`
		insert into notes(vault_id, path, title, kind, ext, mtime, size, sha256, content_hash, first_seen_at, missing_at)
		values(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, null)
		on conflict(vault_id, path) do update set
		  mtime = excluded.mtime, size = excluded.size,
		  sha256 = excluded.sha256, content_hash = excluded.content_hash, missing_at = null`,
		vaultID, rel, title, KindMarkdown, ext, mtime.UTC().Format(timeFmt), len(body), hash, hash, now)
	return err
}

// MarkMissing は `.trash/` へ移したノートに消えた印を付ける（M55）。次の全体の索引でも同じになる。
func MarkMissing(db *store.DB, vaultID int64, rel string) error {
	_, err := db.Exec(`update notes set missing_at = ? where vault_id = ? and path = ? and missing_at is null`,
		time.Now().UTC().Format(timeFmt), vaultID, rel)
	return err
}

// NoteIDByPath は Vault の中のパスからノートの id を引く（消えたものは除く）。無ければ 0。
func NoteIDByPath(db *store.DB, vaultID int64, rel string) (int64, error) {
	var id int64
	err := db.QueryRow(`select id from notes where vault_id = ? and path = ? and missing_at is null`, vaultID, rel).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}
