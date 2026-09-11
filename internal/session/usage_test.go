package session

import (
	"encoding/json"
	"strings"
	"testing"
)

// 残量の共通の形（M40）。**画面はこの形だけを見る**ので、どちらのエージェントの答えも
// 同じ欄に収まることを縛る。答えは実測の形（値は丸めた。web の RuntimeUsage.test.tsx と同じもの）。

func TestClaudeUsageFitsTheCommonShape(t *testing.T) {
	usage := json.RawMessage(`{"subscription_type":"pro","rate_limits":{"limits":[
		{"group":"session","kind":"session","percent":100,"is_active":true,"resets_at":"2126-09-04T18:30:00Z"},
		{"group":"weekly","kind":"weekly_all","percent":48,"is_active":false,"resets_at":"2126-09-10T00:00:00Z"}]},
		"session":{"total_cost_usd":0.1422,"model_usage":{"claude-opus-5":{"inputTokens":4,"outputTokens":226,
		"cacheReadInputTokens":25859,"cacheCreationInputTokens":12264,"thinkingTokens":0,"costUSD":0.1412}}}}`)
	context := json.RawMessage(`{"categories":[{"name":"System prompt","tokens":3296},
		{"name":"Messages","tokens":9408}],"totalTokens":21874,"maxTokens":1000000,"percentage":2}`)
	v := UsageViewOf(AgentClaude, usage, context)
	if v.Plan != "pro" || len(v.Windows) != 2 || v.Windows[0].Label != "セッション" ||
		v.Windows[0].Percent != 100 || !v.Windows[0].Active || v.Windows[1].Label != "週（全体）" {
		t.Fatalf("枠が共通の形に収まっていない: %+v", v)
	}
	if v.Context == nil || v.Context.Used != 21874 || v.Context.Max != 1000000 {
		t.Fatalf("コンテキストが無い: %+v", v.Context)
	}
	if len(v.Tables) != 2 || v.Tables[0].Rows[0].Label != "claude-opus-5" ||
		v.Tables[0].Rows[0].Cells[2] != 25859 || !v.Tables[0].Rows[1].Total ||
		v.Tables[0].Rows[1].Cells[5] != 0.1422 {
		t.Fatalf("モデル別の内訳が無い: %+v", v.Tables)
	}
	if v.Tables[1].Rows[1].Label != "Messages" || v.Tables[1].Columns[1].Unit != "percent" {
		t.Fatalf("コンテキストの内訳が無い: %+v", v.Tables[1])
	}
}

func TestCodexUsageFitsTheCommonShape(t *testing.T) {
	usage := json.RawMessage(`{"rateLimits":{"planType":"plus",
		"primary":{"usedPercent":73,"windowDurationMins":300,"resetsAt":1789000000},
		"secondary":{"usedPercent":78,"windowDurationMins":10080,"resetsAt":1789500000}}}`)
	context := json.RawMessage(`{"tokenUsage":{"total":{"totalTokens":16497,"inputTokens":16378,
		"cachedInputTokens":11904,"outputTokens":119,"reasoningOutputTokens":0},"modelContextWindow":258400}}`)
	v := UsageViewOf(AgentCodex, usage, context)
	if v.Plan != "plus" || len(v.Windows) != 2 || v.Windows[0].Label != "5時間" ||
		v.Windows[0].Percent != 73 || v.Windows[1].Label != "週" ||
		!strings.HasPrefix(v.Windows[0].ResetsAt, "2026-") {
		t.Fatalf("枠が共通の形に収まっていない: %+v", v.Windows)
	}
	if v.Context == nil || v.Context.Used != 16497 || v.Context.Max != 258400 {
		t.Fatalf("コンテキストが無い: %+v", v.Context)
	}
	if len(v.Tables) != 1 || len(v.Tables[0].Rows) != 1 || v.Tables[0].Rows[0].Cells[1] != 11904 {
		t.Fatalf("トークンの内訳が無い: %+v", v.Tables)
	}
}

// **取れなかったことを「空」と読ませない。** 答えの中の失敗は並べ、まだ何も無いときは
// 表の「無い」の文と、null でない空の配列で返す。
func TestUsageFailuresAndEmptinessAreSaid(t *testing.T) {
	v := UsageViewOf(AgentCodex, json.RawMessage(`{"error":"枠を読めない"}`), json.RawMessage(`{"tokenUsage":null}`))
	if len(v.Errors) != 1 || v.Errors[0] != "枠を読めない" {
		t.Fatalf("失敗が並ばない: %+v", v)
	}
	b, _ := json.Marshal(UsageViewOf(AgentClaude, nil, nil))
	if strings.Contains(string(b), "null") {
		t.Fatalf("空の形に null が入る: %s", b)
	}
	if v := UsageViewOf(AgentClaude, nil, nil); v.Tables[0].Empty == "" || len(v.Tables[0].Rows) != 0 {
		t.Fatalf("まだ呼んでいないときの文が無い: %+v", v.Tables[0])
	}
}
