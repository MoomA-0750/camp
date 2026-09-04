package session

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// SSHHost は `~/.ssh/config` から読み取った接続先1つ。
type SSHHost struct {
	Alias    string `json:"alias"`
	HostName string `json:"hostname,omitempty"`
	User     string `json:"user,omitempty"`
	Port     int    `json:"port,omitempty"`
	Identity string `json:"identity,omitempty"`
}

// ReadSSHConfig は `~/.ssh/config` を**読むだけ**。
//
// **書き戻す関数をこのファイルに置かない。** Camp のバグで端末の SSH 設定が
// 壊れると、直すのに Camp が要る、という一番まずい形になる。
// テストはこのファイルに書き込みの呼び出しが無いことを見ている。
//
// Include を辿る（いまどきの config はたいてい分割されている）。
// ワイルドカードを含む Host（`*` や `?`）は接続先ではないので落とす。
func ReadSSHConfig(path string) ([]SSHHost, error) {
	seen := map[string]bool{}
	byAlias := map[string]*SSHHost{}
	if err := readSSHInto(path, byAlias, seen, 0); err != nil {
		return nil, err
	}
	out := make([]SSHHost, 0, len(byAlias))
	for _, h := range byAlias {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out, nil
}

func readSSHInto(path string, byAlias map[string]*SSHHost, seen map[string]bool, depth int) error {
	if depth > 8 || seen[path] {
		return nil // Include の輪で回り続けない
	}
	seen[path] = true

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	var current []*SSHHost
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val := splitSSHLine(line)
		switch strings.ToLower(key) {
		case "host":
			current = nil
			for _, name := range strings.Fields(val) {
				if strings.ContainsAny(name, "*?!") {
					continue // 模様であって接続先ではない
				}
				h := byAlias[name]
				if h == nil {
					h = &SSHHost{Alias: name}
					byAlias[name] = h
				}
				current = append(current, h)
			}
		case "include":
			for _, pat := range strings.Fields(val) {
				for _, p := range expandInclude(filepath.Dir(path), pat) {
					if err := readSSHInto(p, byAlias, seen, depth+1); err != nil {
						return err
					}
				}
			}
		case "hostname":
			setAll(current, func(h *SSHHost) { h.HostName = val })
		case "user":
			setAll(current, func(h *SSHHost) { h.User = val })
		case "identityfile":
			setAll(current, func(h *SSHHost) { h.Identity = val })
		case "port":
			if n, err := strconv.Atoi(val); err == nil {
				setAll(current, func(h *SSHHost) { h.Port = n })
			}
		}
	}
	return sc.Err()
}

func setAll(hs []*SSHHost, f func(*SSHHost)) {
	for _, h := range hs {
		f(h)
	}
}

// splitSSHLine は `Key value` も `Key=value` も受ける（ssh はどちらも読む）。
func splitSSHLine(line string) (string, string) {
	if i := strings.IndexAny(line, " \t="); i >= 0 {
		return line[:i], strings.TrimSpace(strings.TrimLeft(line[i:], " \t="))
	}
	return line, ""
}

func expandInclude(dir, pat string) []string {
	if strings.HasPrefix(pat, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			pat = filepath.Join(home, pat[2:])
		}
	}
	if !filepath.IsAbs(pat) {
		pat = filepath.Join(dir, pat)
	}
	m, err := filepath.Glob(pat)
	if err != nil || len(m) == 0 {
		return nil
	}
	return m
}

// DefaultSSHConfig は読む場所。
func DefaultSSHConfig() string {
	if p := os.Getenv("CAMP_SSH_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ssh", "config")
}
