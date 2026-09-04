import { useEffect, useRef, useState } from 'react'
import { Link, useParams, useSearchParams } from 'react-router-dom'
import { api, type Approval, type LogLine, type RuntimeUsage } from '../api'
import { Empty, Failed, Loading, short, tokens, useAsync } from '../ui'
import { StateBadge } from './Runtime'

// 走っているセッション1本。
//
// **流れてくるものは SSE で受ける。** 子は読み手を待たない——実行面が
// 子の隣に落としていて、ここはそれをカーソルで追いかけるだけ。画面を
// 閉じても子は走り続けるし、開き直せば続きから届く。
export default function RuntimeDetail() {
  const { id = '' } = useParams()
  const [sp, setSp] = useSearchParams()
  const tab = sp.get('tab') ?? 'stream'
  const follow = sp.get('follow') !== '0'

  const [lines, setLines] = useState<LogLine[]>([])
  const [gap, setGap] = useState(0)
  const [live, setLive] = useState(false)
  const [tick, setTick] = useState(0)

  const list = useAsync(() => api.runtime(), [tick])
  const rec = list.data?.sessions.find((s) => s.id === id)
  const waiting = useAsync(() => api.runtimeApprovals(id), [id, tick])

  const bottom = useRef<HTMLDivElement>(null)

  // SSE で追いかける。**繋がなくても子は走る**ので、切れても壊れない。
  useEffect(() => {
    if (!id) return
    const es = new EventSource(`/api/runtime/${encodeURIComponent(id)}/stream`)
    es.onopen = () => setLive(true)
    es.onerror = () => setLive(false)
    es.onmessage = (e) => {
      try {
        const ln = JSON.parse(e.data) as LogLine
        setLines((prev) => (prev.length > 2000 ? [...prev.slice(-1500), ln] : [...prev, ln]))
        if (ln.kind === 'result' || ln.kind.startsWith('control_request')) {
          setTick((v) => v + 1) // 状態と承認を取り直す
        }
      } catch { /* 読めない行は捨てる */ }
    }
    es.addEventListener('gap', (e) => {
      // **黙って飛ばさない。** 落とし先が溢れて、見えていない範囲がある。
      try { setGap(JSON.parse((e as MessageEvent).data).dropped ?? 0) } catch { setGap(-1) }
    })
    return () => es.close()
  }, [id])

  useEffect(() => {
    if (follow) bottom.current?.scrollIntoView({ block: 'end' })
  }, [lines.length, follow])

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
      <p className="crumbs"><Link to="/runtime">← セッション駆動</Link></p>
      <h2>
        {rec ? <StateBadge state={rec.state} /> : null}{' '}
        <span className="mono">{rec?.cwd ?? id}</span>
      </h2>
      {rec && (
        <p className="sub muted">
          起こしたのは {short(rec.created_at)} / pid {rec.pid || '—'}
          {rec.claude_id ? <> / 会話記録 <code>{rec.claude_id.slice(0, 8)}</code></> : null}
          {rec.exit_reason ? <> / 終わり: {rec.exit_reason}</> : null}
        </p>
      )}

      <Ask id={id} rows={waiting} onAnswered={() => setTick((v) => v + 1)} />

      <nav className="tabs">
        <button className={tab === 'stream' ? 'on' : ''} onClick={() => set({ tab: '' })}>
          流れ
        </button>
        <button className={tab === 'usage' ? 'on' : ''} onClick={() => set({ tab: 'usage' })}>
          残量
        </button>
        <button className={tab === 'history' ? 'on' : ''} onClick={() => set({ tab: 'history' })}>
          承認の履歴
        </button>
      </nav>

      {tab === 'usage' && <Usage id={id} />}
      {tab === 'history' && <History id={id} />}
      {tab === 'stream' && (
        <>
          <Talk id={id} state={rec?.state} onSent={() => setTick((v) => v + 1)} />
          <p className="sub muted">
            {live ? '繋がっている' : '繋がっていない（子は走り続ける）'}
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
          <div className="stream">
            {lines.map((ln) => (
              <div key={ln.seq} className="frame">
                <span className="muted mono">{ln.at.slice(11, 19)}</span>{' '}
                <span className="kind">{ln.kind}</span>{' '}
                <span className="mono">{summarize(ln)}</span>
              </div>
            ))}
            <div ref={bottom} />
          </div>
        </>
      )}
    </>
  )
}

// summarize はフレームの中身を1行に畳む。**全文はここでは出さない**
// （会話そのものは取り込まれたあと「セッション」側で読む）。
function summarize(ln: LogLine): string {
  const f = ln.frame as Record<string, unknown> | undefined
  if (!f) return ''
  const msg = f.message as { content?: unknown } | undefined
  if (Array.isArray(msg?.content)) {
    return msg.content
      .map((b) => {
        const x = b as { type?: string; text?: string; name?: string }
        if (x.type === 'text') return (x.text ?? '').slice(0, 200)
        if (x.type === 'tool_use') return `[${x.name}]`
        if (x.type === 'tool_result') return '[結果]'
        return `[${x.type}]`
      })
      .join(' ')
      .slice(0, 300)
  }
  if (typeof msg?.content === 'string') return msg.content.slice(0, 300)
  return ''
}

// Ask は待っている承認。**列として出す**——1ターンに複数来る（実測）。
function Ask({ id, rows, onAnswered }: {
  id: string
  rows: { data?: Approval[]; loading: boolean; error?: string }
  onAnswered: () => void
}) {
  const [err, setErr] = useState('')
  if (!rows.data || rows.data.length === 0) return null
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
      <h3>承認を待っている（{rows.data.length}）</h3>
      <p className="sub muted">
        答えるまで、そのターンは止まっている。
        <strong>答えないままにすると期限切れで拒否になる</strong>（4分30秒）。
      </p>
      {err && <Failed error={err} />}
      {rows.data.map((a) => (
        <div key={a.request_id} className="ask-row">
          <div>
            <strong>{a.tool}</strong>{' '}
            <span className="muted">期限 {short(a.expires_at)}</span>
            <pre className="mono small">{prettyDetail(a.detail)}</pre>
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

function prettyDetail(d?: string): string {
  if (!d) return ''
  try {
    const o = JSON.parse(d) as Record<string, unknown>
    return JSON.stringify(o.input ?? o, null, 1).slice(0, 1200)
  } catch {
    return d.slice(0, 1200)
  }
}

function Talk({ id, state, onSent }: { id: string; state?: string; onSent: () => void }) {
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
      {err && <Failed error={err} />}
    </>
  )
}

// Usage は仕様が求めていた4種のうち3種をここに出す
// （プラン残量の履歴は「使用量」の画面にある）。
function Usage({ id }: { id: string }) {
  const u = useAsync(() => api.runtimeUsage(id), [id])
  if (u.loading) return <Loading />
  if (u.error) return <Failed error={u.error} />
  const d = u.data as RuntimeUsage
  return (
    <>
      {d.warning && <p className="warn">{d.warning}</p>}
      <p className="sub muted">
        同時に走っているのは {d.running} / {d.max} 本。
        <strong>4コアしかない</strong>ので、先に効くのはメモリではなくCPU。
      </p>
      <h3>プラン枠とモデル別</h3>
      {d.usage_error ? <Failed error={d.usage_error} /> : <Json v={d.usage} />}
      <h3>コンテキストの内訳</h3>
      {d.context_error ? <Failed error={d.context_error} /> : <Context v={d.context} />}
    </>
  )
}

function Context({ v }: { v: unknown }) {
  const o = v as { categories?: { name: string; tokens: number }[]; totalTokens?: number
    maxTokens?: number; percentage?: number } | undefined
  if (!o?.categories) return <Json v={v} />
  return (
    <>
      <p>
        {tokens(o.totalTokens ?? 0)} / {tokens(o.maxTokens ?? 0)}（{o.percentage ?? 0}%）
      </p>
      <table>
        <thead><tr><th>分類</th><th>トークン</th></tr></thead>
        <tbody>
          {o.categories.map((c) => (
            <tr key={c.name}><td>{c.name}</td><td className="num">{tokens(c.tokens)}</td></tr>
          ))}
        </tbody>
      </table>
    </>
  )
}

function Json({ v }: { v: unknown }) {
  return <pre className="mono small">{JSON.stringify(v, null, 1)}</pre>
}

function History({ id }: { id: string }) {
  const h = useAsync(() => api.runtimeApprovals(id, true), [id])
  if (h.loading) return <Loading />
  if (h.error) return <Failed error={h.error} />
  if (!h.data || h.data.length === 0) return <Empty>承認は一度も来ていない。</Empty>
  return (
    <table>
      <thead><tr><th>工具</th><th>訊かれた</th><th>答え</th><th>理由</th></tr></thead>
      <tbody>
        {h.data.map((a) => (
          <tr key={a.id}>
            <td>{a.tool}</td>
            <td>{short(a.asked_at)}</td>
            <td>{a.behavior === 'allow' ? '許可' : a.behavior === 'deny' ? '拒否' : '待ち'}</td>
            <td className="muted">
              {a.reason === 'timeout' ? '期限切れ'
                : a.reason === 'session_ended' ? 'セッションが終わった'
                  : a.reason === 'user' ? '本人' : ''}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}
