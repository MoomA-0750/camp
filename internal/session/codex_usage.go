package session

import (
	"encoding/json"
	"fmt"
)

// Codex の残量。get_usage は account/rateLimits/read の答え、get_context_usage は実行面が控えている
// 最後の thread/tokenUsage/updated（2026-09-11 実測、codex-cli 0.154.0）。どちらもモデルを呼ばない。

type codexWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins int     `json:"windowDurationMins"`
	ResetsAt           int64   `json:"resetsAt"`
}

// codexWindowLabel は枠の長さ（分）を名前にする。実測は 300 と 10080。
func codexWindowLabel(mins int) string {
	switch mins {
	case 300:
		return "5時間"
	case 10080:
		return "週"
	case 0:
		return "枠"
	}
	return fmt.Sprintf("%d分", mins)
}

func (codexDriver) Usage(usage, context json.RawMessage) UsageView {
	var u struct {
		Error      string `json:"error"`
		RateLimits *struct {
			PlanType  string       `json:"planType"`
			Primary   *codexWindow `json:"primary"`
			Secondary *codexWindow `json:"secondary"`
		} `json:"rateLimits"`
	}
	json.Unmarshal(usage, &u)
	var v UsageView
	if u.Error != "" {
		v.Errors = append(v.Errors, u.Error)
	}
	if r := u.RateLimits; r != nil {
		v.Plan = r.PlanType
		for _, w := range []*codexWindow{r.Primary, r.Secondary} {
			if w == nil {
				continue
			}
			v.Windows = append(v.Windows, UsageWindow{Label: codexWindowLabel(w.WindowDurationMins),
				Percent: w.UsedPercent, ResetsAt: unixTime(w.ResetsAt)})
		}
	}

	var c struct {
		TokenUsage *struct {
			Total *struct {
				Total     float64 `json:"totalTokens"`
				Input     float64 `json:"inputTokens"`
				Cached    float64 `json:"cachedInputTokens"`
				Output    float64 `json:"outputTokens"`
				Reasoning float64 `json:"reasoningOutputTokens"`
			} `json:"total"`
			ModelContextWindow int64 `json:"modelContextWindow"`
		} `json:"tokenUsage"`
	}
	json.Unmarshal(context, &c)
	t := UsageTable{Title: "トークン（このスレッド）", Empty: "まだ1度もモデルを呼んでいない。",
		Columns: []UsageColumn{{"入力", "tokens"}, {"キャッシュ読み", "tokens"}, {"出力", "tokens"},
			{"推論", "tokens"}}}
	if tu := c.TokenUsage; tu != nil && tu.Total != nil {
		v.Context = &UsageContext{Used: int64(tu.Total.Total), Max: tu.ModelContextWindow}
		t.Rows = []UsageRow{{Label: "合計", Total: true,
			Cells: []float64{tu.Total.Input, tu.Total.Cached, tu.Total.Output, tu.Total.Reasoning}}}
	}
	v.Tables = []UsageTable{t}
	return v
}
