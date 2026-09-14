import { useEffect, useState } from 'react'
import { api, type NoteName } from '../api'

/**
 * 名前の一覧（Phase 5 / M55）。スイッチャーと wikilink の補完が同じものを使う。
 *
 * - どの Vault か: 実行面が書く Vault（同期の様子の vault_id）。実行面が居なければ最初の Vault（見るだけ）
 * - サーバーの世代（gen）が同じなら中身を引き直さない（スマホで毎回数百 KiB を引かない）
 * - **リンクの形はサーバーが決める**（link）。ここは絞り込みと並べ方だけ
 */

type Cache = { vault: number; gen: string; names: NoteName[] }
let cache: Cache | null = null
let loading: Promise<Cache> | null = null
const subs = new Set<(c: Cache) => void>()

async function vaultId(): Promise<number> {
  try {
    const s = await api.noteSync()
    if (s.vault_id) return s.vault_id
  } catch { /* 実行面が居ない・古い campd */ }
  const vs = await api.vaults()
  return vs[0]?.id ?? 0
}

/** loadNames は一覧を新しくする（世代が同じなら引き直さない）。 */
export function loadNames(): Promise<Cache> {
  if (loading) return loading
  loading = (async () => {
    try {
      const vault = cache?.vault || (await vaultId())
      if (!vault) throw new Error('Vault が無い')
      const got = await api.noteNames(vault, cache?.vault === vault ? cache.gen : '')
      if (!(got.same && cache)) cache = { vault, gen: got.gen, names: got.names ?? [] }
      subs.forEach((f) => f(cache!))
      return cache
    } finally {
      loading = null
    }
  })()
  return loading
}

export function currentNames(): NoteName[] {
  return cache?.names ?? []
}

export function currentVault(): number {
  return cache?.vault ?? 0
}

/** 作ったノートを一覧が次に引き直すまでの間も出すため、手元に足す。 */
export function addName(n: NoteName) {
  if (!cache || cache.names.some((x) => x.id === n.id)) return
  cache = { ...cache, gen: '', names: [...cache.names, n] }
  subs.forEach((f) => f(cache!))
}

export function useNames(): { names: NoteName[]; vault: number; error: string } {
  const [c, setC] = useState<Cache | null>(cache)
  const [error, setError] = useState('')
  useEffect(() => {
    subs.add(setC)
    loadNames().then(setC, (e) => setError((e as Error).message))
    return () => { subs.delete(setC) }
  }, [])
  return { names: c?.names ?? [], vault: c?.vault ?? 0, error }
}

const norm = (s: string) => s.normalize('NFC').toLowerCase()

const base = (p: string) => p.slice(p.lastIndexOf('/') + 1).replace(/\.md$/, '')

/** subsequence は needle の文字が hay に順に現れるか（サロゲートペアを1文字として数える）。 */
function subsequence(hay: string, needle: string): boolean {
  const want = [...needle]
  let i = 0
  for (const ch of hay) {
    if (i === want.length) break
    if (ch === want[i]) i++
  }
  return i === want.length
}

/**
 * rank は名前を問い合わせで絞って並べる。空白で区切った語が全部当たるものだけ。
 * ベース名がそのもの＞ベース名の頭＞ベース名に含まれる＞パスに含まれる＞パスに順に並ぶ。同点は短いパスを先に。
 */
export function rank(query: string, names: NoteName[], limit = 50, pick: (n: NoteName) => boolean = () => true): NoteName[] {
  const words = norm(query).split(/\s+/).filter(Boolean)
  const scored: { n: NoteName; score: number }[] = []
  for (const n of names) {
    if (!pick(n)) continue
    const p = norm(n.path)
    const b = norm(base(n.path))
    let score = 0
    let ok = true
    for (const w of words) {
      const i = b.indexOf(w)
      if (b === w) score += 200
      else if (i === 0) score += 150
      else if (i > 0) score += 100
      else if (p.includes(w)) score += 40
      else if (subsequence(p, w)) score += 10
      else { ok = false; break }
    }
    if (ok) scored.push({ n, score })
  }
  scored.sort((a, b) => b.score - a.score || a.n.path.length - b.n.path.length || (a.n.path < b.n.path ? -1 : 1))
  return scored.slice(0, limit).map((x) => x.n)
}

/** hasBase は、入れた文字がそのままベース名のノートがあるか（「作る」を出すかの判断）。 */
export function hasBase(query: string, names: NoteName[]): boolean {
  const q = norm(query.trim())
  return names.some((n) => norm(base(n.path)) === q)
}
