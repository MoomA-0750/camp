import type { Completion, CompletionContext, CompletionResult } from '@codemirror/autocomplete'
import type { EditorView } from '@codemirror/view'
import type { NoteName } from '../api'
import { rank } from './names'

/**
 * wikilink の補完（Phase 5 / M55）。`[[`・`![[` のあと、`]] | # ^` と改行が出るまでを問い合わせにする。
 *
 * **入れる形はサーバーが決めた link**（ベース名が Vault の中で1つならベース名、衝突すればパス。resolve.go で
 * 曖昧にならないことを確かめてある）。link の無い名前（`#` などを含み、リンクに書けない）は出さない。
 */

/** openLink はカーソルの前が開いた wikilink なら、問い合わせの文字と始まりの位置を返す。 */
export function openLink(before: string): { query: string; start: number } | null {
  const m = /\[\[([^[\]|#^\n]*)$/.exec(before)
  if (!m) return null
  return { query: m[1], start: before.length - m[1].length }
}

/** insertion は選んだ link を入れる文字と、入れたあとのカーソルの位置（入れた文字の頭から）。 */
export function insertion(link: string, after: string): { text: string; cursor: number } {
  // すぐ後ろに `]]` があれば足さず、その後ろへ出る。
  if (after.startsWith(']]')) return { text: link, cursor: link.length + 2 }
  return { text: link + ']]', cursor: link.length + 2 }
}

const dirOf = (p: string) => (p.includes('/') ? p.slice(0, p.lastIndexOf('/')) : '')
const label = (p: string) => p.slice(p.lastIndexOf('/') + 1).replace(/\.md$/, '')

export function wikiComplete(names: () => NoteName[]) {
  return (ctx: CompletionContext): CompletionResult | null => {
    const line = ctx.state.doc.lineAt(ctx.pos)
    const open = openLink(line.text.slice(0, ctx.pos - line.from))
    if (!open) return null
    const from = line.from + open.start
    const options: Completion[] = rank(open.query, names(), 50, (n) => !!n.link).map((n) => ({
      label: label(n.path),
      detail: dirOf(n.path),
      apply: (view: EditorView, _c: Completion, a: number, b: number) => {
        const after = view.state.sliceDoc(b, Math.min(b + 2, view.state.doc.length))
        const ins = insertion(n.link!, after)
        view.dispatch({ changes: { from: a, to: b, insert: ins.text }, selection: { anchor: a + ins.cursor } })
      },
    }))
    return { from, to: ctx.pos, options, filter: false }
  }
}
