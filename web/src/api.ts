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

export type AuditEntry = {
  id: number; at: string; actor: string; action: string
  target?: string; session_id?: string; detail?: string
  outcome: string; hash: string
}
export type Block = { kind: string; tool_name?: string; text?: string }
// 「ここに何かあったが消した」。値は入らない。
export type Redaction = {
  at: string; reason: string; actor: string
  bytes_removed: number; recoverable: boolean
}
export type Message = {
  id: number; uuid?: string; parent_uuid?: string; type: string; role?: string
  timestamp?: string; model?: string; blocks?: Block[]; redacted?: Redaction
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

export type VaultInfo = {
  id: number; host: string; name: string; root: string
  notes: number; missing: number; scanned_at?: string
}

export type Note = {
  id: number; vault_id: number; path: string; title: string; kind: string
  size: number; mtime?: string; missing_at?: string
  links: number; backlinks: number; touches: number
}

export type Ref = {
  from_id: number; from_path: string
  to_id?: number; to_path?: string
  target: string; alias?: string; frag?: string
  embed: boolean; resolved: boolean; ambiguous: boolean
  candidates?: string[]; line?: number
}

export type NoteTouch = {
  session_id: string; title?: string; op: string; origin: string
  at: string; note_path: string; note_id?: number; backup_id?: number
}

export type GhostVersion = {
  backup_id: number; version: number; at?: string; size: number; session_id?: string
}

export type Ghost = {
  path: string; abs_path: string; reason: string
  touches: number; sessions: number; last_at: string; backups: number
  versions?: GhostVersion[]
}

export type ViewInfo = {
  base: string; name: string; kind: string; id: string
  rows: number; columns: number; pinned: number; error?: string
}

export type ViewColumn = {
  key: string; label: string; formula?: boolean; pinned?: boolean
  numeric?: boolean; filled: number
}

export type ViewRow = { note_id: number; path: string; name: string; cells: Record<string, string> }

export type ViewGroup = { key: string; rows: ViewRow[]; summary?: Record<string, number> }

export type ViewResult = {
  view: string; kind: string
  columns: ViewColumn[]; groups: ViewGroup[]
  total: number; summary?: Record<string, number>; warnings?: string[]
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


// ---- Phase 3: Camp が起こしたセッション ------------------------------------

export type RuntimeSession = {
  id: string; claude_id?: string; cwd: string; state: string
  requested_by: string; created_at: string; updated_at: string
  pid?: number; scope?: string
  exit_code?: number; exit_reason?: string; ended_at?: string
}

export type RuntimeList = { agent_connected: boolean; sessions: RuntimeSession[] }

export type LogLine = { seq: number; at: string; kind: string; frame?: unknown }

export type Approval = {
  id: number; session_id: string; request_id: string
  tool?: string; detail?: string; asked_at: string; expires_at: string
  answered_at?: string; behavior?: string; reason?: string
}

export type Allowed = {
  id: number; path: string; note?: string; added_at: string; added_by: string
}

export type Destination = {
  id: number; alias: string; hostname?: string; user?: string; port?: number
  identity?: string; tailscale_ip?: string; note?: string
  allowed: boolean; source: string; seen_at: string; updated_at: string
}

/** `get_usage` の中身。実測で出た欄だけを写している（2026-09-04）。 */
export type PlanLimit = {
  kind: string; group?: string; percent: number
  resets_at?: string; severity?: string; is_active?: boolean
}
export type ModelUsage = {
  inputTokens: number; outputTokens: number
  cacheReadInputTokens: number; cacheCreationInputTokens: number
  thinkingTokens: number; costUSD: number; contextWindow?: number
}
export type UsagePayload = {
  subscription_type?: string
  rate_limits?: { limits?: PlanLimit[] }
  session?: {
    total_cost_usd?: number; total_duration_ms?: number
    total_lines_added?: number; total_lines_removed?: number
    model_usage?: Record<string, ModelUsage>
  }
}
export type ContextPayload = {
  categories?: { name: string; tokens: number }[]
  totalTokens?: number; maxTokens?: number; percentage?: number
}
export type RuntimeUsage = {
  usage?: UsagePayload; usage_error?: string
  context?: ContextPayload; context_error?: string
  running: number; max: number; warning?: string
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

/** POST も 401 の扱いを揃える。**画面ごとに書かない。** */
async function postJSON<T>(path: string, body: unknown): Promise<T> {
  const res = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify(body ?? {}),
  })
  if (res.status === 401 && !(body as { password?: string })?.password) {
    location.href = '/login'
    throw new Error('未認証')
  }
  if (!res.ok) {
    const b = await res.json().catch(() => ({}))
    throw new Error(b.error ?? `${res.status} ${res.statusText}`)
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

  vaults: () => fetchJSON<VaultInfo[]>('/api/vaults'),

  audit: (o: { session?: string; action?: string; n?: number } = {}) =>
    fetchJSON<{ audit: AuditEntry[] }>(
      '/api/audit?' + new URLSearchParams({
        ...(o.session ? { session: o.session } : {}),
        ...(o.action ? { action: o.action } : {}),
        ...(o.n ? { limit: String(o.n) } : {}),
      })).then((r) => r.audit),

  views: () => fetchJSON<ViewInfo[]>('/api/views'),
  view: (id: string) =>
    fetchJSON<ViewResult>('/api/views/' + id.split('/').map(encodeURIComponent).join('/')),

  // 本文は blobs から返る。ノートが Vault から消えていても読める。
  noteBody: async (id: number) => {
    const res = await fetch(`/api/notes/${id}/body`)
    if (res.status === 401) { location.href = '/login'; throw new Error('未認証') }
    if (res.status === 404) return null
    if (!res.ok) throw new Error(`${res.status} ${res.statusText}`)
    return res.text()
  },

  notes: (o: { vault?: number; folder?: string; kind?: string; q?: string; missing?: string; n?: number } = {}) =>
    fetchJSON<Note[]>('/api/notes' + qs(o)),

  note: (id: number) => fetchJSON<Note>(`/api/notes/${id}`),

  noteLinks: (id: number) =>
    fetchJSON<{ out: Ref[]; back: Ref[] }>(`/api/notes/${id}/links`),

  noteSessions: (id: number) => fetchJSON<NoteTouch[]>(`/api/notes/${id}/sessions`),

  ghosts: () => fetchJSON<Ghost[]>('/api/vault/ghosts'),

  // 消えたパスの中身。file-history から捕獲した実体を blobs から返す。
  backupContent: async (id: number) => {
    const res = await fetch(`/api/backups/${id}/content`)
    if (res.status === 401) { location.href = '/login'; throw new Error('未認証') }
    if (!res.ok) throw new Error(`${res.status} ${res.statusText}`)
    return res.text()
  },

  vaultIssues: () =>
    fetchJSON<{ ambiguous: Ref[]; dangling: Ref[] }>('/api/vault/issues'),

  runtime: () => fetchJSON<RuntimeList>('/api/runtime'),
  runtimeStart: (cwd: string) => postJSON<RuntimeSession>('/api/runtime', { cwd }),
  runtimeInput: (id: string, text: string) =>
    postJSON<{ ok: boolean }>(`/api/runtime/${encodeURIComponent(id)}/input`, { text }),
  runtimeStop: (id: string, mode: 'interrupt' | 'terminate') =>
    postJSON<{ ok: boolean }>(`/api/runtime/${encodeURIComponent(id)}/stop`, { mode }),
  runtimeApprove: (id: string, request_id: string, behavior: 'allow' | 'deny', message = '') =>
    postJSON<{ ok: boolean }>(`/api/runtime/${encodeURIComponent(id)}/approve`,
      { request_id, behavior, message }),
  runtimeApprovals: (id: string, all = false) =>
    fetchJSON<Approval[]>(`/api/runtime/${encodeURIComponent(id)}/approvals` + qs({ all: all ? 1 : 0 })),
  runtimeLog: (id: string, since = 0, limit = 200) =>
    fetchJSON<{ lines: LogLine[]; gap: boolean; newest: number; dropped: number }>(
      `/api/runtime/${encodeURIComponent(id)}/log` + qs({ since, limit })),
  runtimeUsage: (id: string) =>
    fetchJSON<RuntimeUsage>(`/api/runtime/${encodeURIComponent(id)}/usage`),

  allowlist: () => fetchJSON<Allowed[]>('/api/allowlist'),
  allowlistAdd: (path: string, password: string, note = '') =>
    postJSON<Allowed>('/api/allowlist', { path, password, note }),
  allowlistRemove: (path: string, password: string) =>
    postJSON<{ removed: boolean }>('/api/allowlist/remove', { path, password }),

  sshHosts: () => fetchJSON<Destination[]>('/api/ssh'),
  sshScan: () => postJSON<{ added: number; updated: number }>('/api/ssh/scan', {}),
  sshEdit: (alias: string, note: string, tailscale_ip: string) =>
    postJSON<{ ok: boolean }>(`/api/ssh/${encodeURIComponent(alias)}/edit`, { note, tailscale_ip }),
  sshAllow: (alias: string, allowed: boolean, password: string) =>
    postJSON<{ ok: boolean }>(`/api/ssh/${encodeURIComponent(alias)}/allow`, { allowed, password }),

  logout: async () => {
    await fetch('/api/logout', { method: 'POST' })
    location.href = '/login'
  },
}
