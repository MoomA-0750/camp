import { expect, test } from 'vitest'
import { render, screen } from '@testing-library/react'
import { VirtualList } from './VirtualList'

test('空のときは empty を出す', () => {
  render(<VirtualList items={[]} rowHeight={10} render={() => null} empty={<p>無い</p>} />)
  expect(screen.getByText('無い')).toBeTruthy()
})

// 全体の高さは実件数ぶん確保する。ここが縮むとスクロールバーが嘘をつき、
// 「まだ下にある」ことが分からなくなる。
test('全件ぶんの高さを確保する', () => {
  const items = Array.from({ length: 500 }, (_, i) => i)
  const { container } = render(
    <VirtualList items={items} rowHeight={20} render={(n) => <span>{n}</span>} />)
  const box = container.firstElementChild as HTMLElement
  expect(box.style.height).toBe(`${500 * 20}px`)
})
