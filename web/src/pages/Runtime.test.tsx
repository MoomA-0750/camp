import { afterEach, expect, test, vi } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import Runtime from './Runtime'

function stub(payload: (url: string) => unknown) {
  vi.stubGlobal('fetch', async (url: string) => ({
    ok: true, status: 200,
    json: async () => payload(url),
    text: async () => '',
  } as unknown as Response))
}
afterEach(() => vi.unstubAllGlobals())

function show(path = '/runtime') {
  render(
    <MemoryRouter initialEntries={[path]}>
      <Routes><Route path="/runtime" element={<Runtime />} /></Routes>
    </MemoryRouter>)
}

// **API が null を返しても真っ白にならない。**
//
// Go の nil スライスは JSON で `null` になる。2026-09-04、実ブラウザで開いて
// 初めて分かった——`sessions.length` が null 参照で落ち、画面が真っ白になった。
// テストは JSON しか見ていなかったので出なかった。
test('sessions が null でも落ちない', async () => {
  stub((u) => (u.startsWith('/api/runtime') ? { agent_connected: true, sessions: null } : []))
  show()
  await waitFor(() => expect(screen.getByText(/いま走っているものは無い/)).toBeTruthy())
})

test('許可リストが null でも落ちない', async () => {
  stub((u) => (u.startsWith('/api/allowlist') ? null : { agent_connected: true, sessions: [] }))
  show('/runtime?tab=allow')
  await waitFor(() => expect(screen.getByText(/空。この状態では1本も起こせない/)).toBeTruthy())
})

// 実行面が居ないことは、画面に出る（何もできない理由がそれなので）。
test('実行面が繋がっていないと、そう出る', async () => {
  stub(() => ({ agent_connected: false, sessions: [] }))
  show()
  await waitFor(() => expect(screen.getByText(/実行面が繋がっていない/)).toBeTruthy())
})

// 状態はURLに乗る（Phase 0 からの決まり）。
test('タブがURLに乗る', async () => {
  stub(() => ({ agent_connected: true, sessions: [] }))
  show('/runtime?tab=ssh')
  await waitFor(() => expect(screen.getByText(/読むだけ/)).toBeTruthy())
})

// ---- 向こうで起こす（2026-09-11）----------------------------------------------

const hostsPayload = [
  { id: 1, alias: 'far', allowed: true, source: 'ssh_config', seen_at: '', updated_at: '',
    pinned: { hostname: 'far.example', user: 'me', port: '22', hostkeys: ['SHA256:abc'] } },
  // 行き先が固定されていない許可。起こせない。
  { id: 2, alias: 'old', allowed: true, source: 'ssh_config', seen_at: '', updated_at: '' },
  // 鍵を固定していない古い固定。これも起こせない。
  { id: 4, alias: 'keyless', allowed: true, source: 'ssh_config', seen_at: '', updated_at: '',
    pinned: { hostname: 'k.example' } },
  { id: 3, alias: 'nope', allowed: false, source: 'ssh_config', seen_at: '', updated_at: '' },
]

test('起こせる先は、許して行き先を固定した接続先だけ', async () => {
  stub((u) => (u.startsWith('/api/ssh') ? hostsPayload : { agent_connected: true, sessions: [] }))
  show()
  const sel = await screen.findByLabelText('どこで起こすか')
  await waitFor(() => expect(within(sel).getByText(/far/)).toBeTruthy())
  expect(within(sel).queryByText(/old/)).toBeNull()
  expect(within(sel).queryByText(/nope/)).toBeNull()
  expect(within(sel).queryByText(/keyless/)).toBeNull()
})

test('起こすときに接続先を渡す', async () => {
  const posts: unknown[] = []
  vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
    if (init?.method === 'POST') posts.push(JSON.parse(String(init.body)))
    const body = url.startsWith('/api/ssh') ? hostsPayload : { agent_connected: true, sessions: [] }
    return { ok: true, status: 200, json: async () => body, text: async () => '' } as unknown as Response
  })
  show()
  const sel = await screen.findByLabelText('どこで起こすか')
  await waitFor(() => expect(within(sel).getByText(/far/)).toBeTruthy())
  fireEvent.change(sel, { target: { value: 'far' } })
  fireEvent.change(screen.getByPlaceholderText(/far の上の場所/), { target: { value: '/srv/work' } })
  fireEvent.click(screen.getByText('起こす'))
  await waitFor(() => expect(posts).toContainEqual({ cwd: '/srv/work', host: 'far' }))
})

test('台帳は固定した行き先を出し、固定の無い許可には許し直すよう言う', async () => {
  stub((u) => (u.startsWith('/api/ssh') ? hostsPayload : { agent_connected: true, sessions: [] }))
  show('/runtime?tab=ssh')
  await waitFor(() => expect(screen.getByText('me@far.example・鍵 1')).toBeTruthy())
  // 固定の無い許可と、鍵の無い固定の2つ。
  expect(screen.getAllByText(/許し直す/).length).toBe(2)
})

test('向こうのセッションはホスト名つきで出る', async () => {
  stub((u) => (u.startsWith('/api/runtime')
    ? { agent_connected: true, sessions: [{ id: 'r1', cwd: '/srv/work', host: 'far', state: 'idle',
        requested_by: 'user', created_at: '', updated_at: '' }] }
    : []))
  show()
  await waitFor(() => expect(screen.getByText('far:/srv/work')).toBeTruthy())
})

// ---- 終わったもの（2026-09-11）------------------------------------------------

const base = { requested_by: 'user', created_at: '2026-09-11T01:00:00Z', updated_at: '', state: 'exited' }
const ended = {
  sessions: [
    { ...base, id: 'a1', cwd: '/w/a', ended_at: '2026-09-11T02:00:00Z',
      end_cause: 'user_stop', end_state: 'running', exit_code: -1,
      approvals_asked: 2, approvals_left_waiting: 1, approvals_timed_out: 0 },
    { ...base, id: 'a2', cwd: '/w/b', ended_at: '2026-09-11T01:30:00Z',
      end_cause: 'idle_timeout', end_state: 'idle', exit_code: 0,
      approvals_asked: 1, approvals_left_waiting: 0, approvals_timed_out: 1 },
    // 記録を始める前に終わったもの。終わり方は無い。
    { ...base, id: 'a3', cwd: '/w/c', ended_at: '2026-09-10T00:00:00Z',
      approvals_asked: 0, approvals_left_waiting: 0, approvals_timed_out: 0 },
  ],
  next: '2026-09-10T00:00:00Z|a3',
  counts: { all: 12, mid: 1, waiting: 1, ignored: 1, unknown: 1, user_stop: 1, idle_timeout: 1 },
}

// 本人が探したいもの——動いている途中で終わった・承認を待たせたまま終わった・
// 承認を期限切れにした——が、1行ずつ見分けられる。
test('終わったものに、終わり方・そのとき・承認の内訳が出る', async () => {
  stub((u) => (u.startsWith('/api/runtime/ended') ? ended : { agent_connected: true, sessions: [] }))
  show('/runtime?tab=ended')
  const table = await screen.findByRole('table')
  const rows = within(table).getAllByRole('row')
  expect(within(rows[1]).getByText('本人が止めた')).toBeTruthy()
  expect(within(rows[1]).getByText('動いている途中')).toBeTruthy()
  expect(within(rows[1]).getByText('待たせたまま 1')).toBeTruthy()
  expect(within(rows[2]).getByText('放置で閉じた')).toBeTruthy()
  expect(within(rows[2]).getByText('期限切れ 1')).toBeTruthy()
  // 記録の無いものを、推し量って埋めない。
  expect(within(rows[3]).getByText('記録なし')).toBeTruthy()
  // 件数は頁ではなく全体。
  expect(screen.getByText('12')).toBeTruthy()
  expect(screen.getByText('さらに古いもの')).toBeTruthy()
})

// 絞り込みと続きは URL に乗り、そのまま API へ渡る。
test('絞り込みと続きがURLに乗る', async () => {
  const urls: string[] = []
  stub((u) => {
    urls.push(u)
    return u.startsWith('/api/runtime/ended') ? ended : { agent_connected: true, sessions: [] }
  })
  show('/runtime?tab=ended&kind=waiting')
  await waitFor(() => expect(urls.some((u) => u === '/api/runtime/ended?kind=waiting')).toBe(true))

  fireEvent.click(await screen.findByText('さらに古いもの'))
  await waitFor(() => expect(urls.some((u) => u.includes('kind=waiting') && u.includes('before='))).toBe(true))
})

test('終わったものが無いと、そう出る', async () => {
  stub((u) => (u.startsWith('/api/runtime/ended')
    ? { sessions: [], counts: { all: 0 } }
    : { agent_connected: true, sessions: [] }))
  show('/runtime?tab=ended')
  await waitFor(() => expect(screen.getByText(/まだ1本も終わっていない/)).toBeTruthy())
})
