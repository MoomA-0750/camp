import { afterEach, expect, test, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import Notes from './Notes'
import NoteDetail from './NoteDetail'

const asked: string[] = []
function stub(payload: (url: string) => unknown, text = '') {
  vi.stubGlobal('fetch', async (url: string) => {
    asked.push(url)
    return {
      ok: true, status: 200,
      json: async () => payload(url),
      text: async () => text,
    } as unknown as Response
  })
}
afterEach(() => { asked.length = 0; vi.unstubAllGlobals() })

const note = {
  id: 7, vault_id: 1, path: 'Human/Projects/Camp.md', title: 'Camp',
  kind: 'markdown', size: 100, mtime: '2026-09-02T09:00:00Z',
  links: 1, backlinks: 1, touches: 1,
}

// 絞り込みは全部URLに乗る。リロードしても共有しても同じ画面が出る。
test('絞り込みがURLからそのままAPIに渡る', async () => {
  stub(() => [])
  render(
    <MemoryRouter initialEntries={['/notes?q=Projects/&kind=markdown&missing=only']}>
      <Routes><Route path="/notes" element={<Notes />} /></Routes>
    </MemoryRouter>)
  await waitFor(() => expect(asked.some((u) => u.startsWith('/api/notes?'))).toBe(true))
  const u = asked.find((x) => x.startsWith('/api/notes?'))!
  expect(u).toContain('q=Projects%2F')
  expect(u).toContain('kind=markdown')
  expect(u).toContain('missing=only')
})

// 消えたノートは、本文が読めることと「もう無い」ことを同時に見せる。
// どちらかだけだと、残っているのか消えたのか分からない。
test('消えたノートは本文と消滅を両方見せる', async () => {
  stub((url) => {
    if (url.includes('/links')) return { out: [], back: [] }
    if (url.includes('/sessions')) return []
    return { ...note, missing_at: '2026-09-01T00:00:00Z' }
  }, '控えの本文')
  render(
    <MemoryRouter initialEntries={['/notes/7']}>
      <Routes><Route path="/notes/:id" element={<NoteDetail />} /></Routes>
    </MemoryRouter>)
  await waitFor(() => expect(screen.getByText(/もう無い/)).toBeTruthy())
  await waitFor(() => expect(screen.getByText('控えの本文')).toBeTruthy())
})

// 曖昧なリンクは、選んだ先だけでなく候補も見せる。
// 黙って1つ選ぶと、Obsidian と食い違っていても気付けない。
test('曖昧なリンクは候補も出す', async () => {
  stub((url) => {
    if (url.includes('/links')) {
      return {
        out: [{
          from_id: 7, from_path: 'AI/Profile/x.md', to_id: 9,
          to_path: 'Human/Logs/2026-07-11.md', target: '2026-07-11',
          embed: false, resolved: true, ambiguous: true,
          candidates: ['Human/Logs/2026-07-11.md', 'Data/Health/2026-07-11.md'],
        }],
        back: [],
      }
    }
    if (url.includes('/sessions')) return []
    return note
  })
  render(
    <MemoryRouter initialEntries={['/notes/7']}>
      <Routes><Route path="/notes/:id" element={<NoteDetail />} /></Routes>
    </MemoryRouter>)
  await waitFor(() => expect(screen.getByText('曖昧')).toBeTruthy())
  expect(screen.getByText(/Data\/Health\/2026-07-11\.md/)).toBeTruthy()
  expect(screen.getByText(/Obsidian と食い違う可能性/)).toBeTruthy()
})

// 本文を保存していない種別（添付）は、空欄ではなく理由を出す。
test('本文が無い種別は理由を出す', async () => {
  vi.stubGlobal('fetch', async (url: string) => {
    if (url.includes('/body')) return { ok: false, status: 404 } as Response
    return {
      ok: true, status: 200,
      json: async () => url.includes('/links') ? { out: [], back: [] }
        : url.includes('/sessions') ? [] : { ...note, kind: 'asset' },
    } as unknown as Response
  })
  render(
    <MemoryRouter initialEntries={['/notes/7']}>
      <Routes><Route path="/notes/:id" element={<NoteDetail />} /></Routes>
    </MemoryRouter>)
  await waitFor(() => expect(screen.getByText(/本文を保存していない/)).toBeTruthy())
})

// 一覧は上限で切らない。件数は常に本当の全件で、描くのは見えている分だけ。
// 500件で頭打ちにすると「500件」が全件か打ち切りか読み手に分からない。
test('全件を数え、描くのは一部', async () => {
  const many = Array.from({ length: 3000 }, (_, i) => ({
    ...note, id: i + 1, path: `Data/N/${i}.md`, title: `n${i}`,
  }))
  stub(() => many)
  render(
    <MemoryRouter initialEntries={['/notes']}>
      <Routes><Route path="/notes" element={<Notes />} /></Routes>
    </MemoryRouter>)
  await waitFor(() => expect(screen.getByText('3,000 件')).toBeTruthy())
  // 先頭は出ている
  expect(screen.getByText('n0')).toBeTruthy()
  // 遥か下はDOMに無い（全部描いていたら重い）
  expect(screen.queryByText('n2999')).toBeNull()
})
