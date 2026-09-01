package ingest

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"time"
)

// usageRow は assistant 行1本から取り出したトークン計上。
//
// 主キーは api_message_id（msg_xxx）。1回のAPI呼び出しに1行だけ立てる。
// 同じ id が複数行に現れる理由は2つあり、扱いが違う。
//
//	ストリーミングの途中経過 … 同じファイルの中に、出力が伸びていく途中の行が並ぶ。
//	                          input 側は不変、output 側だけが増える。最後の行が確定値。
//	fork / resume の複製     … 別ファイルに、まったく同じ数値でもう一度現れる。
//
// 前者があるので「最初の1行を採って以降は捨てる」は使えない（実測48件で
// 出力が過少になる）。数えるのは実際に払ったトークンなので、複製側では
// 増えないほうが正しい。したがって列ごとの max を取る。
// max は順序に依らず冪等なので、差分取り込みで途中経過と確定値が
// 別々のパスに分かれても結果が変わらない。
type usageRow struct {
	APIMessageID string
	RequestID    string
	SessionID    string
	RunID        string
	TS           string
	Day          string
	Model        string
	ServiceTier  string
	Speed        string
	Effort       string
	Iterations   any // *int64。キーが無ければ nil

	Input         int64
	Output        int64
	CacheCreate   int64
	CacheRead     int64
	CacheCreate1h int64
	CacheCreate5m int64
	Thinking      int64
	WebSearch     int64
	WebFetch      int64
}

// syntheticModel はCLIがローカルで作った擬似アシスタント行（APIエラーの
// 差し込みなど）。usage は常にゼロで、課金も発生していない。
// これを usage に入れると、モデル別集計のたびに除外を忘れないよう
// 気をつけ続ける必要が出るので、最初から入れない。
const syntheticModel = "<synthetic>"

// newUsageRow は usage に立てる行を作る。立てないときは nil を返す。
func newUsageRow(l *Line, sessID, runID string) *usageRow {
	if l.Message == nil || l.Message.Usage == nil || l.Message.ID == "" {
		return nil
	}
	if l.Message.Model == syntheticModel {
		return nil
	}
	if l.Timestamp == "" {
		return nil // ts は NOT NULL。実測ではゼロ件だが、無いなら計上しない
	}
	u := l.Message.Usage
	r := &usageRow{
		APIMessageID: l.Message.ID,
		RequestID:    l.RequestID,
		SessionID:    sessID,
		RunID:        runID,
		TS:           l.Timestamp,
		Day:          localDay(l.Timestamp),
		Model:        l.Message.Model,
		ServiceTier:  u.ServiceTier,
		Speed:        u.Speed,
		Effort:       l.Effort,
		Input:        u.InputTokens,
		Output:       u.OutputTokens,
		CacheCreate:  u.CacheCreationInputTokens,
		CacheRead:    u.CacheReadInputTokens,
	}
	if u.CacheCreation != nil {
		r.CacheCreate1h = u.CacheCreation.Ephemeral1h
		r.CacheCreate5m = u.CacheCreation.Ephemeral5m
	}
	if u.OutputTokensDetails != nil {
		r.Thinking = u.OutputTokensDetails.ThinkingTokens
	}
	if u.ServerToolUse != nil {
		r.WebSearch = u.ServerToolUse.WebSearchRequests
		r.WebFetch = u.ServerToolUse.WebFetchRequests
	}
	if n, ok := iterationCount(u.Iterations); ok {
		r.Iterations = n
	}
	return r
}

// iterationCount は usage.iterations の要素数を返す。
//
// この項目は整数ではなく、usage と同じ形のオブジェクトの配列だった。
// 実コーパスでは長さが必ず1で、その唯一の要素は上位の usage と
// 完全に一致する（11,468行すべてで output も cache_read も一致）。
// つまり上位がロールアップで、配列はその内訳。
// 長さ2以上の実例をまだ見ていないので、上位が内訳の合計なのか
// 最終イテレーションなのかは未確認。ここでは本数だけ持っておく。
func iterationCount(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	// json.Unmarshal は null を「エラー無し・長さ0」として受ける。
	// キーが無いのと null（実測16件、いずれも <synthetic>）と空配列は区別する。
	if string(bytes.TrimSpace(raw)) == "null" {
		return 0, false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return 0, false
	}
	return int64(len(arr)), true
}

// localDay は RFC3339 のタイムスタンプをこのホストのローカル日付に落とす。
//
// UTCの日付ではなくローカル日付にする。JSTだと 15:00Z 以降は翌日であり、
// 「今日どれだけ使ったか」をUTCで切ると毎日9時間ぶんずれる。
// 利用量の見え方は人間の1日に揃えたい（D-013）。
func localDay(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		if len(ts) >= 10 {
			return ts[:10] // 壊れていたら文字列の頭を使う
		}
		return ts
	}
	return t.Local().Format("2006-01-02")
}

// usageUpsertSQL は取り込みと再構築の両方で使う。
// プレースホルダの順序は usageRow.exec と対で守ること。
const usageUpsertSQL = `
	insert into usage(
		api_message_id, request_id, session_id, run_id, project_id,
		ts, day, model, service_tier, speed, effort, iterations,
		input_tokens, output_tokens,
		cache_creation_input_tokens, cache_read_input_tokens,
		cache_creation_1h_tokens, cache_creation_5m_tokens,
		thinking_tokens, web_search_requests, web_fetch_requests)
	values(?,?,?,?,(select project_id from sessions where id = ?),
		?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	on conflict(api_message_id) do update set
		input_tokens                = max(input_tokens, excluded.input_tokens),
		output_tokens               = max(output_tokens, excluded.output_tokens),
		cache_creation_input_tokens = max(cache_creation_input_tokens, excluded.cache_creation_input_tokens),
		cache_read_input_tokens     = max(cache_read_input_tokens, excluded.cache_read_input_tokens),
		cache_creation_1h_tokens    = max(cache_creation_1h_tokens, excluded.cache_creation_1h_tokens),
		cache_creation_5m_tokens    = max(cache_creation_5m_tokens, excluded.cache_creation_5m_tokens),
		thinking_tokens             = max(thinking_tokens, excluded.thinking_tokens),
		web_search_requests         = max(web_search_requests, excluded.web_search_requests),
		web_fetch_requests          = max(web_fetch_requests, excluded.web_fetch_requests),
		-- max() は引数に NULL があると NULL を返す。キーが無い行（実測203件）を
		-- 0 に潰さないよう、両方あるときだけ max を使う。
		iterations                  = coalesce(max(iterations, excluded.iterations), iterations, excluded.iterations)`

func (u *usageRow) exec(stmt *sql.Stmt) error {
	_, err := stmt.Exec(
		u.APIMessageID, nz(u.RequestID), u.SessionID, nz(u.RunID), u.SessionID,
		u.TS, u.Day, u.Model, nz(u.ServiceTier), nz(u.Speed), nz(u.Effort), u.Iterations,
		u.Input, u.Output, u.CacheCreate, u.CacheRead,
		u.CacheCreate1h, u.CacheCreate5m, u.Thinking, u.WebSearch, u.WebFetch)
	return err
}
