import { useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { api, type Message } from '../api'
import { Empty, Failed, Loading, bytes, num, short, useAsync } from '../ui'

export default function SessionDetail() {
  const { id = '' } = useParams()
  const [after, setAfter] = useState(0)
  const meta = useAsync(() => api.session(id), [id])
  const body = useAsync(() => api.messages(id, after, 120), [id, after])
  const touched = useAsync(() => api.files({ session: id, limit: 40 }), [id])
  const backups = useAsync(() => api.backups({ session: id, limit: 40 }), [id])

  if (meta.loading) return <Loading />
  if (meta.error) return <Failed error={meta.error} />
  const s = meta.data!

  return (
    <>
      <h2>{s.title || '(タイトル無し)'}</h2>
      <p className="sub muted">
        <span className="mono">{s.id}</span>
        {' · '}{s.host} · {s.repo_path}
        {s.git_branch ? ` · ${s.git_branch}` : ''}
        <br />
        {short(s.started_at)} 〜 {short(s.updated_at)} ·{' '}
        {num(s.conversation)} 往復 / {num(s.messages)} 行
        {s.cost_usd ? ` · $${s.cost_usd.toFixed(2)}` : ''}
        {s.model ? ` · ${s.model}` : ''}
      </p>

      {touched.data && touched.data.length > 0 && (
        <details>
          <summary>触ったファイル {touched.data.length} 件</summary>
          <ul className="rows">
            {touched.data.map((t, i) => (
              <li key={i}>
                <span className="mono">{t.rel_path || t.abs_path}</span>{' '}
                <span className="muted">{t.op} · {short(t.at)}</span>
              </li>
            ))}
          </ul>
        </details>
      )}

      {backups.data && backups.data.length > 0 && (
        <details>
          <summary>編集前の中身 {backups.data.length} 件（元が消えても読める）</summary>
          <ul className="rows">
            {backups.data.map((b) => (
              <li key={b.id}>
                <a href={`/api/backups/${b.id}/content`} target="_blank" rel="noreferrer"
                   className="mono">
                  {b.rel_path || b.abs_path}
                </a>{' '}
                <span className="muted">
                  v{b.version} · {short(b.backup_time)} · {bytes(b.size)}
                  {b.missing_at ? ' · 実体は消滅' : ''}
                </span>
              </li>
            ))}
          </ul>
        </details>
      )}

      {body.loading && <Loading />}
      {body.error && <Failed error={body.error} />}
      {body.data && body.data.messages.length === 0 && <Empty>この先に本文は無い</Empty>}

      <div>
        {body.data?.messages.map((m) => <Turn key={m.id} m={m} />)}
      </div>

      <div className="pager">
        {after > 0 && (
          <button className="btn" onClick={() => setAfter(0)}>先頭へ</button>
        )}
        {body.data && body.data.messages.length >= 120 && (
          <button className="btn" onClick={() => setAfter(body.data!.next_after)}>続き</button>
        )}
        <Link to="/sessions">一覧へ戻る</Link>
      </div>
    </>
  )
}

// 1ターン。tool_result は長いので畳んでおく。ここが全体の3分の1を占める。
function Turn({ m }: { m: Message }) {
  const who = m.role || m.type
  return (
    <div className="turn">
      <div className="turn-head">
        <span className={'badge ' + who}>{who}</span>
        <span>{short(m.timestamp)}</span>
        {m.model && <span className="mono">{m.model}</span>}
      </div>
      {m.blocks?.map((b, i) => {
        const label = b.tool_name ? `${b.kind} · ${b.tool_name}` : b.kind
        if (b.kind === 'text') return <pre key={i}>{b.text}</pre>
        return (
          <details key={i}>
            <summary>{label}（{(b.text ?? '').length} 字）</summary>
            <pre>{b.text}</pre>
          </details>
        )
      })}
    </div>
  )
}
