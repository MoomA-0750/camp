package ingest

import (
	"encoding/json"
	"fmt"

	"github.com/MoomA-0750/camp/internal/store"
)

// BackfillUsage は messages.raw_json から usage と sessions.total_cost_usd を作り直す。
//
// なぜディスクを読み直さないのか。Camp の存在理由は、CLI 側から消えた記録を
// 手元に残すことにある。~/.claude/projects を正として取り込み直す運用にすると、
// 「消えたぶんを失う」という一番やってはいけない事故が、新しい派生テーブルを
// 足すたびに起きる。raw_json は無加工で持っているので（D-010）、派生テーブルは
// いつでもDBの中だけで作り直せる。作り直しの入り口はここに集約する。
//
// usage は messages から完全に導出できる。途中で抽出規則を直したときに
// 古い値が max で残り続けないよう、消してから作る。
func BackfillUsage(db *store.DB) (rows int, sessions int, err error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`delete from usage`); err != nil {
		return 0, 0, err
	}
	if _, err := tx.Exec(`update sessions set total_cost_usd = null`); err != nil {
		return 0, 0, err
	}

	// 先に全部読み切ってから書く。同じトランザクションで Rows を開いたまま
	// Exec すると接続が1本しかないので詰まる。
	q, err := tx.Query(`
		select session_id, coalesce(run_id, ''), type, raw_json
		  from messages
		 where coalesce(api_message_id, '') <> '' or type = 'cost-state'
		 order by source_file_id, byte_offset`)
	if err != nil {
		return 0, 0, err
	}

	var pending []*usageRow
	cost := map[string]float64{}
	for q.Next() {
		var sessID, runID, typ string
		var raw []byte
		if err := q.Scan(&sessID, &runID, &typ, &raw); err != nil {
			q.Close()
			return 0, 0, err
		}
		var l Line
		if err := json.Unmarshal(raw, &l); err != nil {
			continue // 壊れた行は messages に残る。ここでは飛ばす
		}
		if typ == "cost-state" {
			if l.TotalCostUSD != nil && *l.TotalCostUSD > cost[sessID] {
				cost[sessID] = *l.TotalCostUSD
			}
			continue
		}
		if u := newUsageRow(&l, sessID, runID); u != nil {
			pending = append(pending, u)
		}
	}
	err = q.Err()
	q.Close()
	if err != nil {
		return 0, 0, err
	}

	stmt, err := tx.Prepare(usageUpsertSQL)
	if err != nil {
		return 0, 0, err
	}
	defer stmt.Close()

	for _, u := range pending {
		if err := u.exec(stmt); err != nil {
			return 0, 0, fmt.Errorf("usage %s: %w", u.APIMessageID, err)
		}
		rows++
	}

	for id, c := range cost {
		if _, err := tx.Exec(`update sessions set total_cost_usd = ? where id = ?`, c, id); err != nil {
			return 0, 0, err
		}
		sessions++
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return rows, sessions, nil
}
