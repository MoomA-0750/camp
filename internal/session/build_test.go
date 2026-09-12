package session

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 実行面が campd と違うビルドで動いていることを知らせる（2026-09-12）。
//
// **規則そのものを縛る。** `selfBuild()` は Go が VCS の情報を埋めたときだけ値を返し、
// 試験用のバイナリでは空になる（`go test` は VCS を刻まない）。だから配線の側だけ見ても
// 「いつも食い違い無し」としか言えない。判定の規則はここで直に縛る。
func TestAStaleExecutionSideIsNoticed(t *testing.T) {
	for _, c := range []struct {
		name         string
		mine, theirs string
		want         bool
	}{
		// **分からないのに警告しない。** VCS の外でビルドしたときや試験。
		{"自分の指紋が取れない", "", "", false},
		{"自分の指紋が取れない（相手は名乗っている）", "", "abc123", false},
		// **名乗らない実行面は古い。** 名乗る仕組みより前のバイナリだから。
		{"相手が名乗らない", "abc123", "", true},
		{"同じビルド", "abc123", "abc123", false},
		{"違うビルド", "abc123", "def456", true},
		{"汚れた作業木は別物として扱う", "abc123", "abc123+dirty", true},
	} {
		if got := buildMismatch(c.mine, c.theirs); got != c.want {
			t.Errorf("%s: buildMismatch(%q, %q) = %v（%v のはず）", c.name, c.mine, c.theirs, got, c.want)
		}
	}
}

// 実行面が繋がっていなければ、古いとは言わない（言う相手が居ない）。
func TestNoStaleWarningWithoutAnExecutionSide(t *testing.T) {
	s := New(newDB(t))
	if stale, build := s.AgentStale(); stale || build != "" {
		t.Fatalf("実行面が居ないのに stale=%v build=%q", stale, build)
	}
}

// hello で名乗った指紋が、campd 側の接続に残る。
//
// **空でない値を載せて確かめる。** `attach` は本物の `Agent` を使うので、名乗る指紋は必ず
// `selfBuild()`——試験用のバイナリでは空になり、控えていなくても `"" == ""` で一致してしまう。
// それでは「控えていない」不具合を素通りさせる（2026-09-12、変異で実際に素通りした）。
// だから制御口へ生で繋ぎ、hello を自分で組み立てて送る。
func TestTheHelloBuildIsKept(t *testing.T) {
	s := New(newDB(t))
	sock := filepath.Join(t.TempDir(), "a.sock")
	c, err := s.Listen(sock, "", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	go c.Serve()
	t.Cleanup(func() { c.Close() })

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	line, err := json.Marshal(Msg{T: MsgHello, Version: "test", Build: "deadbeef1234"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return s.AgentConnected() })

	s.mu.Lock()
	a := s.agent
	s.mu.Unlock()
	if a == nil {
		t.Fatal("実行面が繋がっていない")
	}
	if a.build != "deadbeef1234" {
		t.Fatalf("hello の指紋が控えられていない: %q（deadbeef1234 のはず）", a.build)
	}

	// **campd 自身の指紋が取れるときは、食い違いとして見えること。**
	// 取れないとき（試験用バイナリ）は「分からない」ので何も言わない——そこも規則どおり。
	stale, build := s.AgentStale()
	if build != "deadbeef1234" {
		t.Fatalf("AgentStale が返す指紋が %q", build)
	}
	if want := selfBuild() != ""; stale != want {
		t.Fatalf("stale=%v（自分の指紋 %q なら %v のはず）", stale, selfBuild(), want)
	}
}
