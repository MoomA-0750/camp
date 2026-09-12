package session

// Codex の駆動器（`codex app-server`、stdio の JSON-RPC、1行1メッセージ）。
//
// 実測と設計は dev/active/phase3.6-plan.md・phase3.7-plan.md（codex-cli 0.154.0）。
// **Codex は本人の置き場（`~/.codex`）で、CLI と同じ設定のまま起こす**（D-030。Phase 3.6 の
// Camp 専用の置き場は覆した）。campd へは Claude と同じ Msg で渡し、「ターンが終わった」
// 「承認が来た」は欄（TurnEnd / Ask）で伝える。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type codexDriver struct{}

func (codexDriver) Info() AgentInfo {
	return AgentInfo{Name: AgentCodex, Label: "Codex", Perms: append([]string(nil), allPerms...),
		Notes: []string{
			"「中断」はターンを止めるが、走っていたコマンドは残る（Codex の作り。2026-09-11 実測）。確実に止めるなら「止める」",
			"MCP のツールは Codex の作りとして承認を訊いてこない",
			"「編集は訊かない」は近いもの（on-request・workspace-write）。Codex に「編集だけ訊かない」は無いので、" +
				"作業場所の中のコマンドも訊かずに走る。ネットワークや場所の外への書き込みは、Codex が権限を上げて頼まない限り" +
				"訊かれずに失敗する（2026-09-12 実測）",
		},
		InterruptLeavesTools: true, Remote: true, Resume: true}
}

// RemoteLaunch は向こうで探す名前と置き場（$CODEX_HOME、無ければ ~/.codex。CLI と同じ）。
func (codexDriver) RemoteLaunch() RemoteLaunch {
	return RemoteLaunch{Name: "codex", HomeEnv: "CODEX_HOME", HomeDefault: ".codex"}
}

// Argv は `codex app-server`（stdio の JSON-RPC）。確認の度合いは引数でなく thread/start の欄で渡す
// （codexPerms）。
// Argv は app-server だけ。**続きから起こすのは引数では頼まない**——Codex は
// `thread/resume` という呼び出しで続ける（handshake の中。M48、2026-09-13）。
func (codexDriver) Argv(perm, _ string) ([]string, error) {
	if !validPerm(perm) {
		return nil, fmt.Errorf("Codex の確認の度合い %s は扱わない", perm)
	}
	return []string{"app-server"}, nil
}

// codexPerms は確認の度合いごとに thread/start へ渡す欄（2026-09-12、thread/start の応答で効くことを
// 測った）。cli は何も渡さない（本人の設定のまま）。「編集は訊かない」は Codex にぴったり当たるものが
// 無いので、近いもの（sandbox の中は訊かず、外に出るときだけ訊く）で出す（本人の決定）。
var codexPerms = map[string]map[string]any{
	PermAsk:   {"approvalPolicy": "untrusted"},
	PermEdits: {"approvalPolicy": "on-request", "sandbox": "workspace-write"},
	PermAuto:  {"approvalsReviewer": "auto_review"},
	PermFull:  {"approvalPolicy": "never", "sandbox": "danger-full-access"},
}

// codexSandboxType は thread/start で渡す sandbox の名前と、応答が名乗る型の名前（実測）。
var codexSandboxType = map[string]string{
	"read-only": "readOnly", "workspace-write": "workspaceWrite", "danger-full-access": "dangerFullAccess",
}

// verifyCodexPerm は、渡した確認の度合いの欄が thread/start の応答で効いているかを照らす。
// **渡したものだけ照らす**（cli では何も渡さず、何も照らさない）。欠けていても断る。
func verifyCodexPerm(res json.RawMessage, sent map[string]any) error {
	if len(sent) == 0 {
		return nil
	}
	var r struct {
		ApprovalPolicy    *string `json:"approvalPolicy"`
		ApprovalsReviewer *string `json:"approvalsReviewer"`
		Sandbox           *struct {
			Type string `json:"type"`
		} `json:"sandbox"`
	}
	json.Unmarshal(res, &r)
	str := func(p *string) string {
		if p == nil {
			return "（無い）"
		}
		return *p
	}
	var why []string
	if v, ok := sent["approvalPolicy"]; ok && str(r.ApprovalPolicy) != v {
		why = append(why, fmt.Sprintf("approvalPolicy が %s（%v のはず）", str(r.ApprovalPolicy), v))
	}
	if v, ok := sent["approvalsReviewer"]; ok && str(r.ApprovalsReviewer) != v {
		why = append(why, fmt.Sprintf("approvalsReviewer が %s（%v のはず）", str(r.ApprovalsReviewer), v))
	}
	if v, ok := sent["sandbox"].(string); ok {
		got := "（無い）"
		if r.Sandbox != nil {
			got = r.Sandbox.Type
		}
		if got != codexSandboxType[v] {
			why = append(why, fmt.Sprintf("sandbox が %s（%s のはず）", got, codexSandboxType[v]))
		}
	}
	if len(why) > 0 {
		return fmt.Errorf("頼んだ確認の度合いで起きていない。話し始めない: %s", strings.Join(why, "・"))
	}
	return nil
}

// codexOptOut は受け取らない通知。途中経過は Claude でも取っていない
// （`--include-partial-messages` を付けていない）。完成品は item/completed に全文がある。
// **ターンの終わり・承認・設定の変化に関わるものは入れない**（テストで縛る）。
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

// codexSettingsChanged は途中で設定が変わったしるし。**止めずに記録する**（2026-09-12、本人。
// CLI では止まらない。D-030）。以前はその場で止めていた。
var codexSettingsChanged = map[string]bool{
	"thread/settings/updated": true,
}

// codexState は Codex の子1本ぶんの状態。
type codexState struct {
	mu     sync.Mutex
	thread string
	// root は起こした場所（実パス）。承認のコマンドの cwd がこの外なら、印を付けて見せる。
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
// **本人の置き場で起きたことと、作業場所を照らしてから**スレッド id を覚える。
// rec は流れた行を落とし先へ残す。
//
// 方針（承認・sandbox）は何も渡さない——本人の設定のまま（CLI と同じ）。確認の度合いを
// セッションごとに選ぶ口は Phase 3.7 の M41 で足す（渡したものだけ照らす）。
// resume が空でなければ `thread/start` の代わりに `thread/resume` を呼び、その会話の続きから
// 始める（M48、2026-09-13）。**続けたスレッドの id は元と同じであることまで照らす**——
// 違う id が返ったら、別の会話の続きを本人に見せることになる。
func (cs *codexState) handshake(sc *bufio.Scanner, w io.Writer, cwd string,
	checkHome func(json.RawMessage) error, perm map[string]any, resume string,
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
	// **本人の置き場で起きたか。** CLI と同じ設定で動かすと約束しているので、取り違えて
	// 別の置き場（別の設定・別のログイン）で起きていたら話し始めない。
	if err := checkHome(res); err != nil {
		return err
	}
	if err := write(rpcFrame(map[string]any{"method": "initialized"})); err != nil {
		return err
	}
	// 確認の度合いの欄だけを足す（cli なら cwd だけ。本人の設定のまま）。
	//
	// 続きから起こすときは `thread/resume {threadId}`（実測 2026-09-13。一発で通り、文脈が
	// 続き、スレッド id は元のまま）。**確認の度合いはここでも頼む**——元が「毎回訊く」だった
	// のに続きで本人の設定へ戻ると、安全側でない驚きになる。効いたかは下の verifyCodexPerm が
	// 照らし、効いていなければ話し始めない（実体には「読み込み済みのスレッドへの上書きは無視」
	// という文言があるので、無視されたらそこで止まる）。
	method, params := "thread/start", map[string]any{"cwd": cwd}
	if resume != "" {
		method, params = "thread/resume", map[string]any{"threadId": resume}
	}
	for k, v := range perm {
		params[k] = v
	}
	start, key := cs.request("start", method, params)
	if err := write(start); err != nil {
		return err
	}
	res, err = cs.await(sc, w, key, rec)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	thread, err := verifyCodexStart(res, cwd)
	if err != nil {
		return err
	}
	// **続けたのが頼んだスレッドか。** 別の id が返ったら、別の会話の続きを見せることになる。
	if resume != "" && thread != resume {
		return fmt.Errorf("Codex が別のスレッド %s を続けた（%s のはず）。話し始めない", thread, resume)
	}
	if err := verifyCodexPerm(res, perm); err != nil {
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

// verifyCodexHome は initialize の応答が名乗る置き場が、本人の置き場か（実パスで）照らす。
// **名乗らないことを「合っている」と読まない。**
func verifyCodexHome(res json.RawMessage, home string) error {
	var r struct {
		CodexHome string `json:"codexHome"`
	}
	json.Unmarshal(res, &r)
	if r.CodexHome == "" {
		return errors.New("Codex がどの置き場で起きたか名乗らない。本人の設定で起きたと確かめられないので話し始めない")
	}
	got, err1 := filepath.EvalSymlinks(r.CodexHome)
	want, err2 := filepath.EvalSymlinks(home)
	if err1 != nil || err2 != nil || got != want {
		return fmt.Errorf("Codex が本人の置き場ではない %s で起きた（%s のはず）。"+
			"CLI と別の設定で動くので話し始めない", r.CodexHome, home)
	}
	return nil
}

// verifyRemoteCodexHome は向こうのホストで、Codex が本人の置き場で起きたかを照らす。向こうのパスは
// 手元で実パスに直せないので、向こうの sh が名乗った置き場（直す前か実パス）と文字の上で比べる。
// **名乗らないことを「合っている」と読まない。**
func verifyRemoteCodexHome(res json.RawMessage, homes []string) error {
	var r struct {
		CodexHome string `json:"codexHome"`
	}
	json.Unmarshal(res, &r)
	if r.CodexHome == "" {
		return errors.New("Codex がどの置き場で起きたか名乗らない。本人の設定で起きたと確かめられないので話し始めない")
	}
	for _, h := range homes {
		if h != "" && r.CodexHome == h {
			return nil
		}
	}
	return fmt.Errorf("Codex が向こうの本人の置き場ではない %s で起きた（%s のはず）。"+
		"CLI と別の設定で動くので話し始めない", r.CodexHome, strings.Join(homes, " か "))
}

var threadIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// verifyCodexStart は thread/start の応答から、作業場所とスレッド id を照らす。
// **欠けていても断る**（「無い」を「合っている」と読まない）。
//
// 承認・sandbox の方針は、Camp が何も渡していないので照らさない（本人の設定のまま）。
// 確認の度合いを渡すようになったら（M41）、渡したものだけをここで照らす。
func verifyCodexStart(res json.RawMessage, cwd string) (string, error) {
	var r struct {
		Thread *struct {
			ID string `json:"id"`
		} `json:"thread"`
		Cwd *string `json:"cwd"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return "", fmt.Errorf("thread/start の応答が読めない: %w", err)
	}
	var why []string
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
		return "", fmt.Errorf("Codex の起き方が頼んだものと違う。話し始めない: %s", strings.Join(why, "・"))
	}
	return thread, nil
}

// ---------------------------------------------------------------- 流れているフレームを畳む

// codexEvent は1行を Camp の言葉に直したもの。
type codexEvent struct {
	kind        string
	turnEnd     bool
	interrupted bool
	err         string          // 監査に残す一言（断った・ターンが失敗した）
	note        string          // 監査に残す記録（止めない。設定が途中で変わった等）
	ask         *HeldAsk        // 画面へ出す承認
	replies     [][]byte        // 実行面がすぐ Codex へ返すもの（断り・後回しの中断）
	withdrawn   []string        // もう答えを待たなくなった承認の id（campd の台帳を閉じる）
	deliver     string          // 待っている者へ渡す応答の id（残量の問い合わせ）
	payload     json.RawMessage // その中身
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
	if codexSettingsChanged[m.Method] {
		// **止めない。** 記録して画面に出す（本人の決定。CLI では止まらない）。
		ev.note = "設定が途中で変わった（" + m.Method + "）。止めずに記録した"
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
		// Codex 側で片付いた（中断・自動審査など）。**まだ答えていなかったなら、台帳からも取り下げる。**
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
	// **本人が見る中身と、走るものを取り違えさせない**（Phase 3.6 の outer gate の codex の指摘 4）。
	// 欠けている・別のスレッドなら、見せずに断る。
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
		}
		// 起こした場所の外で走らせようとしているなら、**断らずに印を付けて見せる**
		// （Camp は見せる。決めるのは本人。D-030）。
		if cs.root != "" && !under(filepath.Clean(*p.Cwd), cs.root) {
			d["outside"] = true
		}
		d["view"] = AskView{What: "command", Command: *p.Command, Cwd: *p.Cwd,
			Outside: d["outside"] == true, Reason: codexReason(params)}
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
		changes, err := codexChanges(ch)
		if err != nil {
			return nil, "何を変えるのか読めない（" + err.Error() + "）"
		}
		d["item"] = p.ItemID
		// 差分は view にだけ入れる（二重に持つと、画面へ渡せる大きさを半分しか使えない）。
		d["view"] = AskView{What: "file", Changes: changes, Reason: codexReason(params)}
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

// DefaultCodexHome は本人の Codex の置き場（CLI が使うのと同じ）。
func DefaultCodexHome() string {
	if p := os.Getenv("CODEX_HOME"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// Launch は Codex の実体と本人の置き場。**scope で包めない構成では起こさない**——Codex の子は
// 別のプロセスグループ・別のセッションに居て（実測）、止めるときに取り逃がす。
func (codexDriver) Launch(a *Agent) (Launch, error) {
	if !a.Scope && !a.CodexWithoutScope {
		return Launch{}, errors.New("scope を使わない構成では Codex を起こさない（子が別のプロセスグループ・別のセッションに居て、止めるときに取り逃がす）")
	}
	cannot := errors.New("この実行面は Codex を起こせない（codex の実体か置き場が無い）")
	if a.Codex == "" || a.CodexHome == "" {
		return Launch{}, cannot
	}
	if _, err := os.Stat(a.Codex); err != nil {
		return Launch{}, cannot
	}
	return Launch{Bin: a.Codex, Home: a.CodexHome}, nil
}

// CanCodex は Codex を起こせる形になっているか。
func (a *Agent) CanCodex() bool {
	_, err := codexDriver{}.Launch(a)
	return err == nil
}

// drainConv は子の stdout を読み続け、会話で畳んで campd へ渡す。**必ず落としてから**畳む。
func (a *Agent) drainConv(k *child, sc *bufio.Scanner) {
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		ev := k.conv.Fold(line)
		if ev.Drop {
			continue
		}
		a.record(k, ev.Kind, line)
		if ev.Deliver != "" {
			deliverRaw(k, ev.Deliver, ev.Payload)
		}
		for _, r := range ev.Replies {
			if err := k.write(r); err != nil {
				fmt.Fprintf(os.Stderr, "camp agent: %s へ返せない: %v\n", k.id[:8], err)
			}
		}
		if ev.TurnEnd {
			k.mu.Lock()
			k.turn = false
			k.mu.Unlock()
		}
		m := Msg{T: MsgFrame, Session: k.id, Token: k.token, Kind: ev.Kind,
			ClaudeID: ev.SessionID, TurnEnd: ev.TurnEnd, Interrupted: ev.Interrupted, Note: ev.Note}
		if len(ev.Withdrawn) == 0 {
			m.Error = ev.Err
		}
		if ev.Halt != "" {
			m.Halt, m.Error = true, ev.Halt
		}
		if ev.Ask != nil {
			m.Ask, m.ReqID, m.Text, m.Frame = true, ev.Ask.ReqID, ev.Ask.Tool, ev.Ask.Detail
		}
		if debugFrames {
			fmt.Fprintf(os.Stderr, "camp agent: %s %s\n", k.id[:8], m.Kind)
		}
		a.send(m)
		// 取り下げは1つずつ伝える（campd は ReqID ごとに台帳を閉じる）。
		for _, id := range ev.Withdrawn {
			a.send(Msg{T: MsgFrame, Session: k.id, Token: k.token, Kind: "camp/withdrawn",
				ClaudeID: ev.SessionID, ReqID: id, Withdrawn: true, Error: ev.Err})
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

// controlConv は残量の問い合わせを子へ投げる（または会話の手元の控えを返す）。
func (a *Agent) controlConv(k *child, m Msg) {
	out := Msg{T: MsgCtlRes, Session: m.Session, ReqID: m.ReqID}
	req, key, now, err := k.conv.Query(m.Kind)
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

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
