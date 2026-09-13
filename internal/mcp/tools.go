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
					// **黙って1つ選ばない。** パスの部分一致なので候補が
					// 複数出うる。Phase 1 で曖昧な wikilink について
					// 決めたのと同じ扱いにする。完全一致があればそれを採る。
					ns, err := vault.Notes(s.db, vault.NoteOpts{Q: p, Limit: 10})
					if err != nil {
						return nil, err
					}
					if len(ns) == 0 {
						return nil, fmt.Errorf("%q に当たるノートが無い", p)
					}
					id = 0
					for _, n := range ns {
						if n.Path == p {
							id = n.ID
							break
						}
					}
					if id == 0 {
						if len(ns) > 1 {
							paths := make([]string, 0, len(ns))
							for _, n := range ns {
								paths = append(paths, n.Path)
							}
							return nil, fmt.Errorf(
								"%q に当たるノートが %d 件ある。パスを絞るか id を渡す: %s",
								p, len(ns), strings.Join(paths, " / "))
						}
						id = ns[0].ID
					}
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
					if b.ParseError != "" {
						out = append(out, map[string]any{
							"id": b.Name + "/", "base": b.Name,
							"name": "(読めない)", "error": b.ParseError,
						})
						continue
					}
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
				"列は定義が挙げたものだけでなく実在する全部が入る。" +
				"groups にグループ別の集計、series に時系列（区切りごとに集約済みの点）が入る。" +
				"**集計と時系列は自分で行から計算し直さない**（同じ日に複数行ある残高などで間違える）。",
			Schema: obj(map[string]any{
				"id":    strProp(`ビューID（"Health/テーブル" のような base/name）`),
				"limit": intProp("行数の上限（既定50）"),
				"cols":  strProp("all を渡すと空の列も返す。既定は値のある列だけ"),
			}, "id"),
			Run: func(a map[string]any) (any, error) {
				id := str(a, "id")
				bases, recs, links, err := s.viewMaterial()
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
						views.AttachGraph(r, &b.Views[i], links)
						return clipResult(r, num(a, "limit", 50), str(a, "cols") == "all"), nil
					}
				}
				return nil, fmt.Errorf("ビュー %q が無い", id)
			},
		},
		{
			Name: "note_graph",
			Description: "ノートからリンクを辿ったグラフ（向きを問わず depth 歩以内）。path も id も無ければ" +
				"リンクの一番多いノートを中心にする。節は中心に近い順で、hops が歩数、degree がこのグラフの中のつながりの数。" +
				"辺の from が to を指す。リンクを1本も持たないノートは節にしない（unlinked が数）。",
			Schema: obj(map[string]any{
				"path":  strProp("中心のノートの Vault ルートからの相対パス（完全一致）"),
				"id":    intProp("中心のノートID（path の代わりに）"),
				"depth": intProp("何歩まで辿るか 1〜4（既定1）。0 はリンクを持つノート全部"),
				"limit": intProp("節の上限（既定100）。遠いほうから切る"),
			}),
			Run: func(a map[string]any) (any, error) {
				_, recs, links, err := s.viewMaterial()
				if err != nil {
					return nil, err
				}
				center := int64(num(a, "id", 0))
				if p := str(a, "path"); p != "" {
					center = -1
					for _, r := range recs {
						if r.NPath == p {
							center = r.NoteID
						}
					}
					if center < 0 {
						return nil, fmt.Errorf("%q というノートは無い（完全一致で探す。search_notes で探してから渡す）", p)
					}
				}
				depth := num(a, "depth", 1)
				if depth < 0 || depth > 4 {
					return nil, fmt.Errorf("depth は 0〜4")
				}
				g, err := views.Neighborhood(recs, links, center, depth)
				if err != nil {
					return nil, err
				}
				return graphForModel(g, num(a, "limit", 100)), nil
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

	// **グループ別の集計をここで渡す**（M50、2026-09-13）。D-004 の約束は「集計は Camp が
	// 計算し、モデルには算術させない」。
	//
	// **全体の集計（`r.Summary`）は前から渡していた**（下の `out["summary"]`）。落ちていたのは
	// **グループ鍵（`g.Key`）とグループ別集計（`g.Summary`）**だけ——`r.Groups` を平らにして
	// 行を積んでいたので、`カード-すべて` の月別合計はモデルに見えなかった。
	//
	// 行は切り詰めても**集計は切り詰めない**（数個の数で、これが正確さの源だから）。
	groups := make([]map[string]any, 0, len(r.Groups))
	for _, g := range r.Groups {
		e := map[string]any{"rows": len(g.Rows)}
		if g.Key != "" {
			e["key"] = g.Key
		}
		if len(g.Summary) > 0 {
			e["summary"] = g.Summary
		}
		groups = append(groups, e)
	}

	out := map[string]any{
		"view": r.View, "kind": r.Kind,
		"columns": cols, "rows": rows,
		"total_rows": r.Total, "shown_rows": shown,
		"total_columns": len(r.Columns), "shown_columns": len(cols),
		// **グループは鍵と件数と集計だけ。** 行そのものは上の rows（切り詰め済み）にある。
		"groups": groups,
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
	// **時系列も人と同じものを渡す**（M51）。人が「日ごとの残高推移」を見ているのに、モデルが
	// 行の先頭だけを読んで自分で集約し直すと、同日8件の日で残高を間違える。
	// 点は新しいほうから切り詰める（切ったことは書く）。描けない理由（`error`）は切らない。
	if len(r.Series) > 0 {
		series := make([]map[string]any, 0, len(r.Series))
		for _, s := range r.Series {
			e := map[string]any{"key": s.Key, "label": s.Label, "rows": s.Rows}
			if s.Measure != "" {
				e["measure"] = s.Measure
			}
			if s.Error != "" {
				e["error"] = s.Error
			}
			if s.Skipped > 0 {
				e["skipped_rows"] = s.Skipped
			}
			pts := s.Points
			if len(pts) > maxSeriesPoints {
				e["truncated"] = fmt.Sprintf("全 %d 点のうち新しい %d 点", len(pts), maxSeriesPoints)
				pts = pts[len(pts)-maxSeriesPoints:]
			}
			if len(pts) > 0 {
				e["points"] = pts
			}
			series = append(series, e)
		}
		out["time"] = r.Time
		out["series"] = series
	}
	// **グラフのビューは節と辺も渡す**（M52）。行（上）とは別に、つながりそのものを。
	if r.Graph != nil {
		out["graph"] = graphForModel(r.Graph, 200)
	}
	return out
}

// graphForModel はグラフを節の上限で切り、辺をパスで書く（id の突き合わせをモデルにさせない）。
// 節は近い順に並んでいるので、切ると遠いほうが落ちる。**切ったことは書く。**
//
// **degree は返す節と辺の中で数え直す**（実装後レビュー、codex の指摘7）。切る前の数を転記すると、
// 星形の中心が「返したグラフの中で 399 のつながり」と読めてしまう。切ったと書くときは**切る前の
// 全体の数**を言う（サーバーが先に 400 で切っていても、元の数を失わない）。
func graphForModel(g *views.Graph, limit int) map[string]any {
	if limit <= 0 {
		limit = 100
	}
	nodes := g.Nodes
	out := map[string]any{"unlinked": g.Unlinked, "total_nodes": g.Total}
	if len(nodes) > limit {
		nodes = nodes[:limit]
	}
	if len(nodes) < g.Total {
		out["truncated"] = fmt.Sprintf("全 %d 節のうち近い %d 節（degree は返した節の中で数えた）", g.Total, len(nodes))
	}
	path := make(map[int64]string, len(nodes))
	for _, n := range nodes {
		path[n.ID] = n.Path
	}
	es := []map[string]string{}
	degree := map[int64]int{}
	pair := map[[2]int64]bool{}
	for _, e := range g.Edges {
		if path[e.From] == "" || path[e.To] == "" {
			continue
		}
		es = append(es, map[string]string{"from": path[e.From], "to": path[e.To]})
		a, b := e.From, e.To
		if a > b {
			a, b = b, a
		}
		if !pair[[2]int64{a, b}] { // 往復のリンクはつながり1つ
			pair[[2]int64{a, b}] = true
			degree[e.From]++
			degree[e.To]++
		}
	}
	ns := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		e := map[string]any{"path": n.Path, "hops": n.Hops, "degree": degree[n.ID]}
		if len(n.Tags) > 0 {
			e["tags"] = n.Tags
		}
		ns = append(ns, e)
	}
	out["nodes"], out["edges"] = ns, es
	if c := path[g.Center]; c != "" {
		out["center"], out["depth"] = c, g.Depth
	}
	return out
}

// maxSeriesPoints は1本あたりに渡す点の上限。日ごとで1年ぶん。
const maxSeriesPoints = 366
