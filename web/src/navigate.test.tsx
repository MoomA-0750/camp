import { afterEach, expect, test, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom'
import { hasBase, rank } from './editor/names'
import { insertion, openLink } from './editor/wikicomplete'
import Switcher, { today } from './Switcher'
import NoteNew from './pages/NoteNew'
import type { NoteName } from './api'

// 行き来（Phase 5 / M55）。

const names: NoteName[] = [
  { id: 1, path: 'Human/Logs/2026-09-13.md', link: 'Human/Logs/2026-09-13', editable: true },
  { id: 2, path: 'Data/Health/2026-09-13.md', link: 'Data/Health/2026-09-13' },
  { id: 3, path: 'Human/Projects/Camp.md', link: 'Camp', editable: true },
  { id: 4, path: 'Inbox/キャンプの買い物.md', link: 'キャンプの買い物', editable: true },
  { id: 5, path: 'Human/Projects/a#b.md' }, // リンクに書けない
  { id: 6, path: 'AI/Wiki/Campfire.md', link: 'Campfire', editable: true },
]

test('絞り込み: ベース名そのもの > 頭 > 中 > パス。語は全部当たるものだけ', () => {
  expect(rank('camp', names).map((n) => n.id)).toEqual([3, 6])
  expect(rank('logs 09-13', names).map((n) => n.id)).toEqual([1])
  expect(rank('hpcamp', names).map((n) => n.id)).toEqual([3]) // Human/Projects/Camp に順に並ぶ
  expect(rank('買い物', names).map((n) => n.id)).toEqual([4])
  expect(rank('', names)).toHaveLength(names.length)
  expect(rank('camp', names, 50, (n) => !!n.link).map((n) => n.id)).toEqual([3, 6])
  expect(hasBase('CAMP', names)).toBe(true)
  expect(hasBase('camp2', names)).toBe(false)
})

test('補完: 開いた [[ の中だけを問い合わせにし、]] は二重にしない', () => {
  expect(openLink('今日は [[Cam')).toEqual({ query: 'Cam', start: 6 })
  expect(openLink('![[')).toEqual({ query: '', start: 3 })
  expect(openLink('[[Camp]] のあと')).toBeNull()
  expect(openLink('[[Camp|別名')).toBeNull()
  expect(openLink('[[Camp#見出')).toBeNull()
  expect(openLink('[[a\nb')).toBeNull()
  expect(insertion('Human/Logs/2026-09-13', '')).toEqual({ text: 'Human/Logs/2026-09-13]]', cursor: 23 })
  expect(insertion('Camp', ']] 続き')).toEqual({ text: 'Camp', cursor: 6 })
})

test('今日は端末の時刻で決める', () => {
  expect(today(new Date(2026, 0, 5, 23, 59))).toBe('2026-01-05')
})

function stub(routes: Record<string, unknown>, posts: { url: string; body: unknown }[] = []) {
  vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
    if (init?.method === 'POST') {
      posts.push({ url, body: JSON.parse(String(init.body)) })
      return { ok: true, status: 201, json: async () => ({ note_id: 9, path: 'Inbox/x.md', created: true }) } as unknown as Response
    }
    const key = Object.keys(routes).find((k) => url.startsWith(k))
    return { ok: true, status: 200, json: async () => (key ? routes[key] : {}) } as unknown as Response
  })
}
afterEach(() => { vi.unstubAllGlobals() })

function Where() {
  const l = useLocation()
  return <p data-testid="where">{l.pathname + l.search}</p>
}

test('スイッチャー: 選んで Enter で書く画面へ。当たりが無ければ作る画面へ（作るのは押してから）', async () => {
  const posts: { url: string; body: unknown }[] = []
  stub({ '/api/notes/sync': { vault_id: 1, pending: 0, overwritten: [], push: { kind: '' } },
    '/api/vaults/1/names': { gen: 'g1', names } }, posts)
  const close = vi.fn()
  render(
    <MemoryRouter initialEntries={['/']}>
      <Switcher onClose={close} />
      <Where />
    </MemoryRouter>,
  )
  const input = screen.getByLabelText('ノートの名前・パス')
  await waitFor(() => expect(screen.getAllByRole('option').length).toBe(names.length))
  fireEvent.change(input, { target: { value: 'camp' } })
  fireEvent.keyDown(input, { key: 'ArrowDown' })
  fireEvent.keyDown(input, { key: 'Enter' })
  expect(screen.getByTestId('where').textContent).toBe('/notes/6/edit')
  expect(close).toHaveBeenCalled()

  fireEvent.change(input, { target: { value: '新しい考え' } })
  const create = screen.getByText(/「新しい考え」を新しく作る/)
  fireEvent.click(create)
  expect(screen.getByTestId('where').textContent).toBe('/notes/new?name=' + encodeURIComponent('新しい考え'))
  expect(posts).toHaveLength(0)
})

test('新しいノート: URL の名前は欄に入れるだけ。押すとフォルダと名前で作る', async () => {
  const posts: { url: string; body: unknown }[] = []
  stub({ '/api/notes/sync': { vault_id: 1, pending: 0, overwritten: [], push: { kind: '' } },
    '/api/vaults/1/names': { gen: 'g1', names } }, posts)
  render(
    <MemoryRouter initialEntries={['/notes/new?name=' + encodeURIComponent('思いつき')]}>
      <Routes>
        <Route path="/notes/new" element={<NoteNew />} />
        <Route path="*" element={<Where />} />
      </Routes>
    </MemoryRouter>,
  )
  const name = await screen.findByLabelText('名前') as HTMLInputElement
  expect(name.value).toBe('思いつき')
  await waitFor(() => expect(screen.getByRole('option', { name: 'Human/Projects' })).toBeTruthy())
  // 書けない場所（Data/Health）のフォルダは出さない。
  expect(screen.queryByRole('option', { name: 'Data/Health' })).toBeNull()
  expect(posts).toHaveLength(0)
  fireEvent.change(screen.getByLabelText('フォルダ'), { target: { value: 'Human/Projects' } })
  fireEvent.change(screen.getByLabelText('書き留める（空でもよい）'), { target: { value: 'メモ\n' } })
  fireEvent.click(screen.getByRole('button', { name: '作る' }))
  await waitFor(() => expect(posts).toHaveLength(1))
  expect(posts[0]).toEqual({ url: '/api/vaults/1/notes', body: { path: 'Human/Projects/思いつき.md', body: 'メモ\n' } })
  await waitFor(() => expect(screen.getByTestId('where').textContent).toBe('/notes/9/edit'))
  expect(localStorage.getItem('camp.draft.new')).toBeNull()
})

test('新しいノート: 作る前の書きかけは控え、開き直すと戻る（outer gate）', async () => {
  stub({ '/api/notes/sync': { vault_id: 1, pending: 0, overwritten: [], push: { kind: '' } },
    '/api/vaults/1/names': { gen: 'g1', names } })
  const first = render(
    <MemoryRouter initialEntries={['/notes/new']}>
      <NoteNew />
    </MemoryRouter>,
  )
  fireEvent.change(await screen.findByLabelText('書き留める（空でもよい）'), { target: { value: '長い書きかけ' } })
  first.unmount()
  render(
    <MemoryRouter initialEntries={['/notes/new']}>
      <NoteNew />
    </MemoryRouter>,
  )
  expect((await screen.findByLabelText('書き留める（空でもよい）') as HTMLTextAreaElement).value).toBe('長い書きかけ')
  expect(screen.getByText(/前に書きかけた内容を戻した/)).toBeTruthy()
  localStorage.clear()
})
