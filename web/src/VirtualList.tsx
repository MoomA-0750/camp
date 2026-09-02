import { useEffect, useRef, useState } from 'react'

/**
 * 窓の中に入っている分だけ描く。
 *
 * 一覧を上限で切ると「500件」が全件なのか打ち切りなのか読み手に分からない。
 * かといって4,000行を素直にDOMへ出すと重い。そこで**全件を渡して、
 * 見えている範囲だけ描く**。件数表示は常に本当の全件数になる。
 *
 * ライブラリを足していないのは、必要なのが「等高の行を縦に並べる」だけで、
 * それに1つ依存を増やす価値が無いため。行の高さが可変になったら考え直す。
 */
export function VirtualList<T>({
  items, rowHeight, overscan = 8, render, empty,
}: {
  items: T[]
  rowHeight: number
  overscan?: number
  render: (item: T, index: number) => React.ReactNode
  empty?: React.ReactNode
}) {
  const boxRef = useRef<HTMLDivElement>(null)
  const [range, setRange] = useState({ start: 0, end: 40 })

  useEffect(() => {
    const box = boxRef.current
    if (!box) return

    const update = () => {
      // 窓はページ全体。box の上端がビューポートのどこにあるかで決める。
      const top = Math.max(0, -box.getBoundingClientRect().top)
      const start = Math.max(0, Math.floor(top / rowHeight) - overscan)
      const visible = Math.ceil(window.innerHeight / rowHeight) + overscan * 2
      setRange({ start, end: Math.min(items.length, start + visible) })
    }
    update()
    window.addEventListener('scroll', update, { passive: true })
    window.addEventListener('resize', update)
    return () => {
      window.removeEventListener('scroll', update)
      window.removeEventListener('resize', update)
    }
  }, [items.length, rowHeight, overscan])

  if (items.length === 0) return <>{empty}</>

  const start = Math.min(range.start, Math.max(0, items.length - 1))
  const end = Math.min(range.end, items.length)

  return (
    <div ref={boxRef} style={{ height: items.length * rowHeight, position: 'relative' }}>
      <div style={{ transform: `translateY(${start * rowHeight}px)` }}>
        {items.slice(start, end).map((it, i) => (
          <div key={start + i} style={{ height: rowHeight }}>
            {render(it, start + i)}
          </div>
        ))}
      </div>
    </div>
  )
}
