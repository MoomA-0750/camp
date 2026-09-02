import { api, type Window } from './api'
import { Failed, num, short, useAsync } from './ui'

const KIND: Record<string, string> = {
  five_hour: '5時間',
  seven_day: '7日',
  spend_limit: '支出',
}

/** リセットまでの残り時間。過ぎていれば空。 */
function until(endsAt?: string) {
  if (!endsAt) return ''
  const ms = new Date(endsAt).getTime() - Date.now()
  if (!Number.isFinite(ms) || ms <= 0) return ''
  const h = Math.floor(ms / 3_600_000)
  const m = Math.floor((ms % 3_600_000) / 60_000)
  return h > 0 ? `あと ${h}時間${m}分` : `あと ${m}分`
}

function Bar({ pct }: { pct: number }) {
  const level = pct >= 90 ? 'hot' : pct >= 75 ? 'warm' : 'cool'
  return (
    <div className="bar" role="meter" aria-valuenow={Math.round(pct)} aria-valuemin={0} aria-valuemax={100}>
      <span className={`fill ${level}`} style={{ width: `${Math.min(100, Math.max(0, pct))}%` }} />
    </div>
  )
}

/**
 * いま拘束されている窓を出す。
 *
 * 値の出どころは statusLine のフック（campd limits record）。TUI が描画された
 * ときだけ更新されるので、しばらく Claude Code を開いていなければ古くなる。
 * 「いつ観測した値か」を必ず添えるのはそのため。
 */
export default function Limits() {
  const cur = useAsync(() => api.windows({ current: true }), [])

  if (cur.error) return <Failed error={cur.error} />
  if (!cur.data) return null

  if (cur.data.length === 0) {
    return (
      <p className="muted">
        プラン残量の記録がまだない。Claude Code の TUI を一度開くと
        statusLine のフックが記録を始める。
      </p>
    )
  }

  return (
    <div className="windows">
      {cur.data.map((w) => <WindowCard key={w.id} w={w} />)}
    </div>
  )
}

function WindowCard({ w }: { w: Window }) {
  const left = until(w.ends_at)
  return (
    <div className="window">
      <div className="window-head">
        <span className="window-kind">{KIND[w.kind] ?? w.kind}</span>
        <span className="window-pct">{w.used_pct.toFixed(0)}%</span>
      </div>
      <Bar pct={w.used_pct} />
      <div className="window-foot muted">
        <span>{left || 'リセット済み'}</span>
        <span title={`観測 ${num(w.samples)} 回 / 出どころ ${w.source}`}>
          {short(w.fetched_at)} 時点
        </span>
      </div>
      {w.peak_pct > w.used_pct + 0.5 && (
        <div className="window-foot muted">
          <span>この窓のピーク {w.peak_pct.toFixed(0)}%</span>
        </div>
      )}
    </div>
  )
}
