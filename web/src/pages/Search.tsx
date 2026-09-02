import { Link, useSearchParams } from 'react-router-dom'
import { api } from '../api'
import { Empty, Failed, Loading, short, useAsync } from '../ui'

// 検索語はURLに置く。/search?q=… をそのままブックマークできる。
export default function Search() {
  const [sp, setSp] = useSearchParams()
  const q = sp.get('q') ?? ''
  const kind = sp.get('kind') ?? ''

  const hits = useAsync(
    () => (q ? api.search(q, { kind, limit: 50 }) : Promise.resolve([])),
    [q, kind],
  )

  const set = (patch: Record<string, string>) => {
    const next = new URLSearchParams(sp)
    for (const [k, v] of Object.entries(patch)) {
      if (v) next.set(k, v)
      else next.delete(k)
    }
    setSp(next)
  }

  return (
    <>
      <h2>検索</h2>
      <p className="sub muted">
        日本語は2文字から引ける（bigram 索引）。tool_result の中身も対象。
      </p>

      <form className="filters" onSubmit={(e) => e.preventDefault()}>
        <input
          type="search" placeholder="検索語" defaultValue={q} key={q} autoFocus
          onKeyDown={(e) => {
            if (e.key === 'Enter') set({ q: (e.target as HTMLInputElement).value })
          }}
        />
        <select value={kind} onChange={(e) => set({ kind: e.target.value })}>
          <option value="">すべての種別</option>
          <option value="text">text</option>
          <option value="thinking">thinking</option>
          <option value="tool_use">tool_use</option>
          <option value="tool_result">tool_result</option>
        </select>
      </form>

      {!q && <Empty>語を入れると引く。</Empty>}
      {q && hits.loading && <Loading />}
      {hits.error && <Failed error={hits.error} />}
      {q && hits.data && hits.data.length === 0 && <Empty>該当なし</Empty>}

      <ul className="rows">
        {hits.data?.map((h) => (
          <li key={h.block_id}>
            <div className="row-title">
              <Link to={`/sessions/${h.session_id}`}>{h.title || '(タイトル無し)'}</Link>
            </div>
            <div className="row-meta">
              <span>{short(h.timestamp)}</span>
              <span>{h.tool_name ? `${h.kind} · ${h.tool_name}` : h.kind}</span>
            </div>
            <div className="excerpt">{h.snippet}</div>
          </li>
        ))}
      </ul>
    </>
  )
}
