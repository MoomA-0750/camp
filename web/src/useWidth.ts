import { useEffect, useRef, useState } from 'react'

/**
 * 描く幅を**実際の幅に合わせる**（2026-09-13、実ブラウザの 400px で見つけた）。固定の viewBox を
 * 縮めると、目盛りの文字まで半分の大きさになって読めなかった。測れない環境（jsdom）では fallback。
 */
export function useWidth(fallback = 720, min = 280) {
  const ref = useRef<HTMLDivElement>(null)
  const [w, setW] = useState(fallback)
  useEffect(() => {
    const el = ref.current
    if (!el || typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver(([e]) => {
      const next = Math.round(e.contentRect.width)
      if (next > 0) setW(next)
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [])
  return [ref, Math.max(min, w)] as const
}
