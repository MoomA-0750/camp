package session

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/MoomA-0750/camp/internal/store"
)

// Destination は台帳の1行。
type Destination struct {
	ID          int64  `json:"id"`
	Alias       string `json:"alias"`
	HostName    string `json:"hostname,omitempty"`
	User        string `json:"user,omitempty"`
	Port        int    `json:"port,omitempty"`
	Identity    string `json:"identity,omitempty"`
	TailscaleIP string `json:"tailscale_ip,omitempty"`
	Note        string `json:"note,omitempty"`
	Allowed     bool   `json:"allowed"`
	Source      string `json:"source"`
	SeenAt      string `json:"seen_at"`
	UpdatedAt   string `json:"updated_at"`

	// Pinned は許したときに `ssh -G` で見た行き先。起こすたびに照らす。
	// 許可済みでもこれが空なら起こせない（2026-09-11 より前に許したもの）。
	Pinned *Resolved `json:"pinned,omitempty"`
	// AgentPaths はエージェントごとの向こうの実体（駆動器の名前 → 絶対パス）。無ければ向こうで探す。
	AgentPaths map[string]string `json:"agent_paths,omitempty"`
}

const destCols = `
	select id, alias, coalesce(hostname,''), coalesce(user,''), coalesce(port,0),
	       coalesce(identity,''), coalesce(tailscale_ip,''), coalesce(note,''),
	       allowed, source, seen_at, updated_at, coalesce(pinned,'')
	from ssh_hosts`

// agentPathsOf は台帳の実体の場所を、接続先ごとに読む（alias が空なら全部）。
func agentPathsOf(db *store.DB, alias string) (map[string]map[string]string, error) {
	q, args := `select host, agent, path from ssh_agent_paths`, []any{}
	if alias != "" {
		q, args = q+` where host=?`, append(args, alias)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]string{}
	for rows.Next() {
		var h, a, p string
		if err := rows.Scan(&h, &a, &p); err != nil {
			return nil, err
		}
		if out[h] == nil {
			out[h] = map[string]string{}
		}
		out[h][a] = p
	}
	return out, rows.Err()
}

func scanDest(s scanner) (Destination, error) {
	var d Destination
	var allowed int
	var pinned string
	if err := s.Scan(&d.ID, &d.Alias, &d.HostName, &d.User, &d.Port,
		&d.Identity, &d.TailscaleIP, &d.Note, &allowed, &d.Source,
		&d.SeenAt, &d.UpdatedAt, &pinned); err != nil {
		return d, err
	}
	d.Allowed = allowed != 0
	if pinned != "" {
		var r Resolved
		// **読めない固定を「固定なし」と読まない。** 起こせないほうへ倒す。
		if err := json.Unmarshal([]byte(pinned), &r); err == nil && r.HostName != "" {
			d.Pinned = &r
		}
	}
	return d, nil
}

// ListDestinations は台帳を返す。
func ListDestinations(db *store.DB) ([]Destination, error) {
	rows, err := db.Query(destCols + ` order by alias`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Destination{} // **nil を返さない**（JSON で null になる）
	for rows.Next() {
		d, err := scanDest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	paths, err := agentPathsOf(db, "")
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].AgentPaths = paths[out[i].Alias]
	}
	return out, nil
}

// getDestination は1行読む。
func getDestination(db *store.DB, alias string) (Destination, error) {
	d, err := scanDest(db.QueryRow(destCols+` where alias=?`, alias))
	if err == sql.ErrNoRows {
		return d, fmt.Errorf("台帳に無い接続先: %s", alias)
	}
	if err != nil {
		return d, err
	}
	paths, err := agentPathsOf(db, alias)
	if err != nil {
		return d, err
	}
	d.AgentPaths = paths[alias]
	return d, nil
}

// AllowDestination は許して、そのときの行き先を固定する。**再認証は呼び出し側の責任。**
//
// **known_hosts に鍵の無い先は許さない。** Camp は鍵を受け入れないので、どうせ繋がらない。
// 起こすときではなく、許すときに言う。
func AllowDestination(db *store.DB, alias string, pin Resolved) error {
	if pin.HostName == "" {
		return fmt.Errorf("行き先が空のまま固定しない")
	}
	if len(pin.HostKeys) == 0 {
		return fmt.Errorf("%s のホスト鍵がまだ known_hosts に無い。Camp は鍵を受け入れないので、端末で一度 ssh %s して確かめてから許す", alias, alias)
	}
	b, err := json.Marshal(pin)
	if err != nil {
		return err
	}
	r, err := db.Exec(`update ssh_hosts set allowed=1, pinned=?, updated_at=? where alias=?`,
		string(b), time.Now().UTC().Format(time.RFC3339), alias)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("台帳に無い接続先: %s", alias)
	}
	return nil
}

// SetAgentPath は向こうでの、そのエージェントの実体の場所を書く。空は「向こうで探す」（行を消す）。
//
// **再認証は呼び出し側の責任。** 向こうで何を走らせるかを変えるので、
// 許可と同じ重さで扱う。
func SetAgentPath(db *store.DB, alias, agent, p string) error {
	if !validAgent(agent) {
		return fmt.Errorf("知らないエージェント: %q", agent)
	}
	if err := validAgentPath(p); err != nil {
		return err
	}
	if _, err := getDestination(db, alias); err != nil {
		return err
	}
	var err error
	if p == "" {
		_, err = db.Exec(`delete from ssh_agent_paths where host=? and agent=?`, alias, agent)
	} else {
		_, err = db.Exec(`insert into ssh_agent_paths(host, agent, path) values(?,?,?)
			on conflict(host, agent) do update set path=excluded.path`, alias, agent, p)
	}
	if err != nil {
		return err
	}
	_, err = db.Exec(`update ssh_hosts set updated_at=? where alias=?`,
		time.Now().UTC().Format(time.RFC3339), alias)
	return err
}

// ResolveSSH は実行面に `ssh -G <alias>` を読ませる。**繋がない。**
func (s *Supervisor) ResolveSSH(alias string) (Resolved, error) {
	if err := validAlias(alias); err != nil {
		return Resolved{}, err
	}
	s.mu.Lock()
	agent := s.agent
	s.mu.Unlock()
	if agent == nil {
		return Resolved{}, ErrNoAgent
	}
	req := newID()
	ch := make(chan Msg, 1)
	s.mu.Lock()
	s.waits[req] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.waits, req)
		s.mu.Unlock()
	}()
	if err := agent.send(Msg{T: MsgSSHResolve, ReqID: req, Alias: alias}); err != nil {
		return Resolved{}, err
	}
	select {
	case m := <-ch:
		if m.Error != "" {
			return Resolved{}, fmt.Errorf("%s", m.Error)
		}
		if m.Resolved == nil || m.Resolved.HostName == "" {
			return Resolved{}, fmt.Errorf("実行面が行き先を返さない")
		}
		return *m.Resolved, nil
	case <-time.After(15 * time.Second):
		return Resolved{}, fmt.Errorf("実行面が返事をしない")
	}
}

// ImportSSH は読み取った接続先を台帳へ突き合わせる。
//
// **allowed には触らない。** 許可は人が決めたことで、設定ファイルが
// 決めることではない。`~/.ssh/config` を編集しただけで繋げる先が増えたら、
// 台帳の意味が無い。
//
// 返すのは (足した数, 更新した数)。
func ImportSSH(db *store.DB, hosts []SSHHost) (added, updated int, err error) {
	now := time.Now().UTC().Format(time.RFC3339)
	for _, h := range hosts {
		if h.Alias == "" {
			continue
		}
		var id int64
		e := db.QueryRow(`select id from ssh_hosts where alias=?`, h.Alias).Scan(&id)
		if e == nil {
			if _, err = db.Exec(`
				update ssh_hosts set hostname=?, user=?, port=?, identity=?,
					source='ssh_config', seen_at=?, updated_at=?
				where id=?`,
				nzs(h.HostName), nzs(h.User), h.Port, nzs(h.Identity), now, now, id); err != nil {
				return added, updated, err
			}
			updated++
			continue
		}
		if _, err = db.Exec(`
			insert into ssh_hosts(alias, hostname, user, port, identity,
				allowed, source, seen_at, updated_at)
			values(?,?,?,?,?,0,'ssh_config',?,?)`,
			h.Alias, nzs(h.HostName), nzs(h.User), h.Port, nzs(h.Identity),
			now, now); err != nil {
			return added, updated, err
		}
		added++
	}
	return added, updated, nil
}

// SetDestinationAllowed は許可フラグを切り替える。**再認証は呼び出し側の責任。**
//
// 外すときは固定も消す。**ここで許しても行き先は固定されない**ので、
// そのままでは起こせない（固定するのは AllowDestination）。
func SetDestinationAllowed(db *store.DB, alias string, allowed bool) error {
	v := 0
	if allowed {
		v = 1
	}
	r, err := db.Exec(
		`update ssh_hosts set allowed=?, pinned=case when ? then pinned else null end,
		 updated_at=? where alias=?`,
		v, allowed, time.Now().UTC().Format(time.RFC3339), alias)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("台帳に無い接続先: %s", alias)
	}
	return nil
}

// EditDestination は覚え書きと Tailscale IP を直す。許可フラグはここでは触らない。
func EditDestination(db *store.DB, alias, note, tailscaleIP string) error {
	r, err := db.Exec(`
		update ssh_hosts set note=?, tailscale_ip=?, updated_at=? where alias=?`,
		nzs(note), nzs(tailscaleIP), time.Now().UTC().Format(time.RFC3339), alias)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("台帳に無い接続先: %s", alias)
	}
	return nil
}

func nzs(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ScanSSH は実行面に `~/.ssh/config` を読ませて、台帳へ突き合わせる。
//
// **campd 自身は読まない。** `~/.ssh` は camp ユーザーに開けていないし、
// 開けるべきでもない（鍵の置き場に読み取りを配るのは境界を薄くする）。
func (s *Supervisor) ScanSSH() (added, updated int, err error) {
	s.mu.Lock()
	agent := s.agent
	s.mu.Unlock()
	if agent == nil {
		return 0, 0, ErrNoAgent
	}
	req := newID()
	ch := make(chan Msg, 1)
	s.mu.Lock()
	s.waits[req] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.waits, req)
		s.mu.Unlock()
	}()
	if err := agent.send(Msg{T: MsgSSHScan, ReqID: req}); err != nil {
		return 0, 0, err
	}
	select {
	case m := <-ch:
		if m.Error != "" {
			return 0, 0, fmt.Errorf("%s", m.Error)
		}
		return ImportSSH(s.db, m.SSHHosts)
	case <-time.After(15 * time.Second):
		return 0, 0, fmt.Errorf("実行面が返事をしない")
	}
}
