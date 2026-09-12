package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoomA-0750/camp/internal/store"
)

// 2026-09-11。終わったセッションが「どう終わったか」を選り分けられること。
// 本人の頼み: 動いていたセッション、承認待ちのまま放置されて終わったセッションが
// どれか分かるように記録し、終わったものを全部見られるようにする。

// liveState は campd のメモリ上の状態。DB と食い違うことがあるので別に見る。
func liveState(s *Supervisor, id string) string {
	for _, r := range s.Live() {
		if r.ID == id {
			return r.State
		}
	}
	return ""
}

func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-claude")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// silentBody はターンを返さない子。中断も聞かない。
const silentBody = `#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"silent-1"}'
while IFS= read -r line; do
  case "$line" in
    *'"type":"user"'*) echo '{"type":"assistant","session_id":"silent-1"}' ;;
  esac
done
`

// askingBody は話しかけられると承認を求め、答えが来たらターンを終える。
const askingBody = `#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"ask-1"}'
while IFS= read -r line; do
  case "$line" in
    *'"type":"user"'*) echo '{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Write","input":{"file_path":"x"}}}' ;;
    *control_response*) echo '{"type":"result","subtype":"success","session_id":"ask-1"}' ;;
    *interrupt*) echo '{"type":"result","subtype":"aborted","session_id":"ask-1"}' ;;
  esac
done
`

// quitterBody は名乗ってすぐ自分で終わる。
const quitterBody = `#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"quit-1"}'
exit 3
`

// startWith は body の子を1本起こす。
func startWith(t *testing.T, body string) (*Supervisor, *store.DB, Record) {
	t.Helper()
	db := newDB(t)
	s := New(db)
	attach(t, s, script(t, body))
	rec, err := s.Start("test", allowHere(t, db))
	if err != nil {
		t.Fatal(err)
	}
	return s, db, rec
}

func endedRec(t *testing.T, db *store.DB, id string) Record {
	t.Helper()
	waitFor(t, 10*time.Second, func() bool { return state(t, db, id) == StateExited })
	r, err := get(db, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// **中断はセッションを終わらせない。**
//
// 中断（interrupt）はターンを止めるだけで、子は生きたまま次の入力を待つ。
// 2026-09-11 まではここで stopping のまま残り、入力を受けられず、しかも2分後に
// Tick が「止めろと言ったのに止まらない」で exited と書いていた——生きている
// 子を「終わった」と書くことになる。
func TestInterruptDoesNotEndTheSession(t *testing.T) {
	db := newDB(t)
	s, _ := wire(t, db)
	rec, err := s.Start("test", allowHere(t, db))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })

	if err := s.Stop(rec.ID, StopInterrupt); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for liveState(s, rec.ID) != StateIdle {
		if time.Now().After(deadline) {
			t.Fatalf("中断のあと idle に戻らない（メモリ=%s / DB=%s）。入力を受けられない",
				liveState(s, rec.ID), state(t, db, rec.ID))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.Input(rec.ID, "続き"); err != nil {
		t.Fatalf("中断のあとに話せない: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return liveState(s, rec.ID) == StateIdle })

	// 時間が経っても、生きている子を「終わった」と書かない。
	s.Now = func() time.Time { return time.Now().Add(s.StopAfter + time.Minute) }
	s.Tick()
	if got := state(t, db, rec.ID); got == StateExited {
		t.Fatal("中断しただけのセッションを、時間切れで終わったことにした")
	}
}

// 何も言っていないのに終わったなら「子が自分で終わった」。終了コードも残る。
func TestAChildThatQuitsOnItsOwnIsRecordedAsSuch(t *testing.T) {
	_, db, rec := startWith(t, quitterBody)
	r := endedRec(t, db, rec.ID)
	if r.EndCause != EndSelf {
		t.Fatalf("終わり方が %q（self のはず）", r.EndCause)
	}
	if r.ExitCode == nil || *r.ExitCode != 3 {
		t.Fatalf("終了コードが残っていない: %v", r.ExitCode)
	}
	if r.EndState != StateIdle {
		t.Fatalf("そのときの状態が %q（idle のはず）", r.EndState)
	}
}

// 本人が止めたものは、止めたときに何をしていたかと一緒に残る。
func TestStoppingByHandRecordsWhatItWasDoing(t *testing.T) {
	t.Run("待機中に止めた", func(t *testing.T) {
		s, db, rec := startWith(t, askingBody)
		waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
		if err := s.Stop(rec.ID, StopTerminate); err != nil {
			t.Fatal(err)
		}
		r := endedRec(t, db, rec.ID)
		if r.EndCause != EndUserStop || r.EndState != StateIdle {
			t.Fatalf("終わり方=%q そのとき=%q（user_stop / idle のはず）", r.EndCause, r.EndState)
		}
		if r.Asked != 0 || r.LeftWaiting != 0 {
			t.Fatalf("承認は1つも無かったはず: %+v", r)
		}
	})

	t.Run("承認を待たせたまま止めた", func(t *testing.T) {
		s, db, rec := startWith(t, askingBody)
		waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
		if err := s.Input(rec.ID, "書いて"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 5*time.Second, func() bool { return len(pending(t, s, rec.ID)) == 1 })
		if err := s.Stop(rec.ID, StopTerminate); err != nil {
			t.Fatal(err)
		}
		r := endedRec(t, db, rec.ID)
		if r.EndCause != EndUserStop {
			t.Fatalf("終わり方が %q（user_stop のはず）", r.EndCause)
		}
		// **stopping ではなく、その手前を残す。** stopping と書いても
		// 「止めた」以上のことは分からない。
		if r.EndState != StateRunning {
			t.Fatalf("そのときの状態が %q（running のはず）", r.EndState)
		}
		if r.Asked != 1 || r.LeftWaiting != 1 || r.TimedOut != 0 {
			t.Fatalf("承認の内訳が違う: 訊いた=%d 待たせたまま=%d 期限切れ=%d（1/1/0 のはず）",
				r.Asked, r.LeftWaiting, r.TimedOut)
		}
	})
}

// 承認を放っておいたら期限切れで拒否になり、そのあと放置で閉じた。
// **「放置した承認があった」は、終わり方とは別に残る。**
func TestAnApprovalLeftToExpireIsRecorded(t *testing.T) {
	s, db, rec := startWith(t, askingBody)
	// 既定では承認を期限切れにしない（D-030）。ここでは入れて確かめる。
	// **訊かれる前に入れる**——期限は承認を記録するときに決まる。
	s.ParkAfter = parkLimit
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	if err := s.Input(rec.ID, "書いて"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return len(pending(t, s, rec.ID)) == 1 })

	later := time.Now().Add(parkLimit + time.Minute)
	s.Now = func() time.Time { return later }
	s.Tick() // 期限切れ → 拒否を送る → 子はターンを終える
	waitFor(t, 5*time.Second, func() bool { return liveState(s, rec.ID) == StateIdle })

	// 放置で閉じるのは既定では見ない（D-030）。ここでは入れて確かめる。
	s.IdleAfter = 30 * time.Minute
	later = later.Add(s.IdleAfter + time.Minute)
	s.Tick() // 放置 → 閉じる
	r := endedRec(t, db, rec.ID)
	if r.EndCause != EndIdleTimeout || r.EndState != StateIdle {
		t.Fatalf("終わり方=%q そのとき=%q（idle_timeout / idle のはず）", r.EndCause, r.EndState)
	}
	if r.TimedOut != 1 || r.LeftWaiting != 0 {
		t.Fatalf("期限切れ=%d 待たせたまま=%d（1/0 のはず）", r.TimedOut, r.LeftWaiting)
	}

	page, err := ListEnded(db, EndedQuery{Kind: KindIgnored})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 1 || page.Sessions[0].ID != rec.ID {
		t.Fatalf("「承認を放置した」で引けない: %+v", page.Sessions)
	}
}

// 中断が効かないターンは、しばらく待ってから孫まで止める。
func TestATurnThatIgnoresInterruptIsStoppedAfterAWhile(t *testing.T) {
	s, db, rec := startWith(t, silentBody)
	waitFor(t, 5*time.Second, func() bool { return state(t, db, rec.ID) == StateIdle })
	if err := s.Input(rec.ID, "終わらない話"); err != nil {
		t.Fatal(err)
	}

	// ターンの長さで止めるのは既定では見ない（D-030）。ここでは入れて確かめる。
	s.TurnAfter = 60 * time.Minute
	later := time.Now().Add(s.TurnAfter + time.Minute)
	s.Now = func() time.Time { return later }
	s.Tick() // 中断を投げる。この子は聞かない
	time.Sleep(100 * time.Millisecond)
	if got := state(t, db, rec.ID); got != StateRunning {
		t.Fatalf("中断を投げただけで %s になった", got)
	}

	later = later.Add(s.StopAfter + time.Minute)
	s.Tick() // 効かなかったので止める
	r := endedRec(t, db, rec.ID)
	if r.EndCause != EndTurnTimeout || r.EndState != StateRunning {
		t.Fatalf("終わり方=%q そのとき=%q（turn_timeout / running のはず）", r.EndCause, r.EndState)
	}
}

// 起こせなかったものは「起こせなかった」。
func TestAStartThatFailsIsRecordedAsSuch(t *testing.T) {
	db := newDB(t)
	s := New(db)
	attach(t, s, filepath.Join(t.TempDir(), "居ない-claude"))
	rec, err := s.Start("test", allowHere(t, db))
	if err != nil {
		t.Fatal(err)
	}
	r := endedRec(t, db, rec.ID)
	if r.EndCause != EndStartFailed || r.EndState != StateStarting {
		t.Fatalf("終わり方=%q そのとき=%q（start_failed / starting のはず）", r.EndCause, r.EndState)
	}
}

// 実行面が落ちると子も終わる（2026-09-11 に仕様とした）。そう記録される。
func TestLosingTheAgentIsRecordedAsTheCause(t *testing.T) {
	db := newDB(t)
	s := New(db)
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	mustInsert(t, db, "lost", StateRunning, dead.Process.Pid, 1, BootID())
	if err := ask(db, "lost", "req-1", "Write", "{}", time.Now(), parkLimit); err != nil {
		t.Fatal(err)
	}
	a := &agentConn{who: "test"}
	s.agent = a
	s.live["lost"] = &liveSession{rec: Record{ID: "lost", State: StateRunning}, asked: map[string]bool{}}

	(&Control{s: s, allowUID: -1}).dropAgent(a)
	if got := state(t, db, "lost"); got != StateOrphaned {
		t.Fatalf("実行面が落ちた直後は orphaned のはず: %s", got)
	}
	s.Tick() // もう居ないのを見て閉じる

	r, _ := get(db, "lost")
	if r.State != StateExited {
		t.Fatalf("閉じていない: %s", r.State)
	}
	if r.EndCause != EndAgentLost || r.EndState != StateRunning {
		t.Fatalf("終わり方=%q そのとき=%q（agent_lost / running のはず）", r.EndCause, r.EndState)
	}
	if r.LeftWaiting != 1 {
		t.Fatalf("待たせていた承認が数えられていない: %d", r.LeftWaiting)
	}
}

// campd が止まっている間に終わっていたもの。待っていた承認は「待たせたまま」に数える
// （期限切れとして閉じると「答えずに放置した」に化ける）。
func TestAGhostKeepsItsWaitingApprovalsAsSuch(t *testing.T) {
	db := newDB(t)
	s := New(db)
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	mustInsert(t, db, "ghost", StateRunning, dead.Process.Pid, 1, BootID())
	if err := ask(db, "ghost", "req-1", "Write", "{}", time.Now().Add(-time.Hour), parkLimit); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	s.Tick()
	r, _ := get(db, "ghost")
	if r.EndCause != EndUnseen || r.LeftWaiting != 1 || r.TimedOut != 0 {
		t.Fatalf("終わり方=%q 待たせたまま=%d 期限切れ=%d（unseen / 1 / 0 のはず）",
			r.EndCause, r.LeftWaiting, r.TimedOut)
	}
}

// 控えの規則。**Camp が止めろと言ったなら、その理由が残る。**
func TestTheCauseFollowsWhatCampAskedFor(t *testing.T) {
	db := newDB(t)
	cases := []struct {
		name  string
		do    func(id string)
		cause string
		state string
	}{
		{"止めろと言ったあと子が終わった", func(id string) {
			_ = markStopping(db, id, EndUserStop)
			_ = finish(db, id, 0, "", EndSelf, false)
		}, EndUserStop, StateRunning},
		{"止めろと言ったが止まらなかった", func(id string) {
			_ = markStopping(db, id, EndUserStop)
			_ = finish(db, id, -1, "", EndStopTimeout, true)
		}, EndStopTimeout, StateRunning},
		{"見張りが戻ってから自分で終わった", func(id string) {
			_ = markOrphaned(db, id, EndAgentLost)
			_ = clearOrphanCause(db, id)
			_ = setState(db, id, StateIdle)
			_ = finish(db, id, 0, "", EndSelf, false)
		}, EndSelf, StateIdle},
		{"止めている最中に実行面も落ちた", func(id string) {
			_ = markStopping(db, id, EndUserStop)
			_ = markOrphaned(db, id, EndAgentLost)
			_ = finish(db, id, -1, "", EndUnseen, false)
		}, EndUserStop, StateRunning},
	}
	for i, c := range cases {
		id := "c" + string(rune('0'+i))
		mustInsert(t, db, id, StateRunning, 1, 1, "b")
		c.do(id)
		r, _ := get(db, id)
		if r.State != StateExited || r.EndCause != c.cause || r.EndState != c.state {
			t.Errorf("%s: 状態=%s 終わり方=%q そのとき=%q（exited / %s / %s のはず）",
				c.name, r.State, r.EndCause, r.EndState, c.cause, c.state)
		}
	}
}

// 終わったものは全部引ける。絞り込みと頁送りで、1件も落とさず、1件も重ねない。
func TestEndedSessionsCanBeNarrowedAndPaged(t *testing.T) {
	db := newDB(t)
	// 同じ秒に終わったものが並ぶ（頁の境目を id で切れているかを見る）。
	for i, c := range []string{EndSelf, EndUserStop, EndUserStop, EndIdleTimeout, EndAgentLost} {
		id := "e" + string(rune('0'+i))
		mustInsert(t, db, id, StateRunning, 1, 1, "b")
		if c == EndUserStop {
			_ = markStopping(db, id, EndUserStop)
		}
		_ = finish(db, id, 0, "", c, c != EndUserStop)
	}
	// 記録を始める前に終わった行（end_cause が無い）。
	mustInsert(t, db, "old", StateRunning, 1, 1, "b")
	if _, err := db.Exec(`update runtime_sessions set state=?, ended_at=? where id='old'`,
		StateExited, now()); err != nil {
		t.Fatal(err)
	}
	// 走っているものは出ない。
	mustInsert(t, db, "alive", StateIdle, 1, 1, "b")

	page, err := ListEnded(db, EndedQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Counts["all"] != 6 || page.Counts[EndUserStop] != 2 ||
		page.Counts[KindUnknown] != 1 || page.Counts[KindMidTurn] != 5 {
		t.Fatalf("件数が違う: %v", page.Counts)
	}

	seen := map[string]bool{}
	before := ""
	for pages := 0; ; pages++ {
		p, err := ListEnded(db, EndedQuery{Limit: 2, Before: before})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range p.Sessions {
			if seen[r.ID] {
				t.Fatalf("%s が2つの頁に出た", r.ID)
			}
			if r.ID == "alive" {
				t.Fatal("走っているものが出た")
			}
			seen[r.ID] = true
		}
		if p.Next == "" {
			break
		}
		if pages > 5 {
			t.Fatal("頁送りが終わらない")
		}
		before = p.Next
	}
	if len(seen) != 6 {
		t.Fatalf("頁送りで %d 件しか引けない（6 のはず）", len(seen))
	}

	if p, _ := ListEnded(db, EndedQuery{Kind: KindUnknown}); len(p.Sessions) != 1 || p.Sessions[0].ID != "old" {
		t.Fatalf("記録の無い行を「記録なし」で引けない: %+v", p.Sessions)
	}
	if _, err := ListEnded(db, EndedQuery{Kind: "drop table"}); !errors.Is(err, ErrBadQuery) {
		t.Fatalf("知らない絞り込みを断っていない: %v", err)
	}
	if _, err := ListEnded(db, EndedQuery{Before: "区切りが無い"}); !errors.Is(err, ErrBadQuery) {
		t.Fatalf("読めない続きの位置を断っていない: %v", err)
	}
}
