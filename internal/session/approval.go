package session

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/MoomA-0750/camp/internal/store"
)

// Approval は待っている（あるいは待っていた）承認1件。
type Approval struct {
	ID        int64  `json:"id"`
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`
	Tool      string `json:"tool,omitempty"`
	Detail    string `json:"detail,omitempty"`
	AskedAt   string `json:"asked_at"`
	ExpiresAt string `json:"expires_at"`

	AnsweredAt string `json:"answered_at,omitempty"`
	Behavior   string `json:"behavior,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// 答えた理由。**「答えなかった」も理由として書く。**
const (
	ByUser       = "user"
	ByTimeout    = "timeout"
	BySessionEnd = "session_ended"
)

// parkLimit は Camp が待つ長さ。
//
// `claude` 自身のパーク期限は5分。**その手前で自分から拒否する。**
// 子に先に諦められると、Camp の記録は「答えなかった」で終わり、
// 実際に何が起きたか（拒否として扱われたのか、ターンごと落ちたのか）が
// 分からなくなる。
const parkLimit = 4*time.Minute + 30*time.Second

// ask は承認要求を残す。同じ request_id が二度来ても増やさない。
func ask(db *store.DB, sessionID, reqID, tool, detail string, now time.Time) error {
	if len(detail) > maxApprovalDetail {
		detail = detail[:maxApprovalDetail] + "…（切り詰めた）"
	}
	_, err := db.Exec(`
		insert or ignore into approvals(session_id, request_id, tool, detail_json,
			asked_at, expires_at)
		values(?,?,?,?,?,?)`,
		sessionID, reqID, tool, detail,
		now.UTC().Format(time.RFC3339),
		now.Add(parkLimit).UTC().Format(time.RFC3339))
	return err
}

const maxApprovalDetail = 32 * 1024

// answer は答えを書く。**まだ答えていないものにだけ書ける。**
// 二度目は 0 行になり、呼び出し側が「もう答えてある」と分かる。
func answer(db *store.DB, sessionID, reqID, behavior, reason string, now time.Time) (bool, error) {
	r, err := db.Exec(`
		update approvals set answered_at=?, behavior=?, reason=?
		where session_id=? and request_id=? and answered_at is null`,
		now.UTC().Format(time.RFC3339), behavior, reason, sessionID, reqID)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n > 0, err
}

// reopen は答えを取り消して待ちに戻す。**子へ届かなかったときだけ。**
func reopen(db *store.DB, sessionID, reqID string) error {
	_, err := db.Exec(`
		update approvals set answered_at=null, behavior=null, reason=null
		where session_id=? and request_id=?`, sessionID, reqID)
	return err
}

// openApprovals はまだ答えていないものを返す。session が空なら全部。
//
// **DB から読む。** campd のメモリから読むと、入れ替えた瞬間に
// 「誰が何を訊かれていたか」が消える。
func openApprovals(db *store.DB, sessionID string) ([]Approval, error) {
	q := `select id, session_id, request_id, coalesce(tool,''), coalesce(detail_json,''),
	             asked_at, expires_at
	      from approvals where answered_at is null`
	args := []any{}
	if sessionID != "" {
		q += ` and session_id=?`
		args = append(args, sessionID)
	}
	q += ` order by asked_at`
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Approval{} // **nil を返さない**（JSON で null になる）
	for rows.Next() {
		var a Approval
		if err := rows.Scan(&a.ID, &a.SessionID, &a.RequestID, &a.Tool, &a.Detail,
			&a.AskedAt, &a.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// expired は期限を過ぎた未回答を返す。
func expired(db *store.DB, now time.Time) ([]Approval, error) {
	rows, err := db.Query(`
		select id, session_id, request_id, coalesce(tool,''), coalesce(detail_json,''),
		       asked_at, expires_at
		from approvals where answered_at is null and expires_at <= ?`,
		now.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		var a Approval
		if err := rows.Scan(&a.ID, &a.SessionID, &a.RequestID, &a.Tool, &a.Detail,
			&a.AskedAt, &a.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// closeOpen はセッションが終わったときに、宙に浮いた承認を閉じる。
// **待っていたものを「待っている」まま残さない。**
func closeOpen(db *store.DB, sessionID string, now time.Time) (int, error) {
	r, err := db.Exec(`
		update approvals set answered_at=?, behavior='deny', reason=?
		where session_id=? and answered_at is null`,
		now.UTC().Format(time.RFC3339), BySessionEnd, sessionID)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return int(n), nil
}

// ApprovalHistory はそのセッションの承認を全部、古い順に返す。画面用。
func ApprovalHistory(db *store.DB, sessionID string, limit int) ([]Approval, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := db.Query(`
		select id, session_id, request_id, coalesce(tool,''), coalesce(detail_json,''),
		       asked_at, expires_at, coalesce(answered_at,''), coalesce(behavior,''),
		       coalesce(reason,'')
		from approvals where session_id=? order by asked_at limit ?`,
		sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Approval{} // **nil を返さない**（JSON で null になる）
	for rows.Next() {
		var a Approval
		var answered sql.NullString
		if err := rows.Scan(&a.ID, &a.SessionID, &a.RequestID, &a.Tool, &a.Detail,
			&a.AskedAt, &a.ExpiresAt, &answered, &a.Behavior, &a.Reason); err != nil {
			return nil, err
		}
		a.AnsweredAt = answered.String
		out = append(out, a)
	}
	return out, rows.Err()
}

func approvalError(what string, err error) error {
	return fmt.Errorf("承認の%s: %w", what, err)
}
