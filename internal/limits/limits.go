// Package limits はプラン上限の窓（5時間・7日）の観測を記録し、読み出す。
//
// なぜ statusLine から取るのか（D-007 との関係）:
// D-007 は「制御プロトコル（rate_limit_event / get_usage）のほうが豊富」と
// 結論した。それは正しいが、あれは Phase 3 の `claude -p` が動いていて初めて
// 使える。一方この値は「観測した瞬間の状態」しか取れない性質のもので、
// 過去に遡って作り直せない。JSONL や file-history と違ってディスクに残らず、
// statusLine が描画されるたびに捨てられている。
//
// だから記録だけ先に始める。usage_windows の UNIQUE は source を含むので、
// のちに source='control' の行が同じ窓に並んでも衝突しない。
package limits

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/MoomA-0750/camp/internal/store"
)

// SourceStatusLine は statusLine コマンドの stdin JSON 由来であることを示す。
const SourceStatusLine = "statusline"

// AgentClaudeCode は Claude Code の観測であることを示す。
const AgentClaudeCode = "claude-code"

// windowLen は窓の種類から長さを引く。started_at は ends_at から逆算する。
var windowLen = map[string]time.Duration{
	"five_hour": 5 * time.Hour,
	"seven_day": 7 * 24 * time.Hour,
}

// ErrNoWindows は rate_limits が空だったことを示す。
// TUI が上限情報をまだ持っていない起動直後は普通に起きるので、
// 呼び出し側はこれを失敗として扱わない。
var ErrNoWindows = errors.New("rate_limits が入っていない")

// statusLineInput は statusLine コマンドが stdin で受け取る JSON のうち、
// ここで使う部分だけ。知らないキーは無視する。
type statusLineInput struct {
	RateLimits map[string]struct {
		UsedPercentage *float64 `json:"used_percentage"`
		ResetsAt       *int64   `json:"resets_at"`
	} `json:"rate_limits"`
}

// Reading は1回の観測で書き込んだ内容。
type Reading struct {
	Kind    string  `json:"kind"`
	UsedPct float64 `json:"used_pct"`
	EndsAt  string  `json:"ends_at"`
	New     bool    `json:"new"`
}

// Record は statusLine の stdin JSON を読み、窓ごとに1行へ畳んで記録する。
func Record(db *store.DB, r io.Reader, agent, source string) ([]Reading, error) {
	// statusLine の入力は数KBで収まる。壊れた入力で無限に読まないよう蓋をする。
	body, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return nil, err
	}
	var in statusLineInput
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("statusLine JSON を読めない: %w", err)
	}
	if len(in.RateLimits) == 0 {
		return nil, ErrNoWindows
	}

	now := time.Now().UTC().Format(time.RFC3339)
	out := []Reading{}
	for kind, w := range in.RateLimits {
		if w.UsedPercentage == nil || w.ResetsAt == nil || *w.ResetsAt <= 0 {
			// spend_limit のように値が来ないことがある。黙って飛ばす。
			continue
		}
		ends := time.Unix(*w.ResetsAt, 0).UTC()
		endsAt := ends.Format(time.RFC3339)
		var startedAt any
		if d, ok := windowLen[kind]; ok {
			startedAt = ends.Add(-d).Format(time.RFC3339)
		}

		res, err := db.Exec(`
			insert into usage_windows
			       (agent, kind, started_at, ends_at, used_pct, peak_pct, samples, source, fetched_at)
			values (?, ?, ?, ?, ?, ?, 1, ?, ?)
			on conflict(agent, kind, ends_at, source) do update set
			       used_pct   = excluded.used_pct,
			       peak_pct   = max(coalesce(usage_windows.peak_pct, 0), excluded.used_pct),
			       samples    = usage_windows.samples + 1,
			       fetched_at = excluded.fetched_at`,
			agent, kind, startedAt, endsAt, *w.UsedPercentage, *w.UsedPercentage, source, now)
		if err != nil {
			return nil, err
		}
		// 新規行なら rows affected は1、更新なら SQLite は2を返す。
		n, _ := res.RowsAffected()
		out = append(out, Reading{
			Kind: kind, UsedPct: *w.UsedPercentage, EndsAt: endsAt, New: n == 1,
		})
	}
	if len(out) == 0 {
		return nil, ErrNoWindows
	}
	return out, nil
}

// Window は記録済みの1窓。
type Window struct {
	ID        int64   `json:"id"`
	Agent     string  `json:"agent"`
	Kind      string  `json:"kind"`
	StartedAt string  `json:"started_at,omitempty"`
	EndsAt    string  `json:"ends_at,omitempty"`
	UsedPct   float64 `json:"used_pct"`
	PeakPct   float64 `json:"peak_pct"`
	Samples   int     `json:"samples"`
	Source    string  `json:"source"`
	FetchedAt string  `json:"fetched_at"`
	Current   bool    `json:"current"`
}

// Opts は Windows の絞り込み。
type Opts struct {
	Kind  string
	Limit int
}

// Windows は新しい窓から順に返す。
func Windows(db *store.DB, o Opts) ([]Window, error) {
	if o.Limit <= 0 || o.Limit > 1000 {
		o.Limit = 100
	}
	rows, err := db.Query(`
		select id, agent, kind, coalesce(started_at, ''), coalesce(ends_at, ''),
		       coalesce(used_pct, 0), coalesce(peak_pct, 0), samples, source, fetched_at
		  from usage_windows
		 where (? = '' or kind = ?)
		 order by ends_at desc, kind
		 limit ?`, o.Kind, o.Kind, o.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	out := []Window{}
	for rows.Next() {
		var w Window
		if err := rows.Scan(&w.ID, &w.Agent, &w.Kind, &w.StartedAt, &w.EndsAt,
			&w.UsedPct, &w.PeakPct, &w.Samples, &w.Source, &w.FetchedAt); err != nil {
			return nil, err
		}
		w.Current = w.EndsAt > now
		out = append(out, w)
	}
	return out, rows.Err()
}

// Current は種類ごとに「いま拘束されている窓」を1つずつ返す。
// 期限切れの窓しか無い種類は返さない（残量ではなく過去の記録なので）。
// 「どれが現在か」は ends_at の大きさではなく最後に観測できた時刻で決める。
// リセット時刻が動いたり観測が飛んだりすると、期限だけ先の古い行が
// いちばん未来に見えることがあるため。
func Current(db *store.DB) ([]Window, error) {
	all, err := Windows(db, Opts{Limit: 1000})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].FetchedAt > all[j].FetchedAt })
	seen := map[string]bool{}
	out := []Window{}
	for _, w := range all {
		if !w.Current || seen[w.Kind] {
			continue
		}
		seen[w.Kind] = true
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out, nil
}
