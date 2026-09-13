import { Link } from 'react-router-dom'
import { api } from '../api'
import { Empty, Failed, Loading, num, useAsync } from '../ui'

// life-tracker は時系列なので「チャート」。「グラフ」はノートのつながり（graph）に使う（2026-09-13）。
const KIND: Record<string, string> = {
  table: '表', cards: 'カード', chart: 'チャート', 'life-tracker': 'チャート', graph: 'グラフ',
}

export default function Views() {
  const list = useAsync(() => api.views(), [])

  return (
    <>
      <h2>ビュー</h2>
      <p className="sub muted">
        <strong>Camp の定義</strong>があればそれを、無ければ Vault の <code>.base</code> を読む
        （<code>.base</code> には書き戻さない）。どちらを描いたかは各行に出す。
        ただし<strong>列は既定で全部出す</strong> — 定義の <code>order:</code> は
        許可リストではなく「前に出す指定」として読む。
      </p>

      {list.loading && <Loading />}
      {list.error && <Failed error={list.error} />}
      {list.data?.length === 0 && <Empty>.base が索引されていない。</Empty>}

      {list.data && list.data.length > 0 && (
        <table>
          <thead>
            <tr>
              <th style={{ textAlign: 'left' }}>ビュー</th>
              <th>種別</th><th>行</th><th>列</th><th>うち定義が挙げた</th><th>自動で出た</th>
            </tr>
          </thead>
          <tbody>
            {list.data.map((v) => (
              <tr key={v.id}>
                <td style={{ textAlign: 'left' }}>
                  <Link to={`/views/${v.id.split('/').map(encodeURIComponent).join('/')}`}>
                    {v.base} / {v.name}
                  </Link>
                  {/* **どちらを描いたかを出す**（2026-09-13）。出さないと、変換したつもりで
                      `.base` を見続けていても気づけない。 */}
                  {/* Camp の定義から描いているなら、そこから直せる入口にする。 */}
                  {v.from_db ? (
                    <Link className="tag" to={`/viewdefs/${encodeURIComponent(v.base)}`}
                      title="Camp の定義（DB）から描いている。押すと定義を直せる">
                      Camp
                    </Link>
                  ) : (
                    <span className="tag" title="Vault の .base から描いている（まだ変換していない）">
                      .base
                    </span>
                  )}
                  {v.error && <span className="tag gone">読めない</span>}
                </td>
                <td>{KIND[v.kind] ?? v.kind}</td>
                <td className="n">{num(v.rows)}</td>
                <td className="n">{num(v.columns)}</td>
                <td className="n">{num(v.pinned)}</td>
                <td className="n">{num(v.columns - v.pinned)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  )
}
