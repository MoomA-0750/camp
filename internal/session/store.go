package session

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/MoomA-0750/camp/internal/store"
)

// Record は runtime_sessions の1行。
type Record struct {
	ID          string `json:"id"`
	ClaudeID    string `json:"claude_id,omitempty"`
	Cwd         string `json:"cwd"`
	State       string `json:"state"`
	RequestedBy string `json:"requested_by"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`

	PID     int    `json:"pid,omitempty"`
	Started uint64 `json:"proc_started,omitempty"`
	BootID  string `json:"boot_id,omitempty"`
	Scope   string `json:"scope,omitempty"`

	ExitCode   *int   `json:"exit_code,omitempty"`
	ExitReason string `json:"exit_reason,omitempty"`
	EndedAt    string `json:"ended_at,omitempty"`
}

// Owner はこの行が指しているプロセス。
func (r Record) Owner() Owner {
	return Owner{PID: r.PID, Started: r.Started, BootID: r.BootID}
}

// Live は「まだ終わっていない」状態か。
func (r Record) Live() bool {
	return r.State != StateExited
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// insert は starting の行を1つ作る。**起こす前に書く。**
// 起こしてから書くと、起こした直後に campd が落ちたときに行が残らない。
func insert(db *store.DB, r Record) error {
	if !validState(r.State) {
		return fmt.Errorf("知らない状態: %s", r.State)
	}
	_, err := db.Exec(`
		insert into runtime_sessions(id, cwd, state, requested_by, created_at, updated_at)
		values(?,?,?,?,?,?)`,
		r.ID, r.Cwd, r.State, r.RequestedBy, r.CreatedAt, r.UpdatedAt)
	return err
}

// setState は状態だけを進める。
func setState(db *store.DB, id, state string) error {
	if !validState(state) {
		return fmt.Errorf("知らない状態: %s", state)
	}
	_, err := db.Exec(
		`update runtime_sessions set state=?, updated_at=? where id=?`,
		state, now(), id)
	return err
}

// setOwner は起きた子の身元を書く。
func setOwner(db *store.DB, id string, o Owner, scope string) error {
	_, err := db.Exec(`
		update runtime_sessions
		set state=?, pid=?, proc_started=?, boot_id=?, scope=?, updated_at=?
		where id=?`,
		StateIdle, o.PID, o.Started, o.BootID, scope, now(), id)
	return err
}

// setClaudeID は子が名乗った session_id を結びつける。
// **これがあって初めて、起こしたものと、あとで取り込まれる記録が繋がる。**
func setClaudeID(db *store.DB, id, claudeID string) error {
	_, err := db.Exec(
		`update runtime_sessions set claude_id=?, updated_at=? where id=? and claude_id is null`,
		claudeID, now(), id)
	return err
}

// finish は終わりを書く。**一度 exited にしたら上書きしない。**
func finish(db *store.DB, id string, code int, reason string) error {
	_, err := db.Exec(`
		update runtime_sessions
		set state=?, exit_code=?, exit_reason=?, ended_at=?, updated_at=?
		where id=? and state<>?`,
		StateExited, code, reason, now(), now(), id, StateExited)
	return err
}

// get は1行読む。
func get(db *store.DB, id string) (Record, error) {
	return scanOne(db.QueryRow(selectCols+` where id=?`, id))
}

const selectCols = `
	select id, coalesce(claude_id,''), cwd, state, requested_by, created_at, updated_at,
	       coalesce(pid,0), coalesce(proc_started,0), coalesce(boot_id,''),
	       coalesce(scope,''), exit_code, coalesce(exit_reason,''), coalesce(ended_at,'')
	from runtime_sessions`

type scanner interface {
	Scan(dest ...any) error
}

func scanOne(s scanner) (Record, error) {
	var r Record
	var code sql.NullInt64
	err := s.Scan(&r.ID, &r.ClaudeID, &r.Cwd, &r.State, &r.RequestedBy,
		&r.CreatedAt, &r.UpdatedAt, &r.PID, &r.Started, &r.BootID, &r.Scope,
		&code, &r.ExitReason, &r.EndedAt)
	if err != nil {
		return r, err
	}
	if code.Valid {
		v := int(code.Int64)
		r.ExitCode = &v
	}
	return r, nil
}

// listLive は終わっていない行を全部読む。再起動後の照合に使う。
func listLive(db *store.DB) ([]Record, error) {
	rows, err := db.Query(selectCols+` where state<>? order by created_at`, StateExited)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		r, err := scanOne(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// List は画面と CLI 用。新しい順に n 件。
//
// **1件も無いときは空の配列を返す。nil を返さない。**
// Go の nil スライスは JSON で `null` になる。受け取る側が「配列が来る」
// 前提で書いていると、そこで落ちる（2026-09-04、実ブラウザで実際に落ちた）。
// **「まだ無い」と「そもそも無い」を、受け手に区別させない。**
func List(db *store.DB, n int) ([]Record, error) {
	if n <= 0 || n > 500 {
		n = 50
	}
	rows, err := db.Query(selectCols+` order by created_at desc limit ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		r, err := scanOne(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
