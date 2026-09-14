import { forwardRef, useEffect, useImperativeHandle, useRef } from 'react'
import { EditorState, Transaction } from '@codemirror/state'
import { EditorView, drawSelection, keymap } from '@codemirror/view'
import { defaultKeymap, history, historyKeymap } from '@codemirror/commands'
import { markdown } from '@codemirror/lang-markdown'
import { autocompletion, type CompletionSource } from '@codemirror/autocomplete'
import { defaultHighlightStyle, syntaxHighlighting } from '@codemirror/language'

/**
 * 書く面（CodeMirror 6。Phase 5 / M54）。
 *
 * CodeMirror は中身を DOM の文字として置くので、本文から HTML が差し込まれる経路は無い。貼り付けも文字だけ。
 * **外から当てる置き換え（合わせた本文・ディスクの版）は差分にして、undo の履歴に載せない**
 * （Ctrl+Z で合わせる前に戻すと、相手の編集を消すことになる。Fable の M54 設計レビュー 1）。
 */

export type EditorHandle = { replace: (text: string) => void; focus: () => void }

type Props = {
  initial: string
  readOnly?: boolean
  onChange: (text: string) => void
  onCompose: (on: boolean) => void
  /** complete は wikilink の補完（M55）。作ったときの1つを使い続ける。 */
  complete?: CompletionSource
}

const Editor = forwardRef<EditorHandle, Props>(function Editor({ initial, readOnly, onChange, onCompose, complete }, ref) {
  const host = useRef<HTMLDivElement>(null)
  const view = useRef<EditorView | null>(null)
  const cb = useRef({ onChange, onCompose })
  cb.current = { onChange, onCompose }

  useEffect(() => {
    const v = new EditorView({
      parent: host.current!,
      state: EditorState.create({
        doc: initial,
        extensions: [
          history(),
          drawSelection(),
          ...(complete && !readOnly ? [autocompletion({ override: [complete], icons: false })] : []),
          keymap.of([...defaultKeymap, ...historyKeymap]),
          markdown(),
          syntaxHighlighting(defaultHighlightStyle, { fallback: true }),
          EditorView.lineWrapping,
          EditorState.readOnly.of(!!readOnly),
          EditorView.editable.of(!readOnly),
          EditorView.contentAttributes.of({ 'aria-label': '本文', spellcheck: 'false', autocapitalize: 'off' }),
          EditorView.updateListener.of((u) => {
            // 外から当てた置き換えは画面の状態が既に知っているので、書いたことにしない。
            if (u.docChanged && !u.transactions.some((tr) => tr.annotation(Transaction.remote))) {
              cb.current.onChange(u.state.doc.toString())
            }
          }),
          EditorView.domEventHandlers({
            compositionstart: () => { cb.current.onCompose(true) },
            compositionend: () => {
              // 変換の確定は compositionend のあとに文字が入ることがあるので、1拍おく。
              setTimeout(() => cb.current.onCompose(false), 0)
            },
          }),
        ],
      }),
    })
    view.current = v
    return () => v.destroy()
    // 初期値だけで作る。以後の中身は replace で当てる。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [readOnly])

  useImperativeHandle(ref, () => ({
    replace(text: string) {
      const v = view.current
      if (!v) return
      const cur = v.state.doc.toString()
      if (cur === text) return
      let a = 0
      while (a < cur.length && a < text.length && cur[a] === text[a]) a++
      let b = 0
      while (b < cur.length - a && b < text.length - a && cur[cur.length - 1 - b] === text[text.length - 1 - b]) b++
      v.dispatch({
        changes: { from: a, to: cur.length - b, insert: text.slice(a, text.length - b) },
        annotations: [Transaction.addToHistory.of(false), Transaction.remote.of(true)],
      })
    },
    focus() { view.current?.focus() },
  }), [])

  return <div className="cm-host" ref={host} />
})

export default Editor
