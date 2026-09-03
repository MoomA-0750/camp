import { afterEach, expect, test, vi } from 'vitest'
import { act, render, screen, waitFor } from '@testing-library/react'
import {
  MemoryRouter, Navigate, Route, RouterProvider, Routes, createMemoryRouter,
} from 'react-router-dom'
import Sessions from './pages/Sessions'
import SessionDetail from './pages/SessionDetail'
import Search from './pages/Search'
import Usage from './pages/Usage'

// API は返した URL をそのまま覚えるだけの偽物にする。
// 「画面が何を引きに行ったか」＝「URL が状態になっているか」を見る。
const asked: string[] = []
function stubFetch(payload: (url: string) => unknown) {
  vi.stubGlobal('fetch', async (url: string) => {
    asked.push(url)
    return {
      ok: true, status: 200,
      json: async () => payload(url),
    } as unknown as Response
  })
}

afterEach(() => {
  asked.length = 0
  vi.unstubAllGlobals()
})

function app() {
  return (
    <Routes>
      <Route path="/" element={<Navigate to="/sessions" replace />} />
      <Route path="/sessions" element={<Sessions />} />
      <Route path="/sessions/:id" element={<SessionDetail />} />
      <Route path="/search" element={<Search />} />
      <Route path="/usage" element={<Usage />} />
      <Route path="*" element={<p>そのページは無い</p>} />
    </Routes>
  )
}

const session = {
  id: 'abc', host: 'h', project: 'p', repo_path: '/p', agent: 'claude-code',
  title: 'テストの会話', started_at: '2026-09-02T00:00:00Z',
  updated_at: '2026-09-02T01:00:00Z', messages: 10, conversation: 4,
}

// 受け入れ: /sessions/<id> が URL として機能する。
// 直接その URL でマウントしても、その会話が出ること。
test('/sessions/<id> を直接開くとその会話が出る', async () => {
  stubFetch((url) => {
    if (url.startsWith('/api/sessions/abc/messages')) {
      return { messages: [{ id: 1, type: 'assistant', role: 'assistant',
                            blocks: [{ kind: 'text', text: 'こんにちは' }] }], next_after: 1 }
    }
    if (url.startsWith('/api/sessions/abc')) return session
    return []
  })
  render(<MemoryRouter initialEntries={['/sessions/abc']}>{app()}</MemoryRouter>)

  await waitFor(() => expect(screen.getByText('テストの会話')).toBeTruthy())
  await waitFor(() => expect(screen.getByText('こんにちは')).toBeTruthy())
  expect(asked).toContain('/api/sessions/abc')
})

// 受け入れ: /search?q= が URL として機能する。
// 検索語が URL に載っているので、ブックマークすればその結果が開く。
test('/search?q= を直接開くとその語で引く', async () => {
  stubFetch(() => [{
    block_id: 1, message_id: 2, session_id: 'abc', title: 'テストの会話',
    kind: 'text', timestamp: '2026-09-02T00:00:00Z', snippet: '…認証…', score: 1,
  }])
  render(<MemoryRouter initialEntries={['/search?q=%E8%AA%8D%E8%A8%BC']}>{app()}</MemoryRouter>)

  await waitFor(() => expect(screen.getByText('…認証…')).toBeTruthy())
  expect(asked.some((u) => u.includes('q=%E8%AA%8D%E8%A8%BC'))).toBe(true)
  // 入力欄にも URL の語が入っている（戻ってきたときに空にならない）。
  const input = screen.getByPlaceholderText('検索語') as HTMLInputElement
  expect(input.value).toBe('認証')
})

// 戻る/進むが効くこと。createMemoryRouter は本物の履歴スタックを持つので、
// navigate(-1) / navigate(1) がブラウザの戻る・進むと同じ意味になる。
test('戻る/進むで前後の画面に行き来できる', async () => {
  stubFetch((url) => (url.startsWith('/api/sessions') ? { sessions: [], next_cursor: '' } : []))
  const router = createMemoryRouter(
    [
      { path: '/sessions', element: <Sessions /> },
      { path: '/usage', element: <Usage /> },
    ],
    { initialEntries: ['/sessions'] },
  )
  render(<RouterProvider router={router} />)
  await waitFor(() => expect(screen.getByRole('heading', { name: 'セッション' })).toBeTruthy())

  await act(async () => { await router.navigate('/usage?by=model') })
  await waitFor(() => expect(screen.getByRole('heading', { name: '使用量' })).toBeTruthy())
  expect(asked.some((u) => u.includes('by=model'))).toBe(true)

  // 戻る。
  await act(async () => { await router.navigate(-1) })
  await waitFor(() => expect(screen.getByRole('heading', { name: 'セッション' })).toBeTruthy())
  expect(router.state.location.pathname).toBe('/sessions')

  // 進む。URL のクエリまで戻ってくること（画面の状態が URL に乗っている証拠）。
  await act(async () => { await router.navigate(1) })
  await waitFor(() => expect(screen.getByRole('heading', { name: '使用量' })).toBeTruthy())
  expect(router.state.location.pathname + router.state.location.search).toBe('/usage?by=model')
})

// 絞り込みも URL に置く。置かないとリロードで消えて共有もできない。
test('セッション一覧の絞り込みは URL から復元される', async () => {
  stubFetch((url) => (url.startsWith('/api/hosts') ? [] : { sessions: [], next_cursor: '' }))
  render(
    <MemoryRouter initialEntries={['/sessions?q=%E8%AA%8D%E8%A8%BC&host=general-console']}>
      {app()}
    </MemoryRouter>,
  )
  await waitFor(() => expect(asked.some((u) => u.startsWith('/api/sessions?'))).toBe(true))
  const u = asked.find((x) => x.startsWith('/api/sessions?'))!
  expect(u).toContain('q=%E8%AA%8D%E8%A8%BC')
  expect(u).toContain('host=general-console')
})

// 知らないパスは 404 相当を出す。サーバーは殻を返すので、
// 「無い」と言うのは画面側の仕事になる。
test('知らないパスは画面側で「無い」と言う', async () => {
  stubFetch(() => [])
  render(<MemoryRouter initialEntries={['/nope/deeper']}>{app()}</MemoryRouter>)
  expect(screen.getByText('そのページは無い')).toBeTruthy()
})

// 未認証（401）はログイン画面へ送る。画面ごとに書かない。
test('401 はログイン画面へ送る', async () => {
  const href = { value: '' }
  vi.stubGlobal('location', { get href() { return href.value },
                              set href(v: string) { href.value = v } })
  vi.stubGlobal('fetch', async () => ({ ok: false, status: 401,
                                        json: async () => ({}) }) as unknown as Response)
  render(<MemoryRouter initialEntries={['/usage']}>{app()}</MemoryRouter>)
  await waitFor(() => expect(href.value).toBe('/login'))
})

// 受け入れ（M21）: 消した行が空行と区別できること。
//
// ブロックが0本の行は他にもある（署名だけの thinking など）。同じ見た目に
// なると「あったが消した」のか「元から無かった」のかが分からなくなり、
// 消したことが見えない削除になる。
test('消した行は「ここに何かあったが消した」と出る', async () => {
  stubFetch((url) => {
    if (url.startsWith('/api/sessions/abc/messages')) {
      return { messages: [
        { id: 1, type: 'user', role: 'user',
          redacted: { at: '2026-09-03T00:00:00Z', reason: '平文の認証情報が写っていた',
                      actor: 'campd retain', bytes_removed: 4096, recoverable: true } },
        { id: 2, type: 'assistant', role: 'assistant' },
      ], next_after: 2 }
    }
    if (url.startsWith('/api/sessions/abc')) return session
    return []
  })
  render(<MemoryRouter initialEntries={['/sessions/abc']}>{app()}</MemoryRouter>)

  await waitFor(() => expect(screen.getByText(/ここに何かあったが消した/)).toBeTruthy())
  expect(screen.getByText(/平文の認証情報が写っていた/)).toBeTruthy()
  // 消した行に「本文が残っていない」を重ねて出さない。出すと理由が埋もれる。
  expect(screen.getAllByText(/索引に本文が残っていない行/).length).toBe(1)
})
