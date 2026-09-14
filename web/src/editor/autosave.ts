import type { NoteSaveReply } from '../api'

/**
 * 自動保存の状態機械（Phase 5 / M54）。**画面から切り離した純粋な関数**にしてある
 * （jsdom で CodeMirror は動かないので、ここを単体で試す。Fable の M54 設計レビュー 10）。
 *
 * 守ること（同レビュー 1・4）:
 * - **merged のあとの base はサーバーが返す sent_sha256。** 合わせた版の sha を base にすると、次の保存が
 *   照合を通って直接書き、相手の編集を消す
 * - 送ったあと書いていなければ合わせた本文へ置き換える（画面は undo の履歴に載せずに当てる）
 * - **IME の変換中は保存も置き換えもしない**（届いた返事は変換が終わるまで持っておく）
 * - conflict で自動保存を止め、本人が選ぶまで何も捨てない
 * - 403 は止める（叩き続けない）。503・網の失敗は間を置いてやり直す
 */

export type Mode =
  | 'idle'      // 保存済み、または保存を待っている
  | 'saving'    // 送っている
  | 'conflict'  // 同じところを直していた。本人が選ぶまで止まる
  | 'blocked'   // 直せない（403 など）。止まる
  | 'retry'     // 実行面が居ない・網の失敗。間を置いてやり直す
  | 'manual'    // 指示の紙。自動保存しない（パスワードを訊いて保存）

export type State = {
  text: string
  /** base は text の土台の版のハッシュ（サーバーが知っている版）。 */
  base: string
  /** saved は最後にサーバーへ書けた本文（dirty の判定に使う）。 */
  saved: string
  mode: Mode
  manual: boolean
  composing: boolean
  /** sending は送っている本文。 */
  sending?: string
  /** held は変換中に届いた返事。変換が終わったら当てる。 */
  held?: { reply: NoteSaveReply; now: number }
  /** why は並べた理由（無ければ「同じところを直していた」）。 */
  conflict?: { disk: string; diskSha: string; mine: string; why?: string }
  error?: string
  /** retryAt はやり直す時刻（ms）。 */
  retryAt?: number
  /** notice は本人に知らせる一言（合わせた、など）。 */
  notice?: string
  /** force は中身が同じでも次に書く（「自分の版で上書き」を選んだ）。 */
  force?: boolean
}

export type Effect =
  | { type: 'save'; body: string; base: string }
  | { type: 'replace'; text: string }
  | { type: 'keep'; text: string; why: string } // 本人の版を控えに残す

export type Event =
  | { type: 'edit'; text: string }
  | { type: 'compose'; on: boolean }
  | { type: 'due'; now: number } // 書くのが止まって決まった時間が経った・やり直しの時刻を見る
  | { type: 'reply'; reply: NoteSaveReply; now: number }
  | { type: 'loadDisk' }
  | { type: 'keepMine' }
  | { type: 'manualSave' } // 指示の紙の「保存」（パスワードは画面が持つ）
  | { type: 'diskChanged'; sha: string }

export const retryAfter = 30_000

export function init(text: string, base: string, manual: boolean): State {
  return { text, base, saved: text, mode: manual ? 'manual' : 'idle', manual, composing: false }
}

/**
 * comparing は、Camp の控えの版（mine）とディスクの今の版を**並べて止まった状態**で始める。
 * 「Camp の版で開く」は見るだけで、本人が「自分の版で上書きする」を押すまで書かない
 * （押さなくても数秒で上書きしていた。outer gate の Fable 1）。
 */
export function comparing(mine: string, disk: string, diskSha: string, manual: boolean, why: string): State {
  return { ...init(mine, diskSha, manual), saved: disk, mode: 'conflict', conflict: { disk, diskSha, mine, why } }
}

export function dirty(s: State): boolean {
  return s.text !== s.saved || !!s.force
}

/** step は1つの出来事で状態を進め、画面がすべきことを返す。 */
export function step(s: State, e: Event): { s: State; fx: Effect[] } {
  switch (e.type) {
    case 'edit':
      return { s: { ...s, text: e.text, notice: undefined }, fx: [] }

    case 'compose': {
      const n = { ...s, composing: e.on }
      if (!e.on && n.held) {
        const held = n.held
        return step({ ...n, held: undefined }, { type: 'reply', reply: held.reply, now: held.now })
      }
      return { s: n, fx: [] }
    }

    case 'due': {
      if (s.composing || s.sending !== undefined || !dirty(s)) return { s, fx: [] }
      if (s.mode === 'idle' || (s.mode === 'retry' && (s.retryAt ?? 0) <= e.now)) {
        return send(s)
      }
      return { s, fx: [] }
    }

    case 'manualSave':
      if (s.sending !== undefined || s.composing) return { s, fx: [] }
      return send(s)

    case 'reply': {
      if (s.sending === undefined) return { s, fx: [] }
      if (s.composing) return { s: { ...s, held: { reply: e.reply, now: e.now } }, fx: [] }
      const sent = s.sending
      const back: Mode = s.manual ? 'manual' : 'idle'
      const base = { ...s, sending: undefined as string | undefined, error: undefined as string | undefined }
      const r = e.reply
      if (!r.ok) {
        if (r.code === 0 || r.code === 503 || r.code === 502 || r.code === 504) {
          return { s: { ...base, mode: 'retry', error: r.error, retryAt: e.now + retryAfter }, fx: [] }
        }
        if (r.code === 401 && s.manual) {
          // 指示の紙の 401 はパスワードの入れ違い（ログインは切れていない）。
          return { s: { ...base, mode: back, error: r.error }, fx: [] }
        }
        if (r.code === 401) {
          // ログインが切れた。別のタブでログインし直せば、やり直しで続けて保存できる。
          return { s: { ...base, mode: 'retry', error: 'ログインが切れた。別のタブでログインし直すと、続けて保存する', retryAt: e.now + retryAfter }, fx: [] }
        }
        return { s: { ...base, mode: 'blocked', error: r.error }, fx: [] }
      }
      const res = r.res
      switch (res.status) {
        case 'saved':
          return { s: { ...base, mode: back, base: res.sha256, saved: sent }, fx: [] }
        case 'merged': {
          if (s.text === sent && res.body !== undefined) {
            // 送ったあと書いていない。合わせた本文に置き換え、土台も合わせた版に。
            return {
              s: { ...base, mode: back, text: res.body, saved: res.body, base: res.sha256,
                notice: 'ほかで変わっていたので合わせた' },
              fx: [{ type: 'replace', text: res.body }],
            }
          }
          // 送ったあとも書いていた。**土台は送った版**（サーバーの blobs にある）。次の保存で合わせる。
          return { s: { ...base, mode: back, base: res.sent_sha256, saved: sent,
            notice: 'ほかで変わっていたので合わせた（続きは次の保存で合わせる）' }, fx: [] }
        }
        case 'conflict':
          return {
            s: { ...base, mode: 'conflict',
              conflict: { disk: res.disk ?? '', diskSha: res.disk_sha256 ?? '', mine: s.text } },
            fx: [],
          }
      }
      return { s, fx: [] }
    }

    case 'loadDisk': {
      if (s.mode !== 'conflict' || !s.conflict) return { s, fx: [] }
      const c = s.conflict
      // **押した時点の本人の版**を控えに残す（ぶつかった時点ではなく）。
      return {
        s: { ...s, mode: s.manual ? 'manual' : 'idle', conflict: undefined, text: c.disk, saved: c.disk, base: c.diskSha },
        fx: [{ type: 'keep', text: s.text, why: 'ディスクの版を読み込む前の自分の版' }, { type: 'replace', text: c.disk }],
      }
    }

    case 'keepMine': {
      if (s.mode !== 'conflict' || !s.conflict) return { s, fx: [] }
      // 土台をディスクの版にして、自分の版で書く（ディスクの版はサーバーの blobs に控えてある）。
      return { s: { ...s, mode: s.manual ? 'manual' : 'idle', conflict: undefined, base: s.conflict.diskSha, force: true }, fx: [] }
    }

    case 'diskChanged': {
      if (e.sha === s.base || s.sending !== undefined || s.mode === 'conflict') return { s, fx: [] }
      return { s: { ...s, notice: 'ほかでこのノートが変わった。次の保存で合わせる' }, fx: [] }
    }
  }
}

/**
 * leaving は、画面の中で別のノートへ移るときにすること（outer gate の codex）。
 *
 * - keep: その場で控えに残す本文（書きかけが無ければ null）。**保存の途中でも・変換の途中でも・指示の紙でも残す**
 * - send: now（すぐ送る）・afterReply（送っている保存の返事を待ち、その版を土台に送る）・none（控えだけ）
 */
export function leaving(s: State): { keep: string | null; send: 'now' | 'afterReply' | 'none' } {
  if (!dirty(s)) return { keep: null, send: 'none' }
  const keep = wellFormed(s.text)
  if (s.manual || s.composing || !(s.mode === 'idle' || s.mode === 'retry' || s.mode === 'saving')) {
    return { keep, send: 'none' }
  }
  if (s.sending === undefined) return { keep, send: 'now' }
  return { keep, send: s.sending === keep ? 'none' : 'afterReply' }
}

function send(s: State): { s: State; fx: Effect[] } {
  // 孤立サロゲートは Go の JSON で U+FFFD になり、画面とディスクが永遠に一致しなくなる（Fable 11）。
  const body = wellFormed(s.text)
  const changed = body !== s.text
  return {
    s: { ...s, mode: 'saving', sending: body, text: body, error: undefined, force: false,
      notice: changed ? '壊れた文字を置き換えてから保存した' : s.notice },
    fx: changed ? [{ type: 'replace', text: body }, { type: 'save', body, base: s.base }] : [{ type: 'save', body, base: s.base }],
  }
}

export function wellFormed(t: string): string {
  const f = (t as unknown as { toWellFormed?: () => string }).toWellFormed
  if (f) return f.call(t)
  return t.replace(/[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/g, '�')
}
