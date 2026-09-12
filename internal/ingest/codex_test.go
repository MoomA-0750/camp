package ingest

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// Phase 3.8 の M45: Codex の記録の取り込み。
//
// **合成データで縛る。** 本物の記録は個人の会話なので、形だけを真似た行を組む
// （キーと並びは 2026-09-12 に実データで数えたものに合わせてある）。

// codexFile は rollout-*.jsonl の形の合成データを1本書く。
func codexFile(t *testing.T, root, day, name string, lines ...string) string {
	t.Helper()
	p := filepath.Join(root, "2026", "09", day, name)
	write(t, p, lines...)
	return p
}

func meta(id, cwd string) string {
	return fmt.Sprintf(`{"timestamp":"2026-09-12T00:00:00Z","ordinal":0,"type":"session_meta",`+
		`"payload":{"id":%q,"session_id":%q,"cwd":%q,"cli_version":"0.154.0","source":"cli",`+
		`"git":{"branch":"main"}}}`, id, id, cwd)
}

func subagentMeta(id, parent, cwd string) string {
	return fmt.Sprintf(`{"timestamp":"2026-09-12T00:00:00Z","ordinal":0,"type":"session_meta",`+
		`"payload":{"id":%q,"cwd":%q,"cli_version":"0.154.0",`+
		`"source":{"subagent":{"thread_spawn":{"parent_thread_id":%q,"depth":1,"agent_path":"/root/review"}}}}}`,
		id, cwd, parent)
}

// turnCtx は実データの形に合わせる。**オブジェクトの欄を混ぜてある**——素の型で受けると
// payload 全体のデコードが落ちることを、合成データでも踏めるようにするため（2026-09-12 の実測）。
func turnCtx(turn, model, cwd string) string {
	return fmt.Sprintf(`{"timestamp":"2026-09-12T00:00:01Z","ordinal":1,"type":"turn_context",`+
		`"payload":{"turn_id":%q,"root_turn_id":%q,"model":%q,"cwd":%q,"approval_policy":"on-request",`+
		`"approvals_reviewer":"user","effort":"medium","personality":"none","timezone":"Asia/Tokyo",`+
		`"realtime_active":false,"workspace_roots":[%q],`+
		`"collaboration_mode":{"mode":"default","settings":{"x":1}},`+
		`"permission_profile":{"type":"builtin","file_system":{},"network":{}},`+
		`"sandbox_policy":{"type":"workspaceWrite","network_access":false,"writable_roots":[]}}}`,
		turn, turn, model, cwd, cwd)
}

func msg(role, text string) string {
	return fmt.Sprintf(`{"timestamp":"2026-09-12T00:00:02Z","ordinal":2,"type":"response_item",`+
		`"payload":{"type":"message","role":%q,"id":"msg-1","content":[{"type":"input_text","text":%q}]}}`,
		role, text)
}

func usageRec(respID, turn string) string {
	return fmt.Sprintf(`{"timestamp":"2026-09-12T00:00:03Z","ordinal":3,"type":"token_usage_record",`+
		`"payload":{"response_id":%q,"turn_id":%q,"thread_id":"t-1","session_id":"s-1",`+
		`"usage":{"input_tokens":100,"cached_input_tokens":20,"cache_write_input_tokens":5,`+
		`"output_tokens":30,"reasoning_output_tokens":7,"total_tokens":130}}}`, respID, turn)
}

// **会話の id は記録の中の id。** ファイル名の uuid とは独立（実測で 4本食い違う）。
// turn は runs の行になり、turn_context の model が後の usage に載る。
func TestCodexIngestUsesTheRecordedIDAndTurnsAreRuns(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	// ファイル名の uuid（name-uuid）と、記録の中の id（conv-A）をわざとずらす。
	codexFile(t, root, "12", "rollout-2026-09-12T00-00-00-ffffffff.jsonl",
		meta("conv-A", "/w"),
		turnCtx("turn-1", "gpt-5.6-sol", "/w"),
		msg("user", "こんにちは"),
		usageRec("resp-1", "turn-1"))

	res, err := IngestWith(db, codexCollector{}, "h", root)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sessions != 1 || res.Messages != 4 {
		t.Fatalf("セッション %d / 行 %d（1 と 4 のはず）", res.Sessions, res.Messages)
	}

	var id, agent string
	if err := db.QueryRow(`select id, agent from sessions`).Scan(&id, &agent); err != nil {
		t.Fatal(err)
	}
	if id != "conv-A" || agent != AgentCodex {
		t.Fatalf("sessions が id=%q agent=%q（conv-A / codex のはず。ファイル名の uuid は使わない）", id, agent)
	}

	// **turn が runs の行になる**（本人の決定）。
	var runs int
	if err := db.QueryRow(`select count(*) from runs where id='turn-1'`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("turn が runs になっていない（%d 行）", runs)
	}

	// **usage は model を持つ**（列は NOT NULL）。turn_context は本文より先に来る。
	var model string
	var in, out, cacheRead, cacheWrite, thinking int64
	if err := db.QueryRow(`select model, input_tokens, output_tokens, cache_read_input_tokens,
		cache_creation_input_tokens, thinking_tokens from usage where api_message_id='resp-1'`).
		Scan(&model, &in, &out, &cacheRead, &cacheWrite, &thinking); err != nil {
		t.Fatal(err)
	}
	if model != "gpt-5.6-sol" {
		t.Fatalf("usage.model が %q（turn_context の model を持ち回せていない）", model)
	}

	// **turn_context が degraded になっていない。** オブジェクトの欄を素の型で受けると
	// payload 全体が落ち、turn_id も model も拾えなくなる（実データで踏んだ）。
	var bad int
	if err := db.QueryRow(`select count(*) from messages where type='turn_context' and degraded=1`).
		Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("turn_context の解釈が落ちている（degraded %d 行）", bad)
	}

	// mode と確認の度合いがセッションに入る（collaboration_mode は入れ物の中の名前）。
	var mode, perm string
	if err := db.QueryRow(`select coalesce(last_mode,''), coalesce(last_permission_mode,'') from sessions`).
		Scan(&mode, &perm); err != nil {
		t.Fatal(err)
	}
	if mode != "default" || perm != "on-request" {
		t.Fatalf("セッションの mode=%q permission_mode=%q（default / on-request のはず）", mode, perm)
	}

	// **model も台帳に残る。** `sessions.last_model` はもともと誰も書いていない列で、
	// Codex の `turn_context` から埋めるようにした（2026-09-12。実データで空のままだったのを直した）。
	var lastModel string
	if err := db.QueryRow(`select coalesce(last_model,'') from sessions`).Scan(&lastModel); err != nil {
		t.Fatal(err)
	}
	if lastModel != "gpt-5.6-sol" {
		t.Fatalf("sessions.last_model が %q（gpt-5.6-sol のはず）", lastModel)
	}
	if in != 100 || out != 30 || cacheRead != 20 || cacheWrite != 5 || thinking != 7 {
		t.Fatalf("トークンの対応が違う: in=%d out=%d read=%d write=%d thinking=%d", in, out, cacheRead, cacheWrite, thinking)
	}
}

// **turn_context だけで turn が runs の行になる。**
//
// usage の行にも turn_id が載っているので、turn_context から取らなくても runs が埋まってしまう
// ——それでは「turn を runs にする」決定を縛れない（2026-09-12、変異で素通りして気づいた）。
// ここでは usage を混ぜず、turn_context と本文だけで確かめる。
func TestCodexTurnBecomesARunFromTheTurnContextAlone(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	codexFile(t, root, "12", "rollout-2026-09-12T01-00-00-dddd.jsonl",
		meta("conv-T", "/w"),
		turnCtx("turn-9", "gpt-5.6-sol", "/w"),
		msg("user", "うん"))

	if _, err := IngestWith(db, codexCollector{}, "h", root); err != nil {
		t.Fatal(err)
	}
	var runs, links int
	if err := db.QueryRow(`select
		  (select count(*) from runs where id='turn-9'),
		  (select count(*) from session_runs)`).Scan(&runs, &links); err != nil {
		t.Fatal(err)
	}
	if runs != 1 || links != 1 {
		t.Fatalf("turn_context だけでは runs にならない（runs %d / 対応 %d。1 と 1 のはず）", runs, links)
	}
	// runs の欄も turn_context から埋まる（本人の決定: 1ターン＝1行）。
	var mode, perm, cwd string
	if err := db.QueryRow(`select coalesce(mode,''), coalesce(permission_mode,''), coalesce(cwd,'')
		from runs where id='turn-9'`).Scan(&mode, &perm, &cwd); err != nil {
		t.Fatal(err)
	}
	if mode != "default" || perm != "on-request" || cwd != "/w" {
		t.Fatalf("runs の欄が mode=%q perm=%q cwd=%q（default / on-request / /w のはず）", mode, perm, cwd)
	}
}

// **subagent は親セッションに紐づく**（Claude と同じ欄）。
func TestCodexSubagentLinksToItsParent(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	codexFile(t, root, "12", "rollout-2026-09-12T00-00-00-aaaa.jsonl",
		meta("conv-P", "/w"), turnCtx("turn-1", "m", "/w"), msg("user", "親"))
	codexFile(t, root, "12", "rollout-2026-09-12T00-10-00-bbbb.jsonl",
		subagentMeta("conv-C", "conv-P", "/w"), turnCtx("turn-2", "m", "/w"), msg("assistant", "子"))

	if _, err := IngestWith(db, codexCollector{}, "h", root); err != nil {
		t.Fatal(err)
	}
	var parent string
	if err := db.QueryRow(`select coalesce(parent_session_id,'') from sessions
		where id like 'conv-P.%' or parent_session_id = 'conv-P'`).Scan(&parent); err != nil {
		t.Fatalf("子セッションが引けない: %v", err)
	}
	if parent != "conv-P" {
		t.Fatalf("親が %q（conv-P のはず）", parent)
	}
}

// **rollout- で始まらない .jsonl は拾わない。**
func TestCodexIgnoresOtherJSONL(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	codexFile(t, root, "12", "rollout-2026-09-12T00-00-00-cccc.jsonl",
		meta("conv-A", "/w"), turnCtx("turn-1", "m", "/w"), msg("user", "はい"))
	// 置き場に紛れた別の JSONL（Codex の記録ではない）。
	codexFile(t, root, "12", "notes.jsonl", `{"type":"whatever"}`)

	if _, err := IngestWith(db, codexCollector{}, "h", root); err != nil {
		t.Fatal(err)
	}
	var kept []string
	rows, err := db.Query(`select path from source_files order by path`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, filepath.Base(p))
	}
	if len(kept) != 1 || kept[0] != "rollout-2026-09-12T00-00-00-cccc.jsonl" {
		t.Fatalf("台帳に入ったファイルが %v（rollout- の1本だけのはず）", kept)
	}
}

// M46: プラン枠は記録の中にある（`token_count` の `rate_limits`）。
//
// **形が Camp と違う**ので直して渡す: Camp は「窓の名前 → {used_percentage, resets_at}」の map、
// Codex は固定キーで `primary`/`secondary` が {used_percent, resets_at, window_minutes}。
// 実データでは 300分（5時間）と 10080分（7日）で、値が来ない回もある。
func tokenCount(usedPrimary, usedSecondary float64, resets int64, withWindows bool) string {
	if !withWindows {
		return fmt.Sprintf(`{"timestamp":"2026-09-12T00:00:04Z","ordinal":4,"type":"event_msg",` +
			`"payload":{"type":"token_count","rate_limits":{"primary":null,"secondary":null,` +
			`"plan_type":null,"credits":{},"limit_id":"x"}}}`)
	}
	return fmt.Sprintf(`{"timestamp":"2026-09-12T00:00:04Z","ordinal":4,"type":"event_msg",`+
		`"payload":{"type":"token_count","rate_limits":{`+
		`"primary":{"used_percent":%v,"resets_at":%d,"window_minutes":300},`+
		`"secondary":{"used_percent":%v,"resets_at":%d,"window_minutes":10080},`+
		`"plan_type":"pro","credits":{},"limit_id":"x"}}}`, usedPrimary, resets, usedSecondary, resets)
}

func TestCodexPlanLimitsComeFromTheRecord(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	resets := time.Now().Add(2 * time.Hour).Unix()
	codexFile(t, root, "12", "rollout-2026-09-12T02-00-00-eeee.jsonl",
		meta("conv-L", "/w"), turnCtx("turn-1", "gpt-5.6-sol", "/w"), msg("user", "ん"),
		tokenCount(12.5, 3.5, resets, true))

	res, err := IngestWith(db, codexCollector{}, "h", root)
	if err != nil {
		t.Fatal(err)
	}
	if res.Limits != 1 || res.LimitErrors != 0 {
		t.Fatalf("プラン枠の観測 %d / 失敗 %d（1 と 0 のはず）", res.Limits, res.LimitErrors)
	}

	// 2つの窓が、Camp の語彙で入る。started_at は窓の長さから逆算される。
	rows, err := db.Query(`select kind, agent, source, used_pct, samples,
		case when coalesce(started_at,'')='' then '無' else '有' end
		from usage_windows order by kind`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type w struct {
		kind, agent, source, started string
		used                         float64
		samples                      int
	}
	var got []w
	for rows.Next() {
		var x w
		if err := rows.Scan(&x.kind, &x.agent, &x.source, &x.used, &x.samples, &x.started); err != nil {
			t.Fatal(err)
		}
		got = append(got, x)
	}
	if len(got) != 2 {
		t.Fatalf("窓が %d 件（five_hour と seven_day の 2 件のはず）: %+v", len(got), got)
	}
	if got[0].kind != "five_hour" || got[1].kind != "seven_day" {
		t.Fatalf("窓の名前が %q / %q（five_hour / seven_day のはず）", got[0].kind, got[1].kind)
	}
	for _, x := range got {
		if x.agent != AgentCodex || x.source != "codex-rollout" {
			t.Fatalf("agent=%q source=%q（codex / codex-rollout のはず）", x.agent, x.source)
		}
		if x.started != "有" {
			t.Fatalf("%s の started_at が空（窓の長さから逆算されるはず）", x.kind)
		}
	}
	if got[0].used != 12.5 || got[1].used != 3.5 {
		t.Fatalf("使用率が %v / %v（12.5 / 3.5 のはず。used_percent を読めていない）", got[0].used, got[1].used)
	}
}

// **値の来ない回は窓にしない。** 実データで primary/secondary が null の回がある。
func TestCodexPlanLimitsSkipEmptyWindows(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	codexFile(t, root, "12", "rollout-2026-09-12T03-00-00-ffff.jsonl",
		meta("conv-N", "/w"), turnCtx("turn-1", "m", "/w"), msg("user", "ん"),
		tokenCount(0, 0, 0, false))

	res, err := IngestWith(db, codexCollector{}, "h", root)
	if err != nil {
		t.Fatal(err)
	}
	if res.Limits != 0 || res.LimitErrors != 0 {
		t.Fatalf("観測 %d / 失敗 %d（どちらも 0 のはず）", res.Limits, res.LimitErrors)
	}
	var n int
	if err := db.QueryRow(`select count(*) from usage_windows`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("窓が %d 件できた（値が来ていないので 0 のはず）", n)
	}

	// **行の段階で空だと確かめる。** 台帳の件数だけ見ていると、空の `rate_limits` を渡しても
	// limits が「窓が無い」と言って黙って終わるので、守りが緩んでも気づけない
	// （2026-09-12、変異で素通りして分かった）。
	raw := []byte(`{"timestamp":"2026-09-12T00:00:04Z","ordinal":4,"type":"event_msg",` +
		`"payload":{"type":"token_count","rate_limits":{"primary":null,"secondary":null,` +
		`"plan_type":null,"credits":{},"limit_id":"x"}}}`)
	l, err := codexCollector{}.NewParser(nil)(raw, 0)
	if err != nil {
		t.Fatalf("解釈できない: %v", err)
	}
	if len(l.Limits) != 0 {
		t.Fatalf("値の来ていない窓を渡している: %s", string(l.Limits))
	}
}

// **同じ窓を2回見たら、観測回数が増え、山は下がらない。**
func TestCodexPlanLimitsFoldIntoOneWindow(t *testing.T) {
	db := newTestDB(t)
	root := t.TempDir()
	resets := time.Now().Add(3 * time.Hour).Unix()
	p := codexFile(t, root, "12", "rollout-2026-09-12T04-00-00-aaaa.jsonl",
		meta("conv-F", "/w"), turnCtx("turn-1", "m", "/w"), msg("user", "ん"),
		tokenCount(40, 10, resets, true))
	if _, err := IngestWith(db, codexCollector{}, "h", root); err != nil {
		t.Fatal(err)
	}
	// 同じ窓で、使用率が下がった観測を追記する（7日枠は下がって見えることがある）。
	appendTo(t, p, tokenCount(25, 5, resets, true))
	if _, err := IngestWith(db, codexCollector{}, "h", root); err != nil {
		t.Fatal(err)
	}

	var used, peak float64
	var samples, n int
	if err := db.QueryRow(`select used_pct, peak_pct, samples, (select count(*) from usage_windows)
		from usage_windows where kind='five_hour'`).Scan(&used, &peak, &samples, &n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("窓が %d 件（畳めていない。2 件のはず）", n)
	}
	if used != 25 || peak != 40 || samples != 2 {
		t.Fatalf("使用率=%v 山=%v 観測=%d（25 / 40 / 2 のはず）", used, peak, samples)
	}
}
