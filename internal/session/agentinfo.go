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

// AgentNote は `campd agent` が起動時に出す1行（操作する人が見る）。
type AgentNote struct{ Name, Note string }

// AgentSummary は、この実行面が各エージェントをどこで起こせるかの説明（M49、2026-09-13）。
//
// **名乗りと同じ見分け方で作る**（agents・driverInfos と同じ Launch を見る）。起動時の表示と
// hello の名乗りが食い違うと、画面に出ないところで取り違えが起きる。
func AgentSummary(a *Agent) []AgentNote {
	out := []AgentNote{}
	for _, name := range a.agents() {
		in := drivers[name].Info()
		switch {
		case !a.localOK(name):
			out = append(out, AgentNote{name, in.Label + "（手元に実体が無い。向こうのホストでなら起こせる）"})
		case in.Remote:
			out = append(out, AgentNote{name, in.Label + "（このマシンでも向こうのホストでも起こせる）"})
		default:
			out = append(out, AgentNote{name, in.Label + "（このマシンだけ）"})
		}
	}
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
