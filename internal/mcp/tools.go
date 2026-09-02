package mcp

import (
	"fmt"
	"strings"

	"github.com/MoomA-0750/camp/internal/query"
	"github.com/MoomA-0750/camp/internal/search"
	"github.com/MoomA-0750/camp/internal/vault"
	"github.com/MoomA-0750/camp/internal/views"
)

func str(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func num(args map[string]any, key string, def int) int {
	if v, ok := args[key].(float64); ok && v > 0 {
		return int(v)
	}
	return def
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func (s *Server) builtinTools() []Tool {
	return []Tool{
		{
			Name: "search_sessions",
			Description: "Claude Code / Codex の会話を全文検索する。日本語は2文字から引ける。" +
				"CLI 側から消えたセッションも残っている。",
			Schema: obj(map[string]any{
				"q":       strProp("検索語"),
				"kind":    strProp("ブロック種別で絞る（text / thinking / tool_use / tool_result）"),
				"session": strProp("セッションIDで絞る"),
				"limit":   intProp("件数（既定20）"),
			}, "q"),
			Run: func(a map[string]any) (any, error) {
				q := str(a, "q")
				if q == "" {
					return nil, fmt.Errorf("検索語が要る")
				}
				return search.Query(s.db, q, search.Opts{
					Kind: str(a, "kind"), Session: str(a, "session"),
					Limit: num(a, "limit", 20),
				})
			},
		},
		{
			Name:        "get_session",
			Description: "セッションの本文を取り出す。既定は会話行だけ（制御行は all=true で出る）。",
			Schema: obj(map[string]any{
				"id":    strProp("セッションID"),
				"after": intProp("この id より後ろから"),
				"limit": intProp("行数（既定100）"),
				"all":   map[string]any{"type": "boolean", "description": "制御行も出す"},
			}, "id"),
			Run: func(a map[string]any) (any, error) {
				id := str(a, "id")
				if id == "" {
					return nil, fmt.Errorf("セッションIDが要る")
				}
				all, _ := a["all"].(bool)
				return query.Messages(s.db, id, int64(num(a, "after", 0)),
					num(a, "limit", 100), all)
			},
		},
		{
			Name: "search_notes",
			Description: "Obsidian Vault のノートを引く。パスの部分一致（大文字小文字を区別）。" +
				"Vault から消えたノートも missing_at 付きで残っている。",
			Schema: obj(map[string]any{
				"q":       strProp("パスの部分一致"),
				"folder":  strProp("フォルダで絞る"),
				"kind":    strProp("種別（markdown / base / asset / canvas）"),
				"missing": strProp("only=消えたものだけ / hide=現存だけ"),
				"limit":   intProp("件数（既定50）"),
			}),
			Run: func(a map[string]any) (any, error) {
				return vault.Notes(s.db, vault.NoteOpts{
					Q: str(a, "q"), Folder: str(a, "folder"), Kind: str(a, "kind"),
					Missing: str(a, "missing"), Limit: num(a, "limit", 50),
				})
			},
		},
		{
			Name:        "get_note",
			Description: "ノートの本文と、被リンク・出ていくリンク・触ったセッションを返す。消えたノートも読める。",
			Schema: obj(map[string]any{
				"path": strProp("Vault ルートからの相対パス"),
				"id":   intProp("ノートID（path の代わりに）"),
			}),
			Run: func(a map[string]any) (any, error) {
				id := int64(num(a, "id", 0))
				if p := str(a, "path"); p != "" {
					ns, err := vault.Notes(s.db, vault.NoteOpts{Q: p, Limit: 2})
					if err != nil {
						return nil, err
					}
					if len(ns) == 0 {
						return nil, fmt.Errorf("%q に当たるノートが無い", p)
					}
					id = ns[0].ID
				}
				if id == 0 {
					return nil, fmt.Errorf("path か id が要る")
				}
				n, err := vault.OneNote(s.db, id)
				if err != nil || n == nil {
					return nil, fmt.Errorf("ノート %d が無い", id)
				}
				body, _ := vault.NoteBody(s.db, id)
				out, _ := vault.OutLinks(s.db, id)
				back, _ := vault.Backlinks(s.db, id)
				touch, _ := vault.NoteTouches(s.db, id, 20)
				return map[string]any{
					"note": n, "body": string(body),
					"links_out": out, "backlinks": back, "sessions": touch,
				}, nil
			},
		},
		{
			Name: "views_list",
			Description: "Vault の .base が定義するビューを並べる。" +
				"columns は実在する全列で、pinned は定義が明示的に前へ出した数。",
			Schema: obj(map[string]any{}),
			Run: func(a map[string]any) (any, error) {
				bases, recs, err := s.viewData()
				if err != nil {
					return nil, err
				}
				out := []map[string]any{}
				for _, b := range bases {
					for i := range b.Views {
						v := &b.Views[i]
						row := map[string]any{
							"id": b.Name + "/" + v.Name, "base": b.Name,
							"name": v.Name, "kind": v.Kind,
						}
						if r, err := views.Run(b, v, recs); err == nil {
							pinned := 0
							for _, c := range r.Columns {
								if c.Pinned {
									pinned++
								}
							}
							row["rows"], row["columns"], row["pinned"] = r.Total, len(r.Columns), pinned
						} else {
							row["error"] = err.Error()
						}
						out = append(out, row)
					}
				}
				return out, nil
			},
		},
		{
			Name: "view",
			Description: "ビューを実行して行と列を返す。**人が画面で見るのと同じ結果**。" +
				"列は定義が挙げたものだけでなく実在する全部が入る。",
			Schema: obj(map[string]any{
				"id":    strProp(`ビューID（"Health/テーブル" のような base/name）`),
				"limit": intProp("行数の上限（既定50）"),
				"cols":  strProp("all を渡すと空の列も返す。既定は値のある列だけ"),
			}, "id"),
			Run: func(a map[string]any) (any, error) {
				id := str(a, "id")
				bases, recs, err := s.viewData()
				if err != nil {
					return nil, err
				}
				for _, b := range bases {
					for i := range b.Views {
						if b.Name+"/"+b.Views[i].Name != id {
							continue
						}
						r, err := views.Run(b, &b.Views[i], recs)
						if err != nil {
							return nil, err
						}
						return clipResult(r, num(a, "limit", 50), str(a, "cols") == "all"), nil
					}
				}
				return nil, fmt.Errorf("ビュー %q が無い", id)
			},
		},
		{
			Name:        "usage",
			Description: "トークン消費の内訳（day / model / session / project）とプラン残量。",
			Schema: obj(map[string]any{
				"by":    strProp("day / model / session / project（既定 day）"),
				"limit": intProp("件数（既定30）"),
			}),
			Run: func(a map[string]any) (any, error) {
				by := str(a, "by")
				if by == "" {
					by = "day"
				}
				rows, err := query.UsageSummary(s.db, by, "", "", num(a, "limit", 30))
				if err != nil {
					return nil, err
				}
				return map[string]any{"by": by, "rows": rows}, nil
			},
		},
	}
}

// clipResult は行と列を切り詰める。モデルに 2,687行×107列 をそのまま
// 渡しても読めないし、文脈を食い潰す。**切ったことは必ず書く。**
func clipResult(r *views.Result, limit int, allCols bool) map[string]any {
	cols := r.Columns
	if !allCols {
		var keep []views.Column
		for _, c := range cols {
			if c.Pinned || c.Filled > 0 {
				keep = append(keep, c)
			}
		}
		cols = keep
	}
	keys := make([]string, 0, len(cols))
	for _, c := range cols {
		keys = append(keys, c.Key)
	}

	rows := []map[string]string{}
	shown := 0
	for _, g := range r.Groups {
		for _, rec := range g.Rows {
			if shown >= limit {
				break
			}
			cell := make(map[string]string, len(keys))
			for _, k := range keys {
				if v := rec.Cells[k]; v != "" {
					cell[k] = v
				}
			}
			rows = append(rows, cell)
			shown++
		}
	}

	out := map[string]any{
		"view": r.View, "kind": r.Kind,
		"columns": cols, "rows": rows,
		"total_rows": r.Total, "shown_rows": shown,
		"total_columns": len(r.Columns), "shown_columns": len(cols),
	}
	if shown < r.Total {
		out["truncated"] = fmt.Sprintf("全 %d 行のうち先頭 %d 行", r.Total, shown)
	}
	if len(cols) < len(r.Columns) {
		out["columns_folded"] = fmt.Sprintf(
			"値の入っていない %d 列をたたんだ（cols=\"all\" で全部返す）",
			len(r.Columns)-len(cols))
	}
	if len(r.Summary) > 0 {
		out["summary"] = r.Summary
	}
	if len(r.Warnings) > 0 {
		out["warnings"] = r.Warnings
	}
	return out
}
