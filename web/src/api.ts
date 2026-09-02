// api.ts — campd の HTTP 面をそのまま写したもの。
//
// 未認証は 401 が返る。画面側では握りつぶさず /login へ送る（fetchJSON）。

export type Host = {
  id: number; name: string; sessions: number; messages: number; last_seen_at?: string
}

export type Project = {
  id: number; host: string; repo_path: string; name: string
  worktree_name?: string; git_origin?: string; sessions: number; last_seen_at?: string
}

export type Session = {
  id: string; host: string; project: string; repo_path: string; agent: string
  title: string; first_message?: string; git_branch?: string; model?: string
  started_at: string; updated_at: string
  messages: number; conversation: number; cost_usd?: number; is_sidechain?: boolean
}

export type Block = { kind: string; tool_name?: string; text?: string }
export type Message = {
  id: number; uuid?: string; parent_uuid?: string; type: string; role?: string
  timestamp?: string; model?: string; blocks?: Block[]
}

export type Hit = {
  block_id: number; message_id: number; session_id: string; title: string
  kind: string; tool_name?: string; timestamp: string; snippet: string; score: number
}

export type UsageRow = {
  key: string; label?: string; requests: number
  input_tokens: number; output_tokens: number
  cache_creation_tokens: number; cache_read_tokens: number; thinking_tokens: number
}

export type Window = {
  id: number; agent: string; kind: string
  started_at?: string; ends_at?: string
  used_pct: number; peak_pct: number; samples: number
  source: string; fetched_at: string; current: boolean
}

export type Touch = {
  abs_path: string; rel_path?: string; op: string; origin: string; at: string
  session_id: string; message_uuid?: string; title?: string; backup_name?: string
}

export type Backup = {
  id: number; abs_path: string; rel_path?: string; version: number
  backup_time: string; session_id: string; title?: string
  backup_name: string; origin: string; size: number; missing_at?: string
}

/** 認証が切れていたらログイン画面へ送る。画面ごとに書かない。 */
async function fetchJSON<T>(path: string): Promise<T> {
  const res = await fetch(path, { headers: { Accept: 'application/json' } })
  if (res.status === 401) {
    location.href = '/login'
    throw new Error('未認証')
  }
  if (!res.ok) {
    const body = await res.json().catch(() => ({}))
    throw new Error(body.error ?? `${res.status} ${res.statusText}`)
  }
  return res.json() as Promise<T>
}

const qs = (o: Record<string, string | number | undefined>) => {
  const p = new URLSearchParams()
  for (const [k, v] of Object.entries(o)) {
    if (v !== undefined && v !== '' && v !== 0) p.set(k, String(v))
  }
  const s = p.toString()
  return s ? `?${s}` : ''
}

export const api = {
  hosts: () => fetchJSON<Host[]>('/api/hosts'),
  projects: (host?: string) => fetchJSON<Project[]>('/api/projects' + qs({ host })),

  sessions: (o: {
    host?: string; project?: string; agent?: string; q?: string
    cursor?: string; limit?: number; empty?: string
  }) => fetchJSON<{ sessions: Session[]; next_cursor: string }>('/api/sessions' + qs(o)),

  session: (id: string) => fetchJSON<Session>(`/api/sessions/${encodeURIComponent(id)}`),

  // all=1 で制御行（mode / permission-mode / bridge-session など）も出す。
  // 既定は会話行だけ。実データでは制御行のほうが多い。
  messages: (id: string, after = 0, limit = 100, all = false) =>
    fetchJSON<{ messages: Message[]; next_after: number }>(
      `/api/sessions/${encodeURIComponent(id)}/messages` +
        qs({ after, limit, all: all ? '1' : '' })),

  search: (q: string, o: { kind?: string; session?: string; limit?: number } = {}) =>
    fetchJSON<Hit[]>('/api/search' + qs({ q, ...o })),

  usage: (by: string, limit = 60) => fetchJSON<UsageRow[]>('/api/usage/summary' + qs({ by, limit })),

  windows: (o: { current?: boolean; kind?: string; n?: number } = {}) =>
    fetchJSON<Window[]>('/api/usage/windows' + qs({
      current: o.current ? '1' : '', kind: o.kind, n: o.n,
    })),

  files: (o: { path?: string; session?: string; op?: string; limit?: number }) =>
    fetchJSON<Touch[]>('/api/files' + qs(o)),

  backups: (o: { path?: string; session?: string; limit?: number }) =>
    fetchJSON<Backup[]>('/api/backups' + qs(o)),

  logout: async () => {
    await fetch('/api/logout', { method: 'POST' })
    location.href = '/login'
  },
}
