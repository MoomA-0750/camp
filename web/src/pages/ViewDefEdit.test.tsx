import { afterEach, expect, test, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import ViewDefEdit from './ViewDefEdit'

// ビュー定義を直す画面（2026-09-13）。
//
// **状態を持つ画面なので、押した結果まで見る。** 特に「入れるだけで保存しない」は、
// 間違えるといま書いているものが消えるので、POST が飛ばないことまで縛る。

type Res = { status?: number; body?: unknown }
type Call = { url: string; method: string; body?: string }

// 既存の stub は常に 200 を返すので、404 を作れるものをここに置く。
function stub(handler: (url: string, init?: RequestInit) => Res) {
  const calls: Call[] = []
  vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
    calls.push({
      url, method: (init?.method ?? 'GET').toUpperCase(),
      body: typeof init?.body === 'string' ? init.body : undefined,
    })
    const r = handler(url, init)
    const status = r.status ?? 200
    return {
      ok: status >= 200 && status < 300, status, statusText: 'x',
      json: async () => r.body ?? {},
      text: async () => JSON.stringify(r.body ?? {}),
    } as unknown as Response
  })
  return calls
}
afterEach(() => vi.unstubAllGlobals())

const body1 = 'base: A\nviews:\n  - name: t\n    kind: table\n'

function show(base = 'A') {
  render(
    <MemoryRouter initialEntries={[`/viewdefs/${base}`]}>
      <Routes><Route path="/viewdefs/:base" element={<ViewDefEdit />} /></Routes>
    </MemoryRouter>)
}

// 定義が無ければ、作り方を教える（黙って空の編集欄を出さない）。
test('まだ変換していない台紙では -convert の案内が出る', async () => {
  stub((u) => (u.includes('/def') ? { status: 404 } : { body: [] }))
  show()
  await waitFor(() =>
    expect(screen.getByText(/campd views -convert/)).toBeTruthy())
  // 編集欄は出さない（保存しようがない）。
  expect(screen.queryByLabelText('ビュー定義の YAML')).toBeNull()
})

test('変えるまで保存は押せない', async () => {
  stub((u) => (u.includes('/def')
    ? { body: { base: 'A', body: body1, updated_at: '2026-09-13T00:00:00Z' } }
    : { body: [] }))
  show()
  const box = await waitFor(() => screen.getByLabelText('ビュー定義の YAML'))
  const save = screen.getByRole('button', { name: '保存する' })
  expect(save).toHaveProperty('disabled', true)

  fireEvent.change(box, { target: { value: body1 + '# 直した\n' } })
  await waitFor(() => expect(save).toHaveProperty('disabled', false))

  // 元に戻せば、また押せなくなる。
  fireEvent.click(screen.getByRole('button', { name: '直した分を捨てる' }))
  await waitFor(() => expect(save).toHaveProperty('disabled', true))
})

// **押した瞬間に上書きしない。** 入れるだけ。保存は別の操作。
test('履歴の「編集欄に入れる」は入れるだけで保存しない', async () => {
  const old = 'base: A\nviews:\n  - name: 古い\n    kind: table\n'
  const calls = stub((u) => {
    if (u.includes('/def')) {
      return { body: { base: 'A', body: body1, updated_at: '2026-09-13T00:00:00Z' } }
    }
    return { body: [{ at: '2026-09-12T00:00:00Z', by: 'convert', body: old }] }
  })
  show()
  const box = await waitFor(() => screen.getByLabelText('ビュー定義の YAML'))
  fireEvent.click(await waitFor(() => screen.getByRole('button', { name: '編集欄に入れる' })))

  await waitFor(() => expect((box as HTMLTextAreaElement).value).toBe(old))
  expect(calls.some((c) => c.method === 'POST')).toBe(false)
})

test('保存すると直した本文が POST される', async () => {
  let current = body1
  const calls = stub((u, init) => {
    if (u.includes('/def') && (init?.method ?? 'GET').toUpperCase() === 'POST') {
      current = JSON.parse(String(init?.body)).def as string
      return { body: { ok: true } }
    }
    if (u.includes('/def')) {
      return { body: { base: 'A', body: current, body_sha256: 'sha-of-' + current.length, updated_at: '2026-09-13T00:00:00Z' } }
    }
    return { body: [] }
  })
  show()
  const box = await waitFor(() => screen.getByLabelText('ビュー定義の YAML'))
  const next = body1 + '# 直した\n'
  fireEvent.change(box, { target: { value: next } })
  fireEvent.click(screen.getByRole('button', { name: '保存する' }))

  await waitFor(() => expect(screen.getByText('保存した。')).toBeTruthy())
  const post = calls.find((c) => c.method === 'POST')
  if (!post) throw new Error('POST が飛んでいない')
  expect(post.url).toContain('/api/views/A/def')
  expect(JSON.parse(String(post.body)).def).toBe(next)
  // **読んだ版の指紋を添える**（読んだ版の上にしか書かない。2026-09-13 の実装後レビュー）。
  expect(JSON.parse(String(post.body)).base_sha256).toBe('sha-of-' + body1.length)
})

// 間にほかが書いていて断られたら、書きかけを残したまま読み直せる。
test('競合で断られても書きかけは消えず、読み直してから保存できる', async () => {
  let server = body1
  let posts = 0
  const calls = stub((u, init) => {
    if (u.includes('/def') && (init?.method ?? 'GET').toUpperCase() === 'POST') {
      posts++
      const req = JSON.parse(String(init?.body))
      if (req.base_sha256 !== 'v-' + server) {
        return { status: 409, body: { error: '読んだあとで、ほかが定義を書き換えた（読み直してから書く）' } }
      }
      server = req.def
      return { body: { ok: true } }
    }
    if (u.includes('/def')) return { body: { base: 'A', body: server, body_sha256: 'v-' + server, updated_at: 'x' } }
    return { body: [] }
  })
  show()
  const box = await waitFor(() => screen.getByLabelText('ビュー定義の YAML'))
  server = body1 + '# 別のタブが書いた\n' // 読んだあとで、ほかが書いた
  const mine = body1 + '# 私の書きかけ\n'
  fireEvent.change(box, { target: { value: mine } })
  fireEvent.click(screen.getByRole('button', { name: '保存する' }))
  await waitFor(() => expect(screen.getByText(/ほかが定義を書き換えた/)).toBeTruthy())
  expect((screen.getByLabelText('ビュー定義の YAML') as HTMLTextAreaElement).value).toBe(mine)

  fireEvent.click(screen.getByRole('button', { name: /いまの版を読み直す/ }))
  await waitFor(() => expect(calls.filter((c) => c.method === 'GET' && c.url.includes('/def')).length).toBeGreaterThan(1))
  expect((screen.getByLabelText('ビュー定義の YAML') as HTMLTextAreaElement).value).toBe(mine)
  await waitFor(() => expect(screen.getByRole('button', { name: '保存する' }).hasAttribute('disabled')).toBe(false))
  fireEvent.click(screen.getByRole('button', { name: '保存する' }))
  await waitFor(() => expect(server).toBe(mine))
  expect(posts).toBe(2)
})

// 変換のあとで直していると、`-convert` が上書きしないことを知らせる。
//
// **サーバーの判定（hand_edited）に従い、時刻では決めない。** 変換と保存が同じ秒に並ぶと
// 時刻では見分けられず、画面の注意と `-convert` の実際の振る舞いが食い違う（2026-09-13）。
test('手編集の注意はサーバーの判定に従う（同じ秒でも出る）', async () => {
  stub((u) => (u.includes('/def')
    ? {
      body: {
        base: 'A', body: body1, hand_edited: true,
        converted_at: '2026-09-13T00:00:00Z', updated_at: '2026-09-13T00:00:00Z',
      },
    }
    : { body: [] }))
  show()
  await waitFor(() => expect(screen.getByText(/上書きしない/)).toBeTruthy())
})

test('サーバーが手編集でないと言えば、時刻が新しくても注意は出ない', async () => {
  stub((u) => (u.includes('/def')
    ? {
      body: {
        base: 'A', body: body1, hand_edited: false,
        converted_at: '2026-09-13T00:00:00Z', updated_at: '2026-09-13T01:00:00Z',
      },
    }
    : { body: [] }))
  show()
  await waitFor(() => screen.getByLabelText('ビュー定義の YAML'))
  expect(screen.queryByText(/上書きしない/)).toBeNull()
})
