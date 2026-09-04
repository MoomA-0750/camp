package session

import (
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
}

// ListDestinations は台帳を返す。
func ListDestinations(db *store.DB) ([]Destination, error) {
	rows, err := db.Query(`
		select id, alias, coalesce(hostname,''), coalesce(user,''), coalesce(port,0),
		       coalesce(identity,''), coalesce(tailscale_ip,''), coalesce(note,''),
		       allowed, source, seen_at, updated_at
		from ssh_hosts order by alias`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Destination
	for rows.Next() {
		var d Destination
		var allowed int
		if err := rows.Scan(&d.ID, &d.Alias, &d.HostName, &d.User, &d.Port,
			&d.Identity, &d.TailscaleIP, &d.Note, &allowed, &d.Source,
			&d.SeenAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		d.Allowed = allowed != 0
		out = append(out, d)
	}
	return out, rows.Err()
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
func SetDestinationAllowed(db *store.DB, alias string, allowed bool) error {
	v := 0
	if allowed {
		v = 1
	}
	r, err := db.Exec(
		`update ssh_hosts set allowed=?, updated_at=? where alias=?`,
		v, time.Now().UTC().Format(time.RFC3339), alias)
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
