package session

// Codex の駆動器（`codex app-server`、stdio の JSON-RPC、1行1メッセージ）。
//
// 実測と設計は dev/active/phase3.6-plan.md（codex-cli 0.154.0）。Claude の駆動は
// agent.go のまま触らず、ここで Codex だけを扱う。campd へは Claude と同じ Msg で渡し、
// 「ターンが終わった」「承認が来た」は欄（TurnEnd / Ask）で伝える。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// 起こせるエージェント。**この2つ以外を通さない。**
const (
	AgentClaude = "claude"
	AgentCodex  = "codex"
)

func validAgent(a string) bool { return a == AgentClaude || a == AgentCodex }

// agentsOf は実行面が起こせるエージェント（監査の文用）。
func agentsOf(a *agentConn) []string {
	if len(a.agents) == 0 {
		return []string{AgentClaude}
	}
	return a.agents
}

// agentOr は空を claude と読む。**古い実行面は agent を名乗らない**（Phase 3.6 より前）。
func agentOr(a string) string {
	if a == "" {
		return AgentClaude
	}
	return a
}

// codexPolicy は Camp が Codex に渡す方針。**ここ1か所で決める**
// （後でセッションごとの確認の度合い・範囲の選択に差し替える）。
//
// untrusted は「読むだけの決まったコマンド以外は訊く」。workspace-write でもファイル変更は
// 訊いてくる（実測）。**MCP のツールは訊いてこない**——Codex に承認の要求そのものが無い
// （本人が受け入れた。2026-09-11）。
var codexPolicy = struct {
	approval, reviewer, sandbox, sandboxType string
}{"untrusted", "user", "workspace-write", "workspaceWrite"}

// codexOptOut は受け取らない通知。途中経過は Claude でも取っていない
// （`--include-partial-messages` を付けていない）。完成品は item/completed に全文がある。
// **ターンの終わり・承認・方針の変化に関わるものは入れない**（テストで縛る）。
var codexOptOut = []string{
	"item/agentMessage/delta",
	"item/reasoning/textDelta",
	"item/reasoning/summaryTextDelta",
	"item/reasoning/summaryPartAdded",
	"item/commandExecution/outputDelta",
	"item/fileChange/outputDelta",
	"item/plan/delta",
	"command/exec/outputDelta",
	"process/outputDelta",
	"mcpServer/startupStatus/updated",
	"remoteControl/status/changed",
}

// codexApprovals は承認として画面へ出すサーバー要求。**これ以外には断りを返す。**
// 返事をしないと Codex はそこで待ち続ける（承認と同じ仕組みなので）。
var codexApprovals = map[string]string{
	"item/commandExecution/requestApproval": "codex:command",
	"item/fileChange/requestApproval":       "codex:fileChange",
}

// codexPolicyChanged は途中で方針が変わったしるし。**来たらその場で止める。**
// 話し始める前に照らしたことが、以後も成り立っているとは言えなくなる。
var codexPolicyChanged = map[string]bool{
	"thread/settings/updated":         true,
	"item/autoApprovalReview/started": true,
}

// codexState は Codex の子1本ぶんの状態。
type codexState struct {
	mu     sync.Mutex
	thread string
	// root は起こした場所（実パス）。承認のコマンドの cwd がこの外なら、見せずに断る。
	root    string
	turn    string
	wantInt bool // 中断を頼まれたが、まだターン id が無い
	seq     int
	// own は自分が投げた要求の id（JSON のまま）→ 用途。
	own map[string]string
	// asks は待っている承認。鍵は受け取った id の JSON そのまま（整数の 0 から始まる）。
	asks map[string]HeldAsk
	// changes はファイル変更の差分。**承認の要求そのものには差分が無い**（実測）ので、
	// 直前の item/started の changes を item の id で結ぶ。
	changes map[string]json.RawMessage
	// usage は最後の thread/tokenUsage/updated。残量タブが読む（モデルを呼ばない）。
	usage json.RawMessage
}

func newCodexState() *codexState {
	return &codexState{own: map[string]string{}, asks: map[string]HeldAsk{},
		changes: map[string]json.RawMessage{}}
}

// rpcMsg は JSON-RPC の1行。要求・応答・通知のどれでも受ける。
type rpcMsg struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (m rpcMsg) isResponse() bool { return m.Method == "" && len(m.ID) > 0 }
func (m rpcMsg) isRequest() bool  { return m.Method != "" && len(m.ID) > 0 }
func (m rpcMsg) key() string      { return string(bytes.TrimSpace(m.ID)) }

func rpcFrame(v map[string]any) []byte {
	v["jsonrpc"] = "2.0"
	b, _ := json.Marshal(v)
	return b
}

// request は自分が投げる要求を1つ作る。id は "camp-<n>"（JSON の文字列）。
func (cs *codexState) request(purpose, method string, params any) (frame []byte, key string) {
	cs.mu.Lock()
	cs.seq++
	id := fmt.Sprintf("camp-%d", cs.seq)
	k, _ := json.Marshal(id)
	key = string(k)
	cs.own[key] = purpose
	cs.mu.Unlock()
	return rpcFrame(map[string]any{"id": id, "method": method, "params": params}), key
}

func (cs *codexState) threadID() string {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.thread
}

// ---------------------------------------------------------------- 起こしてから話し始めるまで

// handshake は最初の3往復（initialize → initialized → thread/start）を済ませ、
// **Camp の置き場で起きたこと・効いた方針を照らしてから**スレッド id を覚える。
// rec は流れた行を落とし先へ残す。
func (cs *codexState) handshake(sc *bufio.Scanner, w io.Writer, cwd, home string,
	rec func(kind string, line []byte)) error {
	write := func(b []byte) error {
		_, err := w.Write(append(b, '\n'))
		return err
	}
	init, key := cs.request("init", "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "camp", "title": "Camp", "version": "0"},
		"capabilities": map[string]any{"optOutNotificationMethods": codexOptOut},
	})
	if err := write(init); err != nil {
		return err
	}
	res, err := cs.await(sc, w, key, rec)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	// **Camp の置き場で起きたか。** 本人の「今後訊かない」を持ち込まない守りは CODEX_HOME を
	// 渡す1行に掛かっている。本人の置き場で起きても方針は Camp が渡した値になるので、
	// thread/start の照合では気づけない（Fable の実装後レビュー 1）。
	if err := verifyCodexHome(res, home); err != nil {
		return err
	}
	if err := write(rpcFrame(map[string]any{"method": "initialized"})); err != nil {
		return err
	}
	start, key := cs.request("start", "thread/start", map[string]any{
		"cwd":               cwd,
		"approvalPolicy":    codexPolicy.approval,
		"approvalsReviewer": codexPolicy.reviewer,
		"sandbox":           codexPolicy.sandbox,
	})
	if err := write(start); err != nil {
		return err
	}
	res, err = cs.await(sc, w, key, rec)
	if err != nil {
		return fmt.Errorf("thread/start: %w", err)
	}
	thread, err := verifyCodexStart(res, cwd)
	if err != nil {
		return err
	}
	cs.mu.Lock()
	cs.thread, cs.root = thread, cwd
	cs.mu.Unlock()
	return nil
}

// await は key への応答が来るまで読む。途中の通知は落とし先へ残すだけ。
// 途中で来たサーバー要求には断りを返す（まだ誰も答えられない）。
func (cs *codexState) await(sc *bufio.Scanner, w io.Writer, key string,
	rec func(kind string, line []byte)) (json.RawMessage, error) {
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m rpcMsg
		if json.Unmarshal(line, &m) != nil {
			rec("?", line)
			continue
		}
		switch {
		case m.isResponse():
			rec("response", line)
			if m.key() != key {
				continue
			}
			cs.mu.Lock()
			delete(cs.own, key)
			cs.mu.Unlock()
			if m.Error != nil {
				return nil, fmt.Errorf("Codex が断った: %s", m.Error.Message)
			}
			return m.Result, nil
		case m.isRequest():
			rec("request/"+m.Method, line)
			w.Write(append(refuseFrame(m.ID, m.Method, "話し始める前"), '\n'))
		default:
			rec(m.Method, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("子が手順の途中で終わった")
}

// verifyCodexHome は initialize の応答が名乗る置き場が、Camp の置き場か（実パスで）照らす。
// **名乗らないことを「合っている」と読まない。**
func verifyCodexHome(res json.RawMessage, home string) error {
	var r struct {
		CodexHome string `json:"codexHome"`
	}
	json.Unmarshal(res, &r)
	if r.CodexHome == "" {
		return errors.New("Codex がどの置き場で起きたか名乗らない。本人の設定で起きていないと確かめられないので話し始めない")
	}
	got, err1 := filepath.EvalSymlinks(r.CodexHome)
	want, err2 := filepath.EvalSymlinks(home)
	if err1 != nil || err2 != nil || got != want {
		return fmt.Errorf("Codex が Camp の置き場ではない %s で起きた（%s のはず）。"+
			"本人の「今後訊かない」を持ち込むので話し始めない", r.CodexHome, home)
	}
	return nil
}

var threadIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// verifyCodexStart は thread/start の応答を**構造ごと**照らす。
//
// 本人の設定や将来の版で「訊かない」「自動で審査する」「広い sandbox」に倒れていたとき、
// 承認の画面を通らずに走り出すのを防ぐ。**違えば話し始めない。**
// 「無い」を「合っている」と読まない——欄が欠けていても断る。
//
// activePermissionProfile は今の版の応答には無い（スキーマ上も thread/start の応答ではなく
// thread/settings の欄）。来たら中身を問わず断る。将来の版で常に返るようになれば
// 起こせなくなる——**安全側に倒れる**ので、そのとき読み方を決める（Fable の実装後レビュー 5）。
func verifyCodexStart(res json.RawMessage, cwd string) (string, error) {
	var r struct {
		Thread *struct {
			ID string `json:"id"`
		} `json:"thread"`
		ApprovalPolicy          json.RawMessage            `json:"approvalPolicy"`
		ApprovalsReviewer       json.RawMessage            `json:"approvalsReviewer"`
		Sandbox                 map[string]json.RawMessage `json:"sandbox"`
		ActivePermissionProfile json.RawMessage            `json:"activePermissionProfile"`
		Cwd                     *string                    `json:"cwd"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return "", fmt.Errorf("thread/start の応答が読めない: %w", err)
	}
	str := func(raw json.RawMessage) string {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "（" + string(raw) + "）"
		}
		return s
	}
	var why []string
	if got := str(r.ApprovalPolicy); got != codexPolicy.approval {
		why = append(why, fmt.Sprintf("承認の方針が %s（%s のはず）", got, codexPolicy.approval))
	}
	if got := str(r.ApprovalsReviewer); got != codexPolicy.reviewer {
		why = append(why, fmt.Sprintf("承認を見るのが %s（%s のはず）", got, codexPolicy.reviewer))
	}
	if r.Sandbox == nil {
		why = append(why, "sandbox が返ってこない")
	} else {
		if got := str(r.Sandbox["type"]); got != codexPolicy.sandboxType {
			why = append(why, fmt.Sprintf("sandbox が %s（%s のはず）", got, codexPolicy.sandboxType))
		}
		if got := strings.TrimSpace(string(r.Sandbox["networkAccess"])); got != "false" {
			why = append(why, "sandbox のネットワークが閉じていない（"+got+"）")
		}
		if got := strings.Join(strings.Fields(string(r.Sandbox["writableRoots"])), ""); got != "[]" {
			why = append(why, "sandbox に書ける場所が足されている（"+got+"）")
		}
	}
	if p := strings.TrimSpace(string(r.ActivePermissionProfile)); p != "" && p != "null" {
		why = append(why, "権限のプロファイルが効いている（"+p+"）")
	}
	if r.Cwd == nil || *r.Cwd != cwd {
		got := "（無い）"
		if r.Cwd != nil {
			got = *r.Cwd
		}
		why = append(why, fmt.Sprintf("作業場所が %s（%s のはず）", got, cwd))
	}
	thread := ""
	if r.Thread != nil {
		thread = r.Thread.ID
	}
	if !threadIDRe.MatchString(thread) {
		why = append(why, fmt.Sprintf("スレッド id が読めない（%q）", thread))
	}
	if len(why) > 0 {
		return "", fmt.Errorf("Codex に渡した方針が効いていない。話し始めない: %s", strings.Join(why, "・"))
	}
	return thread, nil
}

// ---------------------------------------------------------------- 流れているフレームを畳む

// codexEvent は1行を Camp の言葉に直したもの。
type codexEvent struct {
	kind        string
	turnEnd     bool
	interrupted bool
	err         string          // 監査に残す一言（断った・方針が変わった・ターンが失敗した）
	ask         *HeldAsk        // 画面へ出す承認
	replies     [][]byte        // 実行面がすぐ Codex へ返すもの（断り・後回しの中断）
	withdrawn   []string        // もう答えを待たなくなった承認の id（campd の台帳を閉じる）
	deliver     string          // 待っている者へ渡す応答の id（残量の問い合わせ）
	payload     json.RawMessage // その中身
	kill        bool            // 方針が変わったので止める
}

// classify は Codex の1行を畳む。**知らないものは種類だけにして通す。**
func (cs *codexState) classify(line []byte) codexEvent {
	var m rpcMsg
	if json.Unmarshal(line, &m) != nil {
		return codexEvent{kind: "?"}
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	switch {
	case m.isResponse():
		ev := codexEvent{kind: "response"}
		purpose := cs.own[m.key()]
		delete(cs.own, m.key())
		switch purpose {
		case "turn":
			// **ターンが始まらなかったことを、ターンの終わりとして伝える。**
			// turn/completed は来ないので、畳まないと 60 分 running のまま残る。
			if m.Error != nil {
				ev.turnEnd = true
				ev.err = "ターンを始められなかった: " + m.Error.Message
			}
		case "ctl":
			ev.deliver = m.key()
			if m.Error != nil {
				ev.payload, _ = json.Marshal(map[string]any{"error": m.Error.Message})
			} else {
				ev.payload = m.Result
			}
		}
		return ev
	case m.isRequest():
		ev := codexEvent{kind: "request/" + m.Method}
		tool, ok := codexApprovals[m.Method]
		if !ok {
			ev.replies = append(ev.replies, refuseFrame(m.ID, m.Method, ""))
			ev.err = "答えられない要求を断った: " + m.Method
			return ev
		}
		detail, why := cs.askDetail(m.Method, m.Params)
		if why != "" {
			// **途中までの中身で許させない。** 断って、そう記録する。
			ev.replies = append(ev.replies, decisionFrame(m.ID, "decline"))
			ev.err = why + "ので断った（" + tool + "）"
			return ev
		}
		a := HeldAsk{ReqID: m.key(), Tool: tool, Detail: detail}
		cs.asks[a.ReqID] = a
		ev.ask = &a
		return ev
	}

	ev := codexEvent{kind: m.Method}
	if m.Method == "" {
		ev.kind = "?"
		return ev
	}
	if codexPolicyChanged[m.Method] {
		ev.kill = true
		ev.err = "方針が途中で変わった（" + m.Method + "）ので止めた"
		return ev
	}
	switch m.Method {
	case "turn/started":
		var p struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		json.Unmarshal(m.Params, &p)
		cs.turn = p.Turn.ID
		if cs.wantInt && cs.turn != "" {
			// 中断はターン id が要る。先に頼まれていたなら、いま投げる。
			cs.wantInt = false
			if b, ok := cs.interruptLocked(); ok {
				ev.replies = append(ev.replies, b)
			}
		}
	case "turn/completed":
		var p struct {
			Turn struct {
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		json.Unmarshal(m.Params, &p)
		ev.turnEnd = true
		ev.interrupted = p.Turn.Status == "interrupted"
		if p.Turn.Status == "failed" {
			ev.err = "ターンが失敗した"
			if p.Turn.Error != nil && p.Turn.Error.Message != "" {
				ev.err += ": " + p.Turn.Error.Message
			}
		}
		cs.turn, cs.wantInt = "", false
	case "error":
		var p struct {
			WillRetry bool `json:"willRetry"`
			Error     struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(m.Params, &p)
		if !p.WillRetry {
			ev.turnEnd = true
			ev.err = "Codex がエラーを返した: " + p.Error.Message
			cs.turn, cs.wantInt = "", false
		}
	case "thread/closed":
		ev.turnEnd = true
		ev.err = "スレッドが閉じた"
		cs.turn, cs.wantInt = "", false
	case "item/started", "item/completed":
		var p struct {
			Item struct {
				Type    string          `json:"type"`
				ID      string          `json:"id"`
				Changes json.RawMessage `json:"changes"`
			} `json:"item"`
		}
		json.Unmarshal(m.Params, &p)
		if p.Item.Type == "fileChange" && p.Item.ID != "" {
			if m.Method == "item/started" {
				cs.changes[p.Item.ID] = p.Item.Changes
			} else {
				delete(cs.changes, p.Item.ID)
			}
		}
	case "item/fileChange/patchUpdated":
		// **承認を訊いたあとで差分が変わったなら、訊いた中身で許させない。**
		// その item の承認を全部断り、campd の台帳からも取り下げる。item の id が
		// 無ければ何とも結べないので、何もしない（コマンドの承認まで巻き込まない）。
		var p struct {
			ItemID  string          `json:"itemId"`
			Changes json.RawMessage `json:"changes"`
		}
		json.Unmarshal(m.Params, &p)
		if p.ItemID == "" {
			return ev
		}
		if len(p.Changes) > 0 {
			cs.changes[p.ItemID] = p.Changes
		}
		for id, a := range cs.asks {
			if askItem(a.Detail) != p.ItemID {
				continue
			}
			delete(cs.asks, id)
			ev.replies = append(ev.replies, decisionFrame(json.RawMessage(id), "decline"))
			ev.withdrawn = append(ev.withdrawn, id)
			ev.err = "承認を訊いたあとで差分が変わったので断った"
		}
	case "serverRequest/resolved":
		// Codex 側で片付いた（中断など）。**まだ答えていなかったなら、台帳からも取り下げる。**
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		json.Unmarshal(m.Params, &p)
		id := string(bytes.TrimSpace(p.RequestID))
		if _, waiting := cs.asks[id]; waiting {
			delete(cs.asks, id)
			ev.withdrawn = append(ev.withdrawn, id)
			ev.err = "答えないうちに Codex 側で片付いた"
		}
	case "thread/tokenUsage/updated":
		var p struct {
			TokenUsage json.RawMessage `json:"tokenUsage"`
		}
		json.Unmarshal(m.Params, &p)
		if len(p.TokenUsage) > 0 {
			cs.usage = p.TokenUsage
		}
	}
	return ev
}

// askDetail は承認カードに出す中身。why が空でなければ、見せられないので断る。
func (cs *codexState) askDetail(method string, params json.RawMessage) ([]byte, string) {
	d := map[string]any{"agent": AgentCodex, "method": method, "params": params}
	// **本人が見る中身と、走るものを取り違えさせない**（codex の outer gate の指摘 4）。
	// 欠けている・別のスレッド・許した場所の外なら、見せずに断る。
	var ids struct {
		ThreadID string `json:"threadId"`
	}
	json.Unmarshal(params, &ids)
	if cs.thread != "" && ids.ThreadID != cs.thread {
		return nil, "別のスレッド（" + ids.ThreadID + "）の承認"
	}
	if method == "item/commandExecution/requestApproval" {
		var p struct {
			Command *string `json:"command"`
			Cwd     *string `json:"cwd"`
		}
		json.Unmarshal(params, &p)
		switch {
		case p.Command == nil || strings.TrimSpace(*p.Command) == "":
			return nil, "何を走らせるのか分からない（command が無い）"
		case p.Cwd == nil || !filepath.IsAbs(*p.Cwd):
			return nil, "どこで走らせるのか分からない（cwd が無い）"
		case cs.root != "" && !under(filepath.Clean(*p.Cwd), cs.root):
			return nil, "許した場所の外（" + *p.Cwd + "）で走らせようとした"
		}
	}
	if method == "item/fileChange/requestApproval" {
		var p struct {
			ItemID    string          `json:"itemId"`
			GrantRoot json.RawMessage `json:"grantRoot"`
		}
		json.Unmarshal(params, &p)
		// grantRoot は「残りのセッションでこの下の書き込みを許す」（スキーマで UNSTABLE）。
		// 許すと以後その下は訊かれなくなりうる。**許す／断るの2つに収まらないので断る。**
		if g := strings.TrimSpace(string(p.GrantRoot)); g != "" && g != "null" {
			return nil, "書き込みの範囲を広げる承認（grantRoot " + g + "）"
		}
		ch, ok := cs.changes[p.ItemID]
		if !ok || len(ch) == 0 || string(ch) == "null" {
			return nil, "何を変えるのか分からない（差分が来ていない）"
		}
		d["changes"] = ch
		d["item"] = p.ItemID
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil, "中身を組み立てられない"
	}
	if len(b) > maxApprovalDetail {
		return nil, fmt.Sprintf("中身が画面へ渡せる大きさを越えた（%d バイト）", len(b))
	}
	return b, ""
}

func askItem(detail []byte) string {
	var d struct {
		Item string `json:"item"`
	}
	json.Unmarshal(detail, &d)
	return d.Item
}

// refuseFrame は答えられない要求への断り。**黙らない**——黙ると Codex は待ち続ける。
func refuseFrame(id json.RawMessage, method, when string) []byte {
	msg := "Camp はこの要求に答えられないので断る: " + method
	if when != "" {
		msg = when + "の要求には答えられないので断る: " + method
	}
	return rpcFrame(map[string]any{"id": id,
		"error": map[string]any{"code": -32601, "message": msg}})
}

func decisionFrame(id json.RawMessage, decision string) []byte {
	return rpcFrame(map[string]any{"id": id, "result": map[string]any{"decision": decision}})
}

// ---------------------------------------------------------------- Codex へ渡すもの

// input は1ターン分の入力。
func (cs *codexState) input(text string) ([]byte, error) {
	cs.mu.Lock()
	thread := cs.thread
	cs.mu.Unlock()
	if thread == "" {
		return nil, errors.New("まだスレッドが無い")
	}
	b, _ := cs.request("turn", "turn/start", map[string]any{
		"threadId": thread,
		"input":    []any{map[string]any{"type": "text", "text": text}},
	})
	return b, nil
}

// approve は待っている承認への答え。**見た id 以外には答えない**——campd から来た
// 文字列をそのまま JSON に埋めると、別の中身を差し込める。
//
// 断るのは decline（そのコマンドだけ飛ばしてターンは続く）。Codex には理由を渡す欄が無い。
func (cs *codexState) approve(reqID, behavior string) ([]byte, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if _, ok := cs.asks[reqID]; !ok {
		return nil, fmt.Errorf("その承認は待っていない: %s", reqID)
	}
	delete(cs.asks, reqID)
	decision := "decline"
	if behavior == "allow" {
		decision = "accept"
	}
	return decisionFrame(json.RawMessage(reqID), decision), nil
}

// interrupt は中断。ターン id がまだ無ければ、来たところで投げる（nil を返す）。
func (cs *codexState) interrupt() []byte {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	b, ok := cs.interruptLocked()
	if !ok {
		cs.wantInt = true
	}
	return b
}

func (cs *codexState) interruptLocked() ([]byte, bool) {
	if cs.turn == "" || cs.thread == "" {
		return nil, false
	}
	cs.seq++
	id := fmt.Sprintf("camp-%d", cs.seq)
	k, _ := json.Marshal(id)
	cs.own[string(k)] = "interrupt"
	return rpcFrame(map[string]any{"id": id, "method": "turn/interrupt",
		"params": map[string]any{"threadId": cs.thread, "turnId": cs.turn}}), true
}

// control は残量の問い合わせ。**どちらもモデルを呼ばない。**
//
//   - get_usage         → account/rateLimits/read（5時間枠・週枠）
//   - get_context_usage → 最後の thread/tokenUsage/updated（問い合わせずに手元の控えを返す）
func (cs *codexState) control(kind string) (req []byte, key string, now json.RawMessage, err error) {
	switch kind {
	case "get_usage":
		req, key = cs.request("ctl", "account/rateLimits/read", nil)
		return req, key, nil, nil
	case "get_context_usage":
		cs.mu.Lock()
		defer cs.mu.Unlock()
		b, _ := json.Marshal(map[string]any{"tokenUsage": cs.usage})
		return nil, "", b, nil
	}
	return nil, "", nil, fmt.Errorf("Codex では扱わない問い合わせ: %s", kind)
}

// waiting は待っている承認。引き取り直しで campd へ名乗る。
func (cs *codexState) waiting() []HeldAsk {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]HeldAsk, 0, len(cs.asks))
	for _, a := range cs.asks {
		out = append(out, a)
	}
	return out
}

// ---------------------------------------------------------------- 実行面で起こす

// DefaultCodexHome は Camp 専用の Codex の置き場。**本人の ~/.codex とは別。**
func DefaultCodexHome() string {
	if p := os.Getenv("CAMP_CODEX_HOME"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local/share/camp/codex")
}

// DefaultCodexSource は本人の Codex の置き場（道具とログインをここから借りる）。
func DefaultCodexSource() string {
	if p := os.Getenv("CODEX_HOME"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// defaultCodexCommand は Camp の置き場を CODEX_HOME にして app-server を起こす。
// scope で包むのは Claude と同じ理由（孫まで止める）。Codex の子は別のプロセスグループ・
// 別のセッションに居る（実測）ので、**プロセスグループでは取り逃がす。**
//
// CODEX_HOME が効いたかは、話し始める前に initialize の応答で照らす（verifyCodexHome）。
func (a *Agent) defaultCodexCommand(id string) *exec.Cmd {
	var cmd *exec.Cmd
	if a.Scope {
		cmd = exec.Command("systemd-run", "--user", "--scope", "--quiet", "--collect",
			"--unit", scopeName(id), "--", a.Codex, "app-server")
	} else {
		cmd = exec.Command(a.Codex, "app-server")
	}
	cmd.Env = append(os.Environ(), "CODEX_HOME="+a.CodexHome)
	return cmd
}

// CanCodex は Codex を起こせる形になっているか。hello で名乗る。
func (a *Agent) CanCodex() bool {
	if a.Codex == "" || a.CodexHome == "" || a.CodexSource == "" {
		return false
	}
	if !a.Scope && !a.CodexWithoutScope {
		return false
	}
	_, err := os.Stat(a.Codex)
	return err == nil
}

// agents は起こせるエージェント。
func (a *Agent) agents() []string {
	out := []string{AgentClaude}
	if a.CanCodex() {
		out = append(out, AgentCodex)
	}
	return out
}

// startCodex は Codex の子を起こす。流れは start（Claude）と同じで、話し始める前の
// 3往復と、置き場・方針の照合が加わる。
func (a *Agent) startCodex(m Msg) {
	fail := func(why string) {
		a.send(Msg{T: MsgFailed, Session: m.Session, Token: m.Token, Error: why})
	}
	real, err := resolveCwd(m.Cwd)
	if err != nil {
		fail(err.Error())
		return
	}
	if m.Root == "" || !under(real, m.Root) {
		fail(fmt.Sprintf("許した場所（%s）の外を渡された: %s", m.Root, real))
		return
	}
	if !a.Scope && !a.CodexWithoutScope {
		fail("scope を使わない構成では Codex を起こさない（子が別のプロセスグループ・別のセッションに居て、止めるときに取り逃がす）")
		return
	}
	if !a.CanCodex() {
		fail("この実行面は Codex を起こせない（codex の実体か置き場が無い）")
		return
	}
	if err := prepareCodexHome(a.CodexHome, a.CodexSource); err != nil {
		fail("Camp の Codex の置き場を用意できない: " + err.Error())
		return
	}

	cmd := a.CodexCommand(m.Session)
	cmd.Dir = real
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fail(err.Error())
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fail(err.Error())
		return
	}
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fail(err.Error())
		return
	}
	pid := cmd.Process.Pid
	st, _ := Starttime(pid)

	k := &child{id: m.Session, token: m.Token, cmd: cmd, stdin: stdin,
		pending: map[string]chan []byte{}, codex: newCodexState()}
	if a.Scope {
		k.scope = scopeName(m.Session)
	}
	giveUp := func(why string) {
		k.kill()
		cmd.Wait()
		if k.log != nil {
			k.log.Close()
		}
		fail(why)
	}
	// **照合したパスと、実際に降りた場所が同じか**（start と同じ。TOCTOU）。
	if where, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err == nil {
		if !under(where, m.Root) {
			giveUp(fmt.Sprintf("起こした先が許した場所の外だった（%s）。止めた", where))
			return
		}
	} else {
		fmt.Fprintf(os.Stderr,
			"camp agent: %s の実際の cwd を確かめられない: %v\n", m.Session[:8], err)
	}
	if lg, err := OpenLog(a.LogDir, m.Session); err == nil {
		k.log = lg
	} else {
		k.logBroken = true
		fmt.Fprintf(os.Stderr, "camp agent: 落とし先を開けない（%v）。フレームは残らない\n", err)
		a.send(Msg{T: MsgDropped, Session: m.Session, Token: m.Token, Dropped: -1,
			Error: "落とし先を開けない: " + err.Error()})
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	// **話し始める前の3往復に時間を切る。** 黙った子を待ち続けない。
	timer := time.AfterFunc(a.HeaderWait, func() { k.kill() })
	err = k.codex.handshake(sc, stdin, real, a.CodexHome, func(kind string, line []byte) {
		a.record(k, kind, line)
	})
	if !timer.Stop() && err != nil {
		err = fmt.Errorf("%v 待っても話し始められない（%v）", a.HeaderWait, err)
	}
	if err != nil {
		giveUp(err.Error())
		return
	}

	a.mu.Lock()
	a.kids[m.Session] = k
	wanted, wasAsked := a.stopWanted[m.Session]
	delete(a.stopWanted, m.Session)
	a.mu.Unlock()

	a.send(Msg{T: MsgStarted, Session: m.Session, Token: m.Token, Agent: AgentCodex,
		PID: pid, Started: st, BootID: BootID(), Scope: k.scope, ClaudeID: k.codex.threadID()})
	if wasAsked {
		go a.stop(Msg{Session: m.Session, Token: m.Token, Mode: wanted})
	}

	go a.drainCodex(k, sc)

	err = cmd.Wait()
	code, reason := 0, "終わった"
	if err != nil {
		reason = err.Error()
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	k.mu.Lock()
	k.dead = true
	k.mu.Unlock()
	// **app-server が終わっても、scope に残りが居るかもしれない**（コマンドは別のセッション、
	// MCP の子は別のプロセスグループ）。数えて止め、止め切れなければそう言う（scope.go）。
	left := a.leftovers(k)
	if left != 0 {
		reason += leftoverNote(left)
	}
	if k.log != nil {
		k.log.Close()
	}
	a.mu.Lock()
	delete(a.kids, m.Session)
	a.mu.Unlock()
	a.send(Msg{T: MsgExited, Session: m.Session, Token: m.Token, Code: code, Reason: reason,
		Leftover: left})
}

// drainCodex は Codex の stdout を読み続ける。**必ず落としてから**畳んで渡す。
func (a *Agent) drainCodex(k *child, sc *bufio.Scanner) {
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		ev := k.codex.classify(line)
		a.record(k, ev.kind, line)
		if ev.deliver != "" {
			deliverRaw(k, ev.deliver, ev.payload)
		}
		for _, r := range ev.replies {
			if err := k.write(r); err != nil {
				fmt.Fprintf(os.Stderr, "camp agent: %s へ返せない: %v\n", k.id[:8], err)
			}
		}
		if ev.turnEnd {
			k.mu.Lock()
			k.turn = false
			k.mu.Unlock()
		}
		thread := k.codex.threadID()
		m := Msg{T: MsgFrame, Session: k.id, Token: k.token, Kind: ev.kind,
			ClaudeID: thread, TurnEnd: ev.turnEnd, Interrupted: ev.interrupted}
		if len(ev.withdrawn) == 0 {
			m.Error = ev.err
		}
		if ev.ask != nil {
			m.Ask, m.ReqID, m.Text, m.Frame = true, ev.ask.ReqID, ev.ask.Tool, ev.ask.Detail
		}
		if debugFrames {
			fmt.Fprintf(os.Stderr, "camp agent: %s %s\n", k.id[:8], m.Kind)
		}
		a.send(m)
		// 取り下げは1つずつ伝える（campd は ReqID ごとに台帳を閉じる）。
		for _, id := range ev.withdrawn {
			a.send(Msg{T: MsgFrame, Session: k.id, Token: k.token, Kind: "camp/withdrawn",
				ClaudeID: thread, ReqID: id, Withdrawn: true, Error: ev.err})
		}
		if ev.kill {
			a.killChild(k)
		}
	}
}

// write は子の stdin へ1行。
func (k *child) write(frame []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.dead {
		return errors.New("子はもう居ない")
	}
	_, err := k.stdin.Write(append(frame, '\n'))
	return err
}

// deliverRaw は待っている者へ応答を渡す。
func deliverRaw(k *child, key string, payload json.RawMessage) {
	k.mu.Lock()
	ch := k.pending[key]
	delete(k.pending, key)
	k.mu.Unlock()
	if ch == nil {
		return
	}
	if len(payload) == 0 {
		payload = json.RawMessage("null")
	}
	select {
	case ch <- payload:
	default:
	}
}

// controlCodex は残量の問い合わせを Codex へ投げる（または手元の控えを返す）。
func (a *Agent) controlCodex(k *child, m Msg) {
	out := Msg{T: MsgCtlRes, Session: m.Session, ReqID: m.ReqID}
	req, key, now, err := k.codex.control(m.Kind)
	switch {
	case err != nil:
		out.Error = err.Error()
		a.send(out)
		return
	case req == nil:
		out.Frame = now
		a.send(out)
		return
	}
	ch := make(chan []byte, 1)
	k.mu.Lock()
	k.pending[key] = ch
	k.mu.Unlock()
	defer func() {
		k.mu.Lock()
		delete(k.pending, key)
		k.mu.Unlock()
	}()
	if err := k.write(req); err != nil {
		out.Error = err.Error()
		a.send(out)
		return
	}
	select {
	case payload := <-ch:
		out.Frame = payload
	case <-time.After(10 * time.Second):
		out.Error = "子が答えない" // **返ってこないことを「空」と読まない。**
	}
	a.send(out)
}

// ---------------------------------------------------------------- Camp 専用の置き場

// prepareCodexHome は Camp 専用の Codex の置き場を**起こすたびに**整える。
//
//   - config.toml は本人の置き場から作り直す（filterCodexConfig）。道具は本人と同じにし、
//     信頼済みの場所・権限のプロファイルは持ち込まない。Codex がここへ書き足した
//     「信頼済みの場所」も、次に起こすときに消える
//   - auth.json・plugins・skills は本人の置き場への symlink（ログインは共有。本人の決定）
//   - **rules は置かない。** 本人が対話で「今後訊かない」にしたものを持ち込まない
//     （持ち込むと、承認なしで sandbox の外で走る。実測）
func prepareCodexHome(home, source string) error {
	if home == "" || source == "" {
		return errors.New("置き場か、本人の置き場が決まっていない")
	}
	if filepath.Clean(home) == filepath.Clean(source) {
		return errors.New("本人の Codex の置き場そのものは使わない")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	// **綴りが違っても同じ場所なら使わない**（symlink・別綴り）。作り直しが本人の
	// config.toml を上書きする（Fable の実装後レビュー 3）。
	if hi, err := os.Stat(home); err != nil {
		return err
	} else if si, err := os.Stat(source); err == nil && os.SameFile(hi, si) {
		return fmt.Errorf("%s は本人の Codex の置き場（%s）と同じ場所。使わない", home, source)
	}
	if _, err := os.Lstat(filepath.Join(home, "rules")); err == nil {
		return fmt.Errorf("%s がある。Camp の Codex には「今後訊かない」を持ち込まない。消してから起こす",
			filepath.Join(home, "rules"))
	}
	src, err := os.ReadFile(filepath.Join(source, "config.toml"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(home, ".config.toml.camp-*")
	if err != nil {
		return err
	}
	if _, err := f.Write(filterCodexConfig(src)); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(home, "config.toml")); err != nil {
		os.Remove(f.Name())
		return err
	}
	for _, name := range []string{"auth.json", "plugins", "skills"} {
		if err := linkInto(home, source, name); err != nil {
			return err
		}
	}
	return nil
}

// linkInto は home/name を source/name への symlink にする。**写しは作らない**
// ——ログインの写しを作ると、鍵の更新で片方が古くなる。
func linkInto(home, source, name string) error {
	dst, want := filepath.Join(home, name), filepath.Join(source, name)
	for try := 0; try < 2; try++ {
		if cur, err := os.Readlink(dst); err == nil {
			if cur == want {
				return nil
			}
			return fmt.Errorf("%s が %s を指している（%s のはず）", dst, cur, want)
		}
		if _, err := os.Lstat(dst); err == nil {
			return fmt.Errorf("%s が symlink ではない。写しを置かない——消すか %s へのリンクにしてから起こす",
				dst, want)
		}
		if _, err := os.Stat(want); err != nil {
			if name == "auth.json" {
				return fmt.Errorf("本人の Codex がログインしていない（%s が無い）", want)
			}
			return nil // 道具が無いだけ
		}
		if err := os.Symlink(want, dst); err == nil || !errors.Is(err, os.ErrExist) {
			return err
		}
		// 同時に起こした別のセッションが先に作った。もう一度見る。
	}
	return fmt.Errorf("%s を用意できない", dst)
}

// codexDropSections は Camp の置き場へ持ち込まない表（と、その下の表）。キーの並びで持つ。
//
//   - projects      信頼済みの場所。Camp から起こすと Codex がここへ書き足す（実測）
//   - permissions   本人の権限のプロファイル。sandbox は Camp が決める
//   - tui / desktop 端末とデスクトップの設定
//   - marketplaces.openai-bundled  source が本人の置き場を指したままだと、同梱のプラグイン
//     （computer use など）が読まれない。落とせば Codex が自分で見つける（実測）
var codexDropSections = [][]string{{"projects"}, {"permissions"}, {"tui"}, {"desktop"},
	{"marketplaces", "openai-bundled"}}

// codexDropKeys は持ち込まない、先頭（どの表にも属さない）のキー。ドット付きのキー
// （`projects."/x".trust_level = …`）とインラインの表（`projects = { … }`）も、最初の
// 区切りで見る。
var codexDropKeys = []string{"default_permissions", "projects", "permissions"}

// filterCodexConfig は本人の config.toml から Camp の置き場の config.toml を作る。
//
// TOML を丸ごと読み下さず行で扱うが、**見出しとキーは TOML の区切りどおりに読む**
// （引用符・空白・ドットを正規化する）。複数行の値（`"""`・`'''`・閉じていない `[` `{`）の
// 途中の行は、見出しと読まず、その値の持ち主と一緒に残すか落とす（Fable の実装後レビュー 4）。
func filterCodexConfig(src []byte) []byte {
	var out bytes.Buffer
	out.WriteString("# Camp が Codex を起こすたびに ~/.codex/config.toml から作り直す。\n" +
		"# ここを書き換えても次で消える（dev/active/phase3.6-plan.md）。\n")
	write := func(l string) {
		out.WriteString(l)
		out.WriteByte('\n')
	}
	dropSec, dropVal, inSection := false, false, false
	ml, depth := "", 0 // 開いている複数行の文字列の区切り、開いている [ { の深さ
	for _, l := range strings.Split(string(src), "\n") {
		if ml != "" || depth > 0 {
			// 複数行の値の続き。持ち主の行と同じ扱い。
			if ml != "" {
				if strings.Count(l, ml)%2 == 1 {
					ml = ""
				}
			} else if depth += bracketDelta(l); depth < 0 {
				depth = 0
			}
			if !dropSec && !dropVal {
				write(l)
			}
			continue
		}
		s := strings.TrimSpace(l)
		dropVal = false
		if path, ok := headerPath(s); ok {
			inSection = true
			dropSec = hasPrefixPath(path, codexDropSections)
			if !dropSec {
				write(l)
			}
			continue
		}
		if k, v, ok := cutKey(s); ok {
			if path := tomlKeyPath(k); !inSection && contains(codexDropKeys, path[0]) {
				dropVal = true
			}
			for _, d := range []string{`"""`, `'''`} {
				if strings.Count(v, d)%2 == 1 {
					ml = d
					break
				}
			}
			if ml == "" {
				if depth = bracketDelta(v); depth < 0 {
					depth = 0
				}
			}
		}
		if !dropSec && !dropVal {
			write(l)
		}
	}
	return out.Bytes()
}

// headerPath は `[a.b]`・`[[a.b]]`・`[ "a" . 'b' ] # 注` を見出しとして読み、キーの並びを返す。
func headerPath(s string) ([]string, bool) {
	if !strings.HasPrefix(s, "[") {
		return nil, false
	}
	body := strings.TrimPrefix(strings.TrimPrefix(s, "["), "[")
	q := byte(0)
	for i := 0; i < len(body); i++ {
		c := body[i]
		if q != 0 {
			if c == '\\' && q == '"' {
				i++
			} else if c == q {
				q = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			q = c
		case ']':
			return tomlKeyPath(body[:i]), true
		}
	}
	return nil, false
}

// tomlKeyPath はキー（`a.b`・`"a" . b`・`'a.b'.c`）をドットで区切り、引用符と空白を外す。
func tomlKeyPath(s string) []string {
	var out []string
	var cur strings.Builder
	q := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == '\\' && q == '"' && i+1 < len(s) {
				cur.WriteByte(s[i+1])
				i++
			} else if c == q {
				q = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '"' || c == '\'':
			q = c
		case c == '.':
			out = append(out, cur.String())
			cur.Reset()
		case c == ' ' || c == '\t':
		default:
			cur.WriteByte(c)
		}
	}
	return append(out, cur.String())
}

// cutKey は `key = value` を、引用符の外の最初の `=` で分ける。注釈行・空行は分けない。
func cutKey(s string) (key, value string, ok bool) {
	if s == "" || strings.HasPrefix(s, "#") {
		return "", "", false
	}
	q := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if q != 0 {
			if c == '\\' && q == '"' {
				i++
			} else if c == q {
				q = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			q = c
		case '=':
			return s[:i], s[i+1:], true
		case '#':
			return "", "", false
		}
	}
	return "", "", false
}

// bracketDelta は文字列と注釈の外にある `[` `{` と `]` `}` の差。
func bracketDelta(s string) int {
	n, q := 0, byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if q != 0 {
			if c == '\\' && q == '"' {
				i++
			} else if c == q {
				q = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			q = c
		case '#':
			return n
		case '[', '{':
			n++
		case ']', '}':
			n--
		}
	}
	return n
}

func hasPrefixPath(path []string, prefixes [][]string) bool {
	for _, p := range prefixes {
		if len(path) < len(p) {
			continue
		}
		match := true
		for i := range p {
			if path[i] != p[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
