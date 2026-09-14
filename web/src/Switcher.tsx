import { useEffect, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { dailyNote } from './api'
import { addName, currentVault, hasBase, loadNames, rank, useNames } from './editor/names'

/**
 * クイックスイッチャー（Phase 5 / M55）。Ctrl+O・⌘O とナビの「開く」で出す。
 *
 * 名前の一覧を画面で絞る（並べ方は画面の事情なので画面に置く）。Enter で書く画面を開く。
 * 当たりが無ければ「作る」を出す——**作るのは次の画面で本人が押してから**（URL で操作を起こさない、D-030）。
 */
export function useSwitcherKey(open: () => void) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === 'o') {
        e.preventDefault()
        open()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open])
}

const dirOf = (p: string) => (p.includes('/') ? p.slice(0, p.lastIndexOf('/')) : '')
const baseOf = (p: string) => p.slice(p.lastIndexOf('/') + 1).replace(/\.md$/, '')

/** ノートを開く先。Markdown は書く画面（直せないものは画面が読み取り専用で出す）、ほかはノートの画面。 */
export const openPath = (n: { id: number; path: string }) => (n.path.endsWith('.md') ? `/notes/${n.id}/edit` : `/notes/${n.id}`)

/** 端末の時刻での今日（YYYY-MM-DD）。日次ログの日付は端末が決める。 */
export function today(d = new Date()): string {
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}

/** useOpenDaily は今日の日次ログを開く（無ければテンプレートから作る。作るのは POST）。開けたら true。 */
export function useOpenDaily(): { openDaily: () => Promise<boolean>; error: string; busy: boolean } {
  const nav = useNavigate()
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const openDaily = async () => {
    setBusy(true); setError('')
    try {
      const vault = currentVault() || (await loadNames()).vault
      const r = await dailyNote(vault, today())
      if (!r.ok) { setError(`日次ログを開けない: ${r.error}`); return false }
      addName({ id: r.res.note_id, path: r.res.path, editable: true })
      nav(`/notes/${r.res.note_id}/edit`, { state: r.res.warn ? { notice: r.res.warn } : undefined })
      return true
    } catch (e) {
      setError((e as Error).message)
      return false
    } finally {
      setBusy(false)
    }
  }
  return { openDaily, error, busy }
}

export default function Switcher({ onClose }: { onClose: () => void }) {
  const nav = useNavigate()
  const { names, error } = useNames()
  const [q, setQ] = useState('')
  const [sel, setSel] = useState(0)
  const input = useRef<HTMLInputElement>(null)
  const daily = useOpenDaily()
  useEffect(() => { input.current?.focus() }, [])

  const hits = rank(q, names, 50)
  const offerCreate = q.trim() !== '' && !hasBase(q, names)
  const rows = hits.length + (offerCreate ? 1 : 0)

  const go = (i: number) => {
    if (i < hits.length) {
      nav(openPath(hits[i]))
    } else if (offerCreate) {
      nav(`/notes/new?name=${encodeURIComponent(q.trim())}`)
    }
    onClose()
  }

  const onKey = (e: React.KeyboardEvent) => {
    if (e.nativeEvent.isComposing) return
    switch (e.key) {
      case 'ArrowDown': e.preventDefault(); setSel((s) => Math.min(rows - 1, s + 1)); break
      case 'ArrowUp': e.preventDefault(); setSel((s) => Math.max(0, s - 1)); break
      case 'Enter': e.preventDefault(); if (rows > 0) go(sel); break
      case 'Escape': e.preventDefault(); onClose(); break
    }
  }

  return (
    <div className="switcher-back" onMouseDown={(e) => { if (e.target === e.currentTarget) onClose() }}>
      <div className="switcher" role="dialog" aria-modal="true" aria-label="ノートを開く">
        <input ref={input} value={q} placeholder="ノートの名前・パス" aria-label="ノートの名前・パス"
          onChange={(e) => { setQ(e.target.value); setSel(0) }} onKeyDown={onKey} />
        <div className="switcher-actions">
          <button className="btn" onClick={() => { void daily.openDaily().then((ok) => { if (ok) onClose() }) }} disabled={daily.busy}>今日の日次ログ</button>
          <button className="btn" onClick={() => { nav('/notes/new'); onClose() }}>新しいノート</button>
        </div>
        {(error || daily.error) && <p className="notice">{error || daily.error}</p>}
        <ul role="listbox" aria-label="候補">
          {hits.map((n, i) => (
            <li key={n.id} role="option" aria-selected={i === sel} className={i === sel ? 'on' : ''}
              onMouseEnter={() => setSel(i)} onClick={() => go(i)}>
              <span>{baseOf(n.path)}</span> <span className="muted mono">{dirOf(n.path)}</span>
            </li>
          ))}
          {offerCreate && (
            <li role="option" aria-selected={sel === hits.length} className={sel === hits.length ? 'on' : ''}
              onMouseEnter={() => setSel(hits.length)} onClick={() => go(hits.length)}>
              「{q.trim()}」を新しく作る…
            </li>
          )}
        </ul>
      </div>
    </div>
  )
}
