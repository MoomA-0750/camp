import { afterEach, expect, test, vi } from 'vitest'
import { clock, short } from './ui'

// **UTC のまま切って見せない。** 2026-09-11、17:15 に終わったセッションが
// 08:15 と出ていた。承認の期限も同じだけずれていた。
//
// 環境の時差で結果が変わらないよう、時差をここで決める（環境のおかげで
// 通るテストにしない。M31 の TestPermissionDeniedExplainsItself の教訓）。
afterEach(() => vi.unstubAllEnvs())

test('時差の付いた時刻は手元の時刻で出す', () => {
  vi.stubEnv('TZ', 'Asia/Tokyo')
  expect(short('2026-09-11T08:15:00Z')).toBe('2026-09-11 17:15')
  expect(short('2026-09-11T20:15:00Z')).toBe('2026-09-12 05:15') // 日付も跨ぐ
  expect(short('2026-09-11T17:15:00+09:00')).toBe('2026-09-11 17:15')
  expect(clock('2026-09-11T08:15:38Z')).toBe('17:15:38')
})

test('時差の無いものは、そのまま', () => {
  vi.stubEnv('TZ', 'Asia/Tokyo')
  expect(short('2026-09-11 08:15:00')).toBe('2026-09-11 08:15')
  expect(short('2026-09-11')).toBe('2026-09-11')
  expect(short('')).toBe('')
  expect(short(undefined)).toBe('')
})
