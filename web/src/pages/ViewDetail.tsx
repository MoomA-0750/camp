import { useMemo } from 'react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { api, type ViewColumn, type ViewResult } from '../api'
import GraphPlot from '../GraphPlot'
import TimeChart from '../TimeChart'
import { Empty, Failed, Loading, num, useAsync } from '../ui'

/**
 * 107列の表をどう見せるか。
 *
 * 全部出すと決めた以上、「多すぎて読めない」は設計で引き受ける必要がある。
 *   - 既定は**値の入っている列だけ**（Health は2,687行のうち大半が空の列を持つ）
 *   - 定義が挙げた列（pinned）は必ず先頭に固定
 *   - 全列を見たいときはURLで切り替える（?cols=all）
 * 列を消すのではなく「畳んで、開けるようにする」。消すと Bases に戻ってしまう。
 */
export default function ViewDetail() {
  const params = useParams()
  const id = (params['*'] ?? '') as string
  const [sp, setSp] = useSearchParams()
  const navigate = useNavigate()
  const showAll = sp.get('cols') === 'all'
  const res = useAsync(() => api.view(id), [id])

  const cols = useMemo<ViewColumn[]>(() => {
    const all = res.data?.columns ?? []
    if (showAll) return all
    return all.filter((c) => c.pinned || c.filled > 0)
  }, [res.data, showAll])

  if (res.loading) return <Loading />
  if (res.error) return <Failed error={res.error} />
  const r = res.data
  if (!r) return <Empty>そのビューは無い。</Empty>

  const hidden = r.columns.length - cols.length
  // 描き方（2026-09-13 から）。独自定義が `emit` を持っていればその順に描く。
  // **チャートだけを挙げた定義でも行は捨てない**——たたんで下に置く（開けば読める）。
  const human = r.emit?.human ?? []
  const charts = human.filter((h) => h.kind === 'chart')
  // グラフのビュー（2026-09-13 から）。節と辺はサーバーが行から作って渡す（`r.graph`）。
  // 描き方の指定が無くても、種別が graph なら描く（色分けが無いだけ）。
  const graphEnc = human.find((h) => h.kind === 'graph') ?? (r.graph ? { kind: 'graph' } : undefined)
  const drawn = charts.length > 0 || (graphEnc !== undefined && r.graph !== undefined)
  const tableEnc = human.find((h) => h.kind === 'table')

  return (
    <>
      <h2>{id}</h2>
      <p className="sub muted">
        {num(r.total)} 行 / {num(r.columns.length)} 列
        （定義が挙げたのは {num(r.columns.filter((c) => c.pinned).length)}、
        残りは自動で出た）
        {' · '}<Link to="/views">ビュー一覧</Link>
      </p>

      {r.warnings?.map((w, i) => (
        <p key={i} className="notice">読めなかった式: {w}</p>
      ))}

      {charts.map((enc, ci) => (enc.values ?? []).map((key) => {
        const s = r.series?.find((x) => x.key === key)
        return s
          ? <TimeChart key={`${ci}-${key}`} series={s} enc={enc} />
          : <p key={`${ci}-${key}`} className="notice">{key} の時系列が返ってきていない。</p>
      }))}

      {graphEnc && r.graph && (
        <section className="chart">
          <p className="sub muted">
            {num(r.graph.nodes.length)} 節 / {num(r.graph.edges.length)} 辺
            {' · '}行どうしのリンクを持たない {num(r.graph.unlinked)} 件は出していない
            {' · '}節を押すとそのノートから辿るグラフへ
          </p>
          {r.graph.nodes.length === 0
            ? <p className="muted">行どうしのリンクが無い。</p>
            : <GraphPlot graph={r.graph} colors={graphEnc.colors}
                onPick={(n) => navigate(`/graph?note=${n.id}&depth=1`)} />}
        </section>
      )}

      {drawn && !tableEnc
        ? (
          <details className="rows-fold">
            <summary>元の行を見る（{num(r.total)} 行）</summary>
            <Rows r={r} cols={cols} hidden={hidden} showAll={showAll} sp={sp} setSp={setSp} />
          </details>
        )
        : <Rows r={r} cols={cols} hidden={hidden} showAll={showAll} sp={sp} setSp={setSp}
            widths={tableEnc?.widths} />}
    </>
  )
}

function Rows({ r, cols, hidden, showAll, sp, setSp, widths }: {
  r: ViewResult; cols: ViewColumn[]; hidden: number; showAll: boolean
  sp: URLSearchParams; setSp: (next: URLSearchParams) => void
  widths?: Record<string, number>
}) {
  return (
    <>
      <form className="filters" onSubmit={(e) => e.preventDefault()}>
        <label>
          <input
            type="checkbox" checked={showAll}
            onChange={(e) => {
              const next = new URLSearchParams(sp)
              if (e.target.checked) next.set('cols', 'all')
              else next.delete('cols')
              setSp(next)
            }}
          />
          {' '}空の列も出す{hidden > 0 && `（いま ${num(hidden)} 列たたんでいる）`}
        </label>
      </form>

      <div className="wide">
        <table className="grid">
          <thead>
            <tr>
              {cols.map((c) => (
                <th key={c.key} className={c.pinned ? 'pin' : ''} title={c.key}
                  // 列幅は本人が Obsidian で合わせた値（`.base` の columnSize から持ち越した）。
                  style={widths?.[c.key] ? { width: widths[c.key], minWidth: widths[c.key] } : undefined}>
                  {c.label}
                  {c.formula && <span className="tag">式</span>}
                </th>
              ))}
            </tr>
          </thead>
          {r.groups.map((g, gi) => (
            <tbody key={gi}>
              {g.key && (
                <tr className="grouphead">
                  <th colSpan={cols.length} style={{ textAlign: 'left' }}>
                    {g.key}（{num(g.rows.length)}）
                  </th>
                </tr>
              )}
              {g.rows.slice(0, 200).map((row, ri) => (
                <tr key={ri}>
                  {cols.map((c) => (
                    <td key={c.key} className={c.numeric ? 'n' : ''}>
                      {row.cells[c.key] ?? ''}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          ))}
        </table>
      </div>
      {r.total > 200 && (
        <p className="muted">先頭 200 行だけ描いている（全 {num(r.total)} 行）。</p>
      )}
      {r.summary && Object.keys(r.summary).length > 0 && (
        <p className="muted">
          集計:{' '}
          {Object.entries(r.summary).map(([k, v]) => `${k} = ${num(Math.round(v))}`).join(' / ')}
        </p>
      )}
    </>
  )
}
