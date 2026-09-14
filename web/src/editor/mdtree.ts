import { GFM, parser, type InlineContext, type MarkdownConfig } from '@lezer/markdown'
import type { SyntaxNode } from '@lezer/common'

/**
 * Markdown を「描いてよい形」の木にする（Phase 5 / M54、2026-09-13）。
 *
 * **HTML の文字列を作らない。** `@lezer/markdown` の構文木から、画面が React の要素にするだけの
 * 平らな値の木を作る（本人の決定9: エディタと同じ部品で読む。markdown-it は入れない）。
 * この関数は Web Worker の中で走る（md.worker.ts）。細工した入力で固まっても画面は止まらない。
 *
 * - 描く節は許可リスト。**知らない節と HTML の節は元の文字**として出す
 * - リンクの宛先は safeHref で照らしたものだけ `href` に入れる（http・https で、Camp 自身でないもの）
 * - 画像は読み込まない。wikilink は宛先の文字だけ持ち、行き先はサーバーが解決する
 * - frontmatter は YAML として解釈しない（行ごとに最初の `:` で割る）
 *
 * 設計は `dev/active/phase5-design.md` の M54、レビューは `review-fable-phase5-m54-2026-09-13.md`。
 */

export type Md =
  | { t: 'text'; s: string }
  | { t: 'br' }
  | { t: 'p' | 'li' | 'quote' | 'em' | 'strong' | 'del' | 'tr' | 'th' | 'td'; c: Md[] }
  | { t: 'ul' | 'ol'; c: Md[] }
  | { t: 'h'; level: number; c: Md[] }
  | { t: 'code' | 'icode'; s: string }
  | { t: 'hr' }
  | { t: 'table'; c: Md[] }
  | { t: 'task'; checked: boolean; c: Md[] }
  | { t: 'link'; href: string; c: Md[] }
  | { t: 'image'; alt: string; src: string; href?: string }
  | { t: 'wiki'; target: string; frag: string; alias: string; embed: boolean; raw: string }
  | { t: 'callout'; kind: CalloutKind; title: string; c: Md[] }
  | { t: 'fm'; rows: { key: string; value: string }[] }

/** Vault で使われているコールアウト（実測 2026-09-13）。ほかは note に写す。**名前をクラスに入れない。** */
export const calloutKinds = ['note', 'info', 'warning', 'important', 'archive'] as const
export type CalloutKind = (typeof calloutKinds)[number]

/** 木の深さの上限。これより深い入れ子は元の文字で出す（React の再帰で固まらないため）。 */
const maxDepth = 40
/** wikilink の中身の長さの上限。探す長さを縛る（`[[` を大量に並べた入力で二乗にしない）。 */
const maxWiki = 1024

const wikiExt: MarkdownConfig = {
  defineNodes: [{ name: 'WikiLink' }, { name: 'WikiEmbed' }],
  parseInline: [
    {
      name: 'WikiLink',
      before: 'Link',
      parse(cx: InlineContext, next: number, pos: number): number {
        let open = pos
        let embed = false
        if (next === 33 /* ! */) {
          if (cx.char(pos + 1) !== 91 || cx.char(pos + 2) !== 91) return -1
          embed = true
          open = pos + 1
        } else if (next !== 91 || cx.char(pos + 1) !== 91) {
          return -1
        }
        const limit = Math.min(cx.end, open + 2 + maxWiki)
        for (let i = open + 2; i < limit; i++) {
          const ch = cx.char(i)
          if (ch === 10) return -1
          // 次の `[[` に出会ったら諦める。探す範囲が重ならないので、`[[` の繰り返しでも線形。
          if (ch === 91 && cx.char(i + 1) === 91) return -1
          if (ch === 93 && cx.char(i + 1) === 93) {
            if (i === open + 2) return -1
            return cx.addElement(cx.elt(embed ? 'WikiEmbed' : 'WikiLink', pos, i + 2))
          }
        }
        return -1
      },
    },
  ],
}

const mdParser = parser.configure([GFM, wikiExt])

/** build は本文を描いてよい形の木にする。selfOrigin は Camp 自身のオリジン（リンクにしない）。 */
export function build(doc: string, selfOrigin: string): Md[] {
  const out: Md[] = []
  let body = doc
  const fm = frontmatter(doc)
  if (fm) {
    out.push({ t: 'fm', rows: fm.rows })
    body = doc.slice(fm.end)
  }
  const b = new Builder(body, selfOrigin)
  const tree = mdParser.parse(body)
  for (let n = tree.topNode.firstChild; n; n = n.nextSibling) {
    const x = b.block(n, 0)
    if (x) out.push(x)
  }
  return out
}

/** frontmatter は先頭の `---` の行から次の `---` の行まで。**YAML として解釈しない。** */
function frontmatter(doc: string): { rows: { key: string; value: string }[]; end: number } | null {
  if (!doc.startsWith('---\n')) return null
  const close = doc.indexOf('\n---', 3)
  if (close < 0) return null
  const after = close + 4
  if (after < doc.length && doc[after] !== '\n') return null
  const rows = doc
    .slice(4, close)
    .split('\n')
    .map((line) => {
      const i = line.indexOf(':')
      // 入れ子や複数行は素の行（key を空に）。
      if (i <= 0 || /^\s/.test(line)) return { key: '', value: line }
      return { key: line.slice(0, i), value: line.slice(i + 1).trim() }
    })
  return { rows, end: Math.min(doc.length, after + 1) }
}

const skipMarks = new Set([
  'QuoteMark', 'HeaderMark', 'ListMark', 'EmphasisMark', 'CodeMark', 'LinkMark',
  'StrikethroughMark', 'TableDelimiter', 'CodeInfo',
])

class Builder {
  constructor(private doc: string, private origin: string) {}

  private slice(n: SyntaxNode): string {
    return this.doc.slice(n.from, n.to)
  }

  block(n: SyntaxNode, depth: number): Md | null {
    if (depth > maxDepth) return { t: 'p', c: [{ t: 'text', s: this.slice(n) }] }
    const name = n.name
    const head = /^(?:ATX|Setext)Heading([1-6])$/.exec(name)
    if (head) return { t: 'h', level: Number(head[1]), c: trimInline(this.inline(n, n.from, n.to, depth)) }
    switch (name) {
      case 'Paragraph':
        return { t: 'p', c: this.inline(n, n.from, n.to, depth) }
      case 'BulletList':
      case 'OrderedList':
        return { t: name === 'BulletList' ? 'ul' : 'ol', c: this.children(n, depth) }
      case 'ListItem':
        return { t: 'li', c: this.children(n, depth) }
      case 'Task': {
        const marker = n.getChild('TaskMarker')
        const checked = !!marker && /x/i.test(this.slice(marker))
        return { t: 'task', checked, c: trimStart(this.inline(n, marker ? marker.to : n.from, n.to, depth)) }
      }
      case 'Blockquote':
        return this.quote(n, depth)
      case 'FencedCode':
      case 'CodeBlock': {
        const parts: string[] = []
        for (let c = n.firstChild; c; c = c.nextSibling) if (c.name === 'CodeText') parts.push(this.slice(c))
        return { t: 'code', s: parts.join('\n') }
      }
      case 'HorizontalRule':
        return { t: 'hr' }
      case 'Table':
        return { t: 'table', c: this.children(n, depth) }
      case 'TableHeader':
      case 'TableRow': {
        const cells: Md[] = []
        for (let c = n.firstChild; c; c = c.nextSibling) {
          if (c.name === 'TableCell') {
            cells.push({ t: name === 'TableHeader' ? 'th' : 'td', c: this.inline(c, c.from, c.to, depth + 1) })
          }
        }
        return { t: 'tr', c: cells }
      }
      default:
        // HTMLBlock・CommentBlock・LinkReference など。**元の文字で出す。**
        return { t: 'p', c: [{ t: 'text', s: this.slice(n) }] }
    }
  }

  private children(n: SyntaxNode, depth: number): Md[] {
    const out: Md[] = []
    for (let c = n.firstChild; c; c = c.nextSibling) {
      if (skipMarks.has(c.name)) continue
      const x = this.block(c, depth + 1)
      if (x) out.push(x)
    }
    return out
  }

  /** 引用。最初の段落が `[!kind] 題` ならコールアウト。 */
  private quote(n: SyntaxNode, depth: number): Md {
    const kids = this.children(n, depth)
    let first: SyntaxNode | null = n.firstChild
    while (first && skipMarks.has(first.name)) first = first.nextSibling
    if (first && first.name === 'Paragraph') {
      const m = /^\[!([A-Za-z-]{1,32})\][+-]?[ \t]*([^\n]*)/.exec(this.slice(first))
      if (m) {
        const k = m[1].toLowerCase()
        const kind = (calloutKinds as readonly string[]).includes(k) ? (k as CalloutKind) : 'note'
        // 最初の段落の1行目（題）を除いた残り。
        const nl = this.doc.indexOf('\n', first.from)
        const rest: Md[] = []
        if (nl >= 0 && nl < first.to) {
          const c = this.inline(first, nl + 1, first.to, depth)
          if (c.length) rest.push({ t: 'p', c })
        }
        return { t: 'callout', kind, title: m[2].trim(), c: [...rest, ...kids.slice(1)] }
      }
    }
    return { t: 'quote', c: kids }
  }

  /** inline は from〜to の中の字句を並べる。子の節の間の文字はそのまま文字（改行は br）。 */
  inline(n: SyntaxNode, from: number, to: number, depth: number): Md[] {
    const out: Md[] = []
    let at = from
    let stripSpace = false
    const text = (s: string) => {
      if (stripSpace) {
        s = s.replace(/^[ \t]/, '')
        stripSpace = false
      }
      if (!s) return
      const lines = s.split('\n')
      lines.forEach((line, i) => {
        if (i > 0) out.push({ t: 'br' })
        const v = i > 0 ? line.replace(/^[ \t]+/, '') : line
        if (v) push(out, { t: 'text', s: v })
      })
    }
    for (let c = n.firstChild; c; c = c.nextSibling) {
      if (c.to <= from || c.from >= to) continue
      if (c.from > at) text(this.doc.slice(at, c.from))
      at = Math.max(at, c.to)
      if (c.name === 'QuoteMark') {
        stripSpace = true
        continue
      }
      if (skipMarks.has(c.name) || c.name === 'TaskMarker') continue
      const x = this.inlineNode(c, depth + 1)
      if (x) push(out, x)
    }
    if (at < to) text(this.doc.slice(at, to))
    return out
  }

  private inlineNode(n: SyntaxNode, depth: number): Md | null {
    if (depth > maxDepth) return { t: 'text', s: this.slice(n) }
    switch (n.name) {
      case 'Emphasis':
        return { t: 'em', c: this.inline(n, n.from, n.to, depth) }
      case 'StrongEmphasis':
        return { t: 'strong', c: this.inline(n, n.from, n.to, depth) }
      case 'Strikethrough':
        return { t: 'del', c: this.inline(n, n.from, n.to, depth) }
      case 'InlineCode': {
        const s = this.slice(n).replace(/^`+/, '').replace(/`+$/, '')
        return { t: 'icode', s }
      }
      case 'HardBreak':
        return { t: 'br' }
      case 'Escape':
        return { t: 'text', s: this.slice(n).slice(1) }
      case 'Entity':
        return { t: 'text', s: decodeEntity(this.slice(n)) }
      case 'URL': {
        // 裸の URL（GFM の自動リンク）。
        const raw = this.slice(n)
        const href = safeHref(raw, this.origin)
        return href ? { t: 'link', href, c: [{ t: 'text', s: raw }] } : { t: 'text', s: raw }
      }
      case 'Autolink': {
        const url = n.getChild('URL')
        const raw = url ? this.slice(url) : this.slice(n)
        const href = url ? safeHref(raw, this.origin) : null
        return href ? { t: 'link', href, c: [{ t: 'text', s: raw }] } : { t: 'text', s: this.slice(n) }
      }
      case 'Link':
        return this.link(n, depth)
      case 'Image': {
        const marks = n.getChildren('LinkMark')
        const url = n.getChild('URL')
        const alt = marks.length >= 2 ? this.doc.slice(marks[0].to, marks[1].from) : ''
        const src = url ? unescapeDest(this.slice(url)) : ''
        const href = url ? safeHref(this.slice(url), this.origin) : null
        return { t: 'image', alt, src, ...(href ? { href } : {}) }
      }
      case 'WikiLink':
      case 'WikiEmbed':
        return wiki(this.slice(n), n.name === 'WikiEmbed')
      default:
        // HTMLTag・Comment・ProcessingInstruction など。**元の文字で出す。**
        return { t: 'text', s: this.slice(n) }
    }
  }

  private link(n: SyntaxNode, depth: number): Md {
    const marks = n.getChildren('LinkMark')
    const url = n.getChild('URL')
    // 参照リンク（`[a][b]`・`[a]`）は宛先を持たない。文字のまま。
    if (!url || marks.length < 2) return { t: 'text', s: this.slice(n) }
    const label = this.inline(n, marks[0].to, marks[1].from, depth)
    const href = safeHref(this.slice(url), this.origin)
    if (!href) return { t: 'text', s: this.slice(n) }
    return { t: 'link', href, c: label }
  }
}

function push(out: Md[], x: Md) {
  const last = out[out.length - 1]
  if (x.t === 'text' && last && last.t === 'text') {
    out[out.length - 1] = { t: 'text', s: last.s + x.s }
  } else {
    out.push(x)
  }
}

function trimStart(c: Md[]): Md[] {
  if (c.length && c[0].t === 'text') c[0] = { t: 'text', s: c[0].s.replace(/^\s+/, '') }
  return c.filter((x) => x.t !== 'text' || x.s !== '')
}

function trimInline(c: Md[]): Md[] {
  if (c.length && c[0].t === 'text') c[0] = { t: 'text', s: c[0].s.replace(/^\s+/, '') }
  const last = c[c.length - 1]
  if (last && last.t === 'text') c[c.length - 1] = { t: 'text', s: last.s.replace(/\s+#*\s*$/, '') }
  return c.filter((x) => x.t !== 'text' || x.s !== '')
}

/** wiki は `[[target#frag|alias]]` を分ける。 */
export function wiki(raw: string, embed: boolean): Md {
  const inner = raw.slice(embed ? 3 : 2, -2)
  const bar = inner.indexOf('|')
  const dest = bar >= 0 ? inner.slice(0, bar) : inner
  const alias = bar >= 0 ? inner.slice(bar + 1) : ''
  const hash = dest.indexOf('#')
  const target = (hash >= 0 ? dest.slice(0, hash) : dest).replace(/\\$/, '').trim()
  const frag = hash >= 0 ? dest.slice(hash + 1) : ''
  return { t: 'wiki', target, frag, alias, embed, raw }
}

const named: Record<string, string> = {
  amp: '&', lt: '<', gt: '>', quot: '"', apos: "'", nbsp: ' ',
}

/** 文字参照を1つ復号する。知らない名前はそのまま。 */
export function decodeEntity(s: string): string {
  const m = /^&(?:#[xX]([0-9a-fA-F]{1,6})|#([0-9]{1,7})|([A-Za-z][A-Za-z0-9]{0,31}));$/.exec(s)
  if (!m) return s
  if (m[3]) return named[m[3]] ?? s
  const cp = m[1] ? parseInt(m[1], 16) : parseInt(m[2], 10)
  if (cp === 0 || cp > 0x10ffff || (cp >= 0xd800 && cp <= 0xdfff)) return '�'
  return String.fromCodePoint(cp)
}

function unescapeDest(s: string): string {
  let v = s.trim()
  if (v.startsWith('<') && v.endsWith('>')) v = v.slice(1, -1)
  v = v.replace(/&(?:#[xX][0-9a-fA-F]{1,6}|#[0-9]{1,7}|[A-Za-z][A-Za-z0-9]{0,31});/g, decodeEntity)
  return v.replace(/\\([!-/:-@[-`{-~])/g, '$1')
}

/**
 * safeHref はリンクにしてよい宛先なら正規化した href を、だめなら null を返す（Fable の M54 設計レビュー 2）。
 *
 * - 文字参照とバックスラッシュの逃がしを**先に復号**してから照らす
 * - `new URL()` は **base 無し**（相対の宛先は例外 → 文字）
 * - http・https だけ。**Camp 自身のオリジンは文字**（本文から Camp の画面へのリンクを作らせない）
 * - 制御文字・バックスラッシュ・利用者名を含むものは文字
 * - 描く値は**検証した url.href**（照らした文字列と描く文字列を別にしない）
 */
export function safeHref(raw: string, selfOrigin: string): string | null {
  const v = unescapeDest(raw)
  if (!v || /[\u0000-\u001f\u007f\\]/.test(v)) return null
  let u: URL
  try {
    u = new URL(v)
  } catch {
    return null
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return null
  if (u.username || u.password) return null
  if (selfOrigin && u.origin === selfOrigin) return null
  return u.href
}
