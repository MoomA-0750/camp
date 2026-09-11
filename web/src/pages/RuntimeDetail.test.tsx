import { expect, test } from 'vitest'
import { codexDetail, summarizeCodex } from './RuntimeDetail'

// Codex（JSON-RPC）の流れを1行に畳む。途中経過は来ない（実行面が断っている）。
test('Codex の発言・入力・コマンド・ターンの終わりを1行にする', () => {
  expect(summarizeCodex({ method: 'item/completed',
    params: { item: { type: 'agentMessage', text: 'fake-ok' } } })).toBe('fake-ok')
  expect(summarizeCodex({ method: 'item/completed',
    params: { item: { type: 'userMessage', content: [{ type: 'text', text: 'hello' }] } } }))
    .toBe('hello')
  expect(summarizeCodex({ method: 'item/completed',
    params: { item: { type: 'commandExecution', status: 'declined', command: "zsh -lc 'rm x'" } } }))
    .toBe("[declined] zsh -lc 'rm x'")
  expect(summarizeCodex({ method: 'turn/completed',
    params: { turn: { status: 'interrupted' } } })).toBe('ターンが終わった（interrupted）')
  expect(summarizeCodex({ method: 'item/completed',
    params: { item: { type: 'mcpToolCall', server: 'cua_repl', tool: 'js', status: 'completed' } } }))
    .toBe('[MCP cua_repl/js] completed')
  expect(summarizeCodex({ id: 'camp-3', error: { message: '枠切れ' } })).toBe('エラー: 枠切れ')
})

// **承認の差分は切り詰めない。** 途中までの中身で許させない
// （画面へ渡せない大きさのものは、実行面が見せずに断っている）。
test('Codex の承認はコマンドと場所、またはファイルの差分を全部出す', () => {
  expect(codexDetail({ method: 'item/commandExecution/requestApproval',
    params: { command: "zsh -lc 'touch a'", cwd: '/w' } })).toBe("$ zsh -lc 'touch a'\n場所: /w")
  const diff = 'x'.repeat(5000)
  const out = codexDetail({ method: 'item/fileChange/requestApproval', params: {},
    changes: [{ path: '/w/a.txt', kind: { type: 'add' }, diff }] })
  expect(out).toContain('--- add: /w/a.txt')
  expect(out).toContain(diff)
})
