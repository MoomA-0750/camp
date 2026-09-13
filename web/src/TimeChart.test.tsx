import { expect, test } from 'vitest'
import { render, screen } from '@testing-library/react'
import TimeChart from './TimeChart'

// 時系列の描き手（2026-09-13 から）。**集約はサーバーが済ませている**ので、ここで縛るのは
// 「点をいじらない」「期間だけ切る」「描けないときは描けないと言う」こと。

const now = new Date(2026, 8, 13) // 2026-09-13

const bank = {
  key: 'balance', label: 'balance', measure: 'last', rows: 11,
  points: [
    { t: '2025-09-12', v: 1, n: 1 },          // 直近365日の外
    { t: '2025-12-26', v: 1200, n: 1 },
    { t: '2025-12-27', v: 1500, n: 8 },      // 同日8件
    { t: '2026-07-29', v: 2000, n: 2 },
  ],
}

test('点はサーバーの値のまま描き、期間の外は切る', () => {
  const { container } = render(
    <TimeChart series={bank} enc={{ kind: 'chart', values: ['balance'], chart: 'line', window: 'last-365-days' }} now={now} />)
  // 期間の外の1点を切って3点。最後の値はサーバーの値そのまま。
  expect(screen.getByText(/3 点/)).toBeTruthy()
  expect(screen.getByText('2,000')).toBeTruthy()
  const titles = [...container.querySelectorAll('circle title')].map((t) => t.textContent)
  expect(titles).toHaveLength(3)
  // 同日8件をまとめた点は、何行をどうまとめたかが読める。
  expect(titles).toContain('2025-12-27: 1,500（8 行の最後）')
  expect(screen.getByText(/1点に最大 8 行を最後でまとめた/)).toBeTruthy()
})

test('描けない理由があれば点を描かずに理由を出す', () => {
  const { container } = render(
    <TimeChart series={{ key: 'balance', label: 'balance', measure: 'only', rows: 0,
      error: '同じ日に複数行ある（最多 2025-12-27 に 8 行）' }}
    enc={{ kind: 'chart', values: ['balance'] }} now={now} />)
  expect(screen.getByText(/描けない: 同じ日に複数行ある/)).toBeTruthy()
  expect(container.querySelector('svg')).toBeNull()
})

test('期間に点が無ければ、最後の点がいつかを言う', () => {
  render(
    <TimeChart series={{ ...bank, points: [bank.points[0]] }}
      enc={{ kind: 'chart', values: ['balance'], window: 'last-365-days' }} now={now} />)
  expect(screen.getByText('直近 365 日に点が無い（最後の点は 2025-09-12）。')).toBeTruthy()
})

// 月の区切りは、期間の始まりを含む月も入れる（2025-09 は 2025-09-13 から始まる期間に掛かる）。
test('月の点は期間の始まりの月も入る', () => {
  const { container } = render(
    <TimeChart series={{ key: 'total_jpy', label: 'total_jpy', measure: 'only', rows: 2,
      points: [{ t: '2025-08', v: 5, n: 1 }, { t: '2025-09', v: 4200, n: 1 }, { t: '2025-10', v: 100, n: 1 }] }}
    enc={{ kind: 'chart', values: ['total_jpy'], chart: 'bar', window: 'last-365-days' }} now={now} />)
  const bars = [...container.querySelectorAll('rect.bar')]
  expect(bars).toHaveLength(2)
  // 棒は 0 から立つ（高さが値に比例する）。
  const h = bars.map((b) => Number(b.getAttribute('height')))
  expect(h[0] / h[1]).toBeCloseTo(4200 / 100, 0)
})

test('値が読めなかった行は数を出す', () => {
  render(
    <TimeChart series={{ ...bank, skipped: 3 }} enc={{ kind: 'chart', values: ['balance'] }} now={now} />)
  expect(screen.getByText('値が読めなかった行 3 件は描いていない。')).toBeTruthy()
  expect(screen.getByText(/全期間 · 4 点/)).toBeTruthy()
})

// ---- 実ブラウザの画面写しで見つけたもの（2026-09-13）。jsdom の試験は全部通っていた。

// 残高は次の取引まで変わらない。斜めに結ぶと、取引の無い期間に「徐々に増えた」ように見える。
test('last は階段で描き、それ以外は点を結ぶ', () => {
  const { container, rerender } = render(
    <TimeChart series={bank} enc={{ kind: 'chart', values: ['balance'] }} now={now} />)
  const d = container.querySelector('path.line')!.getAttribute('d')!
  expect(d).toMatch(/H[\d.]+V[\d.]+/)
  expect(d).not.toContain('L')
  rerender(<TimeChart series={{ ...bank, measure: 'avg' }} enc={{ kind: 'chart', values: ['balance'] }} now={now} />)
  expect(container.querySelector('path.line')!.getAttribute('d')).toContain('L')
})

// 最大値が刻みの途中にある図で、目盛りがその下で止まり、上の点に目盛りが無かった。
test('目盛りは最大値の上まで立つ', () => {
  const { container } = render(
    <TimeChart series={{ key: 'x', label: 'x', measure: 'only', rows: 3,
      points: [{ t: '2025-11', v: 4200, n: 1 }, { t: '2025-12', v: 0, n: 1 }, { t: '2026-01', v: 27000, n: 1 }] }}
    enc={{ kind: 'chart', values: ['x'] }} now={now} />)
  const top = Math.min(...[...container.querySelectorAll('line.grid-line')].map((l) => Number(l.getAttribute('y1'))))
  const highest = Math.min(...[...container.querySelectorAll('circle')].map((c) => Number(c.getAttribute('cy'))))
  expect(top).toBeLessThanOrEqual(highest)
})

// 近い点（体重の 08-18 と 08-19）で日付の文字が重なり、右端の文字が切れていた。
test('日付の目盛りは重ならず、端で切れない', () => {
  const { container } = render(
    <TimeChart series={{ key: 'weight_kg', label: 'weight_kg', measure: 'only', rows: 3,
      points: [{ t: '2026-05-01', v: 55, n: 1 }, { t: '2026-08-18', v: 56, n: 1 }, { t: '2026-08-19', v: 56, n: 1 }] }}
    enc={{ kind: 'chart', values: ['weight_kg'] }} now={now} />)
  const labels = [...container.querySelectorAll('text.axis')].filter((t) => t.getAttribute('y') === '214')
  const texts = labels.map((t) => t.textContent)
  expect(new Set(texts).size).toBe(texts.length)
  const xs = labels.map((t) => Number(t.getAttribute('x'))).sort((a, b) => a - b)
  for (let i = 1; i < xs.length; i++) expect(xs[i] - xs[i - 1]).toBeGreaterThanOrEqual(70)
  const lastLabel = labels.reduce((a, b) => Number(a.getAttribute('x')) > Number(b.getAttribute('x')) ? a : b)
  expect(lastLabel.getAttribute('text-anchor')).toBe('end')
})
