import { useMemo } from 'react'
import type { Graph, GraphNode } from './api'
import { useWidth } from './useWidth'

/**
 * グラフを SVG で描く（Phase 4 / M52、2026-09-13）。**依存を足さない**——力学的な配置を手で書く。
 *
 * **乱数を使わない。** 開くたびに形が変わると、前に見た位置で探せない。初めの位置は
 * 並び（サーバーが中心に近い順に返す）から黄金角の渦巻きで決め、あとは決まった回数だけ
 * 押し引きする。同じグラフなら同じ絵になる。
 */
export default function GraphPlot({ graph, colors, onPick, height = 520 }: {
  graph: Graph
  colors?: { tag: string; color: string }[]
  onPick?: (n: GraphNode) => void
  height?: number
}) {
  const [box, W] = useWidth()
  const H = height
  const pos = useMemo(() => layout(graph, W, H), [graph, W, H])
  const index = new Map(graph.nodes.map((n, i) => [n.id, i]))

  const labelled = (n: GraphNode, i: number) =>
    graph.nodes.length <= 40 || n.id === graph.center || i < 12
  const fill = (n: GraphNode) => {
    for (const c of colors ?? []) if (n.tags?.includes(c.tag)) return c.color
    return undefined
  }
  const radius = (n: GraphNode) => 3.5 + Math.min(7, Math.sqrt(n.degree) * 1.6)

  return (
    <div ref={box}>
      <svg className="graph" viewBox={`0 0 ${W} ${H}`} width={W} height={H} role="img"
        aria-label={`${graph.nodes.length} 節・${graph.edges.length} 辺のグラフ`}>
        {graph.edges.map((e, k) => {
          const a = pos[index.get(e.from)!], b = pos[index.get(e.to)!]
          return a && b ? <line key={k} className="edge" x1={a.x} y1={a.y} x2={b.x} y2={b.y} /> : null
        })}
        {graph.nodes.map((n, i) => {
          const p = pos[i]
          const cls = ['node', n.id === graph.center ? 'center' : '', n.hops < 0 ? 'far' : '']
            .filter(Boolean).join(' ')
          return (
            <g key={n.id} className={cls} transform={`translate(${p.x.toFixed(1)},${p.y.toFixed(1)})`}
              onClick={onPick ? () => onPick(n) : undefined} data-path={n.path}>
              <circle r={radius(n)} style={fill(n) ? { fill: fill(n) } : undefined}>
                <title>{n.path}{n.hops > 0 ? `（${n.hops} 歩）` : ''}・つながり {n.degree}</title>
              </circle>
              {/* 右半分の節は文字を左に出す（右端で切れないように。2026-09-13、実ブラウザで見つけた）。 */}
              {labelled(n, i) && (p.x > W / 2
                ? <text x={-radius(n) - 3} y={4} className="label" textAnchor="end">{short(n.name || n.path)}</text>
                : <text x={radius(n) + 3} y={4} className="label">{short(n.name || n.path)}</text>)}
            </g>
          )
        })}
      </svg>
      {colors && colors.length > 0 && (
        <p className="sub muted legend">
          {colors.map((c) => (
            <span key={c.tag}><span className="swatch" style={{ background: c.color }} />#{c.tag}</span>
          ))}
        </p>
      )}
    </div>
  )
}

const short = (s: string) => (s.length > 18 ? s.slice(0, 17) + '…' : s)

/**
 * 配置。**同じ入力なら同じ出力**（乱数なし）。返す位置は余白を取った枠の中に収める。
 *
 * **つながりの島ごとに配置し、島を行に詰めて並べる**（2026-09-13、実ブラウザで見つけた）。
 * 全体を一度に押し引きすると、2つの拠点の島が互いに押し合って
 * 枠の両端へ飛び、右の島は文字が枠の外で切れ、島の中の節は潰れて重なっていた。
 */
export function layout(graph: Graph, W: number, H: number): { x: number; y: number }[] {
  const n = graph.nodes.length
  if (n === 0) return []
  const pad = 28
  const index = new Map(graph.nodes.map((nd, i) => [nd.id, i]))
  const edges = graph.edges
    .map((e) => [index.get(e.from), index.get(e.to)] as const)
    .filter((e): e is readonly [number, number] => e[0] !== undefined && e[1] !== undefined)

  // 島（連結成分）。並びはサーバーの順（中心に近い順）で最初に出た節の順。
  const root = Array.from({ length: n }, (_, i) => i)
  const find = (i: number): number => (root[i] === i ? i : (root[i] = find(root[i])))
  for (const [a, b] of edges) {
    const ra = find(a), rb = find(b)
    if (ra !== rb) root[Math.max(ra, rb)] = Math.min(ra, rb)
  }
  const groups = new Map<number, number[]>()
  for (let i = 0; i < n; i++) {
    const r = find(i)
    if (!groups.has(r)) groups.set(r, [])
    groups.get(r)!.push(i)
  }
  const islands = [...groups.values()].sort((a, b) => b.length - a.length || a[0] - b[0])

  const k = 60 // 配置の単位。最後に枠へ合わせて伸び縮みさせるので、絶対値に意味は無い
  const xs = new Float64Array(n), ys = new Float64Array(n)
  const boxes = islands.map((members) => {
    forceLayout(members, edges, xs, ys, k)
    let minX = Infinity, maxX = -Infinity, minY = Infinity, maxY = -Infinity
    for (const i of members) {
      minX = Math.min(minX, xs[i]); maxX = Math.max(maxX, xs[i])
      minY = Math.min(minY, ys[i]); maxY = Math.max(maxY, ys[i])
    }
    const m = k * 0.8 // 島の周りの余白（文字の分も）
    return { members, minX: minX - m, minY: minY - m, w: maxX - minX + 2 * m, h: maxY - minY + 2 * m }
  })

  // 島を行に詰める。**行の幅は、いちばん大きく描ける幅を選ぶ**（1行に全部〜1島1行を全部試す）。
  // 縦横比から決め打ちすると、横長の枠に2つの島が縦に積まれて小さく潰れた（2026-09-13、画面写し）。
  const pack = (rowW: number) => {
    let cx = 0, cy = 0, rowH = 0, totalW = 0
    const at = boxes.map((b) => {
      if (cx > 0 && cx + b.w > rowW + 1e-9) { cy += rowH; cx = 0; rowH = 0 }
      const o = { x: cx - b.minX, y: cy - b.minY }
      cx += b.w; rowH = Math.max(rowH, b.h); totalW = Math.max(totalW, cx)
      return o
    })
    const totalH = cy + rowH
    return { at, totalW, totalH, s: Math.min((W - 2 * pad) / totalW, (H - 2 * pad) / totalH) }
  }
  let best = pack(Infinity)
  let acc = 0
  for (const b of boxes) {
    acc += b.w
    const p = pack(Math.max(acc, ...boxes.map((x) => x.w)))
    if (p.s > best.s) best = p
  }
  const { at, totalW, totalH, s } = best

  const ox = (W - totalW * s) / 2, oy = (H - totalH * s) / 2
  const out = new Array<{ x: number; y: number }>(n)
  boxes.forEach((b, bi) => {
    for (const i of b.members) out[i] = { x: ox + (xs[i] + at[bi].x) * s, y: oy + (ys[i] + at[bi].y) * s }
  })
  return out
}

/** 島1つを押し引きする（Fruchterman–Reingold を小さく）。初めの位置は黄金角の渦巻き。 */
function forceLayout(members: number[], allEdges: readonly (readonly [number, number])[],
  xs: Float64Array, ys: Float64Array, k: number) {
  const m = members.length
  const golden = Math.PI * (3 - Math.sqrt(5))
  const local = new Map(members.map((g, i) => [g, i]))
  const x = new Float64Array(m), y = new Float64Array(m)
  for (let i = 0; i < m; i++) {
    const r = k * 0.6 * Math.sqrt(i)
    x[i] = Math.cos(i * golden) * r
    y[i] = Math.sin(i * golden) * r
  }
  const edges = allEdges.filter(([a]) => local.has(a)).map(([a, b]) => [local.get(a)!, local.get(b)!] as const)
  const iters = m > 200 ? 160 : 300
  let temp = k * 2
  const dx = new Float64Array(m), dy = new Float64Array(m)
  for (let it = 0; it < iters && m > 1; it++) {
    dx.fill(0); dy.fill(0)
    for (let i = 0; i < m; i++) {
      for (let j = i + 1; j < m; j++) {
        let ex = x[i] - x[j], ey = y[i] - y[j]
        let d2 = ex * ex + ey * ey
        if (d2 < 0.01) { ex = (i - j) * 0.1; ey = 0.1; d2 = ex * ex + ey * ey }
        const f = (k * k) / d2
        dx[i] += ex * f; dy[i] += ey * f
        dx[j] -= ex * f; dy[j] -= ey * f
      }
    }
    for (const [a, b] of edges) {
      const ex = x[a] - x[b], ey = y[a] - y[b]
      const d = Math.sqrt(ex * ex + ey * ey) || 0.01
      const f = d / k
      dx[a] -= ex * f; dy[a] -= ey * f
      dx[b] += ex * f; dy[b] += ey * f
    }
    for (let i = 0; i < m; i++) {
      dx[i] -= x[i] * 0.02; dy[i] -= y[i] * 0.02
      const d = Math.sqrt(dx[i] * dx[i] + dy[i] * dy[i]) || 1
      const step = Math.min(d, temp)
      x[i] += (dx[i] / d) * step
      y[i] += (dy[i] / d) * step
    }
    temp *= 0.97
  }
  members.forEach((g, i) => { xs[g] = x[i]; ys[g] = y[i] })
}
