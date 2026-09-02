import { useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { api, type Ghost } from '../api'
import { Empty, Failed, Loading, num, short, useAsync } from '../ui'

const REASON: Record<string, { label: string; why: string }> = {
  gone: {
    label: '実体が無い',
    why: 'Vault の下のパスだが、いまそのファイルは無い。フォルダごと再編されたものが多い。',
  },
  'other-case': {
    label: '表記違いのルート',
    why: '改名前のルート（大文字小文字が違う）を指す記録。畳んで現存ノートに繋ぐと、'
      + 'もう存在しないディレクトリへの操作を現存ノートへの操作として見せることになる。',
  },
  worktree: {
    label: 'worktree のコピー',
    why: '.claude/worktrees の中の複製。ノート本体とは別のファイル。',
  },
  hidden: {
    label: '隠しディレクトリ',
    why: 'ドット始まりのディレクトリ。索引の対象外。',
  },
}

export default function VaultHealth() {
  const [sp, setSp] = useSearchParams()
  const tab = sp.get('tab') ?? 'issues'
  const ghosts = useAsync(() => api.ghosts(), [])
  const issues = useAsync(() => api.vaultIssues(), [])

  return (
    <>
      <h2>Vault の点検</h2>
      <p className="sub muted">
        索引と実体が食い違っている場所と、Obsidian と解釈が分かれうるリンク。
      </p>

      <form className="filters" onSubmit={(e) => e.preventDefault()}>
        <select value={tab} onChange={(e) => setSp({ tab: e.target.value })}>
          <option value="issues">リンクの問題</option>
          <option value="ghosts">実体の無い触り跡</option>
        </select>
      </form>

      {tab === 'issues' && (
        <>
          {issues.loading && <Loading />}
          {issues.error && <Failed error={issues.error} />}
          {issues.data && (
            <>
              <h3>曖昧に解決したリンク（{num(issues.data.ambiguous.length)}）</h3>
              <p className="muted">
                同じ名前のノートが複数あって、一意に決まらなかったもの。
                Camp は候補の先頭を選ぶが、<strong>Obsidian の規則と一致する保証はまだ無い</strong>
                （Obsidian のメタデータキャッシュはディスクに無く、突き合わせられない）。
              </p>
              {issues.data.ambiguous.length === 0 && <Empty>曖昧なリンクは無い。</Empty>}
              <ul className="rows">
                {issues.data.ambiguous.map((r, i) => (
                  <li key={i}>
                    <Link to={`/notes/${r.from_id}`}>{r.from_path}</Link>
                    <span className="tag warn">[[{r.target}]]</span>
                    <div className="row-meta warn-text">
                      候補: {(r.candidates ?? []).map((c, j) => (
                        <span key={j}>{j > 0 && ' / '}{j === 0 ? <strong>{c}</strong> : c}</span>
                      ))}
                    </div>
                  </li>
                ))}
              </ul>

              <h3>宙吊りのリンク（{num(issues.data.dangling.length)}）</h3>
              <p className="muted">
                解決先が無いもの。Obsidian でも宙吊りリンクは意味を持つので消さない。
              </p>
              <ul className="rows">
                {issues.data.dangling.map((r, i) => (
                  <li key={i}>
                    <Link to={`/notes/${r.from_id}`}>{r.from_path}</Link>
                    <div className="row-meta">
                      <span className="mono">[[{r.target}]]</span>
                      {r.line ? <span>{r.line} 行目</span> : null}
                    </div>
                  </li>
                ))}
              </ul>
            </>
          )}
        </>
      )}

      {tab === 'ghosts' && (
        <>
          {ghosts.loading && <Loading />}
          {ghosts.error && <Failed error={ghosts.error} />}
          {ghosts.data?.length === 0 && <Empty>食い違いは無い。</Empty>}
          {ghosts.data && ghosts.data.length > 0 && (
            <>
              {['gone', 'other-case', 'worktree', 'hidden'].map((reason) => {
                const rows = ghosts.data!.filter((g) => g.reason === reason)
                if (rows.length === 0) return null
                const meta = REASON[reason]
                return (
                  <div key={reason}>
                    <h3>{meta.label}（{num(rows.length)}）</h3>
                    <p className="muted">{meta.why}</p>
                    <ul className="rows">
                      {rows.map((g, i) => <GhostRow key={i} g={g} />)}
                    </ul>
                  </div>
                )
              })}
            </>
          )}
        </>
      )}
    </>
  )
}


/**
 * 中身の残っている ghost は、その場で開けるようにする。
 *
 * ここが Camp の存在理由がいちばん露骨に出る場所で、**Vault にも git にも
 * もう無いものが読める**。だから「何版あるか」で終わらせず、実際に開ける
 * ところまで繋ぐ。中身が無い ghost は触った記録しかないので、そう書く。
 */
function GhostRow({ g }: { g: Ghost }) {
  const [open, setOpen] = useState(0)
  const [body, setBody] = useState<string | null>(null)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  const show = (id: number) => {
    if (open === id) { setOpen(0); setBody(null); return }
    setOpen(id); setBody(null); setErr(''); setBusy(true)
    api.backupContent(id).then(
      (t) => { setBody(t); setBusy(false) },
      (e: Error) => { setErr(e.message); setBusy(false) },
    )
  }

  return (
    <li>
      <span className="mono">{g.path}</span>
      <div className="row-meta">
        <span>{num(g.touches)} 回</span>
        <span>{num(g.sessions)} セッション</span>
        <span>最後 {short(g.last_at)}</span>
      </div>
      {g.versions && g.versions.length > 0 ? (
        <div className="row-meta">
          <span>Camp に残っている中身:</span>
          {g.versions.map((v) => (
            <button
              key={v.backup_id} type="button" className="linkish"
              onClick={() => show(v.backup_id)}
            >
              v{v.version}
              {v.at ? `（${short(v.at)}）` : ''}
              {open === v.backup_id ? ' ▲' : ' ▼'}
            </button>
          ))}
        </div>
      ) : (
        <div className="row-meta muted">触った記録だけ。中身は残っていない。</div>
      )}
      {open > 0 && (
        <>
          {busy && <Loading />}
          {err && <Failed error={err} />}
          {body !== null && <pre className="note-body">{body}</pre>}
        </>
      )}
    </li>
  )
}
