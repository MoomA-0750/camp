import { useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { api, type RuntimeSession } from '../api'
import { Empty, Failed, Loading, short, useAsync } from '../ui'

// Camp が起こしたセッションの一覧と、新しく起こす口。
//
// **起こす当人はここには居ない。** campd は camp ユーザーで動いていて
// `claude` を起こせないので、実行面（campd agent）が別に走っている。
// 「実行面が繋がっていない」ときに何もできないのは、その形のとおり。
export default function Runtime() {
  const [sp, setSp] = useSearchParams()
  const tab = sp.get('tab') ?? 'sessions'
  const [n, setN] = useState(0)
  const list = useAsync(() => api.runtime(), [n])
  // **API が null を返しても落ちない。**
  // Go の nil スライスは JSON で `null` になる。サーバー側でも空配列を
  // 返すようにしたが、受け手も畳んでおく——2026-09-04 に実ブラウザで
  // 真っ白になったのがこれ（テストでは出なかった）。
  const sessions = list.data?.sessions ?? []
  const allow = useAsync(() => api.allowlist(), [n, tab])

  const [cwd, setCwd] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  const start = async () => {
    setErr('')
    setBusy(true)
    try {
      await api.runtimeStart(cwd)
      setCwd('')
      setN((v) => v + 1)
    } catch (e) {
      setErr(String(e instanceof Error ? e.message : e))
    } finally {
      setBusy(false)
    }
  }

  const set = (patch: Record<string, string>) => {
    const next = new URLSearchParams(sp)
    for (const [k, v] of Object.entries(patch)) {
      if (v) next.set(k, v)
      else next.delete(k)
    }
    setSp(next)
  }
  // タブを移るときは、そのタブにしか意味の無い絞り込みを落とす。
  const go = (t: string) => set({ tab: t, kind: '', before: '' })

  return (
    <>
      <h2>セッション駆動</h2>
      <p className="sub muted">Camp が起こしたセッション。</p>

      <div className="tabs">
        <button className={tab === 'sessions' ? 'on' : ''} onClick={() => go('')}>
          走っているもの
        </button>
        <button className={tab === 'ended' ? 'on' : ''} onClick={() => go('ended')}>
          終わったもの
        </button>
        <button className={tab === 'allow' ? 'on' : ''} onClick={() => go('allow')}>
          許可した場所
        </button>
        <button className={tab === 'ssh' ? 'on' : ''} onClick={() => go('ssh')}>
          接続先の台帳
        </button>
      </div>

      {tab === 'allow' && <Allowlist reload={() => setN((v) => v + 1)} rows={allow} />}
      {tab === 'ssh' && <SSHLedger />}
      {tab === 'ended' && (
        <Ended kind={sp.get('kind') ?? ''} before={sp.get('before') ?? ''} set={set} />
      )}
      {tab === 'sessions' && (
        <>
          {list.loading && <Loading />}
          {list.error && <Failed error={list.error} />}
          {list.data && !list.data.agent_connected && (
            <p className="warn">
              実行面が繋がっていない。<code>systemctl --user start camp-agent</code> で起こす。
            </p>
          )}

          <form className="filters" onSubmit={(e) => { e.preventDefault(); void start() }}>
            <input
              type="text" placeholder="起こす場所（許可リストの中の絶対パス）"
              value={cwd} onChange={(e) => setCwd(e.target.value)} style={{ minWidth: '28rem' }}
            />
            <button disabled={busy || !cwd || !list.data?.agent_connected}>起こす</button>
          </form>
          {err && <Failed error={err} />}

          {!list.loading && !list.error && sessions.length === 0 && (
            <Empty>いま走っているものは無い。</Empty>
          )}
          {sessions.length > 0 && (
            <table>
              <thead>
                <tr>
                  <th className="nowrap">状態</th><th>場所</th>
                  <th className="nowrap">起こした時刻</th><th className="num">pid</th>
                </tr>
              </thead>
              <tbody>
                {sessions.map((s) => (
                  <tr key={s.id}>
                    <td className="nowrap">
                      {/* **開く導線を、折り返す小さなバッジ1つにしない。**
                          2026-09-04、実ブラウザで押せなかった。場所も含めて
                          リンクにする。 */}
                      <Link to={`/runtime/${s.id}`}><StateBadge state={s.state} /></Link>
                    </td>
                    <td className="mono wrap">
                      <Link to={`/runtime/${s.id}`}>{s.cwd}</Link>
                    </td>
                    <td className="nowrap">{short(s.created_at)}</td>
                    <td className="num">{s.pid || ''}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </>
      )}
    </>
  )
}

export function StateBadge({ state }: { state: string }) {
  const label: Record<string, string> = {
    starting: '起動中', idle: '待機', running: '実行中',
    stopping: '停止中', exited: '終了', orphaned: '孤児',
  }
  return <span className={'badge s-' + state}>{label[state] ?? state}</span>
}

// 終わり方。**DB には決まった語だけが入っている**（internal/session/end.go）。
// 文はここで作る。
const END_LABEL: Record<string, string> = {
  self: '子が自分で終わった',
  user_stop: '本人が止めた',
  idle_timeout: '放置で閉じた',
  turn_timeout: 'ターンが長すぎて止めた',
  stop_timeout: '止まらず見張りを諦めた',
  start_failed: '起こせなかった',
  agent_lost: '実行面が落ちた',
  unseen: '見ていない間に終わっていた',
  reaped: '残っていたものを始末した',
}
const CAUSES = Object.keys(END_LABEL)

const AT_END: Record<string, string> = {
  running: '動いている途中', idle: '待機中', starting: '起動中',
}

// 横断的な絞り込み。本人が探したいのはここ（2026-09-11）。
const KINDS: [string, string][] = [
  ['', 'すべて'],
  ['mid', '動いている途中で終わった'],
  ['waiting', '承認を待たせたまま終わった'],
  ['ignored', '承認を期限切れにした'],
]

// endLabel は終わり方の文。**記録を始める前に終わったものは「記録なし」**
// ——理由の文から推し量って埋めない。
export function endLabel(cause?: string): string {
  if (!cause) return '記録なし'
  return END_LABEL[cause] ?? cause
}

export function atEndLabel(state?: string): string {
  if (!state) return ''
  return AT_END[state] ?? state
}

function Ended({ kind, before, set }: {
  kind: string
  before: string
  set: (patch: Record<string, string>) => void
}) {
  const page = useAsync(() => api.runtimeEnded(kind, before), [kind, before])
  const rows = page.data?.sessions ?? []
  const counts = page.data?.counts ?? {}
  const next = page.data?.next ?? ''

  const chip = (k: string, label: string) => (
    <button key={k || 'all'} className={kind === k ? 'on' : ''}
      onClick={() => set({ kind: k, before: '' })}>
      {label} <span className="muted">{counts[k || 'all'] ?? 0}</span>
    </button>
  )

  return (
    <>
      <div className="chips">{KINDS.map(([k, l]) => chip(k, l))}</div>
      <div className="chips">
        {CAUSES.filter((c) => (counts[c] ?? 0) > 0 || kind === c).map((c) => chip(c, END_LABEL[c]))}
        {((counts.unknown ?? 0) > 0 || kind === 'unknown') && chip('unknown', '記録なし')}
      </div>

      {page.loading && <Loading />}
      {page.error && <Failed error={page.error} />}
      {!page.loading && !page.error && rows.length === 0 && (
        <Empty>{kind ? '当てはまるものは無い。' : 'まだ1本も終わっていない。'}</Empty>
      )}
      {rows.length > 0 && (
        <table>
          <thead>
            <tr>
              {/* 探したいもの（終わり方・そのとき・承認）を、長いパスより前に置く。
                  狭い画面ではパスの手前までしか見えない（2026-09-11、400px で撮った）。 */}
              <th className="nowrap">終わった時刻</th>
              <th className="nowrap">終わり方</th><th className="nowrap">そのとき</th>
              <th className="nowrap">承認</th><th>場所</th><th className="num nowrap">コード</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((s) => (
              <tr key={s.id}>
                <td className="nowrap">
                  <Link to={`/runtime/${s.id}`}>{short(s.ended_at ?? '')}</Link>
                </td>
                <td className="nowrap">{endLabel(s.end_cause)}</td>
                <td className="nowrap">{atEndLabel(s.end_state)}</td>
                <td className="nowrap"><ApprovalSummary s={s} /></td>
                <td className="mono wrap"><Link to={`/runtime/${s.id}`}>{s.cwd}</Link></td>
                <td className="num">{s.exit_code ?? ''}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {(before || next) && (
        <p className="sub">
          {before && <button onClick={() => set({ before: '' })}>最新に戻る</button>}{' '}
          {next && <button onClick={() => set({ before: next })}>さらに古いもの</button>}
        </p>
      )}
    </>
  )
}

// 承認の内訳。**待たせたまま・期限切れは目立たせる**（探したいのはそこ）。
export function ApprovalSummary({ s }: { s: RuntimeSession }) {
  const asked = s.approvals_asked ?? 0
  if (asked === 0) return <span className="muted">—</span>
  const left = s.approvals_left_waiting ?? 0
  const late = s.approvals_timed_out ?? 0
  return (
    <>
      {asked} 件
      {left > 0 && <> · <span className="warn-text">待たせたまま {left}</span></>}
      {late > 0 && <> · <span className="warn-text">期限切れ {late}</span></>}
    </>
  )
}

function Allowlist({ rows, reload }: {
  rows: ReturnType<typeof useAsync<import('../api').Allowed[]>>
  reload: () => void
}) {
  const [path, setPath] = useState('')
  const [pw, setPw] = useState('')
  const [err, setErr] = useState('')

  const run = async (fn: () => Promise<unknown>) => {
    setErr('')
    try {
      await fn()
      setPw('')
      reload()
    } catch (e) {
      setErr(String(e instanceof Error ? e.message : e))
    }
  }

  return (
    <>
      <p className="sub muted">
        ここに無い場所ではセッションを起こせない。
        <strong>変更にはパスワードの再入力が要る。</strong>
      </p>
      <form className="filters" onSubmit={(e) => e.preventDefault()}>
        <input type="text" placeholder="許すディレクトリ（絶対パス）" value={path}
          onChange={(e) => setPath(e.target.value)} style={{ minWidth: '24rem' }} />
        <input type="password" placeholder="パスワード" value={pw}
          onChange={(e) => setPw(e.target.value)} autoComplete="current-password" />
        <button disabled={!path || !pw}
          onClick={() => void run(() => api.allowlistAdd(path, pw))}>足す</button>
      </form>
      {err && <Failed error={err} />}
      {rows.loading && <Loading />}
      {/* **null を「まだ読み込み中」と混ぜない。** data が null でも
          読み終わっていれば「空」と出す——出さないと、何も無い画面が
          「読めなかった」のか「空だった」のか分からない。 */}
      {!rows.loading && !rows.error && (rows.data ?? []).length === 0 && (
        <Empty>空。この状態では1本も起こせない。</Empty>
      )}
      {(rows.data ?? []).length > 0 && (
        <table>
          <thead>
            <tr>
              <th>場所</th><th className="nowrap">覚え書き</th>
              <th className="nowrap">足した時刻</th><th></th>
            </tr>
          </thead>
          <tbody>
            {(rows.data ?? []).map((a) => (
              <tr key={a.id}>
                <td className="mono wrap">{a.path}</td>
                <td>{a.note}</td>
                <td className="nowrap">{short(a.added_at)}</td>
                <td className="nowrap">
                  <button disabled={!pw}
                    onClick={() => void run(() => api.allowlistRemove(a.path, pw))}>外す</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  )
}

function SSHLedger() {
  const [n, setN] = useState(0)
  const rows = useAsync(() => api.sshHosts(), [n])
  const [pw, setPw] = useState('')
  const [err, setErr] = useState('')

  const run = async (fn: () => Promise<unknown>) => {
    setErr('')
    try {
      await fn()
      setN((v) => v + 1)
    } catch (e) {
      setErr(String(e instanceof Error ? e.message : e))
    }
  }

  return (
    <>
      <p className="sub muted">
        <code>~/.ssh/config</code> は<strong>読むだけ</strong>で、書き戻さない。
        取り込みで許可は変わらない。<strong>繋ぐのはまだできない。</strong>
      </p>
      <form className="filters" onSubmit={(e) => e.preventDefault()}>
        <button onClick={() => void run(() => api.sshScan())}>
          ~/.ssh/config を読み直す
        </button>
        <input type="password" placeholder="パスワード（許可の変更に要る）" value={pw}
          onChange={(e) => setPw(e.target.value)} autoComplete="current-password" />
      </form>
      {err && <Failed error={err} />}
      {rows.loading && <Loading />}
      {!rows.loading && !rows.error && (rows.data ?? []).length === 0 && (
        <Empty>台帳は空。読み直すと入る。</Empty>
      )}
      {(rows.data ?? []).length > 0 && (
        <table>
          <thead>
            <tr>
              <th className="nowrap">許可</th><th>エイリアス</th><th>接続先</th>
              <th className="nowrap">Tailscale</th><th>覚え書き</th>
            </tr>
          </thead>
          <tbody>
            {(rows.data ?? []).map((d) => (
              <tr key={d.id}>
                <td className="nowrap">
                  <button disabled={!pw}
                    onClick={() => void run(() => api.sshAllow(d.alias, !d.allowed, pw))}>
                    {d.allowed ? '許可済み' : '不許可'}
                  </button>
                </td>
                <td className="mono">{d.alias}</td>
                <td className="mono muted">
                  {d.user ? d.user + '@' : ''}{d.hostname}{d.port ? ':' + d.port : ''}
                </td>
                <td className="mono muted">{d.tailscale_ip}</td>
                <td>{d.note}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  )
}
