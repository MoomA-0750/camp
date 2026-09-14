import { expect, test } from 'vitest'
import { render } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { build, safeHref, type Md } from './mdtree'
import Preview, { wikiTargets } from './Preview'

// 表示の安全（Phase 5 / M54、Fable の設計レビュー 2・10）。
//
// **判定は「宛先ごとに <a> が 0 個で、元の文字が見える」。** 生の HTML の要素（script・iframe・img）は
// 構造上そもそも作られないので、それが 0 個なのを見ても守りの強さは測れない。リンクの判定が壊れたら
// 落ちる形で見る。

const self = 'https://camp.example.ts.net'

function show(md: string, targets = {}) {
  const tree = build(md, self)
  const r = render(<MemoryRouter><Preview tree={tree} targets={targets} /></MemoryRouter>)
  return { tree, root: r.container }
}

const noHrefLinks = [
  '[x](javascript:alert(1))',
  '[x](JaVaScRiPt:alert(1))',
  '[x]( javascript:alert(1))',
  '[x](&#x6a;avascript:alert(1))',
  '[x](&#106;avascript:alert(1))',
  '[x](java\\script:alert(1))',
  '[x](data:text/html,<script>alert(1)</script>)',
  '[x](data:image/png;base64,AAAA)',
  '[x](vbscript:msgbox)',
  '[x](file:///etc/passwd)',
  '[x](//evil.example/x)',
  '[x](/api/runtime/start)',
  '[x](../../api/notes)',
  '[x](http:\\\\evil.example)',
  '[x](https://camp.example.ts.net/runtime/abc)',
  '[x](https://user:pw@evil.example/)',
  '[x](mailto:someone@example.com)',
  '<javascript:alert(1)>',
  'https://camp.example.ts.net/runtime/abc',
  '[x](blob:https://camp.example.ts.net/1)',
]

test.each(noHrefLinks)('リンクにしない: %s', (md) => {
  const { root } = show(md)
  expect(root.querySelectorAll('a').length).toBe(0)
  expect(root.textContent?.length).toBeGreaterThan(0)
})

test('生の HTML は要素にならず、文字として見える', () => {
  const md = [
    '<script>alert(1)</script>',
    '',
    'a <img src=x onerror=alert(1)> b <iframe src="https://evil.example"></iframe>',
    '',
    '<svg onload=alert(1)><a href="javascript:alert(1)">x</a></svg>',
    '',
    '[[<img src=x onerror=alert(1)>]]',
    '',
    '> [!<script>alert(1)</script>] 題',
    '',
    '[<b>題</b>](https://example.com/"onmouseover="alert(1))',
  ].join('\n')
  const { root } = show('---\ntitle: <script>alert(1)</script>\n---\n' + md)
  for (const tag of ['script', 'img', 'iframe', 'svg', 'b', 'style', 'object', 'embed']) {
    expect(root.querySelectorAll(tag).length, tag).toBe(0)
  }
  for (const el of root.querySelectorAll('*')) {
    for (const a of el.attributes) {
      expect(a.name.startsWith('on'), `${el.tagName} ${a.name}`).toBe(false)
      expect(/javascript:/i.test(a.value), `${el.tagName} ${a.name}=${a.value}`).toBe(false)
    }
  }
  expect(root.textContent).toContain('<script>alert(1)</script>')
  expect(root.textContent).toContain('<img src=x onerror=alert(1)>')
  // 外へのリンクは1本だけで、宛先は正規化した値、題の HTML は文字。
  const links = root.querySelectorAll('a')
  expect(links.length).toBe(1)
  expect(links[0].getAttribute('href')).toBe('https://example.com/%22onmouseover=%22alert(1)')
  expect(links[0].getAttribute('rel')).toBe('noopener noreferrer')
  expect(links[0].textContent).toBe('<b>題</b>')
})

test('外へのリンクは http・https だけ、正規化した値で', () => {
  expect(safeHref('<https://example.com/a b>', self)).toBe('https://example.com/a%20b')
  expect(safeHref('https://example.com/ä', self)).toBe('https://example.com/%C3%A4')
  expect(safeHref('HTTPS://EXAMPLE.com', self)).toBe('https://example.com/')
  expect(safeHref('https://camp.example.ts.net:443/x', self)).toBe(null)
  expect(safeHref('https://camp.example.ts.net.evil.example/x', self)).toBe('https://camp.example.ts.net.evil.example/x')
  expect(safeHref('\u0001https://example.com', self)).toBe(null)
})

test('画像は読み込まず、意味だけ出す', () => {
  const { root } = show('![猫](https://tracker.example/pixel.png) ![x](javascript:alert(1))')
  expect(root.querySelectorAll('img').length).toBe(0)
  expect(root.textContent).toContain('画像: 猫')
  const a = root.querySelectorAll('a')
  expect(a.length).toBe(1)
  expect(a[0].getAttribute('href')).toBe('https://tracker.example/pixel.png')
})

test('wikilink はサーバーが解決した行き先だけリンクにする', () => {
  const md = '[[Human/Logs/2026-09-13|日記]] [[無い]] [[同名]] ![[図.png]] [[#見出し]] [[a\\|b]]'
  const tree = build(md, self)
  expect(wikiTargets(tree).sort()).toEqual(['', 'Human/Logs/2026-09-13', 'a', '同名', '図.png', '無い'].sort())
  const { root } = show(md, {
    'Human/Logs/2026-09-13': { to_id: 12, to_path: 'Human/Logs/2026-09-13.md' },
    '無い': {},
    '同名': { to_id: 3, to_path: 'AI/同名.md', ambiguous: true, candidates: ['AI/同名.md', 'Human/同名.md'] },
  })
  const links = [...root.querySelectorAll('a')].map((a) => [a.getAttribute('href'), a.textContent])
  expect(links).toEqual([['/notes/12', '日記'], ['/notes/3', '同名']])
  expect(root.textContent).toContain('無い（無い）')
  expect(root.textContent).toContain('候補 2')
  expect(root.textContent).toContain('埋め込み: 図.png')
})

test('コールアウト・表・タスク・frontmatter を描く。種類の名前はクラスに入れない', () => {
  const md = [
    '---', 'tags: [a, b]', 'nested:', '  child: 1', '---',
    '> [!warning] 気をつけること', '> 1行目', '> 2行目',
    '', '> [!Evil-Class] 知らない種類',
    '', '| a | b |', '|---|---|', '| 1 | **2** |',
    '', '- [x] 済み', '- [ ] まだ',
  ].join('\n')
  const { root, tree } = show(md)
  expect(tree[0]).toEqual({ t: 'fm', rows: [
    { key: 'tags', value: '[a, b]' }, { key: 'nested', value: '' }, { key: '', value: '  child: 1' }] })
  const callouts = root.querySelectorAll('aside')
  expect(callouts.length).toBe(2)
  expect(callouts[0].textContent).toContain('注意: 気をつけること')
  expect(callouts[0].textContent).toContain('1行目')
  expect(callouts[0].textContent).not.toContain('>')
  expect(callouts[1].textContent).toContain('メモ: 知らない種類')
  expect(root.innerHTML).not.toMatch(/evil/i)
  expect(root.querySelectorAll('td strong').length).toBe(1)
  expect(root.textContent).toContain('☑ 済み')
})

test('病的な入力でも深さと時間が暴れない', () => {
  const inputs = ['[['.repeat(100000), '['.repeat(100000), '*'.repeat(100000), '>'.repeat(5000) + ' x',
    '- '.repeat(3000) + 'x', 'a'.repeat(200000)]
  for (const md of inputs) {
    const t0 = performance.now()
    const tree = build(md, self)
    const took = performance.now() - t0
    expect(took, md.slice(0, 8)).toBeLessThan(5000)
    expect(depth(tree)).toBeLessThan(90)
  }
})

function depth(ns: Md[]): number {
  let d = 0
  for (const n of ns) if ('c' in n) d = Math.max(d, 1 + depth(n.c))
  return d
}
