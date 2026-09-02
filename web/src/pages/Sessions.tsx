import { Link, useSearchParams } from 'react-router-dom'
import { api } from '../api'
import { Empty, Failed, Loading, num, short, useAsync } from '../ui'

// 絞り込みは全部クエリ文字列に置く。画面の状態がURLに乗っていれば、
// ブックマークもリロードも共有もそのまま効く。
export default function Sessions() {
  const [sp, setSp] = useSearchParams()
  const q = sp.get('q') ?? ''
  const host = sp.get('host') ?? ''
  const project = sp.get('project') ?? ''
  const cursor = sp.get('cursor') ?? ''

  const hosts = useAsync(() => api.hosts(), [])
  const list = useAsync(
    () => api.sessions({ q, host, project, cursor, limit: 30 }),
    [q, host, project, cursor],
  )

  const set = (patch: Record<string, string>) => {
    const next = new URLSearchParams(sp)
    for (const [k, v] of Object.entries(patch)) {
      if (v) next.set(k, v)
      else next.delete(k)
    }
    if (!('cursor' in patch)) next.delete('cursor') // 条件が変わったら先頭から
    setSp(next)
  }

  return (
    <>
      <h2>セッション</h2>
      <p className="sub muted">
        会話のあるものだけ。起動しただけの記録は隠している。
      </p>

      <form className="filters" onSubmit={(e) => e.preventDefault()}>
        <input
          type="search" placeholder="タイトル・最初の発話" defaultValue={q}
          key={q}
          onKeyDown={(e) => {
            if (e.key === 'Enter') set({ q: (e.target as HTMLInputElement).value })
          }}
        />
        <select value={host} onChange={(e) => set({ host: e.target.value })}>
          <option value="">すべてのホスト</option>
          {hosts.data?.map((h) => (
            <option key={h.name} value={h.name}>{h.name}（{h.sessions}）</option>
          ))}
        </select>
        <input
          type="search" placeholder="プロジェクトのパス" defaultValue={project} key={'p' + project}
          onKeyDown={(e) => {
            if (e.key === 'Enter') set({ project: (e.target as HTMLInputElement).value })
          }}
        />
      </form>

      {list.loading && <Loading />}
      {list.error && <Failed error={list.error} />}
      {list.data && list.data.sessions.length === 0 && <Empty>該当なし</Empty>}

      <ul className="rows">
        {list.data?.sessions.map((s) => (
          <li key={s.id}>
            <div className="row-title">
              <Link to={`/sessions/${s.id}`}>{s.title || '(タイトル無し)'}</Link>
            </div>
            <div className="row-meta">
              <span>{short(s.updated_at)}</span>
              <span>{s.project}</span>
              {s.git_branch && <span className="mono">{s.git_branch}</span>}
              <span>{num(s.conversation)} 往復 / {num(s.messages)} 行</span>
              {s.cost_usd ? <span>${s.cost_usd.toFixed(2)}</span> : null}
              {s.model && <span className="mono">{s.model}</span>}
            </div>
            {s.first_message && <div className="excerpt">{s.first_message}</div>}
          </li>
        ))}
      </ul>

      <div className="pager">
        {cursor && (
          <button className="btn" onClick={() => set({ cursor: '' })}>先頭へ</button>
        )}
        {list.data && list.data.sessions.length >= 30 && (
          <button className="btn" onClick={() => set({ cursor: list.data!.next_cursor })}>
            続き
          </button>
        )}
      </div>
    </>
  )
}
