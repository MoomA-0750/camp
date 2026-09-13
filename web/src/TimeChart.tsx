import type { ViewEncoding, ViewSeries } from './api'
import { num } from './ui'
import { useWidth } from './useWidth'

/**
 * 時系列1本を SVG で描く（Phase 4 / M51、2026-09-13）。**依存を足さない**——描くのは線と棒と
 * 目盛りだけで、ライブラリの重さに見合わない。
 *
 * **集約はサーバーが済ませている。** ここで点を足したり平均したりしない。銀行の残高は
 * 同じ日に最大8件あり、どれを取るか（`last` と、その日の中の並び）は定義が決めている。
 * 画面が行から描き直すと、その決定を黙って捨てることになる。
 *
 * 期間（`window`）だけは描き方なのでここで切る。サーバーは全期間を返す（モデルも同じ点を見る）。
 */
export default function TimeChart({ series, enc, now = new Date() }: {
  series: ViewSeries; enc: ViewEncoding; now?: Date
}) {
  const head = (
    <h3 className="chart-head">
      {series.label}
      {series.measure && <span className="tag">{measureWord[series.measure] ?? series.measure}</span>}
    </h3>
  )
  if (series.error) {
    return <section className="chart">{head}<p className="notice">描けない: {series.error}</p></section>
  }
  const all = series.points ?? []
  const cutoff = windowStart(enc.window, now)
  const pts = cutoff ? all.filter((p) => p.t >= cutoff.slice(0, p.t.length)) : all
  // 読めない値を含む区切りは点にしない（残りだけで合計すると嘘になる）。**点が抜けたことを言う。**
  const skipped = series.skipped
    ? (
      <p className="sub muted">
        値が読めなかった行 {num(series.skipped)} 件は描いていない。
        {series.holes ? <>そのため {num(series.holes)} 個の区切りは点にしていない。</> : null}
      </p>
    )
    : null
  if (pts.length === 0) {
    return (
      <section className="chart">
        {head}
        <p className="muted">
          {all.length === 0
            ? '点が無い。'
            : `${windowWord(enc.window)}に点が無い（最後の点は ${all[all.length - 1].t}）。`}
        </p>
        {skipped}
      </section>
    )
  }

  const last = pts[pts.length - 1]
  const crowded = pts.reduce((m, p) => Math.max(m, p.n), 0)
  return (
    <section className="chart">
      {head}
      <p className="sub muted">
        {windowWord(enc.window)} · {num(pts.length)} 点 · 最後 {last.t} に{' '}
        <strong className="n">{num(round(last.v))}</strong>
        {crowded > 1 && series.measure && (
          <>（1点に最大 {num(crowded)} 行を{measureWord[series.measure] ?? series.measure}でまとめた）</>
        )}
      </p>
      <Plot pts={pts} bar={enc.chart === 'bar'} label={series.label} measure={series.measure} />
      {skipped}
    </section>
  )
}

const measureWord: Record<string, string> = {
  only: '1行', last: '最後', first: '最初', sum: '合計', avg: '平均', min: '最小', max: '最大',
}

function windowStart(w: string | undefined, now: Date): string | null {
  const m = w?.match(/^last-(\d+)-days$/)
  if (!m) return null
  const d = new Date(now.getTime() - Number(m[1]) * 86400_000)
  const p2 = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())}`
}

function windowWord(w?: string) {
  const m = w?.match(/^last-(\d+)-days$/)
  return m ? `直近 ${m[1]} 日` : '全期間'
}

const round = (v: number) => Math.round(v * 100) / 100

/** 区切りの名前（`2025-12-27`・`2025-12`・`2025`）を時刻にする。月と年はその初日。 */
function toTime(t: string) {
  const [y, m = '01', d = '01'] = t.split('-')
  return Date.UTC(Number(y), Number(m) - 1, Number(d))
}

const H = 220
const M = { top: 12, right: 16, bottom: 26, left: 64 }

function Plot({ pts, bar, label, measure }: {
  pts: { t: string; v: number; n: number }[]; bar: boolean; label: string; measure?: string
}) {
  const [box, W] = useWidth()
  const xs = pts.map((p) => toTime(p.t))
  let x0 = xs[0], x1 = xs[xs.length - 1]
  if (x0 === x1) { x0 -= 86400_000; x1 += 86400_000 }
  const vs = pts.map((p) => p.v)
  let y0 = Math.min(...vs), y1 = Math.max(...vs)
  // 棒は 0 から立てる（途中から立てると大小が嘘になる）。線は動きが見える幅に寄せる。
  if (bar) { y0 = Math.min(0, y0); y1 = Math.max(0, y1) }
  if (y0 === y1) { y0 -= 1; y1 += 1 }
  const ticks = niceTicks(y0, y1)
  y0 = Math.min(y0, ticks[0]); y1 = Math.max(y1, ticks[ticks.length - 1])

  const pw = W - M.left - M.right, ph = H - M.top - M.bottom
  const sx = (t: number) => M.left + ((t - x0) / (x1 - x0)) * pw
  const sy = (v: number) => M.top + (1 - (v - y0) / (y1 - y0)) * ph
  const barW = Math.max(2, Math.min(24, (pw / pts.length) * 0.7))

  const spanDays = (x1 - x0) / 86400_000
  const xLabel = (t: string) => t.length === 10 && spanDays <= 92 ? t.slice(5) : t.slice(0, 7)
  // 日付の目盛りは**重ねない・はみ出させない**（2026-09-13、実ブラウザで見つけた）。
  // 近い点（体重の 08-18 と 08-19）を両方挙げると文字が重なり、右端を中央揃えにすると切れていた。
  const xTicks: { x: number; text: string; anchor: 'start' | 'middle' | 'end' }[] = []
  for (const i of pickTicks(pts.length, Math.max(2, Math.floor(pw / 110)))) {
    const x = sx(xs[i]), text = xLabel(pts[i].t)
    const prev = xTicks[xTicks.length - 1]
    if (prev && (prev.text === text || x - prev.x < 70)) continue
    xTicks.push({ x, text, anchor: x - M.left < 30 ? 'start' : W - M.right - x < 30 ? 'end' : 'middle' })
  }

  const word = measure ? measureWord[measure] ?? measure : ''
  const title = (p: { t: string; v: number; n: number }) =>
    `${p.t}: ${num(round(p.v))}` + (p.n > 1 ? `（${num(p.n)} 行の${word}）` : '')

  // **`last` は階段で描く**（2026-09-13、実ブラウザで見つけた）。残高は次の取引まで変わらない。
  // 点と点を斜めに結ぶと、取引の無い 1か月に「徐々に増えた」ように見えて嘘になる。
  const step = measure === 'last'
  const d = pts.map((p, i) => {
    const x = sx(xs[i]).toFixed(1), y = sy(p.v).toFixed(1)
    if (i === 0) return `M${x},${y}`
    return step ? `H${x}V${y}` : `L${x},${y}`
  }).join('')

  return (
    <div ref={box}>
    <svg className="plot" viewBox={`0 0 ${W} ${H}`} width={W} height={H} role="img"
      aria-label={`${label} の推移（${pts.length} 点）`}>
      {ticks.map((v) => (
        <g key={v}>
          <line x1={M.left} x2={W - M.right} y1={sy(v)} y2={sy(v)} className="grid-line" />
          <text x={M.left - 6} y={sy(v)} className="axis" textAnchor="end" dominantBaseline="middle">
            {compact(v)}
          </text>
        </g>
      ))}
      {xTicks.map((t) => (
        <text key={t.text} x={t.x} y={H - 6} className="axis" textAnchor={t.anchor}>{t.text}</text>
      ))}
      {bar
        ? pts.map((p, i) => (
          <rect key={p.t} className="bar" x={sx(xs[i]) - barW / 2} width={barW}
            y={Math.min(sy(p.v), sy(0))} height={Math.abs(sy(p.v) - sy(0))}>
            <title>{title(p)}</title>
          </rect>
        ))
        : (
          <>
            <path className="line" d={d} />
            {pts.map((p, i) => (
              <circle key={p.t} className="dot" cx={sx(xs[i])} cy={sy(p.v)} r={pts.length > 60 ? 1.6 : 3}>
                <title>{title(p)}</title>
              </circle>
            ))}
          </>
        )}
    </svg>
    </div>
  )
}

/**
 * 目盛りは 1・2・5 の刻みで、**最大値と最小値を必ず挟む**（2026-09-13、実ブラウザで見つけた:
 * 最大値が刻みの途中にある図で、目盛りがその下で止まり、上の点に目盛りが無かった）。
 */
function niceTicks(lo: number, hi: number): number[] {
  const raw = (hi - lo) / 4
  const mag = 10 ** Math.floor(Math.log10(raw))
  const step = [1, 2, 5, 10].map((k) => k * mag).find((s) => raw <= s) ?? 10 * mag
  const first = Math.floor(lo / step), last = Math.ceil(hi / step)
  const out: number[] = []
  for (let k = first; k <= last; k++) out.push(k * step)
  return out
}

function pickTicks(n: number, want: number): number[] {
  if (n <= want) return [...Array(n).keys()]
  return [...Array(want).keys()].map((i) => Math.round((i * (n - 1)) / (want - 1)))
}

const compactFmt = new Intl.NumberFormat('ja-JP', { notation: 'compact', maximumFractionDigits: 1 })
const compact = (v: number) => compactFmt.format(v)
