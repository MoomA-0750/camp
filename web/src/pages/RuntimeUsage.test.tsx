import { afterEach, expect, test, vi } from 'vitest'
import { act, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import RuntimeDetail from './RuntimeDetail'

// 2026-09-04 に本物の `claude` から返ってきた形（値は丸めてある）。
// **推測で書かない。** 実測の欄だけを置く。
const usage = {
  running: 1, max: 4,
  usage: {
    subscription_type: 'pro',
    rate_limits: {
      limits: [
        { group: 'session', kind: 'session', percent: 100, is_active: true,
          resets_at: '2126-09-04T18:30:00Z', severity: 'critical' },
        { group: 'weekly', kind: 'weekly_all', percent: 48, is_active: false,
          resets_at: '2126-09-10T00:00:00Z', severity: 'normal' },
      ],
    },
    session: {
      total_cost_usd: 0.1422,
      model_usage: {
        'claude-opus-5': {
          inputTokens: 4, outputTokens: 226, cacheReadInputTokens: 25859,
          cacheCreationInputTokens: 12264, thinkingTokens: 0, costUSD: 0.1412,
        },
      },
    },
  },
  context: {
    categories: [
      { name: 'System prompt', tokens: 3296 },
      { name: 'Messages', tokens: 9408 },
    ],
    totalTokens: 21874, maxTokens: 1000000, percentage: 2,
  },
}

function stub(payload: (url: string) => unknown) {
  vi.stubGlobal('EventSource', class {
    close() {}
    addEventListener() {}
  } as unknown as typeof EventSource)
  vi.stubGlobal('fetch', async (url: string) => ({
    ok: true, status: 200,
    json: async () => payload(url),
    text: async () => '',
  } as unknown as Response))
}
afterEach(() => vi.unstubAllGlobals())

// 仕様が求めた4種のうち3種がここに出る（残り1種＝プラン残量の履歴は「使用量」）。
// **生の JSON を貼るだけでは「出た」ことにならない。**
test('残量タブに、枠・トークン内訳・コンテキスト・同時実行が出る', async () => {
  stub((u) => {
    if (u.includes('/usage')) return usage
    if (u === '/api/runtime/abc') return { id: 'abc', state: 'idle', cwd: '/w', requested_by: 'u', created_at: '', updated_at: '' }
    if (u.startsWith('/api/runtime/')) return []
    return { agent_connected: true, sessions: [] }
  })
  render(
    <MemoryRouter initialEntries={['/runtime/abc?tab=usage&live=0']}>
      <Routes><Route path="/runtime/:id" element={<RuntimeDetail />} /></Routes>
    </MemoryRouter>)

  // 同時実行
  await waitFor(() => expect(screen.getByText(/同時に走っているのは 1 \/ 4 本/)).toBeTruthy())
  // プラン枠（メーター）。色だけでなく**数字が出ている**こと。
  expect(screen.getByText('セッション')).toBeTruthy()
  expect(screen.getByText('100%')).toBeTruthy()
  expect(screen.getByText('拘束中')).toBeTruthy()
  expect(screen.getByText('週（全体）')).toBeTruthy()
  expect(screen.getByText('48%')).toBeTruthy()
  // トークンの内訳
  expect(screen.getByText('claude-opus-5')).toBeTruthy()
  expect(screen.getByText('26k')).toBeTruthy() // キャッシュ読み
  // コンテキスト
  expect(screen.getByText(/22k \/ 1.0M（2%）/)).toBeTruthy()
  expect(screen.getByText('System prompt')).toBeTruthy()
})

// 取れなかったときに、空とは書かない。
test('残量が取れないと、その理由が出る', async () => {
  stub((u) => {
    if (u.includes('/usage')) {
      return { running: 0, max: 4, usage_error: '子が答えない', context_error: '子が答えない' }
    }
    if (u === '/api/runtime/abc') return { id: 'abc', state: 'idle', cwd: '/w', requested_by: 'u', created_at: '', updated_at: '' }
    if (u.startsWith('/api/runtime/')) return []
    return { agent_connected: true, sessions: [] }
  })
  render(
    <MemoryRouter initialEntries={['/runtime/abc?tab=usage&live=0']}>
      <Routes><Route path="/runtime/:id" element={<RuntimeDetail />} /></Routes>
    </MemoryRouter>)
  await waitFor(() => expect(screen.getAllByText(/子が答えない/).length).toBe(2))
})

// **終わったセッションでは流さない。**
//
// 何も来ないのに枠（同時16本）を1つ握り続けるだけになる。
// `?live=0` でも同じ。代わりに1度だけ読んで並べる。
test('終わったセッションでは EventSource を開かない', async () => {
  let opened = 0
  vi.stubGlobal('EventSource', class {
    constructor() { opened++ }
    close() {}
    addEventListener() {}
  } as unknown as typeof EventSource)
  vi.stubGlobal('fetch', async (url: string) => ({
    ok: true, status: 200,
    json: async () => {
      if (url.includes('/log')) return { lines: [{ seq: 1, at: '2026-09-06T00:00:00Z', kind: 'result' }], gap: false, newest: 1, dropped: 0 }
      if (url.startsWith('/api/runtime/') && url !== '/api/runtime/abc') return []
      if (url === '/api/runtime/abc') return { id: 'abc', state: 'exited', cwd: '/w', requested_by: 'u', created_at: '', updated_at: '' }
      return { agent_connected: true, sessions: [] }
    },
    text: async () => '',
  } as unknown as Response))

  render(
    <MemoryRouter initialEntries={['/runtime/abc']}>
      <Routes><Route path="/runtime/:id" element={<RuntimeDetail />} /></Routes>
    </MemoryRouter>)

  await waitFor(() => expect(screen.getByText(/終わったので流していない/)).toBeTruthy())
  expect(opened).toBe(0)
  // 流さなくても、落ちているぶんは読める。
  await waitFor(() => expect(screen.getByText('result')).toBeTruthy())
})

test('live=0 なら走っていても流さない', async () => {
  let opened = 0
  vi.stubGlobal('EventSource', class {
    constructor() { opened++ }
    close() {}
    addEventListener() {}
  } as unknown as typeof EventSource)
  vi.stubGlobal('fetch', async (url: string) => ({
    ok: true, status: 200,
    json: async () => {
      if (url.includes('/log')) return { lines: [], gap: false, newest: 0, dropped: 0 }
      if (url.startsWith('/api/runtime/') && url !== '/api/runtime/abc') return []
      if (url === '/api/runtime/abc') return { id: 'abc', state: 'idle', cwd: '/w', requested_by: 'u', created_at: '', updated_at: '' }
      return { agent_connected: true, sessions: [] }
    },
    text: async () => '',
  } as unknown as Response))

  render(
    <MemoryRouter initialEntries={['/runtime/abc?live=0']}>
      <Routes><Route path="/runtime/:id" element={<RuntimeDetail />} /></Routes>
    </MemoryRouter>)
  await waitFor(() => expect(screen.getByText(/流していない（live=0）/)).toBeTruthy())
  expect(opened).toBe(0)
})

// **Camp 自身の問い合わせは会話ではない。** 畳むが、畳んだことは言う。
test('control_response は流れから畳み、件数を出す', async () => {
  vi.stubGlobal('EventSource', class { close() {} addEventListener() {} } as unknown as typeof EventSource)
  vi.stubGlobal('fetch', async (url: string) => ({
    ok: true, status: 200,
    json: async () => {
      if (url.includes('/log')) {
        return {
          lines: [
            { seq: 1, at: '2026-09-06T08:37:20Z', kind: 'assistant' },
            { seq: 2, at: '2026-09-06T08:37:37Z', kind: 'control_response' },
            { seq: 3, at: '2026-09-06T08:37:38Z', kind: 'control_response' },
            { seq: 4, at: '2026-09-06T08:37:20Z', kind: 'result' },
          ],
          gap: false, newest: 4, dropped: 0,
        }
      }
      if (url.startsWith('/api/runtime/') && url !== '/api/runtime/abc') return []
      if (url === '/api/runtime/abc') return { id: 'abc', state: 'exited', cwd: '/w', requested_by: 'u', created_at: '', updated_at: '' }
      return { agent_connected: true, sessions: [] }
    },
    text: async () => '',
  } as unknown as Response))

  render(
    <MemoryRouter initialEntries={['/runtime/abc']}>
      <Routes><Route path="/runtime/:id" element={<RuntimeDetail />} /></Routes>
    </MemoryRouter>)

  await waitFor(() => expect(screen.getByText('result')).toBeTruthy())
  expect(screen.queryByText('control_response')).toBeNull()
  expect(screen.getByText(/Camp 自身の問い合わせ 2 件は畳んでいる/)).toBeTruthy()
})

// **上へ遡っている最中は引き戻さない。**
//
// 2026-09-07、承認の枠を見ようとしても 0.5 秒おきに最下へ飛ばされて、
// 触ることも読むこともできなかった。追うのは「末尾に居るとき」だけにする。
test('上へ遡ったら、新しい行が来ても引き戻さない', async () => {
  let seq = 0
  const listeners: ((e: MessageEvent) => void)[] = []
  vi.stubGlobal('EventSource', class {
    set onmessage(f: (e: MessageEvent) => void) { listeners.push(f) }
    close() {}
    addEventListener() {}
  } as unknown as typeof EventSource)
  vi.stubGlobal('fetch', async (url: string) => ({
    ok: true, status: 200,
    json: async () => {
      if (url.includes('/log')) return { lines: [], gap: false, newest: 0, dropped: 0 }
      if (url.startsWith('/api/runtime/') && url !== '/api/runtime/abc') return []
      if (url === '/api/runtime/abc') return { id: 'abc', state: 'idle', cwd: '/w', requested_by: 'u', created_at: '', updated_at: '' }
      return { agent_connected: true, sessions: [] }
    },
    text: async () => '',
  } as unknown as Response))

  const { container } = render(
    <MemoryRouter initialEntries={['/runtime/abc']}>
      <Routes><Route path="/runtime/:id" element={<RuntimeDetail />} /></Routes>
    </MemoryRouter>)

  await waitFor(() => expect(listeners.length).toBeGreaterThan(0))
  const box = container.querySelector('.stream') as HTMLDivElement

  // 箱の大きさを装う（jsdom はレイアウトしない）。
  Object.defineProperty(box, 'scrollHeight', { value: 1000, configurable: true })
  Object.defineProperty(box, 'clientHeight', { value: 200, configurable: true })

  const push = () => act(() => {
    seq++
    listeners[0](new MessageEvent('message', {
      data: JSON.stringify({ seq, at: '2026-09-07T00:00:00Z', kind: 'assistant' }),
    }))
  })

  // 末尾に居るあいだは追う。
  push()
  await waitFor(() => expect(box.scrollTop).toBe(1000))

  // 上へ遡る。
  box.scrollTop = 0
  act(() => { box.dispatchEvent(new Event('scroll', { bubbles: true })) })

  // 新しい行が来ても、引き戻さない。
  push()
  await waitFor(() => expect(screen.getAllByText('assistant').length).toBe(2))
  expect(box.scrollTop).toBe(0)

  // 末尾へ戻せば、また追う。
  box.scrollTop = 800
  act(() => { box.dispatchEvent(new Event('scroll', { bubbles: true })) })
  push()
  await waitFor(() => expect(box.scrollTop).toBe(1000))
})

// **繋ぎ直しても、同じ行が積み上がらない。**
//
// 2026-09-07、画面が震えるたびに流れを繋ぎ直し、そのつど先頭から流し直されて
// 同じ行が何度も並んだ。どこまで受け取ったかを覚えて、そこから続ける。
test('繋ぎ直しは、受け取った続きから', async () => {
  const urls: string[] = []
  const listeners: ((e: MessageEvent) => void)[] = []
  vi.stubGlobal('EventSource', class {
    constructor(u: string) { urls.push(u) }
    set onmessage(f: (e: MessageEvent) => void) { listeners.push(f) }
    close() {}
    addEventListener() {}
  } as unknown as typeof EventSource)
  vi.stubGlobal('fetch', async (url: string) => ({
    ok: true, status: 200,
    json: async () => {
      if (url.includes('/log')) return { lines: [], gap: false, newest: 0, dropped: 0 }
      if (url.startsWith('/api/runtime/') && url !== '/api/runtime/abc') return []
      if (url === '/api/runtime/abc') return { id: 'abc', state: 'idle', cwd: '/w', requested_by: 'u', created_at: '', updated_at: '' }
      return { agent_connected: true, sessions: [] }
    },
    text: async () => '',
  } as unknown as Response))

  render(
    <MemoryRouter initialEntries={['/runtime/abc']}>
      <Routes><Route path="/runtime/:id" element={<RuntimeDetail />} /></Routes>
    </MemoryRouter>)
  await waitFor(() => expect(listeners.length).toBeGreaterThan(0))

  // 最初は先頭から。
  expect(urls[0]).toContain('since=0')

  const push = (seq: number) => act(() => {
    listeners[listeners.length - 1](new MessageEvent('message', {
      data: JSON.stringify({ seq, at: '2026-09-07T00:00:00Z', kind: 'assistant' }),
    }))
  })
  push(1); push(2)
  await waitFor(() => expect(screen.getAllByText('assistant').length).toBe(2))

  // 同じ番号がもう一度来ても、増えない。
  push(1); push(2)
  expect(screen.getAllByText('assistant').length).toBe(2)
  push(3)
  await waitFor(() => expect(screen.getAllByText('assistant').length).toBe(3))

  // **繋ぎ直したら、続きから。** 先頭からやり直すと、同じ行が積み上がる。
  const before = urls.length
  act(() => { (screen.getByLabelText('流す') as HTMLInputElement).click() })  // 止める
  await waitFor(() => expect(screen.getByText(/流していない（live=0）/)).toBeTruthy())
  act(() => { (screen.getByLabelText('流す') as HTMLInputElement).click() })  // 繋ぎ直す
  await waitFor(() => expect(urls.length).toBeGreaterThan(before))
  expect(urls[urls.length - 1]).toContain('since=3')
})
