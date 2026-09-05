import { afterEach, expect, test, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
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
  await waitFor(() => expect(screen.getByText(/まだ1本も起こしていない/)).toBeTruthy())
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
