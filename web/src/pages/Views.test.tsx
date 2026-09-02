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
