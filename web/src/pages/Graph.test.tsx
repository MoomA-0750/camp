import { afterEach, expect, test, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom'
import Graph from './Graph'

// ノートから辿るグラフ（2026-09-13 から）。受け入れは「ハブから辿って戻れる」。
// ハブ（_index）は被リンクを持たないので、行った先の被リンクにハブが出て、そこから戻る。

const graphs: Record<string, unknown> = {
  1: { center: 1, depth: 1, unlinked: 4000,
    nodes: [{ id: 1, path: 'hub/index.md', name: 'index', degree: 1, hops: 0 },
      { id: 2, path: 'ctx/lab.md', name: 'lab', degree: 1, hops: 1 }],
    edges: [{ from: 1, to: 2 }] },
  2: { center: 2, depth: 1, unlinked: 4000,
    nodes: [{ id: 2, path: 'ctx/lab.md', name: 'lab', degree: 2, hops: 0 },
      { id: 1, path: 'hub/index.md', name: 'index', degree: 1, hops: 1 },
      { id: 3, path: 'lab/kvm.md', name: 'kvm', degree: 1, hops: 1 }],
    edges: [{ from: 1, to: 2 }, { from: 2, to: 3 }] },
}
const seen: string[] = []
afterEach(() => { vi.unstubAllGlobals(); seen.length = 0 })

function Where() {
  const l = useLocation()
  return <p data-testid="where">{l.search}</p>
}

test('ハブから出て、被リンクをたどって戻れる', async () => {
  vi.stubGlobal('fetch', async (url: string) => {
    seen.push(url)
    const note = new URL(url, 'http://x').searchParams.get('note') ?? '1'
    return { ok: true, status: 200, json: async () => graphs[note] } as unknown as Response
  })
  render(
    <MemoryRouter initialEntries={['/graph?note=1']}>
      <Routes><Route path="/graph" element={<><Graph /><Where /></>} /></Routes>
    </MemoryRouter>)
  await waitFor(() => expect(screen.getByText('被リンク（0）')).toBeTruthy())
  expect(screen.getByText(/リンクを持たないノート 4,000 件は出していない/)).toBeTruthy()

  // ハブから homelab へ（出ていくリンク）。
  fireEvent.click(screen.getByRole('link', { name: 'lab' }))
  await waitFor(() => expect(screen.getByText('被リンク（1）')).toBeTruthy())
  expect(screen.getByTestId('where').textContent).toBe('?note=2&depth=1')
  // 行った先の被リンクにハブがいて、押すと戻る。
  fireEvent.click(screen.getByRole('link', { name: 'index' }))
  await waitFor(() => expect(screen.getByText('被リンク（0）')).toBeTruthy())
  expect(screen.getByTestId('where').textContent).toBe('?note=1&depth=1')
})

test('節を押しても中心が移り、歩数を変えると depth を付けて取り直す（0 は全部）', async () => {
  vi.stubGlobal('fetch', async (url: string) => {
    seen.push(url)
    return { ok: true, status: 200, json: async () => graphs[2] } as unknown as Response
  })
  const { container } = render(
    <MemoryRouter initialEntries={['/graph']}>
      <Routes><Route path="/graph" element={<><Graph /><Where /></>} /></Routes>
    </MemoryRouter>)
  await waitFor(() => expect(container.querySelector('g[data-path="lab/kvm.md"]')).toBeTruthy())
  expect(seen[0]).toBe('/api/graph?depth=1')
  fireEvent.click(screen.getByRole('button', { name: 'リンクのあるノート全部' }))
  await waitFor(() => expect(seen).toContain('/api/graph?depth=0'))
  fireEvent.click(container.querySelector('g[data-path="lab/kvm.md"]')!)
  await waitFor(() => expect(screen.getByTestId('where').textContent).toBe('?note=3&depth=0'))
})
