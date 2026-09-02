import { useEffect, useState } from 'react'

/** 読み込み中・失敗・中身の3状態をまとめて扱う。画面ごとに書かない。 */
export function useAsync<T>(fn: () => Promise<T>, deps: unknown[]) {
  const [state, setState] = useState<{ data?: T; error?: string; loading: boolean }>(
    { loading: true })
  useEffect(() => {
    let alive = true
    setState({ loading: true })
    fn().then(
      (data) => { if (alive) setState({ data, loading: false }) },
      (e: Error) => { if (alive) setState({ error: e.message, loading: false }) },
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

/** 日時は「YYYY-MM-DD HH:MM」まで。秒より下は一覧では見ない。 */
export function short(ts?: string) {
  if (!ts || ts.length < 16) return ts ?? ''
  return ts.slice(0, 10) + ' ' + ts.slice(11, 16)
}

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
