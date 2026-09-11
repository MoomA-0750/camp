package session

import (
	"fmt"
	"strings"

	"github.com/MoomA-0750/camp/internal/store"
)

// 終わり方。**決まった語だけを書く。** 文は画面と CLI が作る。
//
// 2026-09-11 に足した。それまでは exit_reason（自由な文）しか無く、
// 「動いている途中で終わったのか」「承認を待たせたまま終わったのか」を
// 後から選り分けられなかった。
const (
	EndSelf        = "self"         // 子が自分で終わった
	EndUserStop    = "user_stop"    // 本人が止めた
	EndIdleTimeout = "idle_timeout" // 何も来ないまま時間が経ったので Camp が閉じた
	EndTurnTimeout = "turn_timeout" // ターンが長すぎ、中断も効かないので Camp が止めた
	EndStopTimeout = "stop_timeout" // 止めろと言ったのに止まらず、Camp が見張りを諦めた
	EndStartFailed = "start_failed" // 起こせなかった
	EndAgentLost   = "agent_lost"   // 実行面が落ち、見張りが外れている間に終わった
	EndUnseen      = "unseen"       // campd が止まっている間に終わっていた
	EndReaped      = "reaped"       // 前回の残りを、実行面に頼んで始末した
	// EndConnLost は SSH の接続が切れた（2026-09-11、リモート起動と一緒に足した）。
	// **向こうの子はそれで終わるとは限らない**ので、実行面が見に行って始末する。
	EndConnLost = "conn_lost"
)

// EndCauses は画面が絞り込みに使える終わり方の全部。並びは画面に出す順。
var EndCauses = []string{
	EndSelf, EndUserStop, EndIdleTimeout, EndTurnTimeout, EndStopTimeout,
	EndStartFailed, EndAgentLost, EndConnLost, EndUnseen, EndReaped,
}

var endLabels = map[string]string{
	EndSelf:        "子が自分で終わった",
	EndUserStop:    "本人が止めた",
	EndIdleTimeout: "放置で閉じた",
	EndTurnTimeout: "ターンが長すぎて止めた",
	EndStopTimeout: "止まらず見張りを諦めた",
	EndStartFailed: "起こせなかった",
	EndAgentLost:   "実行面が落ちた",
	EndConnLost:    "SSH が切れた",
	EndUnseen:      "見ていない間に終わっていた",
	EndReaped:      "残っていたものを始末した",
}

// EndLabel は CLI 用の文。
func EndLabel(cause string) string {
	if cause == "" {
		return "記録なし（2026-09-11 より前）"
	}
	if l, ok := endLabels[cause]; ok {
		return l
	}
	return cause
}

// 絞り込みの種類。終わり方（EndCauses）に加えて、横断的に選り分けるもの。
const (
	KindMidTurn = "mid"     // 動いている途中で終わった
	KindWaiting = "waiting" // 承認を待たせたまま終わった
	KindIgnored = "ignored" // 承認を答えないまま期限切れにした
	KindUnknown = "unknown" // どう終わったかの記録が無い（2026-09-11 より前）
)

// kindWhere は種類ごとの条件。**許した語だけを SQL にする。**
func kindWhere(kind string) (string, []any, error) {
	switch kind {
	case "":
		return "", nil, nil
	case KindMidTurn:
		return ` and end_state = ?`, []any{StateRunning}, nil
	case KindWaiting:
		return ` and exists (select 1 from approvals a where a.session_id = runtime_sessions.id
		           and a.reason = ?)`, []any{BySessionEnd}, nil
	case KindIgnored:
		return ` and exists (select 1 from approvals a where a.session_id = runtime_sessions.id
		           and a.reason = ?)`, []any{ByTimeout}, nil
	case KindUnknown:
		return ` and end_cause is null`, nil, nil
	}
	for _, c := range EndCauses {
		if c == kind {
			return ` and end_cause = ?`, []any{c}, nil
		}
	}
	return "", nil, fmt.Errorf("知らない絞り込み: %s", kind)
}

// EndedQuery は終わったセッションの引き方。
type EndedQuery struct {
	Kind   string // 空なら全部
	Before string // "ended_at|id"。これより古いものだけ（前の頁の Next）
	Limit  int
}

// EndedPage は1頁ぶん。
type EndedPage struct {
	Sessions []Record `json:"sessions"`
	// Next は続きがあるときの Before。無ければ空。
	Next string `json:"next,omitempty"`
	// Counts は種類ごとの件数。**頁に関係なく全体を数える**——
	// 頁の中だけ数えると「3件」が「全部で3件」に見える。
	Counts map[string]int `json:"counts"`
}

// ErrBadQuery は絞り込みや続きの位置が読めないとき。**頼み方の誤りで、DB の故障ではない。**
var ErrBadQuery = fmt.Errorf("頼み方が読めない")

// ListEnded は終わったセッションを、終わった時刻の新しい順に返す。
//
// 並びは (ended_at, id)。ended_at は秒までなので、同じ秒に終わったものを
// 頁の境目で落とさないよう id でも切る。
func ListEnded(db *store.DB, q EndedQuery) (EndedPage, error) {
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 200
	}
	where, args, err := kindWhere(q.Kind)
	if err != nil {
		return EndedPage{}, fmt.Errorf("%w: %v", ErrBadQuery, err)
	}
	sqlq := selectCols + ` where state = ?` + where
	all := append([]any{StateExited}, args...)
	if q.Before != "" {
		i := strings.LastIndex(q.Before, "|")
		if i <= 0 || i == len(q.Before)-1 {
			return EndedPage{}, fmt.Errorf("%w: 続きの位置 %q", ErrBadQuery, q.Before)
		}
		at, id := q.Before[:i], q.Before[i+1:]
		sqlq += ` and (ended_at < ? or (ended_at = ? and id < ?))`
		all = append(all, at, at, id)
	}
	sqlq += ` order by ended_at desc, id desc limit ?`
	all = append(all, q.Limit+1)

	rows, err := db.Query(sqlq, all...)
	if err != nil {
		return EndedPage{}, err
	}
	recs, err := scanAll(rows)
	if err != nil {
		return EndedPage{}, err
	}
	page := EndedPage{Sessions: recs}
	if len(recs) > q.Limit {
		page.Sessions = recs[:q.Limit]
		last := page.Sessions[q.Limit-1]
		page.Next = last.EndedAt + "|" + last.ID
	}
	if page.Counts, err = endedCounts(db); err != nil {
		return EndedPage{}, err
	}
	return page, nil
}

// endedCounts は種類ごとの件数。"all" は全部。
func endedCounts(db *store.DB) (map[string]int, error) {
	kinds := append([]string{"", KindMidTurn, KindWaiting, KindIgnored, KindUnknown}, EndCauses...)
	out := make(map[string]int, len(kinds))
	for _, k := range kinds {
		where, args, err := kindWhere(k)
		if err != nil {
			return nil, err
		}
		var n int
		if err := db.QueryRow(`select count(*) from runtime_sessions where state = ?`+where,
			append([]any{StateExited}, args...)...).Scan(&n); err != nil {
			return nil, err
		}
		if k == "" {
			k = "all"
		}
		out[k] = n
	}
	return out, nil
}
