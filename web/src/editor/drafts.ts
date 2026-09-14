/**
 * ブラウザの控え（Phase 5 / M54、Fable の設計レビュー 4）。
 *
 * - 書くたび（300ms 間引き）に、本文と**そのときの土台（base）**を残す。保存できたら消す
 * - 鍵にタブの id を入れる（同じノートを2つのタブで開いても潰し合わない）
 * - 戻すときの土台は控えのもの。今の sha にしない（相手の編集の上に直接書かないため）
 * - ログアウトで消す（Vault の中身を端末に残し続けない）
 * - localStorage が使えない・溢れたら false を返す（画面に出す）
 */

export type Draft = { note: number; tab: string; text: string; base: string; at: number }
export type Kept = { note: number; text: string; why: string; at: number }

const prefix = 'camp.draft.'
const keptPrefix = 'camp.kept.'

// sessionStorage が使えないときの、このページの読み込みの間だけの id（固定の値にすると、同じノートを
// 開いた2つのタブの控えが同じ鍵で潰し合う。outer gate の codex）。
const pageTab = Math.random().toString(36).slice(2, 10)

export function tabId(): string {
  try {
    let id = sessionStorage.getItem('camp.tab')
    if (!id) {
      id = pageTab
      sessionStorage.setItem('camp.tab', id)
    }
    return id
  } catch {
    return pageTab
  }
}

export function putDraft(d: Omit<Draft, 'at'>): boolean {
  try {
    localStorage.setItem(`${prefix}${d.note}.${d.tab}`, JSON.stringify({ ...d, at: Date.now() }))
    return true
  } catch {
    return false
  }
}

export function dropDraft(note: number, tab: string) {
  try {
    localStorage.removeItem(`${prefix}${note}.${tab}`)
  } catch {
    /* 使えないなら残っていない */
  }
}

/**
 * dropDraftIf は、控えの本文が text と同じときだけ消す。保存の返事が遅れて届いたときに、そのあと書いた
 * 新しい控えを消さない（outer gate の Fable 8）。
 */
export function dropDraftIf(note: number, tab: string, text: string) {
  try {
    const k = `${prefix}${note}.${tab}`
    const v = JSON.parse(localStorage.getItem(k) ?? 'null') as Draft | null
    if (!v || v.text === text) localStorage.removeItem(k)
  } catch {
    /* 使えないなら残っていない */
  }
}

/** 新しいノートの画面の書きかけ（作る前。ログアウトで消える）。 */
export type NewDraft = { folder: string; name: string; body: string; at: number }
const newKey = `${prefix}new`

export function putNewDraft(d: Omit<NewDraft, 'at'>): boolean {
  try {
    if (!d.body && !d.name) localStorage.removeItem(newKey)
    else localStorage.setItem(newKey, JSON.stringify({ ...d, at: Date.now() }))
    return true
  } catch {
    return false
  }
}

export function newDraft(): NewDraft | null {
  try {
    const v = JSON.parse(localStorage.getItem(newKey) ?? 'null') as NewDraft | null
    return v && typeof v.body === 'string' ? v : null
  } catch {
    return null
  }
}

export function dropNewDraft() {
  try {
    localStorage.removeItem(newKey)
  } catch {
    /* 使えないなら残っていない */
  }
}

/** drafts はそのノートの控え（新しい順）。 */
export function drafts(note: number): Draft[] {
  const out: Draft[] = []
  try {
    for (let i = 0; i < localStorage.length; i++) {
      const k = localStorage.key(i)
      if (!k || !k.startsWith(`${prefix}${note}.`)) continue
      const v = JSON.parse(localStorage.getItem(k) ?? 'null') as Draft | null
      if (v && typeof v.text === 'string' && typeof v.base === 'string') out.push(v)
    }
  } catch {
    return []
  }
  return out.sort((a, b) => b.at - a.at)
}

export function keep(note: number, text: string, why: string): boolean {
  try {
    localStorage.setItem(`${keptPrefix}${note}.${Date.now()}`, JSON.stringify({ note, text, why, at: Date.now() }))
    return true
  } catch {
    return false
  }
}

export function kept(note: number): Kept[] {
  const out: Kept[] = []
  try {
    for (let i = 0; i < localStorage.length; i++) {
      const k = localStorage.key(i)
      if (!k || !k.startsWith(`${keptPrefix}${note}.`)) continue
      const v = JSON.parse(localStorage.getItem(k) ?? 'null') as Kept | null
      if (v) out.push(v)
    }
  } catch {
    return []
  }
  return out.sort((a, b) => b.at - a.at)
}

/** clearAll はログアウトのときに控えを全部消す。 */
export function clearAll() {
  try {
    const ks: string[] = []
    for (let i = 0; i < localStorage.length; i++) {
      const k = localStorage.key(i)
      if (k && (k.startsWith(prefix) || k.startsWith(keptPrefix))) ks.push(k)
    }
    ks.forEach((k) => localStorage.removeItem(k))
  } catch {
    /* 使えないなら残っていない */
  }
}
