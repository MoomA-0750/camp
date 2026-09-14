import { Link } from 'react-router-dom'
import type { WikiTarget } from '../api'
import type { Md } from './mdtree'

/**
 * mdtree の木を React の要素にする（Phase 5 / M54）。
 *
 * **`dangerouslySetInnerHTML` を使わない。** 本文から来る値は文字として置くか、名前で決めた属性
 * （`href` は mdtree の safeHref が照らしたもの）に入れるだけ。値を要素へ spread しない。
 * コールアウトの種類はクラスに入れず、決まった見出しの文字に写す。
 */

const calloutLabel: Record<string, string> = {
  note: 'メモ', info: '情報', warning: '注意', important: '重要', archive: '記録',
}

export default function Preview({ tree, targets }: { tree: Md[]; targets: Record<string, WikiTarget | undefined> }) {
  return <div className="md">{tree.map((n, i) => <Node key={i} n={n} targets={targets} />)}</div>
}

function Kids({ c, targets }: { c: Md[]; targets: Record<string, WikiTarget | undefined> }) {
  return <>{c.map((n, i) => <Node key={i} n={n} targets={targets} />)}</>
}

function Node({ n, targets }: { n: Md; targets: Record<string, WikiTarget | undefined> }) {
  switch (n.t) {
    case 'text': return <>{n.s}</>
    case 'br': return <br />
    case 'p': return <p><Kids c={n.c} targets={targets} /></p>
    case 'h': {
      const H = (['h1', 'h2', 'h3', 'h4', 'h5', 'h6'] as const)[Math.min(Math.max(n.level, 1), 6) - 1]
      return <H><Kids c={n.c} targets={targets} /></H>
    }
    case 'ul': return <ul><Kids c={n.c} targets={targets} /></ul>
    case 'ol': return <ol><Kids c={n.c} targets={targets} /></ol>
    case 'li': return <li><Kids c={n.c} targets={targets} /></li>
    case 'task':
      return <p className="md-task"><span aria-label={n.checked ? '済み' : '未'}>{n.checked ? '☑' : '☐'}</span>{' '}<Kids c={n.c} targets={targets} /></p>
    case 'quote': return <blockquote><Kids c={n.c} targets={targets} /></blockquote>
    case 'callout':
      return (
        <aside className="md-callout" role="note">
          <div className="md-callout-title">{calloutLabel[n.kind] ?? 'メモ'}{n.title && <>: {n.title}</>}</div>
          <Kids c={n.c} targets={targets} />
        </aside>
      )
    case 'code': return <pre className="md-code"><code>{n.s}</code></pre>
    case 'icode': return <code>{n.s}</code>
    case 'hr': return <hr />
    case 'em': return <em><Kids c={n.c} targets={targets} /></em>
    case 'strong': return <strong><Kids c={n.c} targets={targets} /></strong>
    case 'del': return <del><Kids c={n.c} targets={targets} /></del>
    case 'table':
      return <div className="md-table"><table><tbody><Kids c={n.c} targets={targets} /></tbody></table></div>
    case 'tr': return <tr><Kids c={n.c} targets={targets} /></tr>
    case 'th': return <th><Kids c={n.c} targets={targets} /></th>
    case 'td': return <td><Kids c={n.c} targets={targets} /></td>
    case 'link':
      return <a href={n.href} target="_blank" rel="noopener noreferrer"><Kids c={n.c} targets={targets} /></a>
    case 'image':
      // **読み込まない。** 意味だけ出す。
      return (
        <span className="md-image">
          画像: {n.alt || '（説明なし）'}
          {n.href ? <> （<a href={n.href} target="_blank" rel="noopener noreferrer">{n.src}</a>）</> : n.src && <> （{n.src}）</>}
        </span>
      )
    case 'wiki': return <Wiki n={n} targets={targets} />
    case 'fm':
      return (
        <table className="md-fm"><tbody>
          {n.rows.map((r, i) => <tr key={i}><th>{r.key}</th><td>{r.value}</td></tr>)}
        </tbody></table>
      )
  }
}

function Wiki({ n, targets }: { n: Extract<Md, { t: 'wiki' }>; targets: Record<string, WikiTarget | undefined> }) {
  const label = n.alias || (n.target || '') + (n.frag ? '#' + n.frag : '')
  const t = targets[n.target]
  const prefix = n.embed ? '埋め込み: ' : ''
  if (!t) return <span className="md-wiki muted">{prefix}{label}</span>
  if (!t.to_id) return <span className="md-wiki md-missing" title="索引の時点で無いノート">{prefix}{label}（無い）</span>
  return (
    <span className="md-wiki">
      {prefix}<Link to={`/notes/${t.to_id}`} title={t.to_path}>{label}</Link>
      {t.ambiguous && <span className="muted" title={(t.candidates ?? []).join('\n')}>（候補 {t.candidates?.length ?? 0}）</span>}
    </span>
  )
}

/** wikiTargets は木に出てくる wikilink の宛先（重複なし）。 */
export function wikiTargets(tree: Md[]): string[] {
  const out = new Set<string>()
  const walk = (ns: Md[]) => {
    for (const n of ns) {
      if (n.t === 'wiki') out.add(n.target)
      if ('c' in n) walk(n.c)
    }
  }
  walk(tree)
  return [...out]
}
