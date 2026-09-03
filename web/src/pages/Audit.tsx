import { useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { api, type AuditEntry } from '../api'
import { Empty, Failed, Loading, short, useAsync } from '../ui'

// 監査ログ。**読むだけ。**
//
// Phase 3 で Camp は任意のコードを動かす側になる。実行専用OSユーザーも
// VM分離も採らないと決めた以上、ここが後から辿れる唯一の場所になる。
export default function Audit() {
  const [params, setParams] = useSearchParams()
  const session = params.get('session') ?? ''
  const action = params.get('action') ?? ''
  const [draft, setDraft] = useState(session)

  const { data, error, loading } = useAsync(
    () => api.audit({ session, action }), [session, action])

  return (
    <div>
      <h2>監査ログ</h2>
      <p className="muted">
        追記だけ。書き換えも削除もできない。連鎖のハッシュが切れていないかは
        <code> campd doctor </code>が見ている。
      </p>

      <form
        onSubmit={(e) => {
          e.preventDefault()
          const next = new URLSearchParams(params)
          if (draft) next.set('session', draft)
          else next.delete('session')
          setParams(next)
        }}
      >
        <input
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          placeholder="セッションIDで絞る"
          size={40}
        />
        <button type="submit">絞る</button>
        {(session || action) && (
          <button type="button" onClick={() => { setDraft(''); setParams({}) }}>
            解除
          </button>
        )}
      </form>

      {loading && <Loading />}
      {error && <Failed error={error} />}
      {data && data.length === 0 && <Empty>まだ何も記録されていない。</Empty>}
      {data && data.length > 0 && (
        <table>
          <thead>
            <tr>
              <th>いつ</th><th>だれが</th><th>何を</th><th>対象</th><th>結果</th>
            </tr>
          </thead>
          <tbody>
            {data.map((e: AuditEntry) => (
              <tr key={e.id} className={e.outcome === 'ok' ? '' : 'notable'}>
                <td className="mono">{short(e.at)}</td>
                <td>{e.actor}</td>
                <td className="mono">{e.action}</td>
                <td className="mono">{e.target ?? ''}</td>
                <td>{e.outcome}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}
