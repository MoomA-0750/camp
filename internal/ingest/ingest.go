package ingest

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/MoomA-0750/camp/internal/store"
)

// Result は1回の取り込みの結果。
type Result struct {
	Host        string
	Root        string
	RoleCounts  map[string]int
	Projects    int
	CWDs        int
	Sessions    int
	Runs        int
	SessionRuns int
	SourceFiles int
	Messages    int
	Reread      int      // (dev,inode,size) の食い違いで先頭から取り直したファイル
	Orphans     []string // 親が見つからず stub に落としたサイドカー候補
}

// Ingest は root 以下の会話記録を DB に取り込む。再実行しても重複しない。
func Ingest(db *store.DB, host, root string) (*Result, error) {
	corpus, err := Survey(root)
	if err != nil {
		return nil, err
	}

	res := &Result{Host: host, Root: root, RoleCounts: map[string]int{}}
	for _, f := range corpus.Files {
		res.RoleCounts[f.Role]++
	}

	hostID, err := upsertHost(db, host)
	if err != nil {
		return nil, err
	}

	// メタデータは1トランザクションで作る。セッションが半端に出来た状態で
	// messages を書き始めると外部キーで詰まるため、先にここを確定させる。
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	projectIDs, nProjects, err := writeProjects(tx, hostID, corpus)
	if err != nil {
		return nil, err
	}
	res.Projects = nProjects
	res.CWDs = len(projectIDs)

	sessions := groupBySession(corpus)
	if err := writeSessions(tx, hostID, projectIDs, corpus, sessions); err != nil {
		return nil, err
	}
	res.Sessions = len(sessions)

	fileIDs, reread, err := writeSourceFiles(tx, hostID, corpus, sessions)
	if err != nil {
		return nil, err
	}
	res.SourceFiles = len(fileIDs)
	res.Reread = reread

	nRuns, nLinks, err := writeRuns(tx, corpus, fileIDs)
	if err != nil {
		return nil, err
	}
	res.Runs, res.SessionRuns = nRuns, nLinks

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	// run の存在確認はセッション横断で行う。/clear で生まれたセッションの行が
	// 「別セッションが最初に登録した run」を指すのは正しい姿なので、
	// セッション単位で絞ると本物の run_id を落としてしまう。
	knownRuns, err := allRunIDs(db)
	if err != nil {
		return nil, err
	}

	// messages はファイル単位のトランザクションにする。オフセットの前進と
	// レコードの挿入が同じ tx に入るので、途中で落ちても取りこぼさない。
	for _, f := range corpus.Files {
		n, err := writeMessages(db, corpus, f, fileIDs, knownRuns)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Rel, err)
		}
		res.Messages += n
	}

	if err := refreshCounts(db); err != nil {
		return nil, err
	}
	return res, nil
}

func upsertHost(db *store.DB, name string) (int64, error) {
	if _, err := db.Exec(`insert into hosts(name) values(?) on conflict(name) do nothing`, name); err != nil {
		return 0, err
	}
	var id int64
	err := db.QueryRow(`select id from hosts where name = ?`, name).Scan(&id)
	return id, err
}

// writeProjects は行に現れた cwd をプロジェクトのルートへ畳んで登録する。
// マングルされたディレクトリ名はパースしない（4方向に曖昧で復元できない）。
//
// 返り値は cwd -> project_id。cwd はルートでないことのほうが多いので、
// セッションに割り当てるときは必ずこの表を通す。
func writeProjects(tx *sql.Tx, hostID int64, c *Corpus) (map[string]int64, int, error) {
	set := map[string]struct{}{}
	for _, f := range c.Files {
		for _, p := range f.CWDs {
			set[p] = struct{}{}
		}
	}
	cwds := make([]string, 0, len(set))
	for p := range set {
		cwds = append(cwds, p)
	}
	sort.Strings(cwds)

	assign, roots := ResolveProjects(cwds)

	// 親を先に入れる。parent_project_id が外部キーなので順序が要る。
	paths := make([]string, 0, len(roots))
	for p := range roots {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(i, j int) bool {
		a, b := roots[paths[i]], roots[paths[j]]
		if a.IsWorktree != b.IsWorktree {
			return !a.IsWorktree
		}
		return paths[i] < paths[j]
	})

	ids := map[string]int64{}
	for _, p := range paths {
		ref := roots[p]
		var parentID any
		if ref.ParentPath != "" {
			if id, ok := ids[ref.ParentPath]; ok {
				parentID = id
			}
		}
		if _, err := tx.Exec(`
			insert into projects(host_id, repo_path, name, git_origin, is_worktree, worktree_name, parent_project_id, is_repo)
			values(?,?,?,?,?,?,?,?)
			on conflict(host_id, repo_path) do update set
				name=excluded.name,
				git_origin=coalesce(excluded.git_origin, projects.git_origin),
				is_worktree=excluded.is_worktree,
				worktree_name=excluded.worktree_name,
				parent_project_id=coalesce(excluded.parent_project_id, projects.parent_project_id),
				is_repo=excluded.is_repo`,
			hostID, ref.Path, ref.Name, nz(ref.GitOrigin), b2i(ref.IsWorktree),
			nz(ref.WorktreeName), parentID, b2i(ref.IsRepo)); err != nil {
			return nil, 0, err
		}
		var id int64
		if err := tx.QueryRow(
			`select id from projects where host_id = ? and repo_path = ?`, hostID, ref.Path).Scan(&id); err != nil {
			return nil, 0, err
		}
		ids[ref.Path] = id
	}

	byCWD := map[string]int64{}
	for cwd, root := range assign {
		byCWD[cwd] = ids[root]
	}
	return byCWD, len(ids), nil
}

// groupBySession は sessions.id ごとに関係するファイルを集める。
// サイドカーは自分のセッションを持たず、親の一部として合流する。
func groupBySession(c *Corpus) map[string][]*FileSummary {
	g := map[string][]*FileSummary{}
	for _, f := range c.Files {
		key := f.SessionKey
		if f.Role == RoleSidecar {
			key = c.ParentSession(f)
		}
		if key == "" || f.Role == RoleEmpty {
			continue
		}
		g[key] = append(g[key], f)
	}
	for _, fs := range g {
		sort.Slice(fs, func(i, j int) bool { return fs[i].MTime.Before(fs[j].MTime) })
	}
	return g
}

func writeSessions(tx *sql.Tx, hostID int64, projectIDs map[string]int64, c *Corpus, g map[string][]*FileSummary) error {
	keys := make([]string, 0, len(g))
	for k := range g {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// 親セッションを先に作る。subagent の parent_session_id が外部キーなので、
	// 挿入順を間違えると弾かれる。
	sort.SliceStable(keys, func(i, j int) bool {
		return strings.Count(keys[i], ".") < strings.Count(keys[j], ".")
	})

	for _, key := range keys {
		files := g[key]
		primary := c.SessionFile(key)
		if primary == nil {
			primary = files[len(files)-1]
		}

		s := mergeSession(files)
		projectID, ok := projectIDs[s.cwdFirst]
		if !ok {
			return fmt.Errorf("session %s: cwd %q に対応する project が無い", key, s.cwdFirst)
		}

		var parentSession, parentAgent any
		if primary.Role == RoleSubagent {
			parentAgent = primary.AgentID
			// 親ファイルが既に消えている subagent もありうる。
			// その場合は親を張らずに独立した記録として残す（外部キーで落とさない）。
			if _, ok := g[primary.SessionID]; ok {
				parentSession = primary.SessionID
			}
		}

		_, err := tx.Exec(`
			insert into sessions(
				id, host_id, project_id, agent, parent_session_id, parent_agent_id,
				ai_title, first_user_message, last_prompt, last_prompt_leaf,
				git_branch, last_cwd, last_cli_version, last_mode, last_permission_mode,
				bridge_session_id, is_sidechain, started_at, updated_at, total_cost_usd)
			values(?,?,?,'claude-code',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			on conflict(id) do update set
				ai_title=excluded.ai_title,
				first_user_message=excluded.first_user_message,
				last_prompt=excluded.last_prompt,
				last_prompt_leaf=excluded.last_prompt_leaf,
				git_branch=excluded.git_branch,
				last_cwd=excluded.last_cwd,
				last_cli_version=excluded.last_cli_version,
				last_mode=excluded.last_mode,
				last_permission_mode=excluded.last_permission_mode,
				bridge_session_id=excluded.bridge_session_id,
				updated_at=excluded.updated_at,
				total_cost_usd=coalesce(excluded.total_cost_usd, sessions.total_cost_usd)`,
			key, hostID, projectID, parentSession, parentAgent,
			nz(s.aiTitle), nz(s.firstUser), nz(s.lastPrompt), nz(s.lastPromptLeaf),
			nz(s.gitBranch), nz(s.cwdLast), nz(s.cliVersion), nz(s.mode), nz(s.permissionMode),
			nz(s.bridge), b2i(primary.Role == RoleSubagent || primary.IsSidechain),
			s.startedAt, s.updatedAt, s.cost)
		if err != nil {
			return fmt.Errorf("session %s: %w", key, err)
		}
	}
	return nil
}

type sessionFields struct {
	cwdFirst, cwdLast            string
	startedAt, updatedAt         string
	aiTitle, firstUser           string
	lastPrompt, lastPromptLeaf   string
	gitBranch, cliVersion        string
	mode, permissionMode, bridge string
	cost                         any
}

// mergeSession は同じセッションに属するファイル群を1行に畳む。
// files は mtime 昇順。後から書かれたものが last_* を上書きする。
func mergeSession(files []*FileSummary) sessionFields {
	var s sessionFields
	for _, f := range files {
		st, up := f.Timestamps()
		if s.startedAt == "" || st < s.startedAt {
			s.startedAt = st
		}
		if up > s.updatedAt {
			s.updatedAt = up
		}
		if s.cwdFirst == "" {
			s.cwdFirst = f.FirstCWD
		}
		set(&s.cwdLast, f.LastCWD)
		set(&s.aiTitle, f.AITitle)
		if s.firstUser == "" {
			s.firstUser = f.FirstUserMessage
		}
		set(&s.lastPrompt, f.LastPrompt)
		set(&s.lastPromptLeaf, f.LastPromptLeaf)
		set(&s.gitBranch, f.GitBranch)
		set(&s.cliVersion, f.CLIVersion)
		set(&s.mode, f.Mode)
		set(&s.permissionMode, f.PermissionMode)
		set(&s.bridge, f.BridgeSessionID)
		if f.TotalCostUSD != nil {
			s.cost = *f.TotalCostUSD
		}
	}
	if s.cwdFirst == "" {
		s.cwdFirst = s.cwdLast
	}
	return s
}

func set(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

func nz(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// writeSourceFiles は物理ファイル層を作る。
// (dev, inode) が変わっていたら別の実体なので、オフセットを0に戻して取り直す。
func writeSourceFiles(tx *sql.Tx, hostID int64, c *Corpus, g map[string][]*FileSummary) (map[string]int64, int, error) {
	ids := map[string]int64{}
	reread := 0

	for _, f := range c.Files {
		// 0バイトのファイルは sessions を作っていない。存在しないセッションを
		// 指させると外部キーで落ちるので、実在するものだけ張る。
		sess := f.SessionKey
		if f.Role == RoleSidecar {
			sess = c.ParentSession(f)
		}
		if _, ok := g[sess]; !ok {
			sess = ""
		}

		var prevID, prevDev, prevInode, prevSize, prevOffset sql.NullInt64
		err := tx.QueryRow(
			`select id, dev, inode, size, ingested_offset from source_files where host_id = ? and path = ?`,
			hostID, f.Path).Scan(&prevID, &prevDev, &prevInode, &prevSize, &prevOffset)
		if err != nil && err != sql.ErrNoRows {
			return nil, 0, err
		}

		if prevID.Valid {
			rotated := prevDev.Int64 != f.Dev || prevInode.Int64 != f.Inode || f.Size < prevOffset.Int64
			if rotated {
				// 中身が入れ替わった。過去のレコードは残したまま先頭から読み直す。
				if _, err := tx.Exec(`update source_files set ingested_offset = 0 where id = ?`, prevID.Int64); err != nil {
					return nil, 0, err
				}
				reread++
			}
			if _, err := tx.Exec(`
				update source_files set role = ?, session_id = ?, agent_id = ?,
					dev = ?, inode = ?, size = ?, mtime = ?, missing_at = null
				where id = ?`,
				f.Role, nz(sess), nz(f.AgentID), f.Dev, f.Inode, f.Size,
				f.MTime.Format("2006-01-02T15:04:05.000000000Z07:00"), prevID.Int64); err != nil {
				return nil, 0, err
			}
			ids[f.Path] = prevID.Int64
			continue
		}

		r, err := tx.Exec(`
			insert into source_files(host_id, path, role, session_id, agent_id,
				dev, inode, size, mtime, ingested_offset, first_seen_at)
			values(?,?,?,?,?,?,?,?,?,0,?)`,
			hostID, f.Path, f.Role, nz(sess), nz(f.AgentID),
			f.Dev, f.Inode, f.Size,
			f.MTime.Format("2006-01-02T15:04:05.000000000Z07:00"),
			f.MTime.Format("2006-01-02T15:04:05.000000000Z07:00"))
		if err != nil {
			return nil, 0, fmt.Errorf("source_file %s: %w", f.Rel, err)
		}
		id, err := r.LastInsertId()
		if err != nil {
			return nil, 0, err
		}
		ids[f.Path] = id
	}
	return ids, reread, nil
}

// writeRuns は行に現れた session_id を実行（run）として登録し、
// セッションとの対応を session_runs に張る。
//
// run とセッションは多対多なので、run に「持ち主のセッション」を1つ選ばせない。
// /clear は同じ run のまま新しい会話ファイルへ移るため、選ばせようとすると
// 8セッションのうち1つを恣意的に選ぶことになる。
func writeRuns(tx *sql.Tx, c *Corpus, fileIDs map[string]int64) (runs, links int, err error) {
	sidecarByRun := map[string]int64{}
	for _, f := range c.Files {
		if f.Role == RoleSidecar {
			sidecarByRun[f.SessionID] = fileIDs[f.Path]
		}
	}

	// セッションごとに run の出現順を集める。同じセッションが複数ファイル
	// （main ＋ サイドカー）に分かれることがあるので、mtime 昇順に見る。
	files := append([]*FileSummary(nil), c.Files...)
	sort.Slice(files, func(i, j int) bool {
		if !files[i].MTime.Equal(files[j].MTime) {
			return files[i].MTime.Before(files[j].MTime)
		}
		return files[i].Path < files[j].Path
	})

	seqOf := map[string]map[string]int{} // sessionKey -> runID -> seq
	order := []struct{ sess, run string }{}
	seenRun := map[string]struct{}{}

	for _, f := range files {
		sess := f.SessionKey
		if f.Role == RoleSidecar {
			sess = c.ParentSession(f)
		}
		if sess == "" {
			continue
		}
		for _, rid := range f.RunIDs {
			if _, ok := seqOf[sess]; !ok {
				seqOf[sess] = map[string]int{}
			}
			if _, dup := seqOf[sess][rid]; dup {
				continue
			}
			seqOf[sess][rid] = len(seqOf[sess])
			order = append(order, struct{ sess, run string }{sess, rid})
		}
	}

	for _, o := range order {
		if _, done := seenRun[o.run]; !done {
			seenRun[o.run] = struct{}{}
			var sidecarID any
			if id, ok := sidecarByRun[o.run]; ok {
				sidecarID = id
			}
			f := c.SessionFile(o.sess)
			var ver, cwd, mode, perm any
			if f != nil {
				ver, cwd, mode, perm = nz(f.CLIVersion), nz(f.FirstCWD), nz(f.Mode), nz(f.PermissionMode)
			}
			if _, err := tx.Exec(`
				insert into runs(id, sidecar_file_id, cli_version, cwd, mode, permission_mode)
				values(?,?,?,?,?,?)
				on conflict(id) do update set
					sidecar_file_id = coalesce(excluded.sidecar_file_id, runs.sidecar_file_id)`,
				o.run, sidecarID, ver, cwd, mode, perm); err != nil {
				return 0, 0, fmt.Errorf("run %s: %w", o.run, err)
			}
			runs++
		}
		if _, err := tx.Exec(`
			insert into session_runs(session_id, run_id, seq) values(?,?,?)
			on conflict(session_id, run_id) do update set seq = excluded.seq`,
			o.sess, o.run, seqOf[o.sess][o.run]); err != nil {
			return 0, 0, fmt.Errorf("session_run %s/%s: %w", o.sess, o.run, err)
		}
		links++
	}
	return runs, links, nil
}

// writeMessages は1ファイルぶんのレコードを書き、オフセットを進める。
// 挿入とオフセット更新を同じトランザクションに入れるのが肝。
func writeMessages(db *store.DB, c *Corpus, f *FileSummary, fileIDs map[string]int64, knownRuns map[string]struct{}) (int, error) {
	if f.Role == RoleEmpty {
		return 0, nil
	}
	fileID := fileIDs[f.Path]

	var offset int64
	if err := db.QueryRow(`select ingested_offset from source_files where id = ?`, fileID).Scan(&offset); err != nil {
		return 0, err
	}
	if offset >= f.EndOffset {
		return 0, nil // 新しいバイトは無い
	}

	sessID := f.SessionKey
	if f.Role == RoleSidecar {
		sessID = c.ParentSession(f)
	}
	if sessID == "" {
		return 0, fmt.Errorf("セッションが決まらない（role=%s）", f.Role)
	}

	// このファイルに対応する run。サイドカーは自分自身が run。
	ownRun := ""
	if f.Role == RoleSidecar {
		ownRun = f.SessionID
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		insert into messages(
			uuid, session_id, run_id, source_file_id, byte_offset,
			parent_uuid, logical_parent_uuid, type, subtype, role, timestamp,
			cwd, cli_version, is_sidechain, agent_id, is_meta, is_compact_summary,
			is_api_error, api_message_id, request_id, model, service_tier, effort,
			degraded, raw_json)
		values(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		on conflict(source_file_id, byte_offset) do nothing`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	n := 0
	var end int64
	res, walkErr := WalkFile(f.Path, func(l *Line) error {
		if l.Offset < offset {
			return nil
		}
		runID := ownRun
		if l.RunID != "" {
			runID = l.RunID
		}
		if _, ok := knownRuns[runID]; !ok {
			runID = "" // 外部キーの無い run は付けない
		}

		var apiID, model, tier string
		if l.Message != nil {
			apiID, model = l.Message.ID, l.Message.Model
			if l.Message.Usage != nil {
				tier = l.Message.Usage.ServiceTier
			}
		}
		role := ""
		if l.Message != nil {
			role = l.Message.Role
		}

		_, err := stmt.Exec(
			nz(l.UUID), sessID, nz(runID), fileID, l.Offset,
			nz(l.ParentUUID), nz(l.LogicalParentUUID), l.Type, nz(l.Subtype), nz(role), nz(l.Timestamp),
			nz(l.CWD), nz(l.CLIVersion), b2i(l.IsSidechain), nz(l.AgentID), b2i(l.IsMeta),
			b2i(l.IsCompactSummary), b2i(l.IsAPIErrorMessage), nz(apiID), nz(l.RequestID),
			nz(model), nz(tier), nz(l.Effort), b2i(l.Degraded), l.Raw)
		if err != nil {
			return err
		}
		n++
		return nil
	})
	if walkErr != nil {
		return 0, walkErr
	}
	end = res.EndOffset

	var pending any
	if len(res.Pending) > 0 {
		pending = res.Pending
	}
	if _, err := tx.Exec(
		`update source_files set ingested_offset = ?, pending_tail = ? where id = ?`,
		end, pending, fileID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

func allRunIDs(db *store.DB) (map[string]struct{}, error) {
	rows, err := db.Query(`select id from runs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// refreshCounts は messages から件数と run の期間を数え直す。
// 集計を挿入時に足し込むと、再実行や取り直しでずれる。
//
// conversation_count は user/assistant だけを数える。これが0のセッションは
// 起動しただけで何も話していないので、一覧から外せる。
func refreshCounts(db *store.DB) error {
	if _, err := db.Exec(`
		update sessions set
			message_count = coalesce(
				(select count(*) from messages m where m.session_id = sessions.id), 0),
			conversation_count = coalesce(
				(select count(*) from messages m
				  where m.session_id = sessions.id
				    and m.type in ('user','assistant')), 0)`); err != nil {
		return err
	}
	// run の生存期間。/clear をまたいだ実行がどこまで続いたかがここで見える。
	_, err := db.Exec(`
		update runs set
			started_at = (select min(m.timestamp) from messages m
			               where m.run_id = runs.id and m.timestamp is not null),
			ended_at   = (select max(m.timestamp) from messages m
			               where m.run_id = runs.id and m.timestamp is not null)`)
	return err
}
