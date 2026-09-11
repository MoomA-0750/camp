package session

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/MoomA-0750/camp/internal/store"
)

// Record は runtime_sessions の1行。
type Record struct {
	ID string `json:"id"`
	// Agent は起こしたエージェント（claude / codex）。2026-09-11 より前の行は claude。
	Agent string `json:"agent"`
	// Perm は確認の度合い（cli / ask / edits / auto / full）。2026-09-12 より前の行は cli、
	// ただし Phase 3.6 の Codex の行は legacy（専用の置き場で起こしていた。表示だけ）。
	Perm string `json:"perm"`
	// ClaudeID はエージェント自身のセッション id。Claude なら session_id、
	// Codex ならスレッド id（列名は変えない。名前を変える移行のほうが危ない）。
	ClaudeID string `json:"claude_id,omitempty"`
	// AgentSessionID は ClaudeID と同じ値。**API の名前をエージェントに依らないものにした**
	// （claude_id は古い画面のために当面残す）。台帳の列ではない。
	AgentSessionID string `json:"agent_session_id,omitempty"`
	// AgentLabel は画面に出すエージェントの名前（駆動器の説明から）。台帳の列ではない。
	AgentLabel  string `json:"agent_label,omitempty"`
	Cwd         string `json:"cwd"`
	State       string `json:"state"`
	RequestedBy string `json:"requested_by"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`

	PID     int    `json:"pid,omitempty"`
	Started uint64 `json:"proc_started,omitempty"`
	BootID  string `json:"boot_id,omitempty"`
	Scope   string `json:"scope,omitempty"`

	// Host は ssh の Host 名。空ならこのマシン（2026-09-11 から）。
	// そのとき上の pid / 起動時刻は**手元の ssh** を指す。
	Host string `json:"host,omitempty"`
	// 向こうで起きた子。**実行面の報告で、campd は確かめられない。**
	RemotePID     int    `json:"remote_pid,omitempty"`
	RemoteStarted uint64 `json:"remote_started,omitempty"`
	RemoteBootID  string `json:"remote_boot_id,omitempty"`
	RemoteScope   string `json:"remote_scope,omitempty"`

	ExitCode   *int   `json:"exit_code,omitempty"`
	ExitReason string `json:"exit_reason,omitempty"`
	EndedAt    string `json:"ended_at,omitempty"`

	// どう終わったか（end.go）。2026-09-11 より前に終わった行は空のまま。
	EndCause string `json:"end_cause,omitempty"`
	// 終わる直前に何をしていたか（starting / idle / running）。
	EndState string `json:"end_state,omitempty"`

	// 承認の内訳。**終わったあとで「待たせたまま終わった」を選り分けるため。**
	Asked       int `json:"approvals_asked"`
	LeftWaiting int `json:"approvals_left_waiting"` // 答えないうちにセッションが終わった
	TimedOut    int `json:"approvals_timed_out"`    // 答えないうちに期限が切れた
}

// Owner はこの行が指しているプロセス。
func (r Record) Owner() Owner {
	return Owner{PID: r.PID, Started: r.Started, BootID: r.BootID}
}

// remoteOwner は向こうの子の身元。このマシンの行なら nil。
func (r Record) remoteOwner() *RemoteOwner {
	if r.Host == "" {
		return nil
	}
	return &RemoteOwner{Host: r.Host, PID: r.RemotePID, Started: r.RemoteStarted,
		BootID: r.RemoteBootID, Scope: r.RemoteScope, Cwd: r.Cwd, Session: r.ID}
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
	if !validAgent(agentOr(r.Agent)) {
		return fmt.Errorf("知らないエージェント: %s", r.Agent)
	}
	// **頼める度合いだけを書く**（legacy は移行で入る表示だけの値）。
	if !validPerm(permOr(r.Perm)) {
		return fmt.Errorf("知らない確認の度合い: %s", r.Perm)
	}
	_, err := db.Exec(`
		insert into runtime_sessions(id, cwd, state, requested_by, created_at, updated_at, host, agent, perm)
		values(?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Cwd, r.State, r.RequestedBy, r.CreatedAt, r.UpdatedAt, nzs(r.Host), agentOr(r.Agent),
		permOr(r.Perm))
	return err
}

// setRemote は向こうの子の身元を書く。cwd は向こうで実際に降りた場所に直す。
func setRemote(db *store.DB, id string, o RemoteOwner) error {
	_, err := db.Exec(`
		update runtime_sessions
		set cwd=?, remote_pid=?, remote_started=?, remote_boot_id=?, remote_scope=?, updated_at=?
		where id=?`,
		o.Cwd, o.PID, o.Started, nzs(o.BootID), nzs(o.Scope), now(), id)
	return err
}

// setState は状態だけを進める。
//
// **stopping と orphaned へは入れない。** そこへは markStopping / markOrphaned で
// 入る——手前の状態を控えないと、終わったときに「何をしていたか」が消える。
func setState(db *store.DB, id, state string) error {
	if !validState(state) {
		return fmt.Errorf("知らない状態: %s", state)
	}
	if state == StateStopping || state == StateOrphaned {
		return fmt.Errorf("%s へは setState で入れない", state)
	}
	_, err := db.Exec(
		`update runtime_sessions set state=?, updated_at=? where id=?`,
		state, now(), id)
	return err
}

// markStopping は止めに入ったことを書く。cause は「このまま終われば、これが理由」。
func markStopping(db *store.DB, id, cause string) error {
	_, err := db.Exec(`
		update runtime_sessions
		set prev_state = case when state in (?, ?) then prev_state else state end,
		    state = ?, end_cause = ?, updated_at = ?
		where id = ? and state <> ?`,
		StateStopping, StateOrphaned, StateStopping, cause, now(), id, StateExited)
	return err
}

// markOrphaned は見張りが外れたことを書く。
//
// 止めろと言ってあったなら、その理由を残す（止めろと言ったものが終わっただけ）。
func markOrphaned(db *store.DB, id, cause string) error {
	_, err := db.Exec(`
		update runtime_sessions
		set prev_state = case when state in (?, ?) then prev_state else state end,
		    state = ?, end_cause = coalesce(end_cause, ?), updated_at = ?
		where id = ? and state <> ?`,
		StateStopping, StateOrphaned, StateOrphaned, cause, now(), id, StateExited)
	return err
}

// clearOrphanCause は引き取り直したときに、見張りが外れていたという控えを消す。
// 止めろと言ってあった控えは残す。
func clearOrphanCause(db *store.DB, id string) error {
	_, err := db.Exec(`
		update runtime_sessions set end_cause = null
		where id = ? and end_cause in (?, ?)`, id, EndAgentLost, EndUnseen)
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
//
// cause は「他に理由が控えていなければ、これ」。止めろと言ったあとで子が
// 終わったなら、控えてある理由（本人が止めた・放置で閉じた）のほうが正しい。
// override なら控えを無視する（止まらなかった・起こせなかった・始末した）。
//
// end_state は終わる直前に何をしていたか。stopping / orphaned はその手前を採る。
// **SQLite の UPDATE は右辺を更新前の値で読む**ので、同じ文の中で state を
// 見てから exited にできる。
func finish(db *store.DB, id string, code int, reason, cause string, override bool) error {
	_, err := db.Exec(`
		update runtime_sessions
		set end_state = case when state in (?, ?) then coalesce(prev_state, state) else state end,
		    end_cause = case when ? then ? else coalesce(end_cause, ?) end,
		    state=?, exit_code=?, exit_reason=?, ended_at=?, updated_at=?
		where id=? and state<>?`,
		StateStopping, StateOrphaned,
		override, cause, cause,
		StateExited, code, reason, now(), now(), id, StateExited)
	return err
}

// get は1行読む。
func get(db *store.DB, id string) (Record, error) {
	return scanOne(db.QueryRow(selectCols+` where id=?`, id))
}

// ErrNotFound はその id の行が無いとき。
var ErrNotFound = fmt.Errorf("そのセッションは無い")

// Get は1行読む。画面が1本を開くときに使う（一覧に載っていない古いものも開ける）。
func Get(db *store.DB, id string) (Record, error) {
	r, err := get(db, id)
	if err == sql.ErrNoRows {
		return r, ErrNotFound
	}
	return r, err
}

// 承認の数えは approvals の reason から採る。**別の列に写さない**——
// 写すと、承認が閉じられた時刻と数えた時刻がずれたときに食い違う。
var selectCols = `
	select id, agent, perm, coalesce(claude_id,''), cwd, state, requested_by, created_at, updated_at,
	       coalesce(pid,0), coalesce(proc_started,0), coalesce(boot_id,''),
	       coalesce(scope,''),
	       coalesce(host,''), coalesce(remote_pid,0), coalesce(remote_started,0),
	       coalesce(remote_boot_id,''), coalesce(remote_scope,''),
	       exit_code, coalesce(exit_reason,''), coalesce(ended_at,''),
	       coalesce(end_cause,''), coalesce(end_state,''),
	       (select count(*) from approvals a where a.session_id = runtime_sessions.id),
	       (select count(*) from approvals a where a.session_id = runtime_sessions.id
	          and a.reason = '` + BySessionEnd + `'),
	       (select count(*) from approvals a where a.session_id = runtime_sessions.id
	          and a.reason = '` + ByTimeout + `')
	from runtime_sessions`

type scanner interface {
	Scan(dest ...any) error
}

func scanOne(s scanner) (Record, error) {
	var r Record
	var code sql.NullInt64
	err := s.Scan(&r.ID, &r.Agent, &r.Perm, &r.ClaudeID, &r.Cwd, &r.State, &r.RequestedBy,
		&r.CreatedAt, &r.UpdatedAt, &r.PID, &r.Started, &r.BootID, &r.Scope,
		&r.Host, &r.RemotePID, &r.RemoteStarted, &r.RemoteBootID, &r.RemoteScope,
		&code, &r.ExitReason, &r.EndedAt, &r.EndCause, &r.EndState,
		&r.Asked, &r.LeftWaiting, &r.TimedOut)
	if err != nil {
		return r, err
	}
	if code.Valid {
		v := int(code.Int64)
		r.ExitCode = &v
	}
	r.AgentSessionID, r.AgentLabel = r.ClaudeID, LabelOf(r.Agent)
	return r, nil
}

func scanAll(rows *sql.Rows) ([]Record, error) {
	defer rows.Close()
	out := []Record{} // **nil を返さない**（JSON で null になる）
	for rows.Next() {
		r, err := scanOne(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// listLive は終わっていない行を全部読む。再起動後の照合に使う。
func listLive(db *store.DB) ([]Record, error) {
	rows, err := db.Query(selectCols+` where state<>? order by created_at`, StateExited)
	if err != nil {
		return nil, err
	}
	return scanAll(rows)
}

// ListLive は終わっていないものを新しい順に全部。画面の「走っているもの」。
//
// 同時に走らせられるのは高々数本なので上限は置かない。孤児も入る——
// **見張りが外れているだけで、まだ終わっていない。**
func ListLive(db *store.DB) ([]Record, error) {
	rows, err := db.Query(selectCols+` where state<>? order by created_at desc`, StateExited)
	if err != nil {
		return nil, err
	}
	return scanAll(rows)
}

// List は CLI 用。状態を問わず新しい順に n 件。
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
	return scanAll(rows)
}
