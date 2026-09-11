package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 偽の `codex app-server`。**本物を呼ばずに、測った手順どおりに喋る**
// （dev/active/phase3.6-plan.md の実測表、codex-cli 0.154.0）。
//
// テストの実行ファイル自身を、環境変数つきで子として起こす（TestMain が見る）。
// JSON-RPC の id を読んで返す必要があるので、sh では書かない。
//
// 振る舞いは環境変数で変える:
//
//	CAMP_FAKE_CODEX       起こされたしるし（中身は使わない）
//	CAMP_FAKE_CODEX_LOG   受け取った行をそのまま書き足す先（テストが中身を照らす）
//	CAMP_FAKE_CODEX_POLICY thread/start の応答で返す approvalPolicy（既定は受け取った値）
//	CAMP_FAKE_CODEX_REVIEWER / CAMP_FAKE_CODEX_SANDBOX / CAMP_FAKE_CODEX_CWD も同じ
//	CAMP_FAKE_CODEX_STARTERR thread/start にエラーで答える
//
// turn/start の文に含まれる語で、そのターンの振る舞いを決める:
//
//	ask    コマンドの承認を1つ訊き、答えに応じて completed / declined
//	two    コマンドの承認を2つ続けて訊く（待ち行列）
//	patch  ファイル変更の承認。差分は直前の item/started にだけ入れる（本物と同じ）
//	perm   権限の拡張を訊く（Camp は断りで答えるはず）
//	hang   interrupt が来るまで終わらない
//	その他 発言を1つ返して終わる
func fakeCodexMain() {
	var logf *os.File
	if p := os.Getenv("CAMP_FAKE_CODEX_LOG"); p != "" {
		logf, _ = os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	}
	var mu sync.Mutex
	out := bufio.NewWriter(os.Stdout)
	send := func(v any) {
		b, _ := json.Marshal(v)
		mu.Lock()
		out.Write(append(b, '\n'))
		out.Flush()
		mu.Unlock()
	}
	notify := func(method string, params any) {
		send(map[string]any{"method": method, "params": params})
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}

	const thread = "thr-fake-1"
	var (
		turnN    int
		turnID   string
		reqN     int
		waiting  = map[string]func(json.RawMessage, *rpcErr){} // 自分が投げた要求の返事待ち
		sawReply = make(chan struct{}, 16)
	)
	endTurn := func(status string) {
		notify("turn/completed", map[string]any{"threadId": thread,
			"turn": map[string]any{"id": turnID, "status": status, "items": []any{}}})
		turnID = ""
	}
	ask := func(method string, params map[string]any, then func(json.RawMessage, *rpcErr)) {
		id := reqN
		reqN++
		waiting[fmt.Sprint(id)] = then
		send(map[string]any{"method": method, "id": id, "params": params})
	}
	command := func(item, cmd string, done func()) {
		notify("item/started", map[string]any{"threadId": thread, "turnId": turnID,
			"item": map[string]any{"type": "commandExecution", "id": item, "command": cmd,
				"status": "inProgress"}})
		ask("item/commandExecution/requestApproval", map[string]any{
			"threadId": thread, "turnId": turnID, "itemId": item, "startedAtMs": 1,
			// cwd は本物と同じくスレッドの作業場所（CAMP_FAKE_CODEX_ASKCWD で外を指せる）。
			"kind": "command", "command": cmd,
			"cwd": env("CAMP_FAKE_CODEX_ASKCWD", os.Getenv("CAMP_FAKE_CODEX_THREADCWD")),
			"commandActions":    []any{map[string]any{"type": "unknown", "command": cmd}},
			"availableDecisions": []any{"accept", "cancel"},
		}, func(res json.RawMessage, e *rpcErr) {
			var r struct {
				Decision any `json:"decision"`
			}
			json.Unmarshal(res, &r)
			status := "declined"
			if r.Decision == "accept" {
				status = "completed"
			}
			notify("item/completed", map[string]any{"threadId": thread, "turnId": turnID,
				"item": map[string]any{"type": "commandExecution", "id": item, "command": cmd,
					"status": status}})
			done()
		})
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if logf != nil {
			logf.Write(append(append([]byte{}, line...), '\n'))
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *rpcErr         `json:"error"`
		}
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		if m.Method == "" && len(m.ID) > 0 {
			// 自分が投げた要求（承認など）への返事。
			key := strings.Trim(string(m.ID), `"`)
			if then := waiting[key]; then != nil {
				delete(waiting, key)
				notify("serverRequest/resolved", map[string]any{"threadId": thread, "requestId": key})
				then(m.Result, m.Error)
				select {
				case sawReply <- struct{}{}:
				default:
				}
			}
			continue
		}
		reply := func(result any) {
			send(map[string]any{"id": m.ID, "result": result})
		}
		switch m.Method {
		case "initialize":
			if os.Getenv("CAMP_FAKE_CODEX_SILENT") != "" {
				continue // 黙る（握手の打ち切りを見る）
			}
			// 本物と同じく、受け取った CODEX_HOME を名乗る（上書きできる）。
			reply(map[string]any{"userAgent": "fake-codex/0",
				"codexHome":      env("CAMP_FAKE_CODEX_HOMEOUT", os.Getenv("CODEX_HOME")),
				"platformFamily": "unix", "platformOs": "linux"})
			if os.Getenv("CAMP_FAKE_CODEX_EARLYASK") != "" {
				// 話し始める前に要求を投げる（Camp は断りを返すはず）。
				ask("item/tool/requestUserInput", map[string]any{"threadId": "", "itemId": "early"},
					func(json.RawMessage, *rpcErr) {})
			}
		case "initialized":
		case "thread/start":
			if os.Getenv("CAMP_FAKE_CODEX_STARTERR") != "" {
				send(map[string]any{"id": m.ID, "error": map[string]any{"code": -32000,
					"message": "偽の失敗"}})
				continue
			}
			var p struct {
				Cwd              string `json:"cwd"`
				ApprovalPolicy   string `json:"approvalPolicy"`
				ApprovalsReviewer string `json:"approvalsReviewer"`
				Sandbox          string `json:"sandbox"`
			}
			json.Unmarshal(m.Params, &p)
			os.Setenv("CAMP_FAKE_CODEX_THREADCWD", p.Cwd) // 承認の cwd に使う
			sandbox := map[string]string{"workspace-write": "workspaceWrite",
				"read-only": "readOnly", "danger-full-access": "dangerFullAccess"}[p.Sandbox]
			reply(map[string]any{
				"thread":            map[string]any{"id": thread, "path": "/nonexistent/rollout.jsonl"},
				"approvalPolicy":    env("CAMP_FAKE_CODEX_POLICY", p.ApprovalPolicy),
				"approvalsReviewer": env("CAMP_FAKE_CODEX_REVIEWER", p.ApprovalsReviewer),
				// 本物と同じ欄を返す（approve.jsonl の応答）。照合は欄が欠けていても断る。
				"sandbox": map[string]any{"type": env("CAMP_FAKE_CODEX_SANDBOX", sandbox),
					"writableRoots": []any{}, "networkAccess": false,
					"excludeTmpdirEnvVar": false, "excludeSlashTmp": false},
				"activePermissionProfile": nil,
				"cwd":               env("CAMP_FAKE_CODEX_CWD", p.Cwd),
				"model":             "fake",
			})
			notify("thread/started", map[string]any{"thread": map[string]any{"id": thread}})
		case "turn/start":
			var p struct {
				Input []struct {
					Text string `json:"text"`
				} `json:"input"`
			}
			json.Unmarshal(m.Params, &p)
			text := ""
			for _, in := range p.Input {
				text += in.Text
			}
			turnN++
			turnID = fmt.Sprintf("turn-%d", turnN)
			reply(map[string]any{"turn": map[string]any{"id": turnID, "status": "inProgress"}})
			notify("turn/started", map[string]any{"threadId": thread,
				"turn": map[string]any{"id": turnID, "status": "inProgress"}})
			notify("item/completed", map[string]any{"threadId": thread, "turnId": turnID,
				"item": map[string]any{"type": "userMessage", "id": "u" + turnID,
					"content": []any{map[string]any{"type": "text", "text": text}}}})
			switch {
			case strings.Contains(text, "two"):
				command("exec-a", "touch a", func() {})
				command("exec-b", "touch b", func() {
					endTurn("completed")
				})
			case strings.Contains(text, "ask"):
				command("exec-1", "touch camp.txt", func() { endTurn("completed") })
			case strings.Contains(text, "patch"):
				item := "exec-p1"
				notify("item/started", map[string]any{"threadId": thread, "turnId": turnID,
					"item": map[string]any{"type": "fileChange", "id": item, "status": "inProgress",
						"changes": []any{map[string]any{"path": "/w/notes.txt",
							"kind": map[string]any{"type": "add"}, "diff": "hello\n"}}}})
				// **要求そのものには差分が無い**（実測）。
				ask("item/fileChange/requestApproval", map[string]any{"threadId": thread,
					"turnId": turnID, "itemId": item, "startedAtMs": 1, "reason": nil,
					"grantRoot": nil}, func(json.RawMessage, *rpcErr) { endTurn("completed") })
			case strings.Contains(text, "moved"):
				// 承認を訊いたあとで差分が変わる（Camp は断って、台帳から取り下げるはず）。
				item := "exec-m1"
				notify("item/started", map[string]any{"threadId": thread, "turnId": turnID,
					"item": map[string]any{"type": "fileChange", "id": item, "status": "inProgress",
						"changes": []any{map[string]any{"path": "/w/a.txt", "diff": "a\n"}}}})
				ask("item/fileChange/requestApproval", map[string]any{"threadId": thread,
					"turnId": turnID, "itemId": item, "startedAtMs": 1},
					func(json.RawMessage, *rpcErr) { endTurn("completed") })
				go func() {
					time.Sleep(200 * time.Millisecond)
					notify("item/fileChange/patchUpdated", map[string]any{"threadId": thread,
						"turnId": turnID, "itemId": item,
						"changes": []any{map[string]any{"path": "/w/a.txt", "diff": "b\n"}}})
				}()
			case strings.Contains(text, "settle"):
				// 答えないうちに Codex 側で片付く（Camp は台帳から取り下げるはず）。
				id := reqN
				command("exec-s1", "touch s", func() {})
				go func() {
					time.Sleep(200 * time.Millisecond)
					mu.Lock()
					delete(waiting, fmt.Sprint(id))
					mu.Unlock()
					notify("serverRequest/resolved", map[string]any{"threadId": thread, "requestId": id})
					endTurn("interrupted")
				}()
			case strings.Contains(text, "grant"):
				item := "exec-g1"
				notify("item/started", map[string]any{"threadId": thread, "turnId": turnID,
					"item": map[string]any{"type": "fileChange", "id": item, "status": "inProgress",
						"changes": []any{map[string]any{"path": "/w/g.txt", "diff": "g\n"}}}})
				ask("item/fileChange/requestApproval", map[string]any{"threadId": thread,
					"turnId": turnID, "itemId": item, "startedAtMs": 1, "grantRoot": "/w"},
					func(json.RawMessage, *rpcErr) { endTurn("completed") })
			case strings.Contains(text, "perm"):
				ask("item/permissions/requestApproval", map[string]any{"threadId": thread,
					"turnId": turnID, "itemId": "perm-1"}, func(_ json.RawMessage, e *rpcErr) {
					endTurn("completed")
				})
			case strings.Contains(text, "hang"):
				// interrupt を待つ。
			default:
				notify("item/completed", map[string]any{"threadId": thread, "turnId": turnID,
					"item": map[string]any{"type": "agentMessage", "id": "m" + turnID,
						"text": "fake-ok", "phase": "final_answer"}})
				notify("thread/tokenUsage/updated", map[string]any{"threadId": thread,
					"turnId": turnID, "tokenUsage": map[string]any{
						"total": map[string]any{"totalTokens": 10}, "modelContextWindow": 1000}})
				endTurn("completed")
			}
		case "turn/interrupt":
			reply(map[string]any{})
			if turnID != "" {
				endTurn("interrupted")
			}
		case "account/rateLimits/read":
			reply(map[string]any{"rateLimits": map[string]any{
				"limitId": "codex", "planType": "plus",
				"primary":   map[string]any{"usedPercent": 12, "windowDurationMins": 300, "resetsAt": 1},
				"secondary": map[string]any{"usedPercent": 34, "windowDurationMins": 10080, "resetsAt": 2},
			}})
		default:
			if len(m.ID) > 0 {
				send(map[string]any{"id": m.ID, "error": map[string]any{"code": -32601,
					"message": "偽物は知らない: " + m.Method}})
			}
		}
	}
	os.Exit(0)
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// fakeCodex は偽の app-server を起こす sh を置き、そのパスを返す。
// env は子に渡す追加の環境変数（CAMP_FAKE_CODEX_POLICY=never など）。
func fakeCodex(t *testing.T, env ...string) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "received.jsonl")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("CAMP_FAKE_CODEX=1 CAMP_FAKE_CODEX_LOG=" + shQuote(logPath) + " ")
	for _, e := range env {
		// **値だけを括る。** `'K=v'` と括ると sh は代入ではなくコマンド名として読む。
		k, v, _ := strings.Cut(e, "=")
		b.WriteString(k + "=" + shQuote(v) + " ")
	}
	b.WriteString("exec " + shQuote(self) + " \"$@\"\n")
	bin = filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

// received は偽物が受け取った行を読む。
func received(t *testing.T, logPath string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}
