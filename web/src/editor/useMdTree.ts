import { useEffect, useRef, useState } from 'react'
import { build, type Md } from './mdtree'

/** 解析の上限（Fable の M54 設計レビュー 3）。超えたら Worker ごと捨てて「描けない」と出す。 */
export const parseLimitMs = 2000
const debounceMs = 300

/**
 * useMdTree は本文のプレビューの木を、書くのが止まって 300ms 後に Worker で作る。
 * Worker が無い環境（試験の jsdom）ではその場で作る。
 */
export function useMdTree(doc: string): { tree: Md[]; error: string } {
  const [tree, setTree] = useState<Md[]>([])
  const [error, setError] = useState('')
  const worker = useRef<Worker | null>(null)
  const seq = useRef(0)

  useEffect(() => () => worker.current?.terminate(), [])

  useEffect(() => {
    const t = setTimeout(() => {
      const id = ++seq.current
      if (typeof Worker === 'undefined') {
        setTree(build(doc, location.origin))
        setError('')
        return
      }
      if (!worker.current) worker.current = new Worker(new URL('./md.worker.ts', import.meta.url), { type: 'module' })
      const w = worker.current
      const limit = setTimeout(() => {
        if (seq.current !== id) return
        // 固まった Worker は捨てる（次の描き直しで作り直す）。
        w.terminate()
        if (worker.current === w) worker.current = null
        setError(`大きすぎるか複雑すぎて ${parseLimitMs / 1000} 秒で描けなかった。書く面はそのまま使える`)
      }, parseLimitMs)
      w.onmessage = (e: MessageEvent<{ id: number; tree?: Md[]; error?: string }>) => {
        if (e.data.id !== seq.current) return
        clearTimeout(limit)
        if (e.data.error) setError('描けなかった: ' + e.data.error)
        else {
          setTree(e.data.tree ?? [])
          setError('')
        }
      }
      w.postMessage({ id, doc, origin: location.origin })
    }, debounceMs)
    return () => clearTimeout(t)
  }, [doc])

  return { tree, error }
}
