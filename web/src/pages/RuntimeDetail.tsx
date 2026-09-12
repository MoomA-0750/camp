import { useEffect, useRef, useState } from 'react'
import { Link, useParams, useSearchParams } from 'react-router-dom'
import {
  api, type Approval, type AskView, type LogLine, type RuntimeUsage, type UsageTable,
  type UsageWindow,
} from '../api'
import { Empty, Failed, Loading, clock, short, tokens, useAsync } from '../ui'
import { ApprovalSummary, StateBadge, atEndLabel, endLabel, permLabel } from './Runtime'

// 走っているセッション1本。
//
// **流れてくるものは SSE で受ける。** 子は読み手を待たない——実行面が
// 子の隣に落としていて、ここはそれをカーソルで追いかけるだけ。画面を
// 閉じても子は走り続けるし、開き直せば続きから届く。
//
// **どのエージェントかを読み分けない**（D-031）。表示名・振る舞いの説明・流れの一言・承認の中身・
// 残量は、駆動器が共通の形に直したものが API から来る。
export default function RuntimeDetail() {
  const { id = '' } = useParams()
  const [sp, setSp] = useSearchParams()
  const tab = sp.get('tab') ?? 'stream'
  const follow = sp.get('follow') !== '0'
  // **流さないで読む**という選択肢を URL に置く。
  // 終わったセッションを読み返すだけのときや、回線を使いたくないときに、
  // 開いているだけで枠（同時16本）を1つ潰さないため。
  const wantLive = sp.get('live') !== '0'

  const [lines, setLines] = useState<LogLine[]>([])
  const [gap, setGap] = useState(0)
  const [live, setLive] = useState(false)
  const [tick, setTick] = useState(0)

  // **1本を直接引く。** 一覧から探すと、一覧に載っていないものを開けない
  // （一覧は走っているものだけ。終わったものは「終わったもの」から開く）。
  const one = useAsync(() => api.runtimeOne(id), [id, tick])
  const rec = one.data
  const waiting = useAsync(() => api.runtimeApprovals(id), [id, tick])

  // 流れは**この箱の中だけ**を動かす。ページごと動かすと、上に出ている
  // 承認の枠が視界から飛んでいって、答えられなくなる（2026-09-07 に実際に
  // そうなった。0.5秒ごとに最下へ引き戻されて、読むことすらできなかった）。
  const box = useRef<HTMLDivElement>(null)
  // 末尾に居るときだけ追う。**上へ遡っている最中は引き戻さない。**
  const stick = useRef(true)
  // どこまで受け取ったか。**繋ぎ直しのたびに先頭から流し直させない。**
  // 覚えていないと、繋ぎ直すたびに同じ行が積み上がる（2026-09-07 に実測）。
  const lastSeq = useRef(0)

  // 状態と承認を取り直す。**どの行で取り直すかを、フレームの種類で決めない**（種類は
  // エージェントごとに違う）。行が来たら、少し待ってまとめて取り直す。
  const refresh = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  useEffect(() => () => { if (refresh.current) clearTimeout(refresh.current) }, [])
  const soon = () => {
    if (refresh.current) return
    refresh.current = setTimeout(() => {
      refresh.current = undefined
      setTick((v) => v + 1)
    }, 400)
  }

  // SSE で追いかける。**繋がなくても子は走る**ので、切れても壊れない。
  //
  // 終わったセッションでは繋がない。何も流れてこないのに枠を1つ握り続ける
  // だけになる（枠は全体で16本）。代わりに1度だけ読んで並べる。
  // **状態が分かるまで繋がない。** 分かる前に繋ぐと、終わったセッションを
  // 開いただけでも一瞬だけ枠を取る。
  const known = !!one.data
  const done = rec?.state === 'exited'
  const streaming = wantLive && known && !done

  useEffect(() => {
    if (!id || streaming) return
    let alive = true
    api.runtimeLog(id, 0, 500).then(
      (r) => {
        if (!alive) return
        const got = r.lines ?? []
        setLines(got)
        if (got.length > 0) lastSeq.current = got[got.length - 1].seq
        if (r.gap) setGap(r.dropped || -1)
      },
      () => { /* 落とし先が無いだけのこともある。空のまま出す */ },
    )
    return () => { alive = false }
  }, [id, streaming, tick])

  useEffect(() => {
    if (!id || !streaming) return
    const es = new EventSource(
      `/api/runtime/${encodeURIComponent(id)}/stream?since=${lastSeq.current}`)
    es.onopen = () => setLive(true)
    es.onerror = () => setLive(false)
    es.onmessage = (e) => {
      try {
        const ln = JSON.parse(e.data) as LogLine
        // **同じ番号を二度入れない。** 繋ぎ直しの取りこぼしを直すのは
        // since だが、取り違えの保険はここにも要る。
        if (ln.seq <= lastSeq.current) return
        lastSeq.current = ln.seq
        setLines((prev) => (prev.length > 2000 ? [...prev.slice(-1500), ln] : [...prev, ln]))
        soon()
      } catch { /* 読めない行は捨てる */ }
    }
    es.addEventListener('gap', (e) => {
      // **黙って飛ばさない。** 落とし先が溢れて、見えていない範囲がある。
      try { setGap(JSON.parse((e as MessageEvent).data).dropped ?? 0) } catch { setGap(-1) }
    })
    return () => es.close()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id, streaming])

  // Camp が子へ投げた問い合わせのやりとりは、会話ではない（残量の取得など。駆動器が印を付ける）。
  // **落とし先からは消さない。** 画面で畳むだけ。
  const shown = lines.filter((l) => !l.own)
  const hidden = lines.length - shown.length

  useEffect(() => {
    const el = box.current
    // **増えたときだけ動かす。** 何も増えていないのに動かすと、
    // 読んでいる途中で毎回引き戻される。
    if (!el || !follow || !stick.current) return
    el.scrollTop = el.scrollHeight
  }, [shown.length, follow])

  // 上へ遡ったら追うのをやめ、末尾へ戻したら再開する。
  const onScroll = () => {
    const el = box.current
    if (!el) return
    const slack = el.scrollHeight - el.scrollTop - el.clientHeight
    stick.current = slack < 24
  }

  const set = (patch: Record<string, string>) => {
    const next = new URLSearchParams(sp)
    for (const [k, v] of Object.entries(patch)) {
      if (v) next.set(k, v)
      else next.delete(k)
    }
    setSp(next)
  }

  const sid = rec?.agent_session_id ?? rec?.claude_id
  return (
    <>
      <p className="crumbs"><Link to="/runtime">← セッション駆動</Link></p>
      <h2>
        {rec ? <StateBadge state={rec.state} /> : null}{' '}
        <span className="mono">{rec ? (rec.host ? `${rec.host}:${rec.cwd}` : rec.cwd) : id}</span>
      </h2>
      {rec && (
        <p className="sub muted">
          {rec.agent_label ?? rec.agent}（確認の度合い {permLabel(rec.perm)}） /{' '}
          起こしたのは {short(rec.created_at)} /{' '}
          {rec.host
            ? <>手元の ssh の pid {rec.pid || '—'} / {rec.host} の pid {rec.remote_pid || '—'}</>
            : <>pid {rec.pid || '—'}</>}
          {sid ? <> / 会話記録 <code>{sid.slice(0, 8)}</code></> : null}
        </p>
      )}
      {rec?.state === 'exited' && (
        <p className="sub">
          {short(rec.ended_at ?? '')} に終わった: <strong>{endLabel(rec.end_cause)}</strong>
          {rec.end_state ? <>（{atEndLabel(rec.end_state)}）</> : null}
          {' · 承認 '}<ApprovalSummary s={rec} />
          {rec.exit_reason ? <span className="muted"> · {rec.exit_reason}</span> : null}
        </p>
      )}
      {/* **元のものが生き返ったように見せる**（本人の決定 2026-09-13）。台帳では別の行だが、
          エージェント側の会話は1本のまま繋がっている（記録も同じファイルへ追記される）。 */}
      {rec?.resumed_from && (
        <p className="sub">
          <Link to={`/runtime/${rec.resumed_from}`}>前のセッション</Link> の続き。
          <span className="muted"> 会話はエージェント側で1本に繋がっている。</span>
        </p>
      )}
      {rec?.resumed_by && (
        <p className="sub">
          <Link to={`/runtime/${rec.resumed_by}`}>続きが起きている</Link>。
        </p>
      )}
      {one.error && <Failed error={one.error} />}

      <Ask id={id} rows={waiting} onAnswered={() => setTick((v) => v + 1)} />

      <div className="tabs">
        <button className={tab === 'stream' ? 'on' : ''} onClick={() => set({ tab: '' })}>
          流れ
        </button>
        <button className={tab === 'usage' ? 'on' : ''} onClick={() => set({ tab: 'usage' })}>
          残量
        </button>
        <button className={tab === 'history' ? 'on' : ''} onClick={() => set({ tab: 'history' })}>
          承認の履歴
        </button>
      </div>

      {tab === 'usage' && <Usage id={id} />}
      {tab === 'history' && <History id={id} />}
      {tab === 'stream' && (
        <>
          <Talk id={id} state={rec?.state} notes={rec?.agent_notes}
            onSent={() => setTick((v) => v + 1)} />
          <p className="sub muted">
            {!known ? '状態を確かめている'
              : !wantLive ? '流していない（live=0）'
                : done ? '終わったので流していない'
                  : live ? '繋がっている' : '繋がっていない（子は走り続ける）'}
            {' · '}
            <label>
              <input type="checkbox" checked={wantLive} aria-label="流す"
                onChange={(e) => set({ live: e.target.checked ? '' : '0' })} />
              流す
            </label>
            {' · '}
            <label>
              <input type="checkbox" checked={follow}
                onChange={(e) => set({ follow: e.target.checked ? '' : '0' })} />
              末尾を追う
            </label>
          </p>
          {gap !== 0 && (
            <p className="warn">
              落とし先が溢れて {gap < 0 ? '一部' : gap + ' 件'} 落ちている。
              ここより前は見えない。
            </p>
          )}
          {lines.length === 0 && <Empty>まだ何も流れていない。</Empty>}
          {hidden > 0 && (
            <p className="sub muted">
              Camp 自身の問い合わせ {hidden} 件は畳んでいる。
            </p>
          )}
          <div className="stream" ref={box} onScroll={onScroll}>
            {shown.map((ln) => (
              <div key={ln.seq} className="frame">
                <span className="muted mono">{clock(ln.at)}</span>{' '}
                <span className="kind">{ln.kind}</span>{' '}
                <span className="mono">{ln.summary ?? ''}</span>
              </div>
            ))}
          </div>
        </>
      )}
    </>
  )
}

// Ask は待っている承認。**列として出す**——1ターンに複数来る（実測）。
function Ask({ id, rows, onAnswered }: {
  id: string
  rows: { data?: Approval[]; loading: boolean; error?: string }
  onAnswered: () => void
}) {
  const [err, setErr] = useState('')
  const waiting = rows.data ?? []
  if (waiting.length === 0) return null
  const answer = async (req: string, behavior: 'allow' | 'deny') => {
    setErr('')
    try {
      await api.runtimeApprove(id, req, behavior)
      onAnswered()
    } catch (e) {
      setErr(String(e instanceof Error ? e.message : e))
    }
  }
  return (
    <div className="ask">
      <h3>承認を待っている（{waiting.length}）</h3>
      <p className="sub muted">
        答えるまで、そのターンは止まっている。
        <strong>放っておいても期限切れにはならない</strong>（CLI と同じ。答えるまで待つ）。
      </p>
      {err && <Failed error={err} />}
      {waiting.map((a) => (
        <div key={a.request_id} className="ask-row">
          <div>
            <strong>{a.tool}</strong>{' '}
            {a.expires_at && <span className="muted">期限 {short(a.expires_at)}</span>}
            <pre className="mono small">{askText(a.detail)}</pre>
          </div>
          <div className="ask-buttons">
            <button onClick={() => void answer(a.request_id, 'allow')}>許可</button>
            <button onClick={() => void answer(a.request_id, 'deny')}>拒否</button>
          </div>
        </div>
      ))}
    </div>
  )
}

// askText は承認の中身を文にする。**共通の形（view）だけを見る**——どのエージェントでも同じ。
// **差分は切り詰めない**——途中までの中身で許させない（画面へ渡せない大きさのものは、駆動器が
// 見せずに断っている）。共通の形の無い古い行は、中身をそのまま出す。
export function askText(d?: string): string {
  if (!d) return ''
  try {
    const o = JSON.parse(d) as { view?: AskView; input?: unknown }
    const v = o.view
    if (!v) return JSON.stringify(o.input ?? o, null, 1).slice(0, 1200)
    const lines: string[] = []
    if (v.what === 'command') {
      lines.push(`$ ${v.command ?? '（コマンドが無い）'}`)
      if (v.cwd) lines.push(`場所: ${v.cwd}${v.outside ? '  ← 起こした場所の外' : ''}`)
    } else if (v.what === 'file') {
      for (const c of v.changes ?? []) {
        lines.push(`--- ${c.kind || '?'}: ${c.path}`)
        if (c.patch) lines.push(c.patch)
      }
    } else {
      lines.push(JSON.stringify(v.input ?? {}, null, 1))
    }
    if (v.reason) lines.push(`理由: ${v.reason}`)
    return lines.join('\n')
  } catch {
    return d.slice(0, 1200)
  }
}

function Talk({ id, state, notes, onSent }: {
  id: string; state?: string; notes?: string[]; onSent: () => void
}) {
  const [text, setText] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  const send = async () => {
    setErr(''); setBusy(true)
    try {
      await api.runtimeInput(id, text)
      setText('')
      onSent()
    } catch (e) {
      setErr(String(e instanceof Error ? e.message : e))
    } finally { setBusy(false) }
  }
  const stop = async (mode: 'interrupt' | 'terminate') => {
    setErr('')
    try {
      await api.runtimeStop(id, mode)
      onSent()
    } catch (e) {
      setErr(String(e instanceof Error ? e.message : e))
    }
  }

  const done = state === 'exited'
  return (
    <>
      <form className="talk" onSubmit={(e) => { e.preventDefault(); void send() }}>
        <textarea rows={3} value={text} placeholder="送る内容" disabled={done}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); void send() }
          }} />
        <div className="talk-buttons">
          <button disabled={busy || !text || state !== 'idle'}>送る</button>
          <button type="button" disabled={done} onClick={() => void stop('interrupt')}>
            中断
          </button>
          <button type="button" disabled={done} onClick={() => void stop('terminate')}>
            止める（孫まで）
          </button>
        </div>
      </form>
      {/* そのエージェント固有の振る舞い（駆動器の説明をそのまま出す）。 */}
      {(notes ?? []).map((n) => <p key={n} className="sub muted">{n}</p>)}
      {err && <Failed error={err} />}
    </>
  )
}

// 残量。仕様が求めていた4種のうち3種をここに出す
// （プラン残量の履歴は「使用量」の画面にある）。**共通の形（view）だけを見て描く。**
//
// **どれもチャートにしない。** 測っているのは「大きさ」と「状態」で、
// 種類は2〜8しかない——プラン枠はメーター、内訳は表、同時実行は1行。
// 8分類を積み上げ棒にすると、そこで初めて categorical な配色が要るが、
// このリポジトリはまだそれを持っていない。持っていない配色を
// その場で作るより、表のほうが正確に読める。
function Usage({ id }: { id: string }) {
  const u = useAsync(() => api.runtimeUsage(id), [id])
  if (u.loading) return <Loading />
  if (u.error) return <Failed error={u.error} />
  const d = u.data as RuntimeUsage
  const v = d.view
  const windows = v?.windows ?? []
  const errors = v?.errors ?? []
  const ctx = v?.context

  return (
    <>
      {d.warning && <p className="warn">{d.warning}</p>}
      <p className="sub muted">
        同時に走っているのは {d.running} / {d.max} 本（4コア）。
        {v?.plan ? ` / プラン ${v.plan}` : ''}
      </p>

      <h3>プラン枠</h3>
      {d.usage_error && <Failed error={d.usage_error} />}
      {errors.map((e) => <Failed key={e} error={e} />)}
      {!d.usage_error && errors.length === 0 && windows.length === 0 && (
        <Empty>枠の情報が来ていない。</Empty>
      )}
      {windows.length > 0 && (
        <div className="windows">
          {windows.map((w) => <LimitCard key={w.label} w={w} />)}
        </div>
      )}

      <h3>コンテキスト</h3>
      {d.context_error ? <Failed error={d.context_error} />
        : ctx ? (
          <p>
            {tokens(ctx.used)}
            {ctx.max ? <> / {tokens(ctx.max)}（{Math.round((ctx.used / ctx.max) * 100)}%）</> : null}
          </p>
        ) : <Empty>コンテキストの情報が来ていない。</Empty>}

      {(v?.tables ?? []).map((t) => <UsageTableView key={t.title} t={t} />)}

      <details>
        <summary>返ってきたものをそのまま見る</summary>
        <Json v={{ usage: d.usage, context: d.context }} />
      </details>
    </>
  )
}

function UsageTableView({ t }: { t: UsageTable }) {
  return (
    <>
      <h3>{t.title}</h3>
      {t.rows.length === 0 ? <Empty>{t.empty || '無い。'}</Empty> : (
        <div className="scroll-x">
          <table>
            <thead>
              <tr>
                <th></th>
                {t.columns.map((c) => <th key={c.label} className="num">{c.label}</th>)}
              </tr>
            </thead>
            <tbody>
              {t.rows.map((r) => (
                <tr key={r.label}>
                  <td className={r.total ? 'muted' : 'mono'}>{r.label}</td>
                  {r.cells.map((x, i) => (
                    <td key={i} className="num">{cell(x, t.columns[i]?.unit)}</td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  )
}

// cell は表の1つのますの書き方。単位は駆動器が列に付けている。
function cell(x: number, unit?: string): string {
  if (unit === 'usd') return usd(x)
  if (unit === 'percent') return x.toFixed(1) + '%'
  return tokens(x)
}

/** リセットまでの残り。過ぎていれば空。 */
function until(at?: string) {
  if (!at) return ''
  const ms = new Date(at).getTime() - Date.now()
  if (!Number.isFinite(ms) || ms <= 0) return ''
  const h = Math.floor(ms / 3_600_000)
  const m = Math.floor((ms % 3_600_000) / 60_000)
  return h > 0 ? `あと ${h}時間${m}分` : `あと ${m}分`
}

function LimitCard({ w }: { w: UsageWindow }) {
  const pct = Math.min(100, Math.max(0, w.percent ?? 0))
  // 色は状態（good/warn/critical）であって、系列の識別ではない。
  // **数字を必ず添える**——色だけで伝えない。
  const level = pct >= 90 ? 'hot' : pct >= 75 ? 'warm' : 'cool'
  return (
    <div className="window">
      <div className="window-head">
        <span className="window-kind">
          {w.label}
          {w.active && <span className="tag warn">拘束中</span>}
        </span>
        <span className="window-pct">{Math.round(pct)}%</span>
      </div>
      <div className="bar" role="meter" aria-valuenow={Math.round(pct)}
        aria-valuemin={0} aria-valuemax={100}
        aria-label={`${w.label} の使用率`}>
        <span className={`fill ${level}`} style={{ width: `${pct}%` }} />
      </div>
      <div className="window-foot muted">
        <span>{until(w.resets_at)}</span>
        <span>{short(w.resets_at)} に戻る</span>
      </div>
    </div>
  )
}

function usd(n: number) {
  return '$' + (n ?? 0).toFixed(n >= 1 ? 2 : 4)
}

function Json({ v }: { v: unknown }) {
  return <pre className="mono small">{JSON.stringify(v, null, 1)}</pre>
}

function History({ id }: { id: string }) {
  const h = useAsync(() => api.runtimeApprovals(id, true), [id])
  if (h.loading) return <Loading />
  if (h.error) return <Failed error={h.error} />
  const rows = h.data ?? []
  if (rows.length === 0) return <Empty>承認は一度も来ていない。</Empty>

  return (
    <table>
      <thead><tr><th>工具</th><th>訊かれた</th><th>答え</th><th>理由</th></tr></thead>
      <tbody>
        {rows.map((a) => (
          <tr key={a.id}>
            <td>{a.tool}</td>
            <td>{short(a.asked_at)}</td>
            <td>{a.behavior === 'allow' ? '許可' : a.behavior === 'deny' ? '拒否' : '待ち'}</td>
            <td className="muted">
              {a.reason === 'timeout' ? '期限切れ'
                : a.reason === 'session_ended' ? 'セッションが終わった'
                  : a.reason === 'user' ? '本人'
                    : a.reason === 'withdrawn' ? '取り下げ（子へ届いていない）' : ''}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}
