package session

import "encoding/json"

// codexConv は codexState を Conversation の形で包む（M40 の (1)。中身は codex.go のまま）。
type codexConv struct {
	cs   *codexState
	cwd  string
	home string
	perm map[string]any // thread/start へ足す確認の度合いの欄（cli なら空）
	// remote は向こうのホストの子か。remoteHomes は向こうの sh が名乗った置き場。
	remote      bool
	remoteHomes []string
}

var _ Conversation = (*codexConv)(nil)

func (codexDriver) Open(o OpenOpts) Conversation {
	return &codexConv{cs: newCodexState(), cwd: o.Cwd, home: o.Home, perm: codexPerms[o.Perm],
		remote: o.Remote, remoteHomes: o.RemoteHomes}
}

func (c *codexConv) Begin(o BeginOpts) error {
	check := func(res json.RawMessage) error { return verifyCodexHome(res, c.home) }
	if c.remote {
		check = func(res json.RawMessage) error { return verifyRemoteCodexHome(res, c.remoteHomes) }
	}
	return c.cs.handshake(o.Scanner, o.W, c.cwd, check, c.perm, o.Record)
}

func (c *codexConv) Fold(line []byte) Event {
	ev := c.cs.classify(line)
	return Event{Kind: ev.kind, TurnEnd: ev.turnEnd, Interrupted: ev.interrupted, Err: ev.err,
		Note: ev.note, Ask: ev.ask, Replies: ev.replies, Withdrawn: ev.withdrawn,
		Deliver: ev.deliver, Payload: ev.payload, SessionID: c.cs.threadID()}
}

func (c *codexConv) Input(text string) ([]byte, error) { return c.cs.input(text) }

// Answer は Codex へ答える。**理由は渡せない**（Codex の答えに理由の欄が無い）。
func (c *codexConv) Answer(reqID, behavior, _ string) ([]byte, error) {
	return c.cs.approve(reqID, behavior)
}

func (c *codexConv) Interrupt() []byte { return c.cs.interrupt() }

func (c *codexConv) Query(kind string) ([]byte, string, json.RawMessage, error) {
	return c.cs.control(kind)
}

func (c *codexConv) Waiting() []HeldAsk { return c.cs.waiting() }
func (c *codexConv) SessionID() string  { return c.cs.threadID() }
