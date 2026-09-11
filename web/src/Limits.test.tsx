import { afterEach, expect, test, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import Limits from './Limits'

function stub(rows: unknown) {
  vi.stubGlobal('fetch', async () => ({
    ok: true, status: 200, json: async () => rows,
  } as unknown as Response))
}

afterEach(() => { vi.unstubAllGlobals(); vi.unstubAllEnvs() })

const base = {
  id: 1, agent: 'claude-code', kind: 'five_hour', source: 'statusline',
  started_at: '2026-09-02T09:00:00Z', used_pct: 21, peak_pct: 21,
  samples: 7, fetched_at: '2026-09-02T09:45:00Z', current: true,
}

// 記録がまだ無い状態を「0%」と描いてはいけない。残量ゼロと読めてしまう。
test('記録が無いときは値ではなく理由を出す', async () => {
  stub([])
  render(<Limits />)
  await waitFor(() => expect(screen.getByText(/記録がまだない/)).toBeTruthy())
  expect(screen.queryByText('0%')).toBeNull()
})

// 値そのものより「いつ観測した値か」が要る。TUI を開いていない間は更新されない。
// 観測時刻は手元の時刻で出す（DB は UTC。2026-09-11 まで UTC のまま出ていた）。
test('観測時刻を必ず添える', async () => {
  vi.stubEnv('TZ', 'Asia/Tokyo')
  stub([{ ...base, ends_at: new Date(Date.now() + 2 * 3600_000).toISOString() }])
  render(<Limits />)
  await waitFor(() => expect(screen.getByText('21%')).toBeTruthy())
  expect(screen.getByText(/2026-09-02 18:45 時点/)).toBeTruthy()
  expect(screen.getByText(/あと 1時間5[0-9]分/)).toBeTruthy()
})

// 観測が飛んで値が下がって見えたときだけピークを添える。
test('ピークが最新を上回るときだけ出す', async () => {
  stub([{ ...base, used_pct: 12, peak_pct: 99, ends_at: new Date(Date.now() + 3600_000).toISOString() }])
  const { unmount } = render(<Limits />)
  await waitFor(() => expect(screen.getByText(/ピーク 99%/)).toBeTruthy())
  unmount()

  stub([{ ...base, used_pct: 21, peak_pct: 21, ends_at: new Date(Date.now() + 3600_000).toISOString() }])
  render(<Limits />)
  await waitFor(() => expect(screen.getByText('21%')).toBeTruthy())
  expect(screen.queryByText(/ピーク/)).toBeNull()
})
