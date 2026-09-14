import { useEffect, useMemo, useState } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { createNote } from '../api'
import { addName, useNames } from '../editor/names'
import { dropNewDraft, newDraft, putNewDraft } from '../editor/drafts'
import { today, useOpenDaily } from '../Switcher'

/**
 * 新しいノート（Phase 5 / M55）。フォルダ・名前・書き留める本文を入れて「作る」。
 *
 * - フォルダは自動保存で書ける場所のうち、ノートが既にあるフォルダから選ぶ（**Camp はフォルダを作らない**）
 * - 名前が空なら `YYYY-MM-DD HHmm`（端末の時刻）。NFC に揃え、大文字小文字違いの名前はサーバーが断る
 * - URL の `?name=` は欄に入れるだけ。**作るのは本人が押してから**（D-030）
 * - 書きかけは作る前からブラウザに控える（戻る・移る・リロードで消さない。outer gate の codex）
 */
export default function NoteNew() {
  const nav = useNavigate()
  const [sp] = useSearchParams()
  const { names, vault, error: namesErr } = useNames()
  const [restored] = useState(() => newDraft())
  const [folder, setFolder] = useState(() => restored?.folder || 'Inbox')
  const [name, setName] = useState(() => sp.get('name') ?? restored?.name ?? '')
  const [body, setBody] = useState(() => restored?.body ?? '')
  const [warn, setWarn] = useState('')

  useEffect(() => {
    if (!putNewDraft({ folder, name, body })) setWarn('ブラウザに書きかけを控えられない（容量か設定）。作る前に閉じると失う')
  }, [folder, name, body])
  useEffect(() => {
    const unload = (e: BeforeUnloadEvent) => { if (body) { e.preventDefault(); e.returnValue = '' } }
    window.addEventListener('beforeunload', unload)
    return () => window.removeEventListener('beforeunload', unload)
  }, [body])
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<{ msg: string; noteId?: number } | null>(null)
  const daily = useOpenDaily()

  const folders = useMemo(() => {
    const set = new Set<string>(['Inbox'])
    for (const n of names) {
      if (n.editable && n.path.endsWith('.md') && n.path.includes('/')) {
        set.add(n.path.slice(0, n.path.lastIndexOf('/')))
      }
    }
    return [...set].sort((a, b) => (a === 'Inbox' ? -1 : b === 'Inbox' ? 1 : a.localeCompare(b)))
  }, [names])

  const fallbackName = () => {
    const d = new Date()
    const p = (n: number) => String(n).padStart(2, '0')
    return `${today(d)} ${p(d.getHours())}${p(d.getMinutes())}`
  }

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!vault) return
    setBusy(true); setErr(null)
    const stem = name.trim().replace(/\.md$/, '') || fallbackName()
    const path = `${folder}/${stem}.md`
    const r = await createNote(vault, path, body)
    setBusy(false)
    if (!r.ok) {
      setErr({ msg: r.error, noteId: r.code === 409 ? r.note_id : undefined })
      return
    }
    dropNewDraft()
    addName({ id: r.res.note_id, path: r.res.path, editable: true })
    nav(`/notes/${r.res.note_id}/edit`, { replace: true })
  }

  return (
    <>
      <h2>新しいノート</h2>
      <p className="sub muted">作ると書く画面を開く。1 分書かなければ commit して GitHub へ出す。</p>
      <p>
        <button className="btn" onClick={() => void daily.openDaily()} disabled={daily.busy || !vault}>今日の日次ログを開く</button>
        {daily.error && <span className="error"> {daily.error}</span>}
      </p>
      {namesErr && <p className="notice">{namesErr}</p>}
      {restored?.body && <p className="notice">前に書きかけた内容を戻した（{new Date(restored.at).toLocaleString('ja-JP')}）。</p>}
      {warn && <p className="notice">{warn}</p>}
      <form className="note-new" onSubmit={submit}>
        <label>
          フォルダ
          <select value={folder} onChange={(e) => setFolder(e.target.value)}>
            {folders.map((f) => <option key={f} value={f}>{f}</option>)}
          </select>
        </label>
        <label>
          名前
          <input value={name} onChange={(e) => setName(e.target.value)} placeholder={`空なら「${fallbackName()}」`}
            autoFocus />
        </label>
        <label>
          書き留める（空でもよい）
          <textarea value={body} onChange={(e) => setBody(e.target.value)} rows={6} />
        </label>
        <div>
          <button className="btn" type="submit" disabled={busy || !vault}>作る</button>
        </div>
      </form>
      {err && (
        <p className="notice">
          作れなかった: {err.msg}
          {err.noteId ? <> — <Link to={`/notes/${err.noteId}/edit`}>そのノートを開く</Link></> : null}
        </p>
      )}
    </>
  )
}
