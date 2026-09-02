import { useMemo } from 'react'
import { Link, useParams, useSearchParams } from 'react-router-dom'
import { api, type ViewColumn } from '../api'
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
                <th key={c.key} className={c.pinned ? 'pin' : ''} title={c.key}>
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
