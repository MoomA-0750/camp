import { Link, useSearchParams } from 'react-router-dom'
import { api, type Graph as G } from '../api'
import GraphPlot from '../GraphPlot'
import { Failed, Loading, num, useAsync } from '../ui'

/**
 * ノートから辿るグラフ（Phase 4 / M52、2026-09-13）。
 *
 * **中心と歩数は URL に持つ。** 節を押すと中心が移り、ブラウザの「戻る」で前の中心へ戻れる
 * （受け入れ「ハブから辿って戻れる」）。中心を指定しなければリンクの一番多いノート。
 *
 * **リンクを1本も持たないノートは出さない。** 実データではリンクを持つノートは1割に満たず、
 * 全部出すと点の海になる。出さなかった数は必ず書く。
 */
export default function Graph() {
  const [sp, setSp] = useSearchParams()
  const note = Number(sp.get('note') ?? 0) || undefined
  const depth = sp.has('depth') ? Number(sp.get('depth')) : 1
  const g = useAsync(() => api.graph({ note, depth }), [note, depth])

  const go = (next: { note?: number; depth?: number }) => {
    const p = new URLSearchParams()
    const n = next.note ?? note
    if (n) p.set('note', String(n))
    p.set('depth', String(next.depth ?? depth))
    setSp(p)
  }

  const data = g.data
  const center = data?.nodes.find((n) => n.id === data.center)
  return (
    <>
      <h2>グラフ{center && <>: {center.name || center.path}</>}</h2>
      <p className="sub muted">
        ノートどうしのリンク。節を押すとそのノートを中心にする（ブラウザの「戻る」で戻れる）。
        {!note && ' 中心を選んでいないので、リンクの一番多いノートから始める。'}
      </p>
      <form className="filters" onSubmit={(e) => e.preventDefault()}>
        {[1, 2, 3, 0].map((d) => (
          <button key={d} type="button" className={'btn' + (d === depth ? ' on' : '')}
            aria-pressed={d === depth} onClick={() => go({ depth: d })}>
            {d === 0 ? 'リンクのあるノート全部' : `${d} 歩`}
          </button>
        ))}
      </form>

      {g.loading && !data && <Loading />}
      {g.error && <Failed error={g.error} />}
      {data && <Body g={data} onPick={(id) => go({ note: id })} />}
    </>
  )
}

function Body({ g, onPick }: { g: G; onPick: (id: number) => void }) {
  const byId = new Map(g.nodes.map((n) => [n.id, n]))
  const out = g.edges.filter((e) => e.from === g.center).map((e) => byId.get(e.to)!)
  const back = g.edges.filter((e) => e.to === g.center).map((e) => byId.get(e.from)!)
  const center = g.center ? byId.get(g.center) : undefined

  return (
    <>
      <p className="sub muted">
        {num(g.nodes.length)} 節 / {num(g.edges.length)} 辺
        {' · '}リンクを持たないノート {num(g.unlinked)} 件は出していない
        {center && <>{' · '}<Link to={`/notes/${center.id}`}>中心のノートを開く</Link></>}
      </p>
      {g.truncated && <p className="notice">多すぎるので切った: {g.truncated}（遠いほうから）</p>}
      {g.nodes.length === 0
        ? <p className="muted">リンクが1本も無い。</p>
        : <GraphPlot graph={g} onPick={(n) => onPick(n.id)} />}

      {center && (
        <div className="graph-refs">
          <section>
            <h3>出ていくリンク（{num(out.length)}）</h3>
            <RefLinks nodes={out} onPick={onPick} empty="このノートからのリンクは無い。" />
          </section>
          <section>
            <h3>被リンク（{num(back.length)}）</h3>
            <RefLinks nodes={back} onPick={onPick} empty="このノートを指しているノートは無い。" />
          </section>
        </div>
      )}
    </>
  )
}

function RefLinks({ nodes, onPick, empty }: {
  nodes: { id: number; path: string; name: string }[]; onPick: (id: number) => void; empty: string
}) {
  if (nodes.length === 0) return <p className="muted">{empty}</p>
  return (
    <ul className="refs">
      {[...nodes].sort((a, b) => a.path.localeCompare(b.path)).map((n) => (
        <li key={n.id}>
          <a href={`?note=${n.id}`} onClick={(e) => { e.preventDefault(); onPick(n.id) }} title={n.path}>
            {n.name || n.path}
          </a>
          <span className="muted mono"> {n.path}</span>
        </li>
      ))}
    </ul>
  )
}
