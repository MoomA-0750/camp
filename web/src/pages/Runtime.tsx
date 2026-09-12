import { useState } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { api, type AgentInfo, type Destination, type Pinned, type RuntimeSession } from '../api'
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
  // 起こせる接続先は、許して**行き先を固定したもの**だけ（2026-09-11）。
  const hostsQ = useAsync(() => api.sshHosts(), [n, tab])
  const hosts = Array.isArray(hostsQ.data) ? hostsQ.data : []
  const startable = hosts.filter((d) => d.allowed && pinnedOK(d.pinned))

  const [cwd, setCwd] = useState('')
  const [host, setHost] = useState('')
  const [agent, setAgent] = useState('')
  const [perm, setPerm] = useState('cli')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  // 起こせるエージェントは実行面が名乗り、API が並べる。**画面は名前を決め打ちしない**（D-031）。
  const agents = list.data?.agents ?? []
  const chosen = agents.find((a) => a.name === agent) ?? agents[0]
  const localOnly = !!chosen && !chosen.remote
  // **手元に実体が無いエージェント**（2026-09-13）。このマシンは選べない。
  const remoteOnly = !!chosen?.remote_only
  // 確認の度合いは、そのエージェントが名乗ったものから選ぶ（向こうのホストでも同じ。M42）。
  const perms = chosen?.perms ?? ['cli']
  const chosenPerm = perms.includes(perm) ? perm : 'cli'

  const start = async () => {
    setErr('')
    setBusy(true)
    try {
      await api.runtimeStart(cwd, localOnly ? '' : host, chosen?.name ?? '', chosenPerm)
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

      {tab === 'allow' && <Allowlist reload={() => setN((v) => v + 1)} rows={allow} hosts={hosts} />}
      {tab === 'ssh' && <SSHLedger agents={agents} />}
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
          {/* **実行面だけ古いまま動いている。** 入れ替えの順序（バイナリ → campd → 実行面）を
              間違えると起きる。直したはずの不具合が直らないのに、画面には何も出なかった
              （2026-09-12）。**止めはしない。知らせるだけ。** */}
          {list.data?.agent_connected && list.data.agent_stale && (
            <p className="warn">
              実行面が campd と違うビルドで動いている。
              <code>systemctl --user restart camp-agent</code> で入れ替える。
            </p>
          )}

          <form className="filters" onSubmit={(e) => { e.preventDefault(); void start() }}>
            <select value={chosen?.name ?? ''} aria-label="どのエージェントで起こすか"
              disabled={agents.length === 0}
              onChange={(e) => {
                setAgent(e.target.value)
                const a = agents.find((x) => x.name === e.target.value)
                // 向こうのホストで起こせないエージェントなら、選んでいたホストを外す。
                if (!a?.remote) setHost('')
                // 手元に実体が無いなら、このマシンは選べない。最初の接続先を選んでおく。
                else if (a.remote_only && !host) setHost(startable[0]?.alias ?? '')
              }}>
              {agents.length === 0 && <option value="">（起こせるエージェントが無い）</option>}
              {agents.map((a) => <option key={a.name} value={a.name}>{a.label}</option>)}
            </select>
            <select value={localOnly ? '' : host} onChange={(e) => setHost(e.target.value)}
              aria-label="どこで起こすか" disabled={localOnly}
              title={localOnly ? `${chosen.label} はまだこのマシンだけで起こせる`
                : remoteOnly ? `${chosen?.label} の実体が手元に無い。向こうのホストでなら起こせる`
                  : undefined}>
              {/* **手元に実体が無いなら、このマシンは選べない**（2026-09-13）。 */}
              {!remoteOnly && <option value="">このマシン</option>}
              {startable.map((d) => (
                <option key={d.alias} value={d.alias}>{d.alias}（{pinLabel(d.pinned)}）</option>
              ))}
            </select>
            <select value={chosenPerm} onChange={(e) => setPerm(e.target.value)}
              aria-label="確認の度合い">
              {perms.map((p) => <option key={p} value={p}>{permLabel(p)}</option>)}
            </select>
            <input
              type="text"
              placeholder={host
                ? `${host} の上の場所（許可リストの中の絶対パス）`
                : '起こす場所（許可リストの中の絶対パス）'}
              value={cwd} onChange={(e) => setCwd(e.target.value)} style={{ minWidth: '24rem' }}
            />
            <button disabled={busy || !cwd || !list.data?.agent_connected || !chosen
              || (remoteOnly && !host)}>起こす</button>
          </form>
          {err && <Failed error={err} />}

          {!list.loading && !list.error && sessions.length === 0 && (
            <Empty>いま走っているものは無い。</Empty>
          )}
          {sessions.length > 0 && (
            <table>
              <thead>
                <tr>
                  <th className="nowrap">状態</th><th className="nowrap">エージェント</th>
                  <th className="nowrap">確認の度合い</th><th>場所</th>
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
                    <td className="nowrap">{s.agent_label ?? s.agent ?? ''}</td>
                    <td className="nowrap">{permLabel(s.perm)}</td>
                    <td className="mono wrap">
                      <Link to={`/runtime/${s.id}`}>{where(s)}</Link>
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

// 確認の度合いの名前。**エージェントによらない Camp の語**（エージェントごとの渡し方の違いは、
// 駆動器の説明に出る）。
const PERM_LABEL: Record<string, string> = {
  cli: 'CLI と同じ', ask: '毎回訊く', edits: '編集は訊かない', auto: '自動で判断', full: '全部任せる',
  legacy: 'Phase 3.6 の専用の置き場',
}

// permLabel は確認の度合いの名前。**無ければ CLI と同じ**（2026-09-12 より前の行。台帳も cli で埋まる）。
export function permLabel(p?: string): string {
  return PERM_LABEL[p || 'cli'] ?? p ?? ''
}

// where は起こした場所。向こうなら `host:/path`。
export function where(s: { host?: string; cwd: string }): string {
  return s.host ? `${s.host}:${s.cwd}` : s.cwd
}

// pinnedOK は起こしてよい固定か。**信じるホスト鍵まで固定していないものは固定ではない**
// （2026-09-11 の outer gate で codex が指摘）。
export function pinnedOK(p?: Pinned): boolean {
  return !!p && !!p.hostname && (p.hostkeys?.length ?? 0) > 0
}

// pinLabel は固定した行き先の文。
export function pinLabel(p?: Pinned): string {
  if (!p) return ''
  let s = (p.user ? p.user + '@' : '') + p.hostname
  if (p.port && p.port !== '22') s += ':' + p.port
  if (p.proxyjump) s += `（${p.proxyjump} 経由）`
  if (p.hostkeys?.length) s += `・鍵 ${p.hostkeys.length}`
  return s
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
  conn_lost: 'SSH が切れた',
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

// 「続きから」。終わったセッションの続きを起こし、起きた新しい行へ移る（2026-09-13）。
//
// **元のものが生き返ったように見せる**（本人の決定）。台帳では別の行になるが、エージェント側の
// 会話は1本のまま繋がる（記録も同じファイルへ追記される）。
function ResumeButton({ s }: { s: RuntimeSession }) {
  const nav = useNavigate()
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  // **二重に起こさない。** 続きが既に起きているなら、そちらへの入口だけを出す。
  if (s.resumed_by) return <Link to={`/runtime/${s.resumed_by}`}>続きへ</Link>
  // エージェント側の id を名乗らないまま終わった行は、続きから起こせない。
  if (!(s.agent_session_id ?? s.claude_id)) return <span className="muted">—</span>
  const go = async () => {
    setErr(''); setBusy(true)
    try {
      nav(`/runtime/${(await api.runtimeResume(s.id)).id}`)
    } catch (e) {
      setErr(String(e instanceof Error ? e.message : e))
    } finally { setBusy(false) }
  }
  return (
    <>
      <button disabled={busy} onClick={() => void go()}>続きから</button>
      {err && <div className="muted">{err}</div>}
    </>
  )
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
              <th className="nowrap">承認</th><th className="nowrap">続き</th>
              <th>場所</th><th className="num nowrap">コード</th>
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
                <td className="nowrap"><ResumeButton s={s} /></td>
                <td className="mono wrap"><Link to={`/runtime/${s.id}`}>{where(s)}</Link></td>
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

function Allowlist({ rows, reload, hosts }: {
  rows: ReturnType<typeof useAsync<import('../api').Allowed[]>>
  reload: () => void
  hosts: Destination[]
}) {
  const [path, setPath] = useState('')
  const [host, setHost] = useState('')
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
        向こうの場所は書いたとおりに覚え、起こすときに向こうで実パスに直してから照らす
        （symlink で外へは出られない）。
      </p>
      <form className="filters" onSubmit={(e) => e.preventDefault()}>
        <select value={host} onChange={(e) => setHost(e.target.value)} aria-label="どのホストの場所か">
          <option value="">このマシン</option>
          {hosts.map((d) => <option key={d.alias} value={d.alias}>{d.alias}</option>)}
        </select>
        <input type="text" placeholder="許すディレクトリ（絶対パス）" value={path}
          onChange={(e) => setPath(e.target.value)} style={{ minWidth: '20rem' }} />
        <input type="password" placeholder="パスワード" value={pw}
          onChange={(e) => setPw(e.target.value)} autoComplete="current-password" />
        <button disabled={!path || !pw}
          onClick={() => void run(() => api.allowlistAdd(path, pw, '', host))}>足す</button>
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
              <th className="nowrap">ホスト</th><th>場所</th><th className="nowrap">覚え書き</th>
              <th className="nowrap">足した時刻</th><th></th>
            </tr>
          </thead>
          <tbody>
            {(rows.data ?? []).map((a) => (
              <tr key={(a.host ?? '') + ':' + a.id}>
                <td className="mono nowrap">{a.host ?? <span className="muted">このマシン</span>}</td>
                <td className="mono wrap">{a.path}</td>
                <td>{a.note}</td>
                <td className="nowrap">{short(a.added_at)}</td>
                <td className="nowrap">
                  <button disabled={!pw}
                    onClick={() => void run(() => api.allowlistRemove(a.path, pw, a.host ?? ''))}>外す</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  )
}

function SSHLedger({ agents }: { agents: AgentInfo[] }) {
  const [n, setN] = useState(0)
  const rows = useAsync(() => api.sshHosts(), [n])
  const [pw, setPw] = useState('')
  const [err, setErr] = useState('')
  const [ca, setCa] = useState('')
  const [cg, setCg] = useState('')
  const [cp, setCp] = useState('')
  // どのエージェントの場所か。選択肢は API の agents から（名前を決め打ちしない）。
  const agent = cg || agents[0]?.name || ''
  const labelOf = (name: string) => agents.find((a) => a.name === name)?.label ?? name

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
        取り込みで許可は変わらない。
        <strong>許すときに、そのときの行き先（ssh -G）と、known_hosts が信じるホスト鍵を固定する。</strong>
        あとで config や known_hosts が別の先・別の鍵を指すようになったら起こさない。
        ホスト鍵は Camp では受け入れないので、初めての先は端末で一度 <code>ssh</code> して確かめる。
      </p>
      <form className="filters" onSubmit={(e) => e.preventDefault()}>
        <button onClick={() => void run(() => api.sshScan())}>
          ~/.ssh/config を読み直す
        </button>
        <input type="password" placeholder="パスワード（許可の変更に要る）" value={pw}
          onChange={(e) => setPw(e.target.value)} autoComplete="current-password" />
      </form>
      <form className="filters" onSubmit={(e) => e.preventDefault()}>
        <select value={ca} onChange={(e) => setCa(e.target.value)} aria-label="実体の場所を書く接続先">
          <option value="">実体の場所を書く接続先</option>
          {(Array.isArray(rows.data) ? rows.data : []).map((d) => (
            <option key={d.alias} value={d.alias}>{d.alias}</option>
          ))}
        </select>
        <select value={agent} onChange={(e) => setCg(e.target.value)} aria-label="どのエージェントの場所か"
          disabled={agents.length === 0}>
          {agents.length === 0 && <option value="">（実行面が名乗っていない）</option>}
          {agents.map((a) => <option key={a.name} value={a.name}>{a.label}</option>)}
        </select>
        <input type="text" placeholder="向こうの実体の絶対パス（空なら向こうで探す）" value={cp}
          onChange={(e) => setCp(e.target.value)} style={{ minWidth: '18rem' }} />
        <button disabled={!ca || !pw || !agent}
          onClick={() => void run(() => api.sshPath(ca, agent, cp, pw))}>書く</button>
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
              <th className="nowrap">固定した行き先</th><th className="nowrap">実体の場所</th>
              <th className="nowrap">記録</th>
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
                {/* 長いパスで表が画面の外へ押し出されないよう、折り返す。**td.wrap は
                    width:100% なので1つの表に2つ置かない**——2つ置いたら取り合って、
                    片方が1文字幅に潰れた（2026-09-11 に撮って直した）。 */}
                <td className="mono">
                  {pinnedOK(d.pinned) ? pinLabel(d.pinned)
                    : d.allowed ? <span className="warn-text">許し直す（行き先が固定されていない）</span>
                      : <span className="muted">—</span>}
                </td>
                <td className="mono" style={{ overflowWrap: 'anywhere', minWidth: '10rem' }}>
                  {Object.keys(d.agent_paths ?? {}).length === 0
                    ? <span className="muted">探す</span>
                    : Object.entries(d.agent_paths ?? {}).map(([a, p]) => (
                      <div key={a}>{labelOf(a)}: {p}</div>
                    ))}
                </td>
                {/* 記録を読むか。**行が無ければ読まない**（既定で読みに行かない）。
                    読むようにするときだけパスワードが要る。最後に読めた時刻と最後の失敗を
                    出すのは、**何日も入っていないことに気づけるように**。 */}
                <td className="nowrap">
                  {agents.length === 0 && <span className="muted">—</span>}
                  {agents.map((a) => {
                    const rec = (d.records ?? {})[a.name]
                    const on = rec?.enabled ?? false
                    return (
                      <div key={a.name}>
                        <button disabled={!d.allowed || (!on && !pw)}
                          onClick={() => void run(() => api.sshRecord(d.alias, a.name, !on, pw))}>
                          {labelOf(a.name)}: {on ? '読む' : '読まない'}
                        </button>
                        {on && (
                          <button disabled={!d.allowed}
                            onClick={() => void run(() => api.sshRecordRead(d.alias, a.name))}>
                            いま読む
                          </button>
                        )}
                        {on && rec?.last_error && (
                          <div className="warn-text">{rec.last_error}（{rec.fail_count} 回続けて）</div>
                        )}
                        {on && rec?.last_ok_at && !rec.last_error && (
                          <div className="muted">最後に読めた: {rec.last_ok_at}</div>
                        )}
                      </div>
                    )
                  })}
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
