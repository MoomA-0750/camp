import { expect, test } from 'vitest'
import runtime from './Runtime.tsx?raw'
import detail from './RuntimeDetail.tsx?raw'
import apiSrc from '../api.ts?raw'

// **画面はエージェントの名前を決め打ちしない**（D-031）。選択肢・表示名・振る舞いの説明・
// 残量や承認の形・流れの一言は API から来る。エージェントを足すときに画面へ手を入れなくて済むように。
// 名前を書いてよいのはコメントの中だけ。
function code(src: string): string {
  return src.split('\n')
    .filter((l) => { const t = l.trim(); return !t.startsWith('//') && !t.startsWith('*') && !t.startsWith('/*') && !t.startsWith('{/*') })
    .join('\n')
}

test('画面のコードにエージェントの名前が無い', () => {
  for (const [name, src] of [['Runtime.tsx', runtime], ['RuntimeDetail.tsx', detail], ['api.ts', apiSrc]]) {
    expect(code(src), name).not.toMatch(/codex/i)
    expect(code(src), name).not.toMatch(/['"`]claude['"`]|Claude Code/)
  }
})
