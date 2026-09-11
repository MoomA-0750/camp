import { useEffect, useState } from 'react'

/**
 * 読み込み中・失敗・中身の3状態をまとめて扱う。画面ごとに書かない。
 *
 * **読み直しのあいだ、前の中身を消さない。** 消すと、定期的に読み直す画面が
 * 「出る／消える」を繰り返して上下に震える（2026-09-07、セッションの画面で
 * 実際にそうなった。しかも中身が消えているあいだに別の判断が走って、
 * 流れを繋ぎ直し、同じ行をもう一度受け取る、という輪になっていた）。
 */
export function useAsync<T>(fn: () => Promise<T>, deps: unknown[]) {
  const [state, setState] = useState<{ data?: T; error?: string; loading: boolean }>(
    { loading: true })
  useEffect(() => {
    let alive = true
    setState((prev) => ({ ...prev, loading: true, error: undefined }))
    fn().then(
      (data) => { if (alive) setState({ data, loading: false }) },
      (e: Error) => { if (alive) setState((prev) => ({ ...prev, error: e.message, loading: false })) },
    )
    return () => { alive = false }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps)
  return state
}

export function Loading() {
  return <p className="muted">読み込み中…</p>
}

export function Failed({ error }: { error: string }) {
  return <p className="error">失敗しました: {error}</p>
}

export function Empty({ children }: { children: React.ReactNode }) {
  return <p className="muted">{children}</p>
}

/**
 * 日時は「YYYY-MM-DD HH:MM」まで。秒より下は一覧では見ない。
 *
 * **時差の付いたものは手元の時刻に直す。** DB は UTC（末尾 Z）で持っているので、
 * 文字列を切るだけだと 9 時間ずれて見える（2026-09-11、17:15 に終わったものが
 * 08:15 と出ていた。承認の期限も同じだけずれていた）。時差の無いものはそのまま。
 */
export function short(ts?: string) {
  if (!ts || ts.length < 16) return ts ?? ''
  const d = zoned(ts)
  if (d) return `${ymd(d)} ${p2(d.getHours())}:${p2(d.getMinutes())}`
  return ts.slice(0, 10) + ' ' + ts.slice(11, 16)
}

/** 時刻だけ（HH:MM:SS）。流れの1行に使う。時差の扱いは short と同じ。 */
export function clock(ts?: string) {
  if (!ts) return ''
  const d = zoned(ts)
  if (d) return `${p2(d.getHours())}:${p2(d.getMinutes())}:${p2(d.getSeconds())}`
  return ts.slice(11, 19)
}

function zoned(ts: string): Date | null {
  if (!/(Z|[+-]\d\d:?\d\d)$/.test(ts)) return null
  const d = new Date(ts)
  return isNaN(d.getTime()) ? null : d
}
const p2 = (n: number) => String(n).padStart(2, '0')
const ymd = (d: Date) => `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())}`

export function num(n: number) {
  return n.toLocaleString('ja-JP')
}

export function bytes(n: number) {
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + 'MiB'
  if (n >= 1 << 10) return (n / (1 << 10)).toFixed(1) + 'KiB'
  return n + 'B'
}

/** トークン数は桁が大きいので k / M に丸める。 */
export function tokens(n: number) {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M'
  if (n >= 1_000) return Math.round(n / 1_000) + 'k'
  return String(n)
}
