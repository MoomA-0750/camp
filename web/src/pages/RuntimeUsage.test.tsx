import { afterEach, expect, test, vi } from 'vitest'
import { act, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import RuntimeDetail from './RuntimeDetail'

// 残量は共通の形（view）で来る。駆動器が、本物の `claude`（2026-09-04）・`codex app-server`
// （2026-09-11）から返ってきた形をこれに直す（直し方は internal/session/usage_test.go が縛る）。
// **画面はエージェントを見ない。**
const claudeView = {
  plan: 'pro',
  windows: [
    { label: 'セッション', percent: 100, active: true, resets_at: '2126-09-04T18:30:00Z' },
    { label: '週（全体）', percent: 48, resets_at: '2126-09-10T00:00:00Z' },
  ],
  context: { used: 21874, max: 1000000 },
  tables: [
    { title: 'トークンの内訳（このセッション）', empty: 'まだ1度もモデルを呼んでいない。',
      columns: [{ label: '入力', unit: 'tokens' }, { label: '出力', unit: 'tokens' },
        { label: 'キャッシュ読み', unit: 'tokens' }, { label: 'キャッシュ作成', unit: 'tokens' },
        { label: '思考', unit: 'tokens' }, { label: '費用', unit: 'usd' }],
      rows: [{ label: 'claude-opus-5', cells: [4, 226, 25859, 12264, 0, 0.1412] },
        { label: '合計', total: true, cells: [4, 226, 25859, 12264, 0, 0.1422] }] },
    { title: 'コンテキストの内訳', columns: [{ label: 'トークン', unit: 'tokens' },
      { label: '割合', unit: 'percent' }],
      rows: [{ label: 'System prompt', cells: [3296, 0.33] }, { label: 'Messages', cells: [9408, 0.94] }] },
  ],
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

const idle = { id: 'abc', state: 'idle', cwd: '/w', requested_by: 'u', created_at: '', updated_at: '' }

function showUsage(view: unknown, extra: Record<string, unknown> = {}) {
  stub((u) => {
    if (u.includes('/usage')) return { running: 1, max: 4, view, ...extra }
    if (u === '/api/runtime/abc') return idle
    if (u.startsWith('/api/runtime/')) return []
    return { agent_connected: true, sessions: [] }
  })
  render(
    <MemoryRouter initialEntries={['/runtime/abc?tab=usage&live=0']}>
      <Routes><Route path="/runtime/:id" element={<RuntimeDetail />} /></Routes>
    </MemoryRouter>)
}

// 仕様が求めた4種のうち3種がここに出る（残り1種＝プラン残量の履歴は「使用量」）。
// **生の JSON を貼るだけでは「出た」ことにならない。**
test('残量タブに、枠・トークン内訳・コンテキスト・同時実行が出る', async () => {
  showUsage(claudeView)
  // 同時実行
  await waitFor(() => expect(screen.getByText(/同時に走っているのは 1 \/ 4 本/)).toBeTruthy())
  expect(screen.getByText(/プラン pro/)).toBeTruthy()
  // プラン枠（メーター）。色だけでなく**数字が出ている**こと。
  expect(screen.getByText('セッション')).toBeTruthy()
  expect(screen.getByText('100%')).toBeTruthy()
  expect(screen.getByText('拘束中')).toBeTruthy()
  expect(screen.getByText('週（全体）')).toBeTruthy()
  expect(screen.getByText('48%')).toBeTruthy()
  // トークンの内訳
  expect(screen.getByText('claude-opus-5')).toBeTruthy()
  expect(screen.getAllByText('26k').length).toBeGreaterThan(0) // キャッシュ読み
  expect(screen.getByText('$0.1412')).toBeTruthy()
  // コンテキスト
  expect(screen.getByText(/22k \/ 1.0M（2%）/)).toBeTruthy()
  expect(screen.getByText('System prompt')).toBeTruthy()
})

// 取れなかったときに、空とは書かない。
test('残量が取れないと、その理由が出る', async () => {
  showUsage(undefined, { running: 0, usage_error: '子が答えない', context_error: '子が答えない' })
  await waitFor(() => expect(screen.getAllByText(/子が答えない/).length).toBe(2))
})

test('エージェントが答えの中で返した失敗も出る', async () => {
  showUsage({ windows: [], tables: [], errors: ['枠を読めない'] })
  await waitFor(() => expect(screen.getByText(/枠を読めない/)).toBeTruthy())
  expect(screen.queryByText('枠の情報が来ていない。')).toBeNull()
})

test('Codex の形から直した残量も、同じ画面で出る', async () => {
  const later = new Date(Date.now() + 3 * 3600_000).toISOString()
  showUsage({
    plan: 'plus',
    windows: [{ label: '5時間', percent: 73, resets_at: later }, { label: '週', percent: 78, resets_at: later }],
    context: { used: 16497, max: 258400 },
    tables: [{ title: 'トークン（このスレッド）', columns: [{ label: '入力', unit: 'tokens' },
      { label: 'キャッシュ読み', unit: 'tokens' }, { label: '出力', unit: 'tokens' }, { label: '推論', unit: 'tokens' }],
      rows: [{ label: '合計', total: true, cells: [16378, 11904, 119, 0] }] }],
  })
  await waitFor(() => expect(screen.getByText(/プラン plus/)).toBeTruthy())
  expect(screen.getByText('5時間')).toBeTruthy()
  expect(screen.getByText('73%')).toBeTruthy()
  expect(screen.getByText('週')).toBeTruthy()
  expect(screen.getByText('78%')).toBeTruthy()
  expect(screen.getByText(/16k \/ 258k（6%）/)).toBeTruthy()
  // その答えに無い欄（モデル別の費用）は出ない。
  expect(screen.queryByText('キャッシュ作成')).toBeNull()
})

// **3つ目のエージェントの残量も、画面に手を入れずに出る**（D-031）。
test('知らないエージェントの残量も、共通の形なら出る', async () => {
  showUsage({ windows: [{ label: 'フェイク枠', percent: 42 }],
    tables: [{ title: 'フェイクの内訳', columns: [{ label: '回数', unit: 'tokens' }],
      rows: [{ label: 'fake', cells: [3] }] }] })
  await waitFor(() => expect(screen.getByText('フェイク枠')).toBeTruthy())
  expect(screen.getByText('42%')).toBeTruthy()
  expect(screen.getByText('フェイクの内訳')).toBeTruthy()
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
      if (url.includes('/log')) return { lines: [{ seq: 1, at: '2026-09-06T00:00:00Z', kind: 'result', summary: '終わった' }], gap: false, newest: 1, dropped: 0 }
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
  // 流さなくても、落ちているぶんは読める。一言は駆動器が畳んだもの。
  await waitFor(() => expect(screen.getByText('result')).toBeTruthy())
  expect(screen.getByText('終わった')).toBeTruthy()
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
      if (url === '/api/runtime/abc') return idle
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

// **Camp 自身の問い合わせは会話ではない。** 畳むが、畳んだことは言う。どれがそうかは駆動器が
// 印（own）を付ける——画面はフレームの種類を読まない。
test('Camp 自身の問い合わせは流れから畳み、件数を出す', async () => {
  vi.stubGlobal('EventSource', class { close() {} addEventListener() {} } as unknown as typeof EventSource)
  vi.stubGlobal('fetch', async (url: string) => ({
    ok: true, status: 200,
    json: async () => {
      if (url.includes('/log')) {
        return {
          lines: [
            { seq: 1, at: '2026-09-06T08:37:20Z', kind: 'assistant' },
            { seq: 2, at: '2026-09-06T08:37:37Z', kind: 'control_response', own: true },
            { seq: 3, at: '2026-09-06T08:37:38Z', kind: 'control_response', own: true },
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

// 表示名・固有の振る舞いの説明・エージェント自身のセッション id は、API が埋めたもの。
test('詳細にエージェントの表示名と説明が出る', async () => {
  stub((u) => {
    if (u.includes('/log')) return { lines: [], gap: false, newest: 0, dropped: 0 }
    if (u === '/api/runtime/abc') {
      return { ...idle, agent: 'fake3', agent_label: 'Fake Three', agent_session_id: 'f3-12345678',
        agent_notes: ['中断では何も残らない'], perm: 'edits' }
    }
    if (u.startsWith('/api/runtime/')) return []
    return { agent_connected: true, sessions: [] }
  })
  render(
    <MemoryRouter initialEntries={['/runtime/abc?live=0']}>
      <Routes><Route path="/runtime/:id" element={<RuntimeDetail />} /></Routes>
    </MemoryRouter>)
  await waitFor(() => expect(screen.getByText(/Fake Three/)).toBeTruthy())
  expect(screen.getByText(/確認の度合い 編集は訊かない/)).toBeTruthy()
  expect(screen.getByText('中断では何も残らない')).toBeTruthy()
  expect(screen.getByText('f3-12345')).toBeTruthy()
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
      if (url === '/api/runtime/abc') return idle
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
      if (url === '/api/runtime/abc') return idle
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
