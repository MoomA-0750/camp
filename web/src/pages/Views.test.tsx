import { afterEach, expect, test, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import Views from './Views'
import ViewDetail from './ViewDetail'

function stub(payload: (url: string) => unknown) {
  vi.stubGlobal('fetch', async (url: string) => ({
    ok: true, status: 200, json: async () => payload(url),
  } as unknown as Response))
}
afterEach(() => vi.unstubAllGlobals())

// 一覧は「定義が挙げた列」と「自動で出た列」を分けて見せる。
// 反転が効いていることが数字で分かる場所。
test('定義の列と自動の列を分けて出す', async () => {
  stub(() => [{ base: 'Health', name: 'テーブル', kind: 'table',
    id: 'Health/テーブル', rows: 2687, columns: 107, pinned: 10 }])
  render(
    <MemoryRouter initialEntries={['/views']}>
      <Routes><Route path="/views" element={<Views />} /></Routes>
    </MemoryRouter>)
  await waitFor(() => expect(screen.getByText('107')).toBeTruthy())
  expect(screen.getByText('10')).toBeTruthy()
  expect(screen.getByText('97')).toBeTruthy() // 107 - 10
})

const result = {
  view: 'テーブル', kind: 'table', total: 2,
  columns: [
    { key: 'date', label: 'date', pinned: true, filled: 2 },
    { key: 'steps', label: 'steps', numeric: true, filled: 2 },
    { key: 'blood_oxygen', label: 'blood_oxygen', numeric: true, filled: 0 },
  ],
  groups: [{ key: '', rows: [
    { note_id: 1, path: 'a.md', name: 'a', cells: { date: '2026-01-01', steps: '8000' } },
    { note_id: 2, path: 'b.md', name: 'b', cells: { date: '2026-01-02', steps: '9000' } },
  ] }],
}

// 空の列は既定でたたむ。**消すのではなく畳む** — 消すと Bases に戻ってしまう。
test('空の列は畳むが、開ける', async () => {
  stub(() => result)
  const { unmount } = render(
    <MemoryRouter initialEntries={['/views/Health/テーブル']}>
      <Routes><Route path="/views/*" element={<ViewDetail />} /></Routes>
    </MemoryRouter>)
  await waitFor(() => expect(screen.getByText('steps')).toBeTruthy())
  expect(screen.queryByText('blood_oxygen')).toBeNull()
  expect(screen.getByText(/1 列たたんでいる/)).toBeTruthy()
  unmount()

  stub(() => result)
  render(
    <MemoryRouter initialEntries={['/views/Health/テーブル?cols=all']}>
      <Routes><Route path="/views/*" element={<ViewDetail />} /></Routes>
    </MemoryRouter>)
  await waitFor(() => expect(screen.getByText('blood_oxygen')).toBeTruthy())
})

// 読めなかった式は黙って消さず、画面に出す。
test('読めない式を画面に出す', async () => {
  stub(() => ({ ...result, warnings: ['formula "金額": 知らない関数 nope()'] }))
  render(
    <MemoryRouter initialEntries={['/views/x/y']}>
      <Routes><Route path="/views/*" element={<ViewDetail />} /></Routes>
    </MemoryRouter>)
  await waitFor(() => expect(screen.getByText(/読めなかった式/)).toBeTruthy())
})

// チャートを描く定義でも行は捨てない（たたんで下に置く）。2026-09-13 から。
test('チャートの定義は時系列を描き、行はたたむ', async () => {
  stub(() => ({
    ...result, view: '銀行-残高推移', kind: 'life-tracker',
    time: { axis: 'date', bucket: 'day' },
    series: [{ key: 'steps', label: 'steps', measure: 'last', rows: 2,
      points: [{ t: '2026-01-01', v: 8000, n: 1 }, { t: '2026-01-02', v: 9000, n: 3 }] }],
    emit: { human: [{ kind: 'chart', values: ['steps'], chart: 'line' }] },
  }))
  const { container } = render(
    <MemoryRouter initialEntries={['/views/Payments/銀行-残高推移']}>
      <Routes><Route path="/views/*" element={<ViewDetail />} /></Routes>
    </MemoryRouter>)
  await waitFor(() => expect(container.querySelector('svg.plot')).toBeTruthy())
  const fold = container.querySelector('details.rows-fold')
  expect(fold).toBeTruthy()
  expect(fold!.querySelector('table')).toBeTruthy()
  expect(screen.getByText('元の行を見る（2 行）')).toBeTruthy()
})

test('表の列幅は定義の値で描く', async () => {
  stub(() => ({ ...result, emit: { human: [{ kind: 'table', widths: { steps: 487 } }] } }))
  const { container } = render(
    <MemoryRouter initialEntries={['/views/Health/テーブル']}>
      <Routes><Route path="/views/*" element={<ViewDetail />} /></Routes>
    </MemoryRouter>)
  await waitFor(() => expect(screen.getByText('steps')).toBeTruthy())
  const th = screen.getByTitle('steps') as HTMLElement
  expect(th.style.width).toBe('487px')
  expect(container.querySelector('details.rows-fold')).toBeNull()
})

// グラフのビューは節と辺を描き、行はたたむ。色分けの凡例も出す（2026-09-13 から）。
test('グラフのビューは構成図を描き、行はたたむ', async () => {
  stub(() => ({
    ...result, view: '構成図', kind: 'graph',
    graph: { unlinked: 2, nodes: [
      { id: 1, path: 'lab/a.md', name: 'a', tags: ['east'], degree: 1, hops: 0 },
      { id: 2, path: 'lab/b.md', name: 'b', degree: 1, hops: 0 }],
    edges: [{ from: 1, to: 2 }] },
    emit: { human: [{ kind: 'graph', colors: [{ tag: 'east', color: '#2e7d32' }] }] },
  }))
  const { container } = render(
    <MemoryRouter initialEntries={['/views/Homelab/構成図']}>
      <Routes><Route path="/views/*" element={<ViewDetail />} /></Routes>
    </MemoryRouter>)
  await waitFor(() => expect(container.querySelector('svg.graph')).toBeTruthy())
  expect(screen.getByText(/行どうしのリンクを持たない 2 件は出していない/)).toBeTruthy()
  expect(screen.getByText('#east')).toBeTruthy()
  expect(container.querySelector('details.rows-fold')).toBeTruthy()
})
