import { useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { api } from '../api'
import { Failed, Loading, short, useAsync } from '../ui'

/**
 * ビュー定義（台紙1枚ぶんの YAML）を直す画面。2026-09-13 から。
 *
 * 定義を Camp の DB に持つと決めたので、直す口が無ければ `sqlite3` を直打ちするしかない。
 * `.base` にあった git 履歴の代わりに、書き換えの履歴もここに出す。
 *
 * **URL を `/views/` の下に置かない。** `/views/*` はビューの詳細の catch-all なので、
 * `/views/<台紙>/def` は詳細に飲まれる（サーバー側でも同じ precedence を解いた）。
 * 独立させれば `def` という名前の台紙があっても壊れない。
 */
export default function ViewDefEdit() {
  const base = useParams().base ?? ''
  const [n, setN] = useState(0)
  const def = useAsync(() => api.viewDef(base), [base, n])
  const hist = useAsync(() => api.viewHistory(base), [base, n])

  const [text, setText] = useState<string | null>(null)
  const [err, setErr] = useState('')
  const [saved, setSaved] = useState(false)
  const [busy, setBusy] = useState(false)

  // **まだ触っていなければ、読み込んだ定義そのものを見せる。**
  //
  // `useEffect` で編集欄へ流し込む形にすると、データが来た最初の1描画だけ編集欄が空になり、
  // **その瞬間は「保存する」も押せる**（空の定義を保存しかけられる）。実害はサーバーが
  // 弾くので小さいが、実のある中身の上に空欄が一瞬出るのは「消えた」と読まれる。
  // 2026-09-13、試験がその窓を捕まえた。
  //
  // 触っていれば `text` を優先する——読み直し（保存後や外からの書き換え）で、
  // 書きかけを黙って捨てない。
  const body = text ?? def.data?.body ?? ''
  const dirty = def.data != null && body !== def.data.body
  // 変換したあとに人が直しているか。**この状態では `-convert` は上書きしない**
  // （`campd views -convert -force` を明示したときだけ書く）。
  //
  // **ここで時刻を比べない。サーバーの判定をそのまま出す**（2026-09-13、実ブラウザで見つけた）。
  // 以前は `updated_at > converted_at` で出していたが、`-convert` が実際に使うのは履歴の
  // 最後の書き手で、秒までの時刻は同じ秒に並ぶと比べられない。注意と実際の振る舞いが食い違う。
  const handEdited = def.data?.hand_edited === true

  const save = async () => {
    setErr(''); setSaved(false); setBusy(true)
    try {
      await api.viewDefSave(base, body, def.data?.body_sha256)
      setSaved(true)
      setN((v) => v + 1)
    } catch (e) {
      setErr(String(e instanceof Error ? e.message : e))
    } finally { setBusy(false) }
  }

  return (
    <>
      <p className="crumbs"><Link to="/views">← ビュー</Link></p>
      <h2>{base} の定義</h2>
      <p className="sub muted">
        台紙1枚ぶんの YAML（<code>source</code> / <code>derive</code> / <code>views</code>）。
        保存すると<strong>直ちに</strong>この定義で描かれる。
        <code>.base</code> には書き戻さない。
      </p>

      {def.loading && <Loading />}
      {def.error && (
        <>
          <Failed error={def.error} />
          <p className="sub muted">
            まだ変換していない台紙かもしれない。
            <code>campd views -convert</code> で <code>.base</code> から作る。
          </p>
        </>
      )}

      {def.data && (
        <>
          <p className="sub muted">
            {def.data.origin
              ? <>変換元 <code>{def.data.origin}</code></>
              : <>手で書いた定義（変換元なし）</>}
            {def.data.converted_at && <> / 変換 {short(def.data.converted_at)}</>}
            {' / 最後の書き換え '}{short(def.data.updated_at)}
          </p>
          {handEdited && (
            <p className="notice">
              変換のあとで、絞り込み・列・並び・集計・描き方を直している。
              <code>campd views -convert</code> はこの台紙を上書きしない（上書きするなら <code>-force</code>）。
              時間軸・集約・グラフのビューは、再変換でも <code>-force</code> でも引き継ぐ。
            </p>
          )}

          <form onSubmit={(e) => { e.preventDefault(); void save() }}>
            <textarea
              className="mono" rows={24} value={body} spellCheck={false}
              style={{ width: '100%' }}
              aria-label="ビュー定義の YAML"
              onChange={(e) => { setText(e.target.value); setSaved(false) }}
            />
            <div className="talk-buttons">
              <button disabled={busy || !dirty}>保存する</button>
              <button type="button" disabled={!dirty}
                onClick={() => setText(def.data!.body)}>
                直した分を捨てる
              </button>
            </div>
          </form>
          {/* **壊れた定義は保存されない**（サーバーが読めるか確かめて断る）。
              入ってしまうと、この画面からも直せなくなる。 */}
          {err && (
            <>
              <Failed error={err} />
              {/* ほかのタブや -convert が間に書いていると断られる（読んだ版の上にしか書かない）。
                  **読み直しても書きかけは残す**——`text` は触っていれば読み直しより優先する。 */}
              <button type="button" onClick={() => { setErr(''); setN((v) => v + 1) }}>
                いまの版を読み直す（書きかけは残す）
              </button>
            </>
          )}
          {saved && !dirty && <p className="sub">保存した。</p>}
        </>
      )}

      <h3>書き換えの履歴</h3>
      <p className="sub muted">
        <code>.base</code> にあった git 履歴の代わり。<code>convert</code> は変換が書いたもの。
      </p>
      {hist.loading && <Loading />}
      {hist.error && <Failed error={hist.error} />}
      {hist.data && hist.data.length === 0 && <p className="sub muted">まだ無い。</p>}
      {hist.data && hist.data.length > 0 && (
        <table>
          <thead>
            <tr><th className="nowrap">いつ</th><th className="nowrap">誰が</th><th>行数</th><th /></tr>
          </thead>
          <tbody>
            {hist.data.map((h, i) => (
              <tr key={`${h.at}-${i}`}>
                <td className="nowrap">{short(h.at)}</td>
                <td className="nowrap">{h.by === 'convert' ? '変換' : '手で'}</td>
                <td className="num">{h.body.split('\n').length}</td>
                <td>
                  {/* **入れるだけ。保存はしない。** 押した瞬間に上書きされると、
                      いま書いているものが消える。 */}
                  <button type="button" onClick={() => { setText(h.body); setSaved(false) }}>
                    編集欄に入れる
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  )
}
