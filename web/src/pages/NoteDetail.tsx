import { useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { api, type Ref } from '../api'
import { Empty, Failed, Loading, num, short, useAsync } from '../ui'

export default function NoteDetail() {
  const { id } = useParams()
  const nid = Number(id)
  const note = useAsync(() => api.note(nid), [nid])
  const links = useAsync(() => api.noteLinks(nid), [nid])
  const touches = useAsync(() => api.noteSessions(nid), [nid])

  const [body, setBody] = useState<string | null | undefined>(undefined)
  const [bodyErr, setBodyErr] = useState('')
  useEffect(() => {
    let alive = true
    setBody(undefined); setBodyErr('')
    api.noteBody(nid).then(
      (b) => { if (alive) setBody(b) },
      (e: Error) => { if (alive) setBodyErr(e.message) },
    )
    return () => { alive = false }
  }, [nid])

  if (note.loading) return <Loading />
  if (note.error) return <Failed error={note.error} />
  const n = note.data
  if (!n) return <Empty>そのノートは無い。</Empty>

  return (
    <>
      <h2>{n.title || n.path}</h2>
      <p className="sub muted">
        <span className="mono">{n.path}</span>
        {' · '}{n.kind}{' · '}{num(n.size)} バイト
        {n.mtime && <>{' · '}更新 {short(n.mtime)}</>}
        {' · '}<Link to={`/graph?note=${n.id}&depth=1`}>グラフで見る</Link>
        {n.kind === 'markdown' && !n.missing_at && <>{' · '}<Link to={`/notes/${n.id}/edit`}>書く</Link></>}
      </p>

      {n.missing_at && (
        <p className="notice">
          このノートは Vault に<strong>もう無い</strong>（{short(n.missing_at)} に消えたと判定）。
          下の本文は Camp が持っている控えから出している。
        </p>
      )}

      <h3>本文</h3>
      {bodyErr && <Failed error={bodyErr} />}
      {body === undefined && !bodyErr && <Loading />}
      {body === null && <p className="muted">この種別は本文を保存していない（添付など）。</p>}
      {typeof body === 'string' && <pre className="note-body">{body}</pre>}

      <h3>被リンク（{num(links.data?.back.length ?? 0)}）</h3>
      <RefList refs={links.data?.back} side="from" empty="このノートを指しているノートは無い。" />

      <h3>出ていくリンク（{num(links.data?.out.length ?? 0)}）</h3>
      <RefList refs={links.data?.out} side="to" empty="このノートからのリンクは無い。" />

      <h3>触ったセッション（{num(touches.data?.length ?? 0)}）</h3>
      {touches.data?.length === 0 && <Empty>このノートを触った記録は無い。</Empty>}
      {touches.data && touches.data.length > 0 && (
        <ul className="rows">
          {touches.data.map((t, i) => (
            <li key={i}>
              <Link to={`/sessions/${encodeURIComponent(t.session_id)}`}>{t.title || t.session_id}</Link>
              <div className="row-meta">
                <span>{t.op}</span><span>{t.origin}</span><span>{short(t.at)}</span>
              </div>
            </li>
          ))}
        </ul>
      )}
    </>
  )
}

function RefList({ refs, side, empty }: { refs?: Ref[]; side: 'from' | 'to'; empty: string }) {
  if (!refs) return <Loading />
  if (refs.length === 0) return <Empty>{empty}</Empty>
  return (
    <ul className="rows">
      {refs.map((r, i) => {
        const id = side === 'from' ? r.from_id : r.to_id
        const path = side === 'from' ? r.from_path : r.to_path
        return (
          <li key={i}>
            {id ? <Link to={`/notes/${id}`}>{path}</Link>
                : <span className="muted">{r.target}（宙吊り）</span>}
            {r.ambiguous && <span className="tag warn">曖昧</span>}
            {r.embed && <span className="tag">埋め込み</span>}
            <div className="row-meta">
              <span className="mono">[[{r.target}{r.frag}{r.alias ? '|' + r.alias : ''}]]</span>
              {r.line ? <span>{r.line} 行目</span> : null}
            </div>
            {r.ambiguous && r.candidates && (
              <div className="row-meta warn-text">
                候補が複数: {r.candidates.join(' / ')}
                {' '}— Camp は先頭を選んだが、Obsidian と食い違う可能性がある
              </div>
            )}
          </li>
        )
      })}
    </ul>
  )
}
