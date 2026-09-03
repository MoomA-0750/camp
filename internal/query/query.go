// Package query は一覧・詳細の読み取りをまとめる。
// HTTP からも、のちのMCPサーバーからも同じものを使う。
package query

import (
	"strconv"
	"strings"

	"github.com/MoomA-0750/camp/internal/store"
)

// Host は1ホスト。
type Host struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Sessions int    `json:"sessions"`
	Messages int    `json:"messages"`
	LastSeen string `json:"last_seen_at,omitempty"`
}

func Hosts(db *store.DB) ([]Host, error) {
	rows, err := db.Query(`
		select h.id, h.name, count(distinct s.id), coalesce(sum(s.message_count), 0),
		       coalesce(max(s.updated_at), '')
		  from hosts h left join sessions s on s.host_id = h.id
		 group by h.id, h.name order by h.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Host{}
	for rows.Next() {
		var h Host
		if err := rows.Scan(&h.ID, &h.Name, &h.Sessions, &h.Messages, &h.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Project は1プロジェクト。
type Project struct {
	ID        int64  `json:"id"`
	Host      string `json:"host"`
	RepoPath  string `json:"repo_path"`
	Name      string `json:"name"`
	Worktree  string `json:"worktree_name,omitempty"`
	GitOrigin string `json:"git_origin,omitempty"`
	Sessions  int    `json:"sessions"`
	LastSeen  string `json:"last_seen_at,omitempty"`
}

func Projects(db *store.DB, host string) ([]Project, error) {
	rows, err := db.Query(`
		select p.id, h.name, p.repo_path, p.name, coalesce(p.worktree_name, ''),
		       coalesce(p.git_origin, ''), count(s.id), coalesce(max(s.updated_at), '')
		  from projects p
		  join hosts h on h.id = p.host_id
		  left join sessions s on s.project_id = p.id
		 where (? = '' or h.name = ?)
		 group by p.id order by max(s.updated_at) desc nulls last, p.name`, host, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Host, &p.RepoPath, &p.Name, &p.Worktree,
			&p.GitOrigin, &p.Sessions, &p.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SessionOpts は一覧の絞り込み。
type SessionOpts struct {
	Host    string
	Project string // repo_path の部分一致
	Agent   string
	Q       string // タイトル・最初の発話の部分一致
	From    string
	To      string
	// Empty が false のとき、会話が1件も無いセッション（起動しただけ）を隠す。
	Empty  bool
	Limit  int
	Cursor string // 前ページ最後の updated_at
}

// Session は一覧に出す1セッション。
type Session struct {
	ID           string  `json:"id"`
	Host         string  `json:"host"`
	Project      string  `json:"project"`
	RepoPath     string  `json:"repo_path"`
	Agent        string  `json:"agent"`
	Title        string  `json:"title"`
	FirstMessage string  `json:"first_message,omitempty"`
	Branch       string  `json:"git_branch,omitempty"`
	Model        string  `json:"model,omitempty"`
	StartedAt    string  `json:"started_at"`
	UpdatedAt    string  `json:"updated_at"`
	Messages     int     `json:"messages"`
	Conversation int     `json:"conversation"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	Sidechain    bool    `json:"is_sidechain,omitempty"`
}

const sessionSelect = `
	select s.id, h.name, p.name, p.repo_path, s.agent,
	       coalesce(nullif(s.ai_title, ''), nullif(s.user_title, ''), ''),
	       coalesce(s.first_user_message, ''), coalesce(s.git_branch, ''),
	       coalesce(s.last_model, ''), s.started_at, s.updated_at,
	       s.message_count, s.conversation_count, coalesce(s.total_cost_usd, 0),
	       s.is_sidechain
	  from sessions s
	  join hosts h on h.id = s.host_id
	  join projects p on p.id = s.project_id`

// Sessions は新しい順に返す。cursor は前ページ最後の updated_at。
func Sessions(db *store.DB, o SessionOpts) ([]Session, error) {
	if o.Limit <= 0 || o.Limit > 500 {
		o.Limit = 50
	}
	empty := 0
	if o.Empty {
		empty = 1
	}
	rows, err := db.Query(sessionSelect+`
		 where (? = '' or h.name = ?)
		   and (? = '' or instr(p.repo_path, ?) > 0)
		   and (? = '' or s.agent = ?)
		   and (? = '' or instr(lower(coalesce(s.ai_title, '') || ' ' ||
		                              coalesce(s.user_title, '') || ' ' ||
		                              coalesce(s.first_user_message, '')), lower(?)) > 0)
		   and (? = '' or s.updated_at >= ?)
		   and (? = '' or s.updated_at <= ?)
		   and (? = 1 or s.conversation_count > 0)
		   and (? = '' or s.updated_at < ?)
		 order by s.updated_at desc, s.id
		 limit ?`,
		o.Host, o.Host, o.Project, o.Project, o.Agent, o.Agent, o.Q, o.Q,
		o.From, o.From, o.To, o.To, empty, o.Cursor, o.Cursor, o.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSessions(rows)
}

// One は1セッションを返す。見つからなければ nil。
func One(db *store.DB, id string) (*Session, error) {
	rows, err := db.Query(sessionSelect+` where s.id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := scanSessions(rows)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

type scanner interface {
	Next() bool
	Scan(...any) error
	Err() error
}

func scanSessions(rows scanner) ([]Session, error) {
	out := []Session{}
	for rows.Next() {
		var s Session
		var side int
		if err := rows.Scan(&s.ID, &s.Host, &s.Project, &s.RepoPath, &s.Agent, &s.Title,
			&s.FirstMessage, &s.Branch, &s.Model, &s.StartedAt, &s.UpdatedAt,
			&s.Messages, &s.Conversation, &s.CostUSD, &side); err != nil {
			return nil, err
		}
		s.Sidechain = side != 0
		s.FirstMessage = clip(s.FirstMessage, 300)
		out = append(out, s)
	}
	return out, rows.Err()
}

// Message は本文を持つ1行。
type Message struct {
	ID        int64   `json:"id"`
	UUID      string  `json:"uuid,omitempty"`
	Parent    string  `json:"parent_uuid,omitempty"`
	Type      string  `json:"type"`
	Role      string  `json:"role,omitempty"`
	Timestamp string  `json:"timestamp,omitempty"`
	Model     string  `json:"model,omitempty"`
	Blocks    []Block `json:"blocks,omitempty"`

	// Redacted は消したメッセージに入る。**空ブロックと区別できるようにする。**
	// ブロックが0本のメッセージは他にもある（本文が空の thinking など）ので、
	// 「何も無い」と「あったが消した」を画面で見分けられないと、
	// 消したことが見えない削除になる。
	Redacted *Redaction `json:"redacted,omitempty"`
}

// Redaction は「ここに何かあったが消した」の中身。値は入れない。
type Redaction struct {
	At           string `json:"at"`
	Reason       string `json:"reason"`
	Actor        string `json:"actor"`
	BytesRemoved int64  `json:"bytes_removed"`
	Recoverable  bool   `json:"recoverable"`
}

// Block は本文のかたまり。索引に入っているものと同じ切り方。
type Block struct {
	Kind     string `json:"kind"`
	ToolName string `json:"tool_name,omitempty"`
	Text     string `json:"text,omitempty"`
}

// Messages はセッションの本文を、ファイル内の並び順で返す。
// after は前ページ最後の messages.id。
//
// all が false のときは会話行（user / assistant）だけを返す。既定をこちらに
// するのは、実データでは制御行のほうが多いため。実測（session 77a524b0）:
// 2,825行のうち 1,505行が mode / permission-mode / bridge-session /
// last-prompt / ai-title などで、既定で全部出すと会話が埋もれて読めない。
// 判定は sessions.conversation_count と同じにする。ヘッダの数と中身が
// 食い違うと、どちらが嘘なのか分からなくなる。
func Messages(db *store.DB, sessionID string, after int64, limit int, all bool) ([]Message, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	everything := 0
	if all {
		everything = 1
	}
	rows, err := db.Query(`
		select m.id, coalesce(m.uuid, ''), coalesce(m.parent_uuid, ''), m.type,
		       coalesce(m.role, ''), coalesce(m.timestamp, ''), coalesce(m.model, '')
		  from messages m
		 where m.session_id = ? and m.id > ?
		   and (? = 1 or m.type in ('user','assistant'))
		 order by m.source_file_id, m.byte_offset
		 limit ?`, sessionID, after, everything, limit)
	if err != nil {
		return nil, err
	}
	var out []Message
	var ids []any
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.UUID, &m.Parent, &m.Type, &m.Role,
			&m.Timestamp, &m.Model); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
		ids = append(ids, m.ID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(out) == 0 {
		return []Message{}, nil
	}

	// SetMaxOpenConns(1) なので、上の Rows を閉じてから本文を引く。
	q := `select message_id, kind, coalesce(tool_name, ''), coalesce(text, '')
	        from message_blocks where message_id in (?` + strings.Repeat(",?", len(ids)-1) + `)
	       order by message_id, idx`
	brows, err := db.Query(q, ids...)
	if err != nil {
		return nil, err
	}
	defer brows.Close()
	byID := make(map[int64]int, len(out))
	for i := range out {
		byID[out[i].ID] = i
	}
	for brows.Next() {
		var mid int64
		var b Block
		if err := brows.Scan(&mid, &b.Kind, &b.ToolName, &b.Text); err != nil {
			return nil, err
		}
		if i, ok := byID[mid]; ok {
			out[i].Blocks = append(out[i].Blocks, b)
		}
	}
	if err := brows.Err(); err != nil {
		return nil, err
	}
	brows.Close()

	if err := attachRedactions(db, out, byID, ids); err != nil {
		return nil, err
	}
	return out, nil
}

// attachRedactions は消したメッセージに印を付ける。
func attachRedactions(db *store.DB, out []Message, byID map[int64]int, ids []any) error {
	q := `select ref, redacted_at, reason, actor, bytes_removed, recoverable
	        from tombstones
	       where kind = 'message.raw_json'
	         and cast(ref as integer) in (?` + strings.Repeat(",?", len(ids)-1) + `)`
	rows, err := db.Query(q, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ref string
		var r Redaction
		var rec int
		if err := rows.Scan(&ref, &r.At, &r.Reason, &r.Actor, &r.BytesRemoved, &rec); err != nil {
			return err
		}
		r.Recoverable = rec == 1
		id, err := strconv.ParseInt(ref, 10, 64)
		if err != nil {
			continue
		}
		if i, ok := byID[id]; ok {
			out[i].Redacted = &r
		}
	}
	return rows.Err()
}

// UsageRow は使用量の1行。by で意味が変わる。
type UsageRow struct {
	Key           string  `json:"key"`
	Label         string  `json:"label,omitempty"`
	Requests      int     `json:"requests"`
	Input         int64   `json:"input_tokens"`
	Output        int64   `json:"output_tokens"`
	CacheCreation int64   `json:"cache_creation_tokens"`
	CacheRead     int64   `json:"cache_read_tokens"`
	Thinking      int64   `json:"thinking_tokens"`
	CostUSD       float64 `json:"cost_usd,omitempty"`
}

// UsageSummary は day / model / session / project のいずれかで畳んで返す。
func UsageSummary(db *store.DB, by, from, to string, limit int) ([]UsageRow, error) {
	if limit <= 0 || limit > 2000 {
		limit = 200
	}
	var key, label, join string
	switch by {
	case "", "day":
		key, label = "u.day", "u.day"
	case "model":
		key, label = "u.model", "u.model"
	case "session":
		key = "u.session_id"
		label = `coalesce(nullif(s.ai_title, ''), nullif(s.user_title, ''), u.session_id)`
		join = " join sessions s on s.id = u.session_id"
	case "project":
		key, label = "cast(u.project_id as text)", "p.name"
		join = " join projects p on p.id = u.project_id"
	default:
		return nil, ErrBadBy
	}

	rows, err := db.Query(`
		select `+key+`, `+label+`, count(*),
		       sum(u.input_tokens), sum(u.output_tokens),
		       sum(u.cache_creation_input_tokens), sum(u.cache_read_input_tokens),
		       sum(u.thinking_tokens)
		  from usage u`+join+`
		 where (? = '' or u.day >= ?) and (? = '' or u.day <= ?)
		 group by 1, 2 order by 1 desc limit ?`, from, from, to, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageRow{}
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Key, &r.Label, &r.Requests, &r.Input, &r.Output,
			&r.CacheCreation, &r.CacheRead, &r.Thinking); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// 多バイト文字を割らない。
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}
