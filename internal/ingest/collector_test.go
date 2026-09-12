package ingest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Phase 3.8 の M44: 取り込み器の形。**本体はエージェントの名前を知らない。**
//
// テストの中だけで3つ目の取り込み器を足し、本体（走査→役割→sessions/source_files/messages）が
// そのまま動くことを縛る。駆動器の `fake3_test.go` と同じ手本（D-031）。

const agentFake = "fakeagent"

// fakeCollector は「1行1メッセージ、ファイル名が会話の id」という別のエージェントの記録。
// 形は Claude と違う（キーの名前が違い、uuid が無い）。
type fakeCollector struct{}

var _ Collector = fakeCollector{}

func (fakeCollector) Name() string { return agentFake }

func (fakeCollector) DefaultRoot(home string) string { return home + "/.fake/log" }

func (fakeCollector) RecordSub() string { return "log" }

// **拾うのは .fake だけ。** Claude の .jsonl は見ない。
func (fakeCollector) Wants(path string) bool { return strings.HasSuffix(path, ".fake") }

func (fakeCollector) Identify(root, path string, f *FileSummary) {
	if f.SessionID == "" {
		f.SessionID = strings.TrimSuffix(filepath.Base(path), ".fake")
	}
}

// NewParser は `{"kind":…,"who":…,"at":…,"dir":…}` を Line に翻訳する解釈を1本ぶん作る。
func (fakeCollector) NewParser(*FileSummary) ParseFunc { return fakeParse }

func fakeParse(raw []byte, offset int64) (*Line, error) {
	var v struct {
		Kind string `json:"kind"`
		Who  string `json:"who"`
		At   string `json:"at"`
		Dir  string `json:"dir"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("offset %d: %w", offset, err)
	}
	l := &Line{Type: v.Kind, Timestamp: v.At, CWD: v.Dir, Raw: raw, Offset: offset}
	if v.Who != "" {
		l.Message = &Message{Role: v.Who}
	}
	return l, nil
}

func (fakeCollector) Absorb(f *FileSummary, l *Line, seenRun map[string]struct{}) {
	if l.Message != nil {
		f.HasConversation = true
	}
	if l.CWD != "" {
		if f.FirstCWD == "" {
			f.FirstCWD = l.CWD
		}
		f.LastCWD = l.CWD
		if !contains(f.CWDs, l.CWD) {
			f.CWDs = append(f.CWDs, l.CWD)
		}
	}
	if l.Timestamp != "" {
		if f.FirstTS == "" {
			f.FirstTS = l.Timestamp
		}
		f.LastTS = l.Timestamp
	}
}

func (fakeCollector) RunRefs(f *FileSummary) []string { return nil }

func (fakeCollector) Classify(f *FileSummary, runOwner map[string]string) string {
	if f.Size == 0 || f.Lines == 0 {
		return RoleEmpty
	}
	if f.HasConversation {
		return RoleMain
	}
	return RoleStub
}

func (fakeCollector) SessionKey(f *FileSummary) string { return f.SessionID }

// **本体に手を入れずに、別のエージェントの記録を取り込める。**
func TestTheBodyTakesAnyCollector(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	// 拾うファイルと、拾わないファイル（Claude の形）を同じ場所に置く。
	write(t, filepath.Join(root, "conv-1.fake"),
		`{"kind":"say","who":"user","at":"2026-09-12T00:00:00Z","dir":"/w"}`,
		`{"kind":"say","who":"assistant","at":"2026-09-12T00:00:01Z","dir":"/w"}`)
	// **拾われたときに検査まで到達するよう、Claude として成立する行にしておく。**
	// cwd が無いと取り込み自体が落ちて、「台帳に入ったか」を見る前に止まる。
	write(t, filepath.Join(root, "ignored.jsonl"),
		`{"type":"user","uuid":"u1","sessionId":"ignored","timestamp":"2026-09-12T00:00:00Z","cwd":"/w",`+
			`"message":{"role":"user","content":"x"}}`)

	res, err := IngestWith(db, fakeCollector{}, "h", root)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sessions != 1 || res.Messages != 2 {
		t.Fatalf("セッション %d / メッセージ %d（1 と 2 のはず。.jsonl は拾わない）", res.Sessions, res.Messages)
	}

	// **台帳のエージェント名は取り込み器の名前**（本体が決め打ちしていない）。
	var agent, id string
	if err := db.QueryRow(`select id, agent from sessions`).Scan(&id, &agent); err != nil {
		t.Fatal(err)
	}
	if id != "conv-1" || agent != agentFake {
		t.Fatalf("sessions の行が id=%q agent=%q（conv-1 / %s のはず）", id, agent, agentFake)
	}

	// **拾わないファイルは台帳に入らない。** 直接確かめる——拾ってしまったときに、
	// 別の失敗（project が無い等）で落ちるだけだと理由が分からない。
	var kept []string
	rows, err := db.Query(`select path from source_files order by path`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, filepath.Base(path))
	}
	if len(kept) != 1 || kept[0] != "conv-1.fake" {
		t.Fatalf("台帳に入ったファイルが %v（conv-1.fake だけのはず。.jsonl を拾っている）", kept)
	}

	// 追記してもう一度: 差分だけ読む（本体の差分追尾はエージェントに依らない）。
	appendTo(t, filepath.Join(root, "conv-1.fake"),
		`{"kind":"say","who":"user","at":"2026-09-12T00:00:02Z","dir":"/w"}`)
	res2, err := IngestWith(db, fakeCollector{}, "h", root)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Messages != 1 {
		t.Fatalf("追記分だけ読んでいない: %d 行（1 のはず）", res2.Messages)
	}
}

// 取り込み器は名前で引ける（レジストリ）。**Claude はそこに居る。**
func TestClaudeIsRegisteredAndNamesTheLedgerValue(t *testing.T) {
	col, ok := CollectorFor(AgentClaude)
	if !ok {
		t.Fatal("claude の取り込み器がレジストリに無い")
	}
	if col.Name() != "claude" {
		t.Fatalf("台帳に書く名前が %q（claude のはず。runtime_sessions と揃える）", col.Name())
	}
	if _, ok := CollectorFor("そんなものは無い"); ok {
		t.Fatal("知らない名前で取り込み器が引けた")
	}
}

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}
