package session

import (
	"encoding/json"
	"sort"
)

// Claude の残量（get_usage・get_context_usage の答え。2026-09-04 に実測した欄だけを読む）。

var claudeLimitLabel = map[string]string{
	"session": "セッション", "five_hour": "5時間",
	"weekly_all": "週（全体）", "seven_day": "7日",
	"weekly_opus": "週（Opus）", "seven_day_opus": "7日（Opus）",
}

func (claudeDriver) Usage(usage, context json.RawMessage) UsageView {
	var u struct {
		SubscriptionType string `json:"subscription_type"`
		RateLimits       struct {
			Limits []struct {
				Kind     string  `json:"kind"`
				Percent  float64 `json:"percent"`
				ResetsAt string  `json:"resets_at"`
				IsActive bool    `json:"is_active"`
			} `json:"limits"`
		} `json:"rate_limits"`
		Session struct {
			TotalCostUSD float64 `json:"total_cost_usd"`
			ModelUsage   map[string]struct {
				Input         float64 `json:"inputTokens"`
				Output        float64 `json:"outputTokens"`
				CacheRead     float64 `json:"cacheReadInputTokens"`
				CacheCreation float64 `json:"cacheCreationInputTokens"`
				Thinking      float64 `json:"thinkingTokens"`
				CostUSD       float64 `json:"costUSD"`
			} `json:"model_usage"`
		} `json:"session"`
	}
	json.Unmarshal(usage, &u)
	v := UsageView{Plan: u.SubscriptionType}
	for _, l := range u.RateLimits.Limits {
		label := claudeLimitLabel[l.Kind]
		if label == "" {
			label = l.Kind
		}
		v.Windows = append(v.Windows, UsageWindow{Label: label, Percent: l.Percent,
			ResetsAt: l.ResetsAt, Active: l.IsActive})
	}

	models := UsageTable{Title: "トークンの内訳（このセッション）", Empty: "まだ1度もモデルを呼んでいない。",
		Columns: []UsageColumn{{"入力", "tokens"}, {"出力", "tokens"}, {"キャッシュ読み", "tokens"},
			{"キャッシュ作成", "tokens"}, {"思考", "tokens"}, {"費用", "usd"}}}
	names := make([]string, 0, len(u.Session.ModelUsage))
	for n := range u.Session.ModelUsage {
		names = append(names, n)
	}
	sort.Strings(names)
	total := make([]float64, 6)
	for _, n := range names {
		m := u.Session.ModelUsage[n]
		cells := []float64{m.Input, m.Output, m.CacheRead, m.CacheCreation, m.Thinking, m.CostUSD}
		for i, c := range cells {
			total[i] += c
		}
		models.Rows = append(models.Rows, UsageRow{Label: n, Cells: cells})
	}
	if len(names) > 0 {
		total[5] = u.Session.TotalCostUSD // 費用はセッション全体の値（モデル別の和と違うことがある）
		models.Rows = append(models.Rows, UsageRow{Label: "合計", Cells: total, Total: true})
	}

	var c struct {
		Categories []struct {
			Name   string  `json:"name"`
			Tokens float64 `json:"tokens"`
		} `json:"categories"`
		TotalTokens int64 `json:"totalTokens"`
		MaxTokens   int64 `json:"maxTokens"`
	}
	json.Unmarshal(context, &c)
	if c.TotalTokens > 0 || c.MaxTokens > 0 {
		v.Context = &UsageContext{Used: c.TotalTokens, Max: c.MaxTokens}
	}
	ctx := UsageTable{Title: "コンテキストの内訳", Empty: "コンテキストの内訳が来ていない。",
		Columns: []UsageColumn{{"トークン", "tokens"}, {"割合", "percent"}}}
	for _, cat := range c.Categories {
		pct := 0.0
		if c.MaxTokens > 0 {
			pct = cat.Tokens / float64(c.MaxTokens) * 100
		}
		ctx.Rows = append(ctx.Rows, UsageRow{Label: cat.Name, Cells: []float64{cat.Tokens, pct}})
	}
	v.Tables = []UsageTable{models, ctx}
	return v
}
