import { useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { api } from '../api'
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

  return (
    <>
      <h2>セッション駆動</h2>
      <p className="sub muted">
        Camp が起こしたセッション。実行面（<code>campd agent</code>）が本人のユーザーで
        <code>claude</code> を起こし、campd は決めて記録する。
      </p>

      <nav className="tabs">
        <button className={tab === 'sessions' ? 'on' : ''} onClick={() => set({ tab: '' })}>
          走っているもの
        </button>
        <button className={tab === 'allow' ? 'on' : ''} onClick={() => set({ tab: 'allow' })}>
          許可した場所
        </button>
        <button className={tab === 'ssh' ? 'on' : ''} onClick={() => set({ tab: 'ssh' })}>
          接続先の台帳
        </button>
      </nav>

      {tab === 'allow' && <Allowlist reload={() => setN((v) => v + 1)} rows={allow} />}
      {tab === 'ssh' && <SSHLedger />}
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

          {list.data && list.data.sessions.length === 0 && (
            <Empty>まだ1本も起こしていない。</Empty>
          )}
          {list.data && list.data.sessions.length > 0 && (
            <table>
              <thead>
                <tr><th>状態</th><th>場所</th><th>起こした時刻</th><th>pid</th><th>終わり</th></tr>
              </thead>
              <tbody>
                {list.data.sessions.map((s) => (
                  <tr key={s.id}>
                    <td>
                      <Link to={`/runtime/${s.id}`}>
                        <StateBadge state={s.state} />
                      </Link>
                    </td>
                    <td className="mono">{s.cwd}</td>
                    <td>{short(s.created_at)}</td>
                    <td>{s.pid || ''}</td>
                    <td className="muted">{s.exit_reason ?? ''}</td>
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
        ここに無い場所ではセッションを起こせない（既定は deny）。
        <strong>変更にはパスワードの再入力が要る</strong>——Cookie を盗られただけで
        境界を広げられてはいけないため。
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
      {rows.data && rows.data.length === 0 && (
        <Empty>空。この状態では1本も起こせない。</Empty>
      )}
      {rows.data && rows.data.length > 0 && (
        <table>
          <thead><tr><th>場所</th><th>覚え書き</th><th>足した時刻</th><th></th></tr></thead>
          <tbody>
            {rows.data.map((a) => (
              <tr key={a.id}>
                <td className="mono">{a.path}</td>
                <td>{a.note}</td>
                <td>{short(a.added_at)}</td>
                <td>
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
        <code>~/.ssh/config</code> は<strong>読むだけ</strong>。Camp が書き戻すことはない。
        取り込みで許可は変わらない。<strong>リモート起動は Phase 3 では行わない</strong>
        ——ここで作るのは台帳と許可だけ。
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
      {rows.data && rows.data.length === 0 && <Empty>台帳は空。読み直すと入る。</Empty>}
      {rows.data && rows.data.length > 0 && (
        <table>
          <thead>
            <tr><th>許可</th><th>エイリアス</th><th>接続先</th><th>Tailscale</th><th>覚え書き</th></tr>
          </thead>
          <tbody>
            {rows.data.map((d) => (
              <tr key={d.id}>
                <td>
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
