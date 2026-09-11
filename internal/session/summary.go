package session

// 流れの1行の一言。**画面はフレームの形を知らない**（D-031）——Claude の stream-json も
// Codex の JSON-RPC も、駆動器が畳んで summary に入れる。

// summarize は流れの1行ずつに、そのセッションの駆動器が畳んだ一言を添える。
func (s *Supervisor) summarize(id string, lines []Line) {
	agent := ""
	s.mu.Lock()
	if ls := s.live[id]; ls != nil {
		agent = ls.rec.Agent
	}
	s.mu.Unlock()
	if agent == "" {
		if r, err := get(s.db, id); err == nil {
			agent = r.Agent
		}
	}
	d, ok := drivers[agentOr(agent)]
	if !ok {
		return
	}
	for i := range lines {
		lines[i].Summary, lines[i].Own = d.Summary(lines[i].Kind, lines[i].Frame)
	}
}

// cut は s を n 文字（rune）までにする。
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
