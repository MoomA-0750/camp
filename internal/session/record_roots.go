package session

import (
	"database/sql"
	"time"

	"github.com/MoomA-0750/camp/internal/store"
)

// 向こうのホストの記録を読んでよいかの台帳（移行 0028。M47）。
//
// **行が無ければ読まない。** 起こしてよい接続先と、記録を読んでよいかは別物なので、
// 許可リストとは別の行で持つ（0026 と同じ「行で持つ」作法）。
//
// **パスは持たない**（本人の決定 2026-09-12）。置き場は向こうが名乗るので、
// ここにあるのは「読む／読まない」と、静かな失敗に気づくための跡だけ。

// RecordRoot は台帳の1行。
type RecordRoot struct {
	Host    string `json:"host"`
	Agent   string `json:"agent"`
	Enabled bool   `json:"enabled"`
	// LastOK・LastError は画面に出す。**失敗のたびに監査へ残さない**——寝ている携帯で
	// 1日ぶんが積もるので、状態が変わったときだけ残し、ここは常に最新を持つ。
	LastOK    string `json:"last_ok_at,omitempty"`
	LastError string `json:"last_error,omitempty"`
	// Fails は続けて失敗した回数。見に行く間隔を伸ばす材料。
	Fails int `json:"fail_count"`
}

// ListRecordRoots は台帳の行を全部返す（画面用）。
func ListRecordRoots(db *store.DB) ([]RecordRoot, error) {
	rows, err := db.Query(`select host, agent, enabled,
		coalesce(last_ok_at,''), coalesce(last_error,''), fail_count
		from ssh_record_roots order by host, agent`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecordRoot
	for rows.Next() {
		var r RecordRoot
		var on int
		if err := rows.Scan(&r.Host, &r.Agent, &on, &r.LastOK, &r.LastError, &r.Fails); err != nil {
			return nil, err
		}
		r.Enabled = on != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// DueRecordRoots は「いま見に行く番」の行を返す。
//
// 続けて失敗した接続先は間隔を伸ばす（every → 2倍 → 4倍。上限は cap）。
// **繋がらない携帯を30分ごとに叩き続けない**ため。成功したら Fails が 0 に戻るので、
// 間隔も戻る。
func DueRecordRoots(db *store.DB, now time.Time, every, capEvery time.Duration) ([]RecordRoot, error) {
	all, err := ListRecordRoots(db)
	if err != nil {
		return nil, err
	}
	var out []RecordRoot
	for _, r := range all {
		if !r.Enabled {
			continue
		}
		wait := every
		for i := 0; i < r.Fails && wait < capEvery; i++ {
			wait *= 2
		}
		if wait > capEvery {
			wait = capEvery
		}
		if r.LastOK == "" {
			out = append(out, r) // 一度も読めていない
			continue
		}
		t, err := time.Parse(time.RFC3339, r.LastOK)
		if err != nil || !now.Before(t.Add(wait)) {
			out = append(out, r)
		}
	}
	return out, nil
}

// SetRecordRoot は読む／読まないを決める。**足すのはパスワードの再入力が要る**
// （呼ぶ側で確かめる。許可リストの変更と同じ扱い）。止めるほうは軽くてよい。
func SetRecordRoot(db *store.DB, host, agent string, on bool) error {
	if err := validAlias(host); err != nil {
		return err
	}
	_, err := db.Exec(`insert into ssh_record_roots(host, agent, enabled) values(?,?,?)
		on conflict(host, agent) do update set enabled = excluded.enabled`,
		host, agent, b2int(on))
	return err
}

// recordRootEnabled は行があって、生きているか。**行が無ければ読まない。**
func recordRootEnabled(db *store.DB, host, agent string) (bool, error) {
	var on int
	err := db.QueryRow(`select enabled from ssh_record_roots where host = ? and agent = ?`,
		host, agent).Scan(&on)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return on != 0, nil
}

// MarkRecordOK は読めたことを残す。**続けての失敗の数を 0 に戻す**ので、間隔も戻る。
func MarkRecordOK(db *store.DB, host, agent string, now time.Time) error {
	_, err := db.Exec(`update ssh_record_roots
		set last_ok_at = ?, last_error = NULL, fail_count = 0
		where host = ? and agent = ?`, now.UTC().Format(time.RFC3339), host, agent)
	return err
}

// MarkRecordFail は読めなかったことを残す。**静かに次回へ回す**が、跡は残す——
// 何日も入っていないことに、画面で気づけるように。
func MarkRecordFail(db *store.DB, host, agent, reason string) error {
	_, err := db.Exec(`update ssh_record_roots
		set last_error = ?, fail_count = fail_count + 1
		where host = ? and agent = ?`, reason, host, agent)
	return err
}

func b2int(b bool) int {
	if b {
		return 1
	}
	return 0
}
