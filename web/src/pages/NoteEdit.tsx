import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link, useLocation, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { api, saveNote, trashNote, type NoteSaveReply, type NoteSource, type NoteSync, type NoteTrashed, type NoteWriteRow, type Ref, type WikiTarget } from '../api'
import Editor, { type EditorHandle } from '../editor/Editor'
import Preview, { wikiTargets } from '../editor/Preview'
import { comparing, dirty, init, leaving, step, type Effect, type Event, type State } from '../editor/autosave'
import { drafts, dropDraft, dropDraftIf, keep, putDraft, tabId, type Draft } from '../editor/drafts'
import { currentNames, loadNames } from '../editor/names'
import { wikiComplete } from '../editor/wikicomplete'
import { openPath } from '../Switcher'
import { useMdTree } from '../editor/useMdTree'
import { useWidth } from '../useWidth'
import { Failed, Loading, short } from '../ui'

/**
 * ノートを書く画面（Phase 5 / M54、2026-09-13）。
 *
 * 書くのが止まって 2 秒で保存する（本人の決定2）。書くのは実行面で、ぶつかったら重ならなければ合わせ、
 * 重なれば止めて並べる（決定6）。指示の紙はパスワードを訊いて手で保存する（決定8）。
 * 状態の移り方は editor/autosave.ts（純粋な関数）が決め、ここは出来事を渡して言われたことをするだけ。
 */
const saveAfterMs = 2000
const pollMs = 20_000

export default function NoteEdit() {
  const { id } = useParams()
  const nid = Number(id)
  const [sp] = useSearchParams()
  const restore = Number(sp.get('restore') ?? 0) || 0
  const [src, setSrc] = useState<NoteSource | null>(null)
  const [err, setErr] = useState('')
  const [start, setStart] = useState<Start | null>(null)
  const [offer, setOffer] = useState<Draft | null>(null)

  useEffect(() => {
    let alive = true
    setSrc(null); setErr(''); setStart(null); setOffer(null)
    ;(async () => {
      try {
        const s = await api.noteSource(nid)
        if (!alive) return
        setSrc(s)
        if (restore) {
          // Camp が書いた版を**ディスクの今の版と並べて**開く。本人が「自分の版で上書きする」を押すまで書かない
          // （outer gate の Fable 1: 以前は開いて数秒で、合わせずにディスクの版を置き換えていた）。
          const w = await api.noteWrite(restore)
          if (!alive) return
          if (w.body === s.body) {
            setStart({ text: s.body, base: s.sha256, force: false, note: 'その Camp の版は、いまのディスクと同じ' })
            return
          }
          setStart({ text: w.body, base: s.sha256, force: false,
            compare: { disk: s.body, diskSha: s.sha256, why: 'Camp が書いた版（自分の版）と、いまのディスクの版を並べている。どちらにするか選ぶまで保存しない。' } })
          return
        }
        // もう保存された中身の控えは要らない（閉じる瞬間の保存が届いて、控えだけ残った場合など）。
        const all = drafts(nid)
        all.filter((x) => x.text === s.body).forEach((x) => dropDraft(nid, x.tab))
        const d = all.find((x) => x.text !== s.body)
        if (d) setOffer(d)
        setStart({ text: s.body, base: s.sha256, force: false })
      } catch (e) {
        if (alive) setErr((e as Error).message)
      }
    })()
    return () => { alive = false }
  }, [nid, restore])

  if (err) return <Failed error={err} />
  if (!src || !start) return <Loading />
  return (
    <Writer key={`${nid}:${start.base}:${start.text.length}:${start.force}:${!!start.compare}`} src={src} start={start}
      offer={offer}
      onRestore={(d) => { setOffer(null); setStart({ text: d.text, base: d.base, force: true, note: `控え（${new Date(d.at).toLocaleString('ja-JP')}）から戻した` }) }}
      onDiscardOffer={() => { if (offer) dropDraft(nid, offer.tab); setOffer(null) }} />
  )
}

type Start = {
  text: string; base: string; force: boolean; note?: string
  /** compare があれば、ディスクの版と並べて止まった状態で始める（Camp の版で開く）。 */
  compare?: { disk: string; diskSha: string; why: string }
}

function Writer({ src, start, offer, onRestore, onDiscardOffer }: {
  src: NoteSource
  start: Start
  offer: Draft | null
  onRestore: (d: Draft) => void
  onDiscardOffer: () => void
}) {
  const nid = src.note_id
  const manual = !!src.instruction
  const readOnly = !src.editable
  const tab = useRef(tabId())
  const editor = useRef<EditorHandle>(null)
  const password = useRef('')
  const location = useLocation()
  const arrived = (location.state as { notice?: string } | null)?.notice
  const [s, setS] = useState<State>(() => start.compare
    ? comparing(start.text, start.compare.disk, start.compare.diskSha, manual, start.compare.why)
    : { ...init(start.text, start.base, manual), force: start.force, notice: start.note ?? arrived })
  const sr = useRef(s)
  const [warn, setWarn] = useState('')

  const dispatch = useCallback((e: Event) => {
    const r = step(sr.current, e)
    sr.current = r.s
    setS(r.s)
    for (const fx of r.fx) run(fx)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const run = (fx: Effect) => {
    switch (fx.type) {
      case 'save': {
        const t = tab.current
        const p = saveNote(nid, fx.base, fx.body, manual ? password.current : undefined)
        inflight.current = p
        void p.then((reply) => {
          if (inflight.current === p) inflight.current = null
          // 控えは、送った本文と同じときだけ消す（そのあと書いた分の控えを消さない。Fable 8）。
          if (reply.ok && reply.res.status !== 'conflict') dropDraftIf(nid, t, fx.body)
          dispatch({ type: 'reply', reply, now: Date.now() })
        })
        break
      }
      case 'replace':
        editor.current?.replace(fx.text)
        break
      case 'keep':
        if (!keep(nid, fx.text, fx.why)) setWarn('ブラウザに控えを残せなかった（容量か設定）。自分の版は下の欄から写しておくこと')
        // 書きかけの控えは、いま控えた版と同じものなので消す（開き直したときに「保存していない控え」と出さない）。
        else dropDraft(nid, tab.current)
        break
    }
  }

  const inflight = useRef<Promise<NoteSaveReply> | null>(null)

  // 書くのが止まって 2 秒で保存。控えは 300ms で。
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const draftTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const onChange = useCallback((text: string) => {
    dispatch({ type: 'edit', text })
    clearTimeout(timer.current)
    timer.current = setTimeout(() => dispatch({ type: 'due', now: Date.now() }), saveAfterMs)
    clearTimeout(draftTimer.current)
    draftTimer.current = setTimeout(() => {
      if (!putDraft({ note: nid, tab: tab.current, text: sr.current.text, base: sr.current.base })) {
        setWarn('ブラウザに控えを残せない（容量か設定）。保存できないまま閉じると失う')
      }
    }, 300)
  }, [dispatch, nid])

  // やり直しの時刻を見る・隠れたら待たずに保存・閉じるときに止める。
  useEffect(() => {
    const iv = setInterval(() => dispatch({ type: 'due', now: Date.now() }), 5000)
    const vis = () => { if (document.visibilityState === 'hidden') dispatch({ type: 'due', now: Date.now() }) }
    const unload = (e: BeforeUnloadEvent) => { if (dirty(sr.current)) { e.preventDefault(); e.returnValue = '' } }
    document.addEventListener('visibilitychange', vis)
    window.addEventListener('beforeunload', unload)
    return () => {
      clearInterval(iv); clearTimeout(timer.current); clearTimeout(draftTimer.current)
      document.removeEventListener('visibilitychange', vis)
      window.removeEventListener('beforeunload', unload)
      // 画面の中で別のノートへ移る（スイッチャー・リンク）ときも失わない。
      //   1. **いまの本文をその場で控えに残す**（300ms の間引きを待たない。保存の途中でも。outer gate の codex）
      //   2. 保存を待たずに送る。送っている途中なら、その返事の版を土台にしてから送る
      //   3. 書けたら控えを消す（ぶつかった・変換の途中・止まっているなら控えが残り、開き直すと「控えから戻す」が出る）
      const cur = sr.current
      const plan = leaving(cur)
      if (plan.keep !== null) {
        const body = plan.keep
        const t = tab.current
        putDraft({ note: nid, tab: t, text: body, base: cur.base })
        const send = (base: string) => void saveNote(nid, base, body).then((r) => {
          if (r.ok && r.res.status !== 'conflict') dropDraftIf(nid, t, body)
        })
        if (plan.send === 'now') send(cur.base)
        if (plan.send === 'afterReply') {
          void inflight.current?.then((r) => {
            if (!r.ok) return
            if (r.res.status === 'saved') send(r.res.sha256)
            else if (r.res.status === 'merged') send(r.res.sent_sha256)
          })
        }
      }
    }
  }, [dispatch, nid])

  // 開いている間にディスクが変わったか（ハッシュだけ）。同期の様子も。
  const [sync, setSync] = useState<NoteSync | null>(null)
  const gone = useRef(false) // .trash へ移したら、もうディスクのハッシュを訊かない
  useEffect(() => {
    let alive = true
    const tick = async () => {
      try {
        if (gone.current) throw new Error('移した')
        const d = await api.noteDiskSha(nid)
        if (alive) dispatch({ type: 'diskChanged', sha: d.sha256 })
      } catch { /* 次に */ }
      try {
        const y = await api.noteSync()
        if (alive) setSync(y)
      } catch { /* 次に */ }
    }
    void tick()
    const iv = setInterval(tick, pollMs)
    return () => { alive = false; clearInterval(iv) }
  }, [nid, dispatch])

  // プレビュー。
  const { tree, error: treeErr } = useMdTree(s.text)
  const [targets, setTargets] = useState<Record<string, WikiTarget | undefined>>({})
  useEffect(() => {
    const want = wikiTargets(tree).filter((t) => !(t in targets)).slice(0, 500)
    if (want.length === 0) return
    let alive = true
    api.noteResolve(nid, want).then((got) => { if (alive) setTargets((m) => ({ ...m, ...got })) }, () => {})
    return () => { alive = false }
  }, [tree, nid, targets])

  // wikilink の補完（M55）。名前の一覧は開いたときに新しくする。
  const complete = useMemo(() => wikiComplete(currentNames), [])
  useEffect(() => { void loadNames().catch(() => {}) }, [])

  const [trashed, setTrashed] = useState<NoteTrashed | null>(null)

  const [ref, width] = useWidth(1000)
  const narrow = width < 760
  const [pane, setPane] = useState<'write' | 'view'>('write')

  return (
    <div ref={ref}>
      <h2>{src.path.replace(/\.md$/, '').split('/').pop()}</h2>
      <p className="sub muted">
        <span className="mono">{src.path}</span>{' · '}<Link to={`/notes/${nid}`}>ノートの画面へ</Link>
      </p>
      {readOnly && <p className="notice">このノートは編集面では直せない: {src.read_only}</p>}
      {manual && <p className="notice">エージェントの指示の紙なので、自動では保存しない。直したらパスワードを入れて保存する。</p>}
      {offer && (
        <p className="notice">
          保存していない控えがある（{new Date(offer.at).toLocaleString('ja-JP')}）。{' '}
          <button className="btn" onClick={() => onRestore(offer)}>控えから戻す</button>{' '}
          <button className="btn" onClick={onDiscardOffer}>戻さない</button>
        </p>
      )}
      <Status s={s} sync={sync} />
      {warn && <p className="notice">{warn}</p>}
      {s.mode === 'conflict' && s.conflict && (
        <Conflict mine={s.text} disk={s.conflict.disk} why={s.conflict.why}
          onLoadDisk={() => dispatch({ type: 'loadDisk' })} onKeepMine={() => dispatch({ type: 'keepMine' })} />
      )}
      {manual && !readOnly && (
        <form className="filters" onSubmit={(e) => { e.preventDefault(); dispatch({ type: 'manualSave' }) }}>
          <input type="password" autoComplete="current-password" placeholder="パスワード" aria-label="パスワード"
            onChange={(e) => { password.current = e.target.value }} />
          <button className="btn" type="submit" disabled={!dirty(s) || s.mode === 'saving'}>保存</button>
        </form>
      )}
      {narrow && (
        <div className="filters" role="tablist">
          <button className={'btn' + (pane === 'write' ? ' on' : '')} aria-pressed={pane === 'write'} onClick={() => setPane('write')}>書く</button>
          <button className={'btn' + (pane === 'view' ? ' on' : '')} aria-pressed={pane === 'view'} onClick={() => setPane('view')}>見る</button>
        </div>
      )}
      <div className={narrow ? 'edit-one' : 'edit-split'}>
        <div hidden={narrow && pane !== 'write'}>
          <Editor ref={editor} initial={start.text} readOnly={readOnly} onChange={onChange} complete={complete}
            onCompose={(on) => dispatch({ type: 'compose', on })} />
        </div>
        <div className="edit-preview" hidden={narrow && pane !== 'view'}>
          {treeErr && <p className="notice">{treeErr}</p>}
          <Preview tree={tree} targets={targets} />
          <Backlinks nid={nid} />
          <Versions nid={nid} />
        </div>
      </div>
      {!readOnly && !trashed && (
        <Trash nid={nid} s={s} manual={manual} password={password} onDone={(t) => {
          if (t.status === 'trashed') {
            gone.current = true
            drafts(nid).forEach((d) => dropDraft(nid, d.tab))
          }
          setTrashed(t)
        }} />
      )}
      {trashed && <TrashedNotice t={trashed} onAgain={() => setTrashed(null)} />}
    </div>
  )
}

/** Camp が書いた版（merge で消えた段落・上書きされた版を含む。blobs から画面で辿れるように。Fable 2）。 */
function Versions({ nid }: { nid: number }) {
  const [rows, setRows] = useState<NoteWriteRow[] | null>(null)
  const [open, setOpen] = useState(false)
  useEffect(() => {
    if (!open) return
    let alive = true
    api.noteWrites(nid).then((w) => { if (alive) setRows(w) }, () => {})
    return () => { alive = false }
  }, [nid, open])
  const label: Record<string, string> = {
    planned: '書けたか確かめている', pending: 'commit 待ち', superseded: '次の保存に置き換わった',
    committed: 'commit 済み', overwritten: 'ほかで書き換えられた',
  }
  const writes = rows?.filter((w) => w.op === 'write') ?? []
  return (
    <details className="edit-backlinks" onToggle={(e) => setOpen((e.target as HTMLDetailsElement).open)}>
      <summary>Camp が書いた版</summary>
      {rows && writes.length === 0 && <p className="muted">まだ無い。</p>}
      {writes.length > 0 && (
        <ul>
          {writes.map((w) => (
            <li key={w.id}>
              {short(w.at)} {label[w.state] ?? w.state}{' '}
              <Link to={`/notes/${nid}/edit?restore=${w.id}`}>いまの版と並べる</Link>
            </li>
          ))}
        </ul>
      )}
    </details>
  )
}

function Backlinks({ nid }: { nid: number }) {
  const [back, setBack] = useState<Ref[] | null>(null)
  useEffect(() => {
    let alive = true
    api.noteLinks(nid).then((l) => { if (alive) setBack(l.back) }, () => {})
    return () => { alive = false }
  }, [nid])
  if (!back) return null
  // 同じノートから何本来ていても1つにまとめる。
  const from = [...new Map(back.map((r) => [r.from_id, r.from_path])).entries()]
  return (
    <section className="edit-backlinks">
      <h3>被リンク（{from.length}）</h3>
      {from.length === 0 ? <p className="muted">このノートを指しているノートは無い。</p> : (
        <ul>
          {from.map(([id, path]) => (
            <li key={id}><Link to={openPath({ id, path })} className="mono">{path}</Link></li>
          ))}
        </ul>
      )}
    </section>
  )
}

/**
 * `.trash/` へ移す（M55）。**画面で見ている版（base）とディスクが同じときだけ**移す（サーバーが照らす）。
 * 保存していない変更があれば押せない。押したらもう一度確かめる。
 */
function Trash({ nid, s, manual, password, onDone }: {
  nid: number; s: State; manual: boolean; password: React.MutableRefObject<string>; onDone: (t: NoteTrashed) => void
}) {
  const [ask, setAsk] = useState(false)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const blocked = dirty(s) || s.mode === 'saving' || s.mode === 'conflict' || s.sending !== undefined
  const go = async () => {
    setBusy(true); setErr('')
    const r = await trashNote(nid, s.base, manual ? password.current : undefined)
    setBusy(false); setAsk(false)
    if (!r.ok) { setErr(r.error); return }
    onDone(r.res)
  }
  return (
    <section className="edit-trash">
      {!ask ? (
        <button className="btn" disabled={blocked} onClick={() => setAsk(true)}>.trash へ移す</button>
      ) : (
        <p>
          このノートを <span className="mono">.trash/</span> へ移し、git からは削除として commit・push する
          （中身は .trash と Camp の控えに残る）。{manual && '指示の紙なので、上の欄のパスワードを使う。'}{' '}
          <button className="btn" disabled={busy || blocked} onClick={() => void go()}>移す</button>{' '}
          <button className="btn" onClick={() => setAsk(false)}>やめる</button>
        </p>
      )}
      {blocked && !ask && <span className="muted"> 保存していない変更があるうちは移せない</span>}
      {err && <p className="notice">移せなかった: {err}</p>}
    </section>
  )
}

function TrashedNotice({ t, onAgain }: { t: NoteTrashed; onAgain: () => void }) {
  const nav = useNavigate()
  const msg = {
    trashed: `.trash へ移した（${t.to}）。1 分後に削除として commit する。`,
    changed: 'ほかでこのノートが変わっていたので移していない。開き直して確かめてから移す。',
    gone: 'もう無い（ほかで消された・移された）。',
    kept: `移す間にほかで書かれた。移した版は ${t.to} に、新しい版は元の場所にある。`,
    waiting: '書いた版の commit を待っている（1 分ほど）。その版を git の履歴に残してから移すので、commit されてからもう一度押す。',
  }[t.status]
  return (
    <div className="notice" role="status">
      {msg}{' '}
      {t.status === 'changed' && <button className="btn" onClick={() => { onAgain(); nav(0) }}>開き直す</button>}
      {t.status === 'waiting' && <button className="btn" onClick={onAgain}>わかった</button>}
      {(t.status === 'trashed' || t.status === 'gone' || t.status === 'kept') && <Link to="/notes">ノートの一覧へ</Link>}
    </div>
  )
}

function Status({ s, sync }: { s: State; sync: NoteSync | null }) {
  const label = (() => {
    switch (s.mode) {
      case 'saving': return '保存している…'
      case 'conflict': return s.conflict?.why ? '2つの版を並べている。下で選ぶまで保存しない' : 'ほかの書き手と同じところを直していた。下で選ぶまで保存しない'
      case 'blocked': return `保存できない: ${s.error}（直すまで保存しない）`
      case 'retry': return `${s.error}。書いた中身は画面とブラウザの控えにある。30 秒おきにやり直す`
      case 'manual': return (s.error ? `保存できなかった: ${s.error}。` : '') + (dirty(s) ? '保存していない変更がある' : '保存済み')
      default: return dirty(s) ? '書いている（止まったら保存する）' : '保存済み'
    }
  })()
  const push = sync?.push
  const pushLabel = push && ({
    agent_pending: `GitHub へ出すのを見送っている: Camp が作ったと確かめられない commit が ${push.pending ?? ''} 本ある（エージェントの commit など。本人が確かめて push すると続きが出る）`,
    hook_refused: `GitHub へ出すのを pre-push が止めた: ${push.detail ?? ''}`,
    merge_conflict: 'GitHub の変更と取り込むとぶつかるので出していない',
    behind_dirty: 'GitHub の変更を取り込めない（作業中のファイルと重なる）ので出していない',
    failed: `GitHub へ出せない: ${push.detail ?? ''}`,
    remote_refused: `GitHub が受け取りを断った（push protection などの規則）: ${push.detail ?? ''}`,
  } as Record<string, string>)[push.kind]
  return (
    <div className="edit-status" role="status">
      <span className={s.mode === 'blocked' || s.mode === 'conflict' || s.error ? 'error' : ''}>{label}</span>
      {s.notice && <span className="muted"> · {s.notice}</span>}
      {sync && sync.pending > 0 && <span className="muted"> · commit 待ち {sync.pending}</span>}
      {pushLabel && <div className="notice">{pushLabel}</div>}
      {sync && sync.overwritten.length > 0 && (
        <details className="notice">
          <summary>Camp で書いたあと、commit の前にほかで書き換えられたもの（{sync.overwritten.length}）</summary>
          <ul>
            {sync.overwritten.map((o) => (
              <li key={o.id}>
                <span className="mono">{o.path}</span> {short(o.at)}{' '}
                <OverwrittenLink wid={o.id} />
              </li>
            ))}
          </ul>
        </details>
      )}
    </div>
  )
}

function OverwrittenLink({ wid }: { wid: number }) {
  const [noteId, setNoteId] = useState(0)
  useEffect(() => { api.noteWrite(wid).then((w) => setNoteId(w.note_id), () => {}) }, [wid])
  if (!noteId) return null
  return <Link to={`/notes/${noteId}/edit?restore=${wid}`}>Camp の版と並べる</Link>
}

function Conflict({ mine, disk, why, onLoadDisk, onKeepMine }: {
  mine: string; disk: string; why?: string; onLoadDisk: () => void; onKeepMine: () => void
}) {
  return (
    <section className="edit-conflict">
      <p>
        {why ?? '書いている間に、ほかの書き手（エージェント・別の端末）がこのノートの同じところを直していた。'}
        どちらの版も残っている——自分の版はこの画面に、ほかの版はディスクと Camp の控えにある。
      </p>
      <div className="filters">
        <button className="btn" onClick={onLoadDisk}>ほかの版を読み込む（自分の版はブラウザに控える）</button>
        <button className="btn" onClick={onKeepMine}>自分の版で上書きする</button>
      </div>
      <div className="edit-split">
        <div><h3>自分の版</h3><pre className="note-body">{mine}</pre></div>
        <div><h3>ほかの版（ディスク）</h3><pre className="note-body">{disk}</pre></div>
      </div>
    </section>
  )
}
