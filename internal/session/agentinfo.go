package session

import "sort"

// 画面・API へ出すエージェントの説明。**画面はここから来るものしか知らない**
// （エージェントの名前・表示名・振る舞いの説明を決め打ちしない。D-031）。

// Agents は、いま繋がっている実行面が起こせるエージェントの説明（画面の選択肢）。
// 繋がっていなければ空。
func (s *Supervisor) Agents() []AgentInfo {
	s.mu.Lock()
	ac := s.agent
	s.mu.Unlock()
	out := []AgentInfo{}
	if ac == nil {
		return out
	}
	for _, name := range agentsOf(ac) {
		if in, ok := ac.info(name); ok {
			out = append(out, in)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// LabelOf は画面に出すエージェントの名前。知らない名前はそのまま返す。
func LabelOf(agent string) string {
	if d, ok := drivers[agentOr(agent)]; ok {
		return d.Info().Label
	}
	return agent
}

// NotesOf はそのエージェント固有の振る舞いの説明（画面にそのまま出す）。
func NotesOf(agent string) []string {
	if d, ok := drivers[agentOr(agent)]; ok {
		return d.Info().Notes
	}
	return nil
}
