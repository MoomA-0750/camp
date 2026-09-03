// Package audit は「何が起きたか」を消せない形で残す。
//
// Phase 3 で Camp は任意のコードを動かす側になる。実行専用OSユーザーも
// VM分離も採らないと決めた以上、**後から辿れること**が最後の砦になる。
package audit

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/MoomA-0750/camp/internal/store"
)

// Entry は監査ログの1行。
type Entry struct {
	ID        int64  `json:"id"`
	At        string `json:"at"`
	Actor     string `json:"actor"`
	Action    string `json:"action"`
	Target    string `json:"target,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Outcome   string `json:"outcome"`
	Hash      string `json:"hash"`
}

// よく使う outcome。増やすのは構わないが、綴りを揃える。
const (
	OK      = "ok"
	Denied  = "denied"
	Error   = "error"
	Timeout = "timeout"
)

// Append は1行足す。**足すことしかできない。**
//
// 連鎖のハッシュは1つ前の行から作るので、同じトランザクションの中で
// 直前の行を読んでから書く。SetMaxOpenConns(1) なので書き手は1つしか居ない。
func Append(db *store.DB, e Entry) (int64, error) {
	if e.Actor == "" || e.Action == "" {
		return 0, errors.New("actor と action は必須")
	}
	if e.Outcome == "" {
		return 0, errors.New("outcome は必須。何が起きたか分からない記録は残さない")
	}
	if e.At == "" {
		e.At = time.Now().UTC().Format(time.RFC3339)
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var prev string
	err = tx.QueryRow(`select hash from audit order by id desc limit 1`).Scan(&prev)
	if err == sql.ErrNoRows {
		prev = ""
	} else if err != nil {
		return 0, err
	}

	e.Hash = chain(prev, e)
	r, err := tx.Exec(`
		insert into audit(at, actor, action, target, session_id, detail_json,
			outcome, prev_hash, hash)
		values(?,?,?,?,?,?,?,?,?)`,
		e.At, e.Actor, e.Action, nz(e.Target), nz(e.SessionID), nz(e.Detail),
		e.Outcome, prev, e.Hash)
	if err != nil {
		return 0, err
	}
	id, err := r.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// Opts は読み出しの絞り込み。
type Opts struct {
	Session string
	Action  string
	Limit   int
	Before  int64 // この id より小さいものだけ（次ページ）
}

// List は新しい順に読む。
func List(db *store.DB, o Opts) ([]Entry, error) {
	if o.Limit <= 0 || o.Limit > 500 {
		o.Limit = 100
	}
	rows, err := db.Query(`
		select id, at, actor, action, coalesce(target,''), coalesce(session_id,''),
		       coalesce(detail_json,''), outcome, hash
		  from audit
		 where (? = '' or session_id = ?)
		   and (? = '' or action = ?)
		   and (? = 0 or id < ?)
		 order by id desc limit ?`,
		o.Session, o.Session, o.Action, o.Action, o.Before, o.Before, o.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Action, &e.Target,
			&e.SessionID, &e.Detail, &e.Outcome, &e.Hash); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Verify は連鎖をたどって、抜けと書き換えを探す。
//
// トリガは DROP TRIGGER で外せる。このファイルを持っている者は sqlite3 で
// 何でもできる。**止められないので、気づけるようにする。**
func Verify(db *store.DB) (checked int, err error) {
	rows, err := db.Query(`
		select id, at, actor, action, coalesce(target,''), coalesce(session_id,''),
		       coalesce(detail_json,''), outcome, prev_hash, hash
		  from audit order by id`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	prev := ""
	first := true
	for rows.Next() {
		var e Entry
		var gotPrev string
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Action, &e.Target,
			&e.SessionID, &e.Detail, &e.Outcome, &gotPrev, &e.Hash); err != nil {
			return checked, err
		}
		checked++
		if first && e.Hash == "genesis" {
			// 最初の1行はマイグレーションが直接入れたもの。
			prev = e.Hash
			first = false
			continue
		}
		first = false
		if gotPrev != prev {
			return checked, fmt.Errorf("id=%d で連鎖が切れている。前の行が消されたか差し替えられた", e.ID)
		}
		if want := chain(prev, e); want != e.Hash {
			return checked, fmt.Errorf("id=%d の中身が書き換わっている", e.ID)
		}
		prev = e.Hash
	}
	return checked, rows.Err()
}

// chain は1行ぶんのハッシュを作る。区切りに \x00 を使うのは、
// 中身に区切り文字が入っても境目が動かないようにするため。
func chain(prev string, e Entry) string {
	h := sha256.New()
	for _, s := range []string{prev, e.At, e.Actor, e.Action, e.Target,
		e.SessionID, e.Detail, e.Outcome} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TriggersInPlace は追記専用を守るトリガが今もあるかを見る。
func TriggersInPlace(db *store.DB) ([]string, error) {
	rows, err := db.Query(`
		select name from sqlite_schema
		 where type='trigger' and tbl_name='audit' order by name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Missing は無くなっているトリガの名前を返す。
func Missing(have []string) []string {
	want := []string{"audit_no_delete", "audit_no_update"}
	var out []string
	for _, w := range want {
		found := false
		for _, h := range have {
			if h == w {
				found = true
			}
		}
		if !found {
			out = append(out, w)
		}
	}
	return out
}

func nz(s string) any {
	if s == "" {
		return nil
	}
	return s
}
