import { expect, test } from 'vitest'
import { render } from '@testing-library/react'
import GraphPlot, { layout } from './GraphPlot'
import type { Graph } from './api'

// グラフの描き手（2026-09-13 から）。

const g: Graph = {
  center: 1, depth: 1, unlinked: 3,
  nodes: [
    { id: 1, path: 'hub/index.md', name: 'index', degree: 3, hops: 0 },
    { id: 2, path: 'lab/nas(east).md', name: 'nas(east)', tags: ['lab', 'east'], degree: 1, hops: 1 },
    { id: 3, path: 'lab/kvm.md', name: 'kvm', tags: ['west'], degree: 1, hops: 1 },
    { id: 4, path: 'x.md', name: 'x', degree: 1, hops: 1 },
    { id: 5, path: 'island.md', name: 'island', degree: 0, hops: -1 },
  ],
  edges: [{ from: 1, to: 2 }, { from: 1, to: 3 }, { from: 4, to: 1 }],
}

// **乱数を使わない。** 開くたびに形が変わると、前に見た位置で探せない。
test('同じグラフなら同じ配置になり、枠からはみ出さない', () => {
  const a = layout(g, 720, 520), b = layout(g, 720, 520)
  expect(a).toEqual(b)
  for (const p of a) {
    expect(p.x).toBeGreaterThanOrEqual(0); expect(p.x).toBeLessThanOrEqual(720)
    expect(p.y).toBeGreaterThanOrEqual(0); expect(p.y).toBeLessThanOrEqual(520)
  }
  // 重ならない（押し合っている）。
  for (let i = 0; i < a.length; i++) for (let j = i + 1; j < a.length; j++) {
    expect(Math.hypot(a[i].x - a[j].x, a[i].y - a[j].y)).toBeGreaterThan(10)
  }
})

test('つながった節は、つながっていない節より近くに置く', () => {
  const p = layout(g, 720, 520)
  const d = (i: number, j: number) => Math.hypot(p[i].x - p[j].x, p[i].y - p[j].y)
  expect(d(0, 1)).toBeLessThan(d(4, 1))
})

test('タグの色分けは先に書いたものが勝ち、中心と辿れない島は見分けられる', () => {
  const { container } = render(
    <GraphPlot graph={g} colors={[{ tag: 'east', color: '#2e7d32' }, { tag: 'lab', color: '#999999' },
      { tag: 'west', color: '#1565c0' }]} />)
  const circle = (path: string) => container.querySelector(`g[data-path="${path}"] circle`) as SVGCircleElement
  expect(circle('lab/nas(east).md').style.fill).toBe('rgb(46, 125, 50)')
  expect(circle('lab/kvm.md').style.fill).toBe('rgb(21, 101, 192)')
  expect(circle('x.md').style.fill).toBe('')
  expect(container.querySelector('g.node.center')?.getAttribute('data-path')).toBe('hub/index.md')
  expect(container.querySelector('g.node.far')?.getAttribute('data-path')).toBe('island.md')
  expect(container.querySelectorAll('line.edge')).toHaveLength(3)
  expect(container.textContent).toContain('#east')
})

// 島が2つ（2つの拠点）あっても、両端へ飛ばず、島の中の節が重ならない。
// 2026-09-13、実ブラウザで見つけた: 右の島は枠の外で文字が切れ、節が潰れて重なっていた。
test('離れた島は並べて置き、島の中を潰さない', () => {
  const nodes = Array.from({ length: 12 }, (_, i) => ({ id: i + 1, path: `n${i}.md`, name: `n${i}`, degree: 1, hops: 0 }))
  const edges = [
    { from: 1, to: 2 }, { from: 1, to: 3 }, { from: 1, to: 4 }, { from: 1, to: 5 },
    { from: 6, to: 7 }, { from: 7, to: 8 }, { from: 7, to: 9 }, { from: 9, to: 10 }, { from: 9, to: 11 }, { from: 11, to: 12 },
  ]
  const p = layout({ nodes, edges, unlinked: 0 }, 900, 520)
  let closest = Infinity
  for (let i = 0; i < p.length; i++) for (let j = i + 1; j < p.length; j++) {
    closest = Math.min(closest, Math.hypot(p[i].x - p[j].x, p[i].y - p[j].y))
  }
  expect(closest).toBeGreaterThan(25)
  for (const q of p) {
    expect(q.x).toBeGreaterThanOrEqual(28 - 1e-6); expect(q.x).toBeLessThanOrEqual(900 - 28 + 1e-6)
  }
})

// 横長の枠に島が2つなら横に並べる（縦に積むと小さく潰れた。2026-09-13、画面写しで見つけた）。
test('横長の枠では島を横に並べて大きく描く', () => {
  const nodes = Array.from({ length: 6 }, (_, i) => ({ id: i + 1, path: `n${i}.md`, name: `n${i}`, degree: 1, hops: 0 }))
  const edges = [{ from: 1, to: 2 }, { from: 2, to: 3 }, { from: 4, to: 5 }, { from: 5, to: 6 }]
  const p = layout({ nodes, edges, unlinked: 0 }, 1000, 400)
  const left = p.slice(0, 3).map((q) => q.x), right = p.slice(3).map((q) => q.x)
  const sep = Math.max(...left) < Math.min(...right) || Math.max(...right) < Math.min(...left)
  expect(sep).toBe(true)
})

// 右半分の節の文字は左に出す（右端で枠の外へ切れていた。2026-09-13、画面写しで見つけた）。
test('右半分の節の名前は左へ出す', () => {
  const { container } = render(<GraphPlot graph={g} />)
  const groups = [...container.querySelectorAll('g.node')]
  let right = 0
  for (const el of groups) {
    const x = Number(el.getAttribute('transform')!.match(/translate\(([\d.]+)/)![1])
    const anchor = el.querySelector('text')?.getAttribute('text-anchor')
    if (x > 360) { right++; expect(anchor).toBe('end') } else expect(anchor).not.toBe('end')
  }
  expect(right).toBeGreaterThan(0)
})

// **本物の構成図の形**（13 節・14 辺、2つの拠点の島）。2026-09-13 の実ブラウザで、
// 全体を一度に押し引きしていたころは島が枠の両端へ飛び、島の中が潰れて節が重なった。
// その形で測ると、最も近い2節は 31px・辺の平均は 48px だった（島ごとに配置すると 70px・110px）。
test('Homelab の構成図の形で、島の中の節を潰さない', () => {
  const edges = [[3, 5], [3, 1], [11, 2], [4, 10], [4, 1], [4, 13], [1, 3], [1, 5], [1, 9], [1, 6], [2, 7], [2, 8], [2, 12], [6, 1]]
    .map(([from, to]) => ({ from, to }))
  const nodes = Array.from({ length: 13 }, (_, i) => ({ id: i + 1, path: `n${i}.md`, name: `n${i}`, degree: 1, hops: 0 }))
  const p = layout({ nodes, edges, unlinked: 0 }, 940, 520)
  let closest = Infinity
  for (let i = 0; i < p.length; i++) for (let j = i + 1; j < p.length; j++) {
    closest = Math.min(closest, Math.hypot(p[i].x - p[j].x, p[i].y - p[j].y))
  }
  const mean = edges.reduce((s, e) => s + Math.hypot(p[e.from - 1].x - p[e.to - 1].x, p[e.from - 1].y - p[e.to - 1].y), 0) / edges.length
  expect(closest).toBeGreaterThan(50)
  expect(mean).toBeGreaterThan(80)
})
