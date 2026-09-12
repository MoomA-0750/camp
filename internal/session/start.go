package session

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"syscall"
	"time"
)

// 実行面の本流（このマシン）。**どのエージェントでも同じ手順で起こし、違いは駆動器が持つ**
// （M40 の (3)。Claude の start と Codex の startCodex を1本にした）。
//
// 手順: 照合 → 起こす → 実際に降りた場所を照らす → 落とし先を開く → 話し始める（駆動器）
// → 名乗る → 読み続ける → 終わったら scope の残りを数える。

// agents は起こせるエージェント（hello で名乗る）。**駆動器が「この設定なら起こせる」と
// 言うものだけ。**
func (a *Agent) agents() []string {
	names := make([]string, 0, len(drivers))
	for n := range drivers {
		names = append(names, n)
	}
	sort.Strings(names)
	out := []string{}
	for _, n := range names {
		if _, err := drivers[n].Launch(a); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// start は子を起こす（このマシン）。
func (a *Agent) start(m Msg) {
	agent, perm := agentOr(m.Agent), permOr(m.Perm)
	fail := func(why string) {
		a.send(Msg{T: MsgFailed, Session: m.Session, Token: m.Token, Error: why})
	}
	d, ok := drivers[agent]
	if !ok {
		fail("知らないエージェント: " + agent)
		return
	}
	// **campd 側でも照合しているが、ここでも見る。** 境界として数えるのは
	// campd 側だけ（同じユーザーで動く以上、ここの検査は迂回できる）。
	// それでも、campd の取り違えをそのまま実行しないだけの価値はある。
	real, err := resolveCwd(m.Cwd)
	if err != nil {
		fail(err.Error())
		return
	}
	if m.Root == "" || !under(real, m.Root) {
		fail(fmt.Sprintf("許した場所（%s）の外を渡された: %s", m.Root, real))
		return
	}
	l, err := d.Launch(a)
	if err != nil {
		fail(err.Error())
		return
	}
	args, err := d.Argv(perm, m.Resume)
	if err != nil {
		fail(err.Error())
		return
	}

	cmd := a.Command(agent, m.Session, append([]string{l.Bin}, args...))
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
	// scope を使わない場合でも、せめて自分のプロセスグループから切る。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fail(err.Error())
		return
	}
	pid := cmd.Process.Pid
	st, _ := Starttime(pid)

	k := &child{id: m.Session, token: m.Token, cmd: cmd, stdin: stdin,
		pending: map[string]chan []byte{}, name: agent,
		conv: d.Open(OpenOpts{Session: m.Session, Perm: perm, Cwd: real, Home: l.Home,
			Resume: m.Resume})}
	if a.Scope {
		k.scope = scopeName(m.Session)
	}
	// 起こしたあとで諦めるときは、孫まで止めて、終わるのを待ち、落とし先を閉じてから言う。
	giveUp := func(why string) {
		k.kill()
		cmd.Wait()
		if k.log != nil {
			k.log.Close()
		}
		fail(why)
	}

	// **照合したパスと、実際に降りた場所が同じか。**
	//
	// 照合してから exec するまでの間に、ディレクトリを rename や symlink で
	// 差し替えられると、同じ文字列が別の場所を指しうる（TOCTOU）。
	// 起こしたあとに /proc/<pid>/cwd を読めば、実際どこに居るかが分かる。
	// 違えば、その場で止める。
	if where, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err == nil {
		if !under(where, m.Root) {
			giveUp(fmt.Sprintf("起こした先が許した場所の外だった（%s）。止めた", where))
			return
		}
	} else {
		// 読めなかったことを「合っていた」と読ませない。
		fmt.Fprintf(os.Stderr,
			"camp agent: %s の実際の cwd を確かめられない: %v\n", m.Session[:8], err)
	}

	// **落とし先を先に開く。** 開けなければ drain は行き場を失い、
	// パイプが詰まって子が止まる。黙って進めない。
	if lg, err := OpenLog(a.LogDir, m.Session); err == nil {
		k.log = lg
	} else {
		// **campd にも言う。** stderr にしか出さないと、画面は
		// 「フレームは来ているのに中身が無い」を「中身が無かった」と読む。
		k.logBroken = true
		fmt.Fprintf(os.Stderr, "camp agent: 落とし先を開けない（%v）。フレームは残らない\n", err)
		a.send(Msg{T: MsgDropped, Session: m.Session, Token: m.Token, Dropped: -1,
			Error: "落とし先を開けない: " + err.Error()})
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	// **話し始めるまでに時間を切る。** 黙った子を待ち続けない（手順の無い駆動器はすぐ戻る）。
	timer := time.AfterFunc(a.HeaderWait, func() { k.kill() })
	err = k.conv.Begin(BeginOpts{Scanner: sc, W: stdin, Record: func(kind string, line []byte) {
		a.record(k, kind, line)
	}})
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

	a.send(Msg{T: MsgStarted, Session: m.Session, Token: m.Token, Agent: agent,
		PID: pid, Started: st, BootID: BootID(), Scope: k.scope, ClaudeID: k.conv.SessionID(),
		Perm: perm})

	// **生まれる前に止めろと言われていたなら、生まれた直後に止める。**
	if wasAsked {
		fmt.Fprintf(os.Stderr, "camp agent: %s は生まれる前に止めろと言われていた\n",
			m.Session[:8])
		go a.stop(Msg{Session: m.Session, Token: m.Token, Mode: wanted})
	}

	// **stdout は常時読む。** 読まないと子が詰まる。
	go a.drainConv(k, sc)

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
	// **子が終わっても、scope に残りが居るかもしれない**（Codex のコマンドは別のセッション、
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
