import { Link } from 'react-router-dom'
import { api } from '../api'
import { Empty, Failed, Loading, num, useAsync } from '../ui'

const KIND: Record<string, string> = {
  table: '表', cards: 'カード', 'life-tracker': 'グラフ',
}

export default function Views() {
  const list = useAsync(() => api.views(), [])

  return (
    <>
      <h2>ビュー</h2>
      <p className="sub muted">
        Vault の <code>.base</code> をそのまま読んでいる（書き戻さない）。
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
