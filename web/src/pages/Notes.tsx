import { Link, useSearchParams } from 'react-router-dom'
import { api } from '../api'
import { Empty, Failed, Loading, num, short, useAsync } from '../ui'

const KINDS = [
  ['', 'すべての種別'], ['markdown', 'ノート'], ['base', 'Bases'],
  ['asset', '添付'], ['canvas', 'Canvas'], ['other', 'その他'],
] as const

// 絞り込みは全部クエリ文字列に置く（D-016）。
export default function Notes() {
  const [sp, setSp] = useSearchParams()
  const q = sp.get('q') ?? ''
  const folder = sp.get('folder') ?? ''
  const kind = sp.get('kind') ?? ''
  const missing = sp.get('missing') ?? ''

  const vaults = useAsync(() => api.vaults(), [])
  const list = useAsync(
    () => api.notes({ q, folder, kind, missing, n: 500 }),
    [q, folder, kind, missing],
  )

  const set = (patch: Record<string, string>) => {
    const next = new URLSearchParams(sp)
    for (const [k, v] of Object.entries(patch)) {
      if (v) next.set(k, v)
      else next.delete(k)
    }
    setSp(next)
  }

  const v = vaults.data?.[0]

  return (
    <>
      <h2>ノート</h2>
      <p className="sub muted">
        {v
          ? <>{v.root} を索引したもの。<strong>Camp は読むだけ</strong>で、書き手は Obsidian のまま。
              最終走査 {short(v.scanned_at)}</>
          : 'まだ索引していない（campd vault index）'}
      </p>

      <form className="filters" onSubmit={(e) => e.preventDefault()}>
        <input
          type="search" placeholder="パスの部分一致（大文字小文字を区別）"
          defaultValue={q} key={q}
          onKeyDown={(e) => {
            if (e.key === 'Enter') set({ q: (e.target as HTMLInputElement).value })
          }}
        />
        <select value={kind} onChange={(e) => set({ kind: e.target.value })}>
          {KINDS.map(([val, label]) => <option key={val} value={val}>{label}</option>)}
        </select>
        <select value={missing} onChange={(e) => set({ missing: e.target.value })}>
          <option value="">現存も消えたものも</option>
          <option value="hide">現存だけ</option>
          <option value="only">消えたものだけ</option>
        </select>
      </form>

      {list.loading && <Loading />}
      {list.error && <Failed error={list.error} />}
      {list.data?.length === 0 && <Empty>その条件のノートは無い。</Empty>}

      {list.data && list.data.length > 0 && (
        <>
          <p className="muted">{num(list.data.length)} 件</p>
          <ul className="rows">
            {list.data.map((n) => (
              <li key={n.id}>
                <Link to={`/notes/${n.id}`}>{n.title || n.path}</Link>
                {n.missing_at && <span className="tag gone">消えている</span>}
                <div className="row-meta">
                  <span className="mono">{n.path}</span>
                  {n.backlinks > 0 && <span>被リンク {num(n.backlinks)}</span>}
                  {n.links > 0 && <span>リンク {num(n.links)}</span>}
                  {n.touches > 0 && <span>セッションが触った {num(n.touches)}</span>}
                </div>
              </li>
            ))}
          </ul>
        </>
      )}
    </>
  )
}
