import { useSearchParams } from 'react-router-dom'
import { api } from '../api'
import Limits from '../Limits'
import { Failed, Loading, num, tokens, useAsync } from '../ui'

const AXES = [
  ['day', '日'], ['model', 'モデル'], ['session', 'セッション'], ['project', 'プロジェクト'],
] as const

// 集計は api_message_id 単位で重複排除済み（D-013）。
// メッセージ行で数えると出力トークンが約2倍になる。
export default function Usage() {
  const [sp, setSp] = useSearchParams()
  const by = sp.get('by') ?? 'day'
  const rows = useAsync(() => api.usage(by, 60), [by])

  const total = (rows.data ?? []).reduce(
    (a, r) => ({
      requests: a.requests + r.requests,
      output: a.output + r.output_tokens,
      creation: a.creation + r.cache_creation_tokens,
      read: a.read + r.cache_read_tokens,
      thinking: a.thinking + r.thinking_tokens,
    }),
    { requests: 0, output: 0, creation: 0, read: 0, thinking: 0 },
  )

  return (
    <>
      <h2>使用量</h2>

      <h3>プラン残量</h3>
      <Limits />

      <h3>トークン内訳</h3>
      <p className="sub muted">
        リクエスト単位で数えている。行単位で合計すると出力が約2倍になる。
      </p>

      <form className="filters" onSubmit={(e) => e.preventDefault()}>
        <select value={by} onChange={(e) => setSp({ by: e.target.value })}>
          {AXES.map(([v, label]) => <option key={v} value={v}>{label}別</option>)}
        </select>
      </form>

      {rows.loading && <Loading />}
      {rows.error && <Failed error={rows.error} />}

      {rows.data && (
        <table>
          <thead>
            <tr>
              <th>{AXES.find(([v]) => v === by)?.[1]}</th>
              <th>要求</th><th>入力</th><th>出力</th>
              <th>キャッシュ作成</th><th>キャッシュ読み</th><th>思考</th>
            </tr>
          </thead>
          <tbody>
            {rows.data.map((r) => (
              <tr key={r.key}>
                <td title={r.key}>{r.label || r.key}</td>
                <td className="n">{num(r.requests)}</td>
                <td className="n">{tokens(r.input_tokens)}</td>
                <td className="n">{tokens(r.output_tokens)}</td>
                <td className="n">{tokens(r.cache_creation_tokens)}</td>
                <td className="n">{tokens(r.cache_read_tokens)}</td>
                <td className="n">{tokens(r.thinking_tokens)}</td>
              </tr>
            ))}
          </tbody>
          <tfoot>
            <tr>
              <th>この表の合計</th>
              <th className="n">{num(total.requests)}</th>
              <th />
              <th className="n">{tokens(total.output)}</th>
              <th className="n">{tokens(total.creation)}</th>
              <th className="n">{tokens(total.read)}</th>
              <th className="n">{tokens(total.thinking)}</th>
            </tr>
          </tfoot>
        </table>
      )}
    </>
  )
}
