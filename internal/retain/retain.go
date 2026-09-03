// Package retain は「消したことを消さない」削除を実装する。
//
// Camp の存在理由は元が消えても読めることなので、削除は例外的な操作になる。
// 例外を黙って通すと、あとから「ここに何かあったのか、元から無かったのか」が
// 区別できなくなる。だからこのパッケージを通した削除は必ず tombstone を1行残す。
package retain

import (
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/MoomA-0750/camp/internal/store"
)

// Kind は tombstone.kind に入る値。
const (
	KindRawJSON = "message.raw_json"
	KindBlocks  = "message_blocks"
	KindBlob    = "blob"
	// KindTrim は行を残したまま、読めない部分だけを落としたとき。
	KindTrim = "message.trim"
)

// Op は削除1件の指示。
type Op struct {
	Reason string // なぜ消すか。人が読む文。空は許さない
	Actor  string // 誰が消すか。'campd retain' / 'manual' など
	Note   string // 任意の補足
}

// Outcome は削除の結果。
type Outcome struct {
	Messages      int   // raw_json を空にしたメッセージ
	Blocks        int   // 消した message_blocks の行
	Blobs         int   // 消した blob
	BytesRemoved  int64 // 実際に落としたバイト数
	Unrecoverable int   // 元ファイルが無く、作り直せない削除だった件数
}

// Message は1メッセージの raw_json と派生行を落とし、tombstone を残す。
//
// **行そのものは消さない。** UNIQUE(source_file_id, byte_offset) を生かすためで、
// 行ごと消すと元ファイルが手元にある限り次の取り込みで戻ってくる。
//
// FTS は external-content なので、message_blocks を消すだけでは索引に残る。
// 'delete' コマンドに元の値を渡して消す必要がある（値がずれると索引が壊れる）。
func Message(db *store.DB, id int64, op Op) (Outcome, error) {
	var out Outcome
	if op.Reason == "" || op.Actor == "" {
		return out, fmt.Errorf("reason と actor は必須")
	}

	var srcID sql.NullInt64
	var off sql.NullInt64
	var size int64
	err := db.QueryRow(`
		select source_file_id, byte_offset, length(raw_json) from messages where id = ?`,
		id).Scan(&srcID, &off, &size)
	if err == sql.ErrNoRows {
		return out, fmt.Errorf("message %d が無い", id)
	}
	if err != nil {
		return out, err
	}

	recoverable, err := sourceFileExists(db, srcID)
	if err != nil {
		return out, err
	}

	tx, err := db.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()

	blocks, blockBytes, err := dropBlocks(tx, id)
	if err != nil {
		return out, err
	}

	if _, err := tx.Exec(`update messages set raw_json = x'' where id = ?`, id); err != nil {
		return out, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	total := size + blockBytes
	if err := insertTombstone(tx, tombstone{
		Kind: KindRawJSON, Ref: fmt.Sprint(id),
		SourceFileID: srcID, ByteOffset: off,
		Reason: op.Reason, Actor: op.Actor, At: now,
		Bytes: total, Recoverable: recoverable, Note: op.Note,
	}); err != nil {
		return out, err
	}
	if err := tx.Commit(); err != nil {
		return out, err
	}

	out.Messages = 1
	out.Blocks = blocks
	out.BytesRemoved = total
	if !recoverable {
		out.Unrecoverable = 1
	}
	return out, nil
}

// Blob は blobs の1件を消して tombstone を残す。
//
// blobs は sha256 が主キーで、notes と file_backups から参照される。
// 参照側の行は残す（「あったが消した」と言えるようにするため）。
func Blob(db *store.DB, sha string, op Op) (Outcome, error) {
	var out Outcome
	if op.Reason == "" || op.Actor == "" {
		return out, fmt.Errorf("reason と actor は必須")
	}

	var size int64
	err := db.QueryRow(`select length(content) from blobs where sha256 = ?`, sha).Scan(&size)
	if err == sql.ErrNoRows {
		return out, fmt.Errorf("blob %s が無い", sha)
	}
	if err != nil {
		return out, err
	}

	tx, err := db.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()

	// 中身だけ落とす。行を消すと notes / file_backups の外部キーが折れる。
	if _, err := tx.Exec(`update blobs set content = x'', codec = 'redacted' where sha256 = ?`, sha); err != nil {
		return out, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if err := insertTombstone(tx, tombstone{
		Kind: KindBlob, Ref: sha,
		Reason: op.Reason, Actor: op.Actor, At: now,
		Bytes: size, Recoverable: false, Note: op.Note,
	}); err != nil {
		return out, err
	}
	if err := tx.Commit(); err != nil {
		return out, err
	}
	out.Blobs = 1
	out.BytesRemoved = size
	out.Unrecoverable = 1
	return out, nil
}

// dropBlocks は1メッセージの message_blocks と FTS 索引を対で消す。
func dropBlocks(tx *sql.Tx, messageID int64) (n int, bytes int64, err error) {
	rows, err := tx.Query(`
		select id, coalesce(bigrams,''), length(coalesce(text,''))
		  from message_blocks where message_id = ?`, messageID)
	if err != nil {
		return 0, 0, err
	}
	type blk struct {
		id      int64
		bigrams string
	}
	var blks []blk
	for rows.Next() {
		var b blk
		var sz int64
		if err := rows.Scan(&b.id, &b.bigrams, &sz); err != nil {
			rows.Close()
			return 0, 0, err
		}
		bytes += sz
		blks = append(blks, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	for _, b := range blks {
		// external-content の削除は 'delete' コマンドに元の値を渡す。
		// ずれた値を渡すと索引が壊れ、doctor の integrity-check が落ちる。
		if _, err := tx.Exec(
			`insert into messages_fts(messages_fts, rowid, bigrams) values('delete', ?, ?)`,
			b.id, b.bigrams); err != nil {
			return 0, 0, err
		}
		if _, err := tx.Exec(`delete from message_blocks where id = ?`, b.id); err != nil {
			return 0, 0, err
		}
	}
	return len(blks), bytes, nil
}

// sourceFileExists は元ファイルが今もディスクにあるかを見る。
//
// source_files.missing_at は前回の走査時点の話でしかない。消す判断は
// 「いま戻せるか」に依るので、列ではなく実体を見る。
func sourceFileExists(db *store.DB, id sql.NullInt64) (bool, error) {
	if !id.Valid {
		return false, nil
	}
	var path string
	err := db.QueryRow(`select path from source_files where id = ?`, id.Int64).Scan(&path)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, statErr := os.Stat(path)
	return statErr == nil, nil
}

type tombstone struct {
	Kind         string
	Ref          string
	SourceFileID sql.NullInt64
	ByteOffset   sql.NullInt64
	Reason       string
	Actor        string
	At           string
	Bytes        int64
	Recoverable  bool
	Note         string
}

func insertTombstone(tx *sql.Tx, t tombstone) error {
	rec := 0
	if t.Recoverable {
		rec = 1
	}
	_, err := tx.Exec(`
		insert into tombstones(kind, ref, source_file_id, byte_offset,
			reason, actor, redacted_at, bytes_removed, recoverable, note)
		values(?,?,?,?,?,?,?,?,?,?)`,
		t.Kind, t.Ref, t.SourceFileID, t.ByteOffset,
		t.Reason, t.Actor, t.At, t.Bytes, rec, nullStr(t.Note))
	return err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
