package session

import (
	"encoding/json"
	"time"
)

// 残量の共通の形。**画面はこの形だけを見て描き、エージェントを見ない**（D-031）。
// 駆動器が、そのエージェントの問い合わせの答えをこの形に直す（Driver.Usage）。
// 生の答えも API は並べて返す（画面の「そのまま見る」）。

// UsageView は1本のセッションの残量。
type UsageView struct {
	Plan    string        `json:"plan,omitempty"`    // プランの名前
	Windows []UsageWindow `json:"windows"`           // プラン枠（メーター）
	Context *UsageContext `json:"context,omitempty"` // コンテキストの埋まり具合
	Tables  []UsageTable  `json:"tables"`            // 内訳（モデル別・分類別など）
	// Errors はエージェントが答えの中で返した失敗。**空と書かない**ために並べる。
	Errors []string `json:"errors,omitempty"`
}

// UsageWindow はプラン枠1つ。
type UsageWindow struct {
	Label    string  `json:"label"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resets_at,omitempty"` // RFC3339
	Active   bool    `json:"active,omitempty"`    // いま拘束している
}

// UsageContext はコンテキストの埋まり具合（トークン）。
type UsageContext struct {
	Used int64 `json:"used"`
	Max  int64 `json:"max,omitempty"`
}

// UsageTable は内訳の表1つ。
type UsageTable struct {
	Title   string        `json:"title"`
	Columns []UsageColumn `json:"columns"`
	Rows    []UsageRow    `json:"rows"`
	Empty   string        `json:"empty,omitempty"` // 行が無いときの文
}

// UsageColumn は列。Unit は画面の書き方（tokens / usd / percent）。
type UsageColumn struct {
	Label string `json:"label"`
	Unit  string `json:"unit"`
}

// UsageRow は行。Cells は Columns と同じ順。
type UsageRow struct {
	Label string    `json:"label"`
	Cells []float64 `json:"cells"`
	Total bool      `json:"total,omitempty"` // 合計の行
}

// UsageViewOf は agent の残量の答えを共通の形に直す。知らないエージェントなら空の形。
// **nil のスライスを返さない**（JSON で null になり、画面が落ちる）。
func UsageViewOf(agent string, usage, context json.RawMessage) UsageView {
	var v UsageView
	if d, ok := drivers[agentOr(agent)]; ok {
		v = d.Usage(usage, context)
	}
	if v.Windows == nil {
		v.Windows = []UsageWindow{}
	}
	if v.Tables == nil {
		v.Tables = []UsageTable{}
	}
	for i := range v.Tables {
		if v.Tables[i].Rows == nil {
			v.Tables[i].Rows = []UsageRow{}
		}
	}
	return v
}

// unixTime は Unix 秒を RFC3339 に。0 以下なら空。
func unixTime(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}
