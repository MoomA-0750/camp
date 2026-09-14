import { expect, test } from 'vitest'
import type { NoteSaveReply } from '../api'
import { comparing, dirty, init, leaving, step, type Effect, type Event, type State } from './autosave'

// 自動保存の状態機械（Phase 5 / M54、Fable の設計レビュー 1・4・11）。

function run(s: State, ...es: Event[]): { s: State; fx: Effect[] } {
  let fx: Effect[] = []
  for (const e of es) {
    const r = step(s, e)
    s = r.s
    fx = fx.concat(r.fx)
  }
  return { s, fx }
}
const ok = (res: Partial<NoteSaveReply & { ok: true }>['res']): NoteSaveReply => ({ ok: true, res: res! })
const now = 1000

test('保存できたら土台を返ったハッシュにし、送っている間の続きは次に送る', () => {
  let { s, fx } = run(init('a', 'A', false), { type: 'edit', text: 'ab' }, { type: 'due', now })
  expect(fx).toEqual([{ type: 'save', body: 'ab', base: 'A' }])
  ;({ s, fx } = run(s, { type: 'edit', text: 'abc' }, { type: 'due', now })) // 送っている間は送らない
  expect(fx).toEqual([])
  ;({ s } = run(s, { type: 'reply', reply: ok({ status: 'saved', sha256: 'AB', sent_sha256: 'AB' }), now }))
  expect(s.base).toBe('AB')
  ;({ s, fx } = run(s, { type: 'due', now }))
  expect(fx).toEqual([{ type: 'save', body: 'abc', base: 'AB' }])
})

test('merged: 送ったあと書いていなければ合わせた本文に置き換える', () => {
  let { s } = run(init('a', 'A', false), { type: 'edit', text: 'ab' }, { type: 'due', now })
  let fx: Effect[]
  ;({ s, fx } = run(s, { type: 'reply', reply: ok({ status: 'merged', sha256: 'M', sent_sha256: 'AB', body: 'X\nab' }), now }))
  expect(fx).toEqual([{ type: 'replace', text: 'X\nab' }])
  expect(s.text).toBe('X\nab')
  expect(s.base).toBe('M')
  ;({ fx } = run(s, { type: 'due', now }))
  expect(fx).toEqual([]) // 置き換えただけでは保存しない
})

test('merged: 送ったあとも書いていたら、土台は送った版（合わせた版ではない）', () => {
  let { s } = run(init('a', 'A', false), { type: 'edit', text: 'ab' }, { type: 'due', now }, { type: 'edit', text: 'abc' })
  let fx: Effect[]
  ;({ s, fx } = run(s, { type: 'reply', reply: ok({ status: 'merged', sha256: 'M', sent_sha256: 'AB', body: 'X\nab' }), now }))
  expect(fx).toEqual([])
  expect(s.text).toBe('abc')
  expect(s.base).toBe('AB')
  ;({ fx } = run(s, { type: 'due', now }))
  expect(fx).toEqual([{ type: 'save', body: 'abc', base: 'AB' }])
})

test('IME の変換中は保存せず、届いた返事は変換が終わってから当てる', () => {
  let { s, fx } = run(init('a', 'A', false), { type: 'edit', text: 'ab' }, { type: 'compose', on: true }, { type: 'due', now })
  expect(fx).toEqual([])
  ;({ s } = run(s, { type: 'compose', on: false }, { type: 'due', now }))
  ;({ s, fx } = run(s, { type: 'compose', on: true },
    { type: 'reply', reply: ok({ status: 'merged', sha256: 'M', sent_sha256: 'AB', body: 'X\nab' }), now }))
  expect(fx).toEqual([])
  expect(s.text).toBe('ab')
  ;({ s, fx } = run(s, { type: 'compose', on: false }))
  expect(fx).toEqual([{ type: 'replace', text: 'X\nab' }])
})

test('conflict で止まり、ディスクの版を読むときは押した時点の自分の版を控える', () => {
  let { s } = run(init('a', 'A', false), { type: 'edit', text: 'mine' }, { type: 'due', now })
  let fx: Effect[]
  ;({ s } = run(s, { type: 'reply', reply: ok({ status: 'conflict', sha256: '', sent_sha256: 'MI', disk: 'theirs', disk_sha256: 'T' }), now }))
  expect(s.mode).toBe('conflict')
  ;({ s, fx } = run(s, { type: 'edit', text: 'mine2' }, { type: 'due', now }))
  expect(fx).toEqual([]) // 止まっている
  ;({ s, fx } = run(s, { type: 'loadDisk' }))
  expect(fx).toEqual([{ type: 'keep', text: 'mine2', why: expect.any(String) }, { type: 'replace', text: 'theirs' }])
  expect(s.base).toBe('T')
  expect(s.mode).toBe('idle')
})

test('conflict で自分の版を選ぶと、土台をディスクの版にして書く', () => {
  let { s } = run(init('a', 'A', false), { type: 'edit', text: 'mine' }, { type: 'due', now },
    { type: 'reply', reply: ok({ status: 'conflict', sha256: '', sent_sha256: 'MI', disk: 'theirs', disk_sha256: 'T' }), now })
  let fx: Effect[]
  ;({ s, fx } = run(s, { type: 'keepMine' }, { type: 'due', now }))
  expect(fx).toEqual([{ type: 'save', body: 'mine', base: 'T' }])
})

test('403 は止まって叩き続けない。503・網の失敗は 30 秒後にやり直す', () => {
  let { s, fx } = run(init('a', 'A', false), { type: 'edit', text: 'b' }, { type: 'due', now },
    { type: 'reply', reply: { ok: false, code: 403, error: '直せない' }, now })
  expect(s.mode).toBe('blocked')
  ;({ fx } = run(s, { type: 'due', now: now + 60_000 }))
  expect(fx).toEqual([])

  ;({ s } = run(init('a', 'A', false), { type: 'edit', text: 'b' }, { type: 'due', now },
    { type: 'reply', reply: { ok: false, code: 503, error: '実行面が居ない' }, now }))
  expect(s.mode).toBe('retry')
  expect(s.text).toBe('b')
  ;({ fx } = run(s, { type: 'due', now: now + 1000 }))
  expect(fx).toEqual([])
  ;({ fx } = run(s, { type: 'due', now: now + 30_000 }))
  expect(fx).toEqual([{ type: 'save', body: 'b', base: 'A' }])
  ;({ s } = run(init('a', 'A', false), { type: 'edit', text: 'b' }, { type: 'due', now },
    { type: 'reply', reply: { ok: false, code: 0, error: '届かなかった' }, now }))
  expect(s.mode).toBe('retry')
})

test('指示の紙は自動保存しない', () => {
  let { s, fx } = run(init('a', 'A', true), { type: 'edit', text: 'b' }, { type: 'due', now })
  expect(fx).toEqual([])
  ;({ s, fx } = run(s, { type: 'manualSave' }))
  expect(fx).toEqual([{ type: 'save', body: 'b', base: 'A' }])
  ;({ s } = run(s, { type: 'reply', reply: { ok: false, code: 401, error: 'パスワードが違う' }, now }))
  expect(s.mode).toBe('manual')
  expect(s.error).toBe('パスワードが違う')
})

test('孤立サロゲートは置き換えてから送る', () => {
  const { fx } = run(init('a', 'A', false), { type: 'edit', text: 'x\uD800y' }, { type: 'due', now })
  expect(fx).toEqual([{ type: 'replace', text: 'x�y' }, { type: 'save', body: 'x�y', base: 'A' }])
})

test('ログインが切れたら止めずにやり直す（指示の紙の 401 はパスワードの入れ違い）', () => {
  const { s } = run(init('a', 'A', false), { type: 'edit', text: 'b' }, { type: 'due', now },
    { type: 'reply', reply: { ok: false, code: 401, error: '未認証' }, now })
  expect(s.mode).toBe('retry')
  expect(run(s, { type: 'due', now: now + 30_000 }).fx).toEqual([{ type: 'save', body: 'b', base: 'A' }])
})

test('ディスクが土台のままなら「ほかで変わった」と言わない', () => {
  const s0 = init('a', 'A', false)
  expect(run(s0, { type: 'diskChanged', sha: 'A' }).s.notice).toBeUndefined()
  expect(run(s0, { type: 'diskChanged', sha: 'B' }).s.notice).toMatch(/ほかで/)
})

test('Camp の版で開くと、選ぶまで保存しない。上書きを選んだら今のディスクを土台に書く（outer gate）', () => {
  let s = comparing('Camp の版\n', 'エージェントの版\n', 'disk', false, 'Camp の控え')
  expect(s.mode).toBe('conflict')
  let r = step(s, { type: 'due', now: 1e12 })
  expect(r.fx).toEqual([])
  r = step(r.s, { type: 'keepMine' })
  r = step(r.s, { type: 'due', now: 1e12 })
  expect(r.fx).toEqual([{ type: 'save', body: 'Camp の版\n', base: 'disk' }])
  s = comparing('Camp の版\n', 'エージェントの版\n', 'disk', false, 'Camp の控え')
  r = step(s, { type: 'loadDisk' })
  expect(r.s.text).toBe('エージェントの版\n')
  expect(dirty(r.s)).toBe(false)
})

test('画面の中で移るとき: 保存の途中に書き足した分・変換の途中・指示の紙も控えに残す（outer gate）', () => {
  const s0 = init('A', 'b0', false)
  expect(leaving(s0)).toEqual({ keep: null, send: 'none' })
  let r = step(s0, { type: 'edit', text: 'A1' })
  expect(leaving(r.s)).toEqual({ keep: 'A1', send: 'now' })
  r = step(r.s, { type: 'due', now: 0 }) // A1 を送っている
  expect(leaving(r.s)).toEqual({ keep: 'A1', send: 'none' })
  r = step(r.s, { type: 'edit', text: 'A1B' }) // 返事を待つ間に書き足した
  expect(leaving(r.s)).toEqual({ keep: 'A1B', send: 'afterReply' })
  const c = step(step(init('A', 'b0', false), { type: 'compose', on: true }).s, { type: 'edit', text: 'Aか' }).s
  expect(leaving(c)).toEqual({ keep: 'Aか', send: 'none' })
  const m = step(init('指示', 'b0', true), { type: 'edit', text: '指示2' }).s
  expect(leaving(m)).toEqual({ keep: '指示2', send: 'none' })
})
