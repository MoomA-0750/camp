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

// parkLimit は「期限を入れるなら」の目安の長さ。**既定では使わない**（Supervisor.ParkAfter は 0）。
//
// Phase 3 では「`claude` 自身のパーク期限は5分だから、その手前で自分から拒否する」としていた。
// **それは誤りだった**（2026-09-12 実測、`dev/scripts/park_probe.py`）: Camp と同じ起こし方
// （`-p`・stream-json・`--permission-prompt-tool stdio`）で承認を10分放置しても、claude は
// フレームを1つも出さずに待ち続けた。`CLAUDE_CODE_USER_DIALOG_TIMEOUT_MS`（既定5分）は
// **遠くの相手へ回した**ダイアログの期限で、local-only の承認には効かない。対話の CLI にも
// 承認の期限は無い。**CLI と同じく、答えるまで待つ**（D-030、本人の決定 2026-09-12）。
const parkLimit = 4*time.Minute + 30*time.Second

// noDeadline は「期限を見ない」ときに書く値。列は NOT NULL なので、決して来ない時刻を入れる
// （expired の `expires_at <= now` に当たらない）。**読むときは空にして**、画面に嘘の期限を出さない。
const noDeadline = "9999-12-31T23:59:59Z"

// shownDeadline は「期限を見ない」印を空にする。
func shownDeadline(s string) string {
	if s == noDeadline {
		return ""
	}
	return s
}

// ask は承認要求を残す。同じ request_id が二度来ても増やさない。
func ask(db *store.DB, sessionID, reqID, tool, detail string, now time.Time, park time.Duration) error {
	if len(detail) > maxApprovalDetail {
		detail = detail[:maxApprovalDetail] + "…（切り詰めた）"
	}
	// park が 0 なら期限を見ない（既定）。
	exp := noDeadline
	if park > 0 {
		exp = now.Add(park).UTC().Format(time.RFC3339)
	}
	_, err := db.Exec(`
		insert or ignore into approvals(session_id, request_id, tool, detail_json,
			asked_at, expires_at)
		values(?,?,?,?,?,?)`,
		sessionID, reqID, tool, detail,
		now.UTC().Format(time.RFC3339), exp)
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

// ByWithdrawn は、実行面が取り下げた（その承認はもう子に届かない）。
// 答えていたなら、その答えは**子に届いていない**。
const ByWithdrawn = "withdrawn"

// withdraw は承認を「取り下げられた」で閉じる。答え済みの行も上書きする——
// 「本人が許した」と残っていても、実際には子へ届いていないので。
func withdraw(db *store.DB, sessionID, reqID string, now time.Time) (bool, error) {
	r, err := db.Exec(`
		update approvals set answered_at=coalesce(answered_at, ?), behavior=coalesce(behavior, 'deny'),
		       reason=?
		where session_id=? and request_id=? and coalesce(reason,'') <> ?`,
		now.UTC().Format(time.RFC3339), ByWithdrawn, sessionID, reqID, ByWithdrawn)
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
		a.ExpiresAt = shownDeadline(a.ExpiresAt)
		out = append(out, a)
	}
	return out, rows.Err()
}

// expired は期限を過ぎた未回答を返す。**期限を見ない設定なら呼ばない**（Supervisor.Tick）。
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
