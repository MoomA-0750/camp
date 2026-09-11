import { expect, test } from 'vitest'
import { askText } from './RuntimeDetail'

// 承認の中身は共通の形（view）だけを見て描く。どのエージェントの承認でも同じ（D-031）。
test('コマンドの承認はコマンドと場所を出し、起こした場所の外なら印を付ける', () => {
  expect(askText(JSON.stringify({ view: { what: 'command', command: "zsh -lc 'touch a'", cwd: '/w' } })))
    .toBe("$ zsh -lc 'touch a'\n場所: /w")
  // 断らずに印を付けて見せる（決めるのは本人。D-030）。
  expect(askText(JSON.stringify({ view: { what: 'command', command: 'ls', cwd: '/etc', outside: true } })))
    .toBe('$ ls\n場所: /etc  ← 起こした場所の外')
})

// **承認の差分は切り詰めない。** 途中までの中身で許させない
// （画面へ渡せない大きさのものは、駆動器が見せずに断っている）。
test('ファイル変更は差分を全部出す', () => {
  const patch = '+' + 'x'.repeat(5000) + '\n'
  const out = askText(JSON.stringify({ view: { what: 'file',
    changes: [{ path: '/w/a.txt', kind: 'add', patch }] } }))
  expect(out).toContain('--- add: /w/a.txt')
  expect(out).toContain(patch)
})

test('その他の道具は入力をそのまま出し、理由も添える', () => {
  const out = askText(JSON.stringify({ view: { what: 'tool', tool: 'WebFetch',
    input: { url: 'https://example.com' }, reason: '調べる' } }))
  expect(out).toContain('example.com')
  expect(out).toContain('理由: 調べる')
})

test('共通の形の無い古い行は、中身をそのまま出す', () => {
  expect(askText(JSON.stringify({ tool_name: 'Bash', input: { command: 'ls' } })))
    .toContain('"command": "ls"')
})
