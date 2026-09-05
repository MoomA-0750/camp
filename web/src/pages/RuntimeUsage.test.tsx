import { afterEach, expect, test, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
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
    if (u.startsWith('/api/runtime/')) return []
    return { agent_connected: true, sessions: [] }
  })
  render(
    <MemoryRouter initialEntries={['/runtime/abc?tab=usage']}>
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
    if (u.startsWith('/api/runtime/')) return []
    return { agent_connected: true, sessions: [] }
  })
  render(
    <MemoryRouter initialEntries={['/runtime/abc?tab=usage']}>
      <Routes><Route path="/runtime/:id" element={<RuntimeDetail />} /></Routes>
    </MemoryRouter>)
  await waitFor(() => expect(screen.getAllByText(/子が答えない/).length).toBe(2))
})
