package session

// Claude Code の駆動器（`claude -p`、stream-json、1行1フレーム）。

import (
	"encoding/json"
	"fmt"
	"sync"
)

type claudeDriver struct{}

func (claudeDriver) Info() AgentInfo {
	return AgentInfo{Name: AgentClaude, Label: "Claude Code", Perms: append([]string(nil), allPerms...),
		Remote: true, Resume: true}
}

// claudeModes は確認の度合いごとの --permission-mode。Camp の起こし方（-p・stream-json・
// --permission-prompt-tool stdio）で5つとも効くことを測った（2026-09-12、probe_claude_modes.py）。
// cli は渡さない（本人の設定のまま）。
var claudeModes = map[string]string{
	PermAsk: "default", PermEdits: "acceptEdits", PermAuto: "auto", PermFull: "bypassPermissions",
}

// Launch は claude の実体。**ここでは確かめない**（Phase 3.6 までと同じく、起こせなければ
// 起こしたときに分かる）。
//
// Codex は確かめるので、ここに差がある（`codex exec` のレビュー、2026-09-12）。**揃えるなら
// 「手元では起こせないが向こうでなら起こせる」を名乗れるようにするのが先**——hello は手元の
// Launch だけから作るので、ここで実体を確かめると、手元に claude の無い実行面が向こうのホストの
// claude も頼めなくなる（いまの Codex がそうなっている）。Phase 3.8 以降の宿題。
func (claudeDriver) Launch(a *Agent) (Launch, error) { return Launch{Bin: a.Claude}, nil }

// RemoteLaunch は向こうで探す名前と置き場（$CLAUDE_CONFIG_DIR、無ければ ~/.claude。CLI と同じ）。
//
// 置き場は M47 で足した。**向こうの記録を読むときに、置き場を向こうに解決させるため**——
// campd は向こうの $HOME も環境変数も知らないので、パスを打ち込ませる代わりに規則だけ渡す
// （本人の決定 2026-09-12）。起こすときの振る舞いは変わらない（wrapperScript は名乗るだけで、
// 置き場が無くても止めない）。
func (claudeDriver) RemoteLaunch() RemoteLaunch {
	return RemoteLaunch{Name: "claude", HomeEnv: "CLAUDE_CONFIG_DIR", HomeDefault: ".claude"}
}

// Argv は `claude -p` を stream-json で起こす引数。承認は stdio で Camp へ訊かせる。
func (claudeDriver) Argv(perm, resume string) ([]string, error) {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-prompt-tool", "stdio",
	}
	// **続きから起こす**（M48、2026-09-13）。実測: Camp と同じ引数のまま `--resume <id>` を
	// 足すと文脈が続き、子が名乗る session_id も記録の JSONL も**元のまま**（追記される）。
	//
	// **`--fork-session` は使わない。** 複製した別ファイルができ、その**全行に isSidechain**
	// が付く（2026-09-13 実測）。取り込みはそれを見てサブエージェントとして入れるので、
	// fork した会話が丸ごとサブエージェント扱いになり、同じ会話が2本になる。
	// 記録には「fork された」と分かる印が無く、本物のサブエージェントと見分けられない。
	if resume != "" {
		args = append(args, "--resume", resume)
	}
	if perm == PermCLI {
		return args, nil
	}
	mode, ok := claudeModes[perm]
	if !ok {
		return nil, fmt.Errorf("Claude Code の確認の度合い %s は扱わない", perm)
	}
	return append(args, "--permission-mode", mode), nil
}

func (claudeDriver) Open(o OpenOpts) Conversation {
	return &claudeConv{session: o.Session, asks: map[string]HeldAsk{}, perm: o.Perm,
		want: claudeModes[o.Perm]}
}

// claudeConv は Claude Code の子1本との会話。
type claudeConv struct {
	session string // Camp が採番した id（中断の request_id に使う）

	mu sync.Mutex
	// asks は待っている承認。**見た id にだけ答える**（Codex と同じ。Fable の設計レビュー）。
	asks map[string]HeldAsk
	sid  string // 最後に名乗った session_id

	// 確認の度合い。want は渡した --permission-mode（cli なら空で、照らさない）。
	perm, want string
	// inited は最初の system/init を見たか、mode はそこで名乗った permissionMode。
	// **system/init はターンごとに来る**（最初の入力のあとが1回目）。
	inited bool
	mode   string
}

var _ Conversation = (*claudeConv)(nil)

// Begin は何もしない。Claude は話し始めるまでの手順が無い（system/init は最初の入力のあとに来る）。
func (c *claudeConv) Begin(BeginOpts) error { return nil }

// Fold は stream-json の1行を畳む。**中身は承認の要求だけ渡す**（他のフレームは種類だけ）。
func (c *claudeConv) Fold(line []byte) Event {
	var f map[string]any
	if err := json.Unmarshal(line, &f); err != nil {
		// 読めない行は落とし先にも残さず、campd へも渡さない（Phase 3.6 までの Claude と同じ）。
		return Event{Drop: true}
	}
	kind := FrameKind(f)
	ev := Event{Kind: kind, TurnEnd: kind == "result"}
	if s, ok := f["session_id"].(string); ok {
		ev.SessionID = s
		c.mu.Lock()
		c.sid = s
		c.mu.Unlock()
	}
	switch kind {
	case "system/init":
		// **起こしたときに効いたかを1回だけ照らす。途中の変化は止めずに記録する**（本人の決定。
		// plan mode・承認の updatedPermissions などで変わる。CLI では止まらない）。
		mode, _ := f["permissionMode"].(string)
		c.mu.Lock()
		first, prev := !c.inited, c.mode
		c.inited, c.mode = true, mode
		c.mu.Unlock()
		switch {
		case first && c.want != "" && mode != c.want:
			// 名乗らない（欠けている）ことも「効いていない」と読む。
			ev.Halt = fmt.Sprintf("確認の度合い %s（--permission-mode %s）を頼んだのに %q で起きた",
				c.perm, c.want, mode)
		case !first && mode != prev:
			ev.Note = fmt.Sprintf("確認の度合いが途中で変わった（%s → %s）。止めずに記録した", prev, mode)
		}
	case "control_response":
		// 子からの制御応答は、待っている者へ回す。**画面へは流さない。**
		if resp, ok := f["response"].(map[string]any); ok {
			if id, _ := resp["request_id"].(string); id != "" {
				b, err := json.Marshal(resp["response"])
				if err != nil {
					b = []byte("null")
				}
				ev.Deliver, ev.Payload = id, b
			}
		}
	case "control_request/can_use_tool":
		var a HeldAsk
		if id, ok := f["request_id"].(string); ok {
			a.ReqID = id
		}
		if req, ok := f["request"].(map[string]any); ok {
			if n, ok := req["tool_name"].(string); ok {
				a.Tool = n
			}
			// **何を承認しようとしているかは、画面に出さないと答えられない。**
			a.Detail = claudeAskDetail(a.Tool, req)
		}
		if a.ReqID != "" {
			c.mu.Lock()
			c.asks[a.ReqID] = a
			c.mu.Unlock()
		}
		ev.Ask = &a
	}
	return ev
}

func (c *claudeConv) Input(text string) ([]byte, error) { return userFrame(text), nil }

// Answer は待っている承認への答え。断るときの理由は空にしない（approveFrame）。
func (c *claudeConv) Answer(reqID, behavior, reason string) ([]byte, error) {
	c.mu.Lock()
	_, ok := c.asks[reqID]
	delete(c.asks, reqID)
	c.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("その承認は待っていない: %s", reqID)
	}
	return approveFrame(reqID, behavior, reason), nil
}

// Interrupt は制御フレームでの中断。result が返る。
func (c *claudeConv) Interrupt() []byte {
	b, _ := json.Marshal(map[string]any{
		"type":       "control_request",
		"request_id": "camp-stop-" + c.session,
		"request":    map[string]any{"subtype": "interrupt"},
	})
	return b
}

// Query は制御フレームで訊く。残量（get_usage / get_context_usage）はこの経路でしか取れない。
// **モデル呼び出しは起きない**ので、押すたびにトークンを使うことはない。
func (c *claudeConv) Query(kind string) ([]byte, string, json.RawMessage, error) {
	key := "camp-ctl-" + newID()
	b, _ := json.Marshal(map[string]any{
		"type": "control_request", "request_id": key,
		"request": map[string]any{"subtype": kind},
	})
	return b, key, nil, nil
}

func (c *claudeConv) Waiting() []HeldAsk {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]HeldAsk, 0, len(c.asks))
	for _, a := range c.asks {
		out = append(out, a)
	}
	return out
}

func (c *claudeConv) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sid
}
