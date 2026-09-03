package retain

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/MoomA-0750/camp/internal/audit"
	"github.com/MoomA-0750/camp/internal/ingest"
	"github.com/MoomA-0750/camp/internal/store"
)

// KindSecret は既知の値を伏せたとき。
const KindSecret = "message.secret"

// SecretPlan は伏せる前の見積り。
type SecretPlan struct {
	Messages  []int64 // raw_json に写っている
	Blobs     []string
	Blocks    int // message_blocks に写っている本数
	Bigrams   int // FTS の実体に写っている本数
	SourceHas map[string]int
}

// Total は写っている場所の合計。
func (p SecretPlan) Total() int {
	return len(p.Messages) + p.Blocks + p.Bigrams + len(p.Blobs)
}

// FindSecret は既知の値がどこに何件写っているかを数える。**値は返さない。**
func FindSecret(db *store.DB, secret []byte) (SecretPlan, error) {
	var p SecretPlan
	p.SourceHas = map[string]int{}

	rows, err := db.Query(`select id from messages where instr(raw_json, ?) > 0`, secret)
	if err != nil {
		return p, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return p, err
		}
		p.Messages = append(p.Messages, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return p, err
	}

	if err := db.QueryRow(`
		select count(*) from message_blocks where instr(coalesce(text,''), ?) > 0`,
		secret).Scan(&p.Blocks); err != nil {
		return p, err
	}
	if err := db.QueryRow(`
		select count(*) from message_blocks where instr(coalesce(bigrams,''), ?) > 0`,
		secret).Scan(&p.Bigrams); err != nil {
		return p, err
	}

	brows, err := db.Query(`select sha256, codec, content from blobs`)
	if err != nil {
		return p, err
	}
	defer brows.Close()
	for brows.Next() {
		var sha, codec string
		var content []byte
		if err := brows.Scan(&sha, &codec, &content); err != nil {
			return p, err
		}
		plain, err := decodeBlob(codec, content)
		if err != nil {
			continue
		}
		if bytes.Contains(plain, secret) {
			p.Blobs = append(p.Blobs, sha)
		}
	}
	return p, brows.Err()
}

// Secret は既知の値を、写っているすべての場所から伏せる。
//
// **同じ長さの `*` に差し替える。** 消して詰めると、`sensitive_findings` が
// `raw_json` のバイト位置で持っている所見が全部ずれる。長さを変えなければ
// 何も動かない。
//
// 行は消さない。`message_blocks` は伏せた `raw_json` から作り直す
// （手で直すと FTS の索引とずれる）。
func Secret(db *store.DB, secret []byte, op Op) (Outcome, error) {
	var out Outcome
	if op.Reason == "" || op.Actor == "" {
		return out, fmt.Errorf("reason と actor は必須")
	}
	if len(secret) < 4 {
		return out, fmt.Errorf("短すぎる値は伏せない。関係ない場所まで潰す")
	}

	plan, err := FindSecret(db, secret)
	if err != nil {
		return out, err
	}
	if plan.Total() == 0 {
		return out, nil
	}

	if _, err := audit.Append(db, audit.Entry{
		Actor:  op.Actor,
		Action: "retain.secret",
		Target: fmt.Sprintf("既知の値（%d バイト / sha256 %s）", len(secret),
			hex.EncodeToString(hashOf(secret))[:8]),
		Detail: fmt.Sprintf("messages %d / blocks %d / bigrams %d / blobs %d — %s",
			len(plan.Messages), plan.Blocks, plan.Bigrams, len(plan.Blobs), op.Reason),
		Outcome: audit.OK,
	}); err != nil {
		return out, err
	}

	mask := bytes.Repeat([]byte("*"), len(secret))
	now := time.Now().UTC().Format(time.RFC3339)

	for _, id := range plan.Messages {
		var raw []byte
		var srcID, off sql.NullInt64
		if err := db.QueryRow(`
			select raw_json, source_file_id, byte_offset from messages where id = ?`,
			id).Scan(&raw, &srcID, &off); err != nil {
			return out, err
		}
		n := bytes.Count(raw, secret)
		masked := bytes.ReplaceAll(raw, secret, mask)
		if !json.Valid(masked) {
			return out, fmt.Errorf("message %d を伏せると JSON が壊れる。手を付けない", id)
		}

		tx, err := db.Begin()
		if err != nil {
			return out, err
		}
		if _, err := tx.Exec(`update messages set raw_json = ? where id = ?`, masked, id); err != nil {
			tx.Rollback()
			return out, err
		}
		if err := insertTombstone(tx, tombstone{
			Kind: KindSecret, Ref: fmt.Sprint(id),
			SourceFileID: srcID, ByteOffset: off,
			Reason: op.Reason, Actor: op.Actor, At: now,
			Bytes: int64(len(secret) * n), Recoverable: false,
			Note: "既知の値を同じ長さの伏字にした。位置は動かしていない",
		}); err != nil {
			tx.Rollback()
			return out, err
		}
		if err := tx.Commit(); err != nil {
			return out, err
		}
		out.Messages++
		out.BytesRemoved += int64(len(secret) * n)
	}

	// 派生を作り直す。手で text と bigrams を直すと FTS の索引とずれる。
	blocks, err := ingest.RebuildBlocksFor(db, plan.Messages)
	if err != nil {
		return out, err
	}
	out.Blocks = blocks

	for _, sha := range plan.Blobs {
		n, err := maskBlob(db, sha, secret, mask, op, now)
		if err != nil {
			return out, err
		}
		out.Blobs++
		out.BytesRemoved += int64(n)
	}
	return out, nil
}

// maskBlob は blob の中身を展開して伏せ、同じ codec で入れ直す。
//
// **sha256 は変えない。** blobs.sha256 は notes と file_backups から
// 参照されていて、変えると繋がりが切れる。中身とハッシュが食い違うことに
// なるが、それは tombstone に残す（黙って食い違わせない）。
func maskBlob(db *store.DB, sha string, secret, mask []byte, op Op, now string) (int, error) {
	var codec string
	var content []byte
	if err := db.QueryRow(`select codec, content from blobs where sha256 = ?`, sha).
		Scan(&codec, &content); err != nil {
		return 0, err
	}
	plain, err := decodeBlob(codec, content)
	if err != nil {
		return 0, err
	}
	n := bytes.Count(plain, secret)
	if n == 0 {
		return 0, nil
	}
	masked := bytes.ReplaceAll(plain, secret, mask)

	stored, err := encodeBlob(codec, masked)
	if err != nil {
		return 0, err
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`update blobs set content = ? where sha256 = ?`, stored, sha); err != nil {
		return 0, err
	}
	if err := insertTombstone(tx, tombstone{
		Kind: KindSecret, Ref: sha,
		Reason: op.Reason, Actor: op.Actor, At: now,
		Bytes: int64(len(secret) * n), Recoverable: false,
		Note: "中身を伏せた。sha256 は参照元があるので変えていない（中身とは一致しない）",
	}); err != nil {
		return 0, err
	}
	return len(secret) * n, tx.Commit()
}

func decodeBlob(codec string, content []byte) ([]byte, error) {
	if codec != "gzip" {
		return content, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(content))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

func encodeBlob(codec string, plain []byte) ([]byte, error) {
	if codec != "gzip" {
		return plain, nil
	}
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write(plain); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func hashOf(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}
