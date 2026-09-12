package session

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// 実行面のリモート起動。考え方は remote.go の頭に書いた。

// sshArgs は Camp が起こす ssh の引数。
func (a *Agent) sshArgs(extra ...string) []string {
	args := make([]string, 0, len(sshSafeOpts)+len(extra)+2)
	if a.SSHFile != "" {
		// **既定の config を使うときは -F を付けない。** 付けると
		// /etc/ssh/ssh_config（配布元の暗号方針など）が読まれなくなり、
		// 端末で `ssh <alias>` したときと違う繋ぎ方になる。
		args = append(args, "-F", a.SSHFile)
	}
	args = append(args, sshSafeOpts...)
	return append(args, extra...)
}

// resolve は `ssh -G` で、いま alias がどこを指すかを読む。**繋がない。**
func (a *Agent) resolve(alias string) (Resolved, error) {
	if err := validAlias(alias); err != nil {
		return Resolved{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tb := &tailBuf{max: 2048}
	cmd := exec.CommandContext(ctx, a.SSH, a.sshArgs("-G", "--", alias)...)
	cmd.Stderr = tb
	out, err := cmd.Output()
	if err != nil {
		return Resolved{}, fmt.Errorf("ssh -G %s が通らない: %v %s", alias, err, lastLine(tb.String()))
	}
	r, files := parseSSHG(out)
	if r.HostName == "" {
		return Resolved{}, fmt.Errorf("ssh -G %s から行き先が読めない", alias)
	}
	keys, err := a.knownKeys(r, files)
	if err != nil {
		return Resolved{}, err
	}
	r.HostKeys = keys
	return r, nil
}

// knownKeys は、この行き先について ssh が信じるホスト鍵の指紋を全部集める。
//
// 引く名前は ssh と同じ: HostKeyAlias があればそれ、無ければ hostname
// （22 以外の port なら `[hostname]:port`）。known_hosts は `ssh -G` が言う
// ファイルを全部見る（KnownHostsCommand は Camp の ssh では切ってある）。
// **読めないファイルを「載っていない」と読まない。**
func (a *Agent) knownKeys(r Resolved, files []string) ([]string, error) {
	name := r.HostName
	switch {
	case r.HostKeyAlias != "":
		name = r.HostKeyAlias
	case r.Port != "" && r.Port != "22":
		name = "[" + name + "]:" + r.Port
	}
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasPrefix(f, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				f = filepath.Join(home, f[2:])
			}
		}
		if _, err := os.Stat(f); os.IsNotExist(err) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		out, err := exec.CommandContext(ctx, a.SSHKeygen, "-l", "-F", name, "-f", f).Output()
		cancel()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 && len(out) == 0 {
				continue // 載っていない
			}
			return nil, fmt.Errorf("%s の鍵を読めない: %v", f, err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "#") {
				continue
			}
			for _, w := range strings.Fields(line) {
				if strings.HasPrefix(w, "SHA256:") {
					seen[w] = true
				}
			}
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

func (a *Agent) resolveSSH(m Msg) {
	out := Msg{T: MsgSSHResolved, ReqID: m.ReqID, Alias: m.Alias}
	if r, err := a.resolve(m.Alias); err != nil {
		out.Error = err.Error()
	} else {
		out.Resolved = &r
	}
	a.send(out)
}

type headerResult struct {
	owner RemoteOwner
	ok    bool
	// err は向こうの sh が断った理由（CAMP-ERR）か、待ちきれなかったこと。
	// 空で ok=false なら、名乗らないまま終わった（ssh の失敗）。
	err string
}

// startRemote は ssh の向こうに子を起こす。
//
// 手元の start と同じ形にしてある。違うのは、(1) 繋ぐ前に行き先を照らす、
// (2) 1行目の名乗りを読むまでフレームとして扱わない、(3) 終わったら必ず
// 向こうを見に行く、の3つ。
func (a *Agent) startRemote(m Msg) {
	fail := func(e string) {
		a.send(Msg{T: MsgFailed, Session: m.Session, Token: m.Token, Error: e})
	}
	spec := m.Remote
	if err := validAlias(spec.Alias); err != nil {
		fail(err.Error())
		return
	}
	cwd, err := cleanRemotePath(m.Cwd)
	if err != nil {
		fail(err.Error())
		return
	}
	root, err := cleanRemotePath(m.Root)
	if err != nil {
		fail(err.Error())
		return
	}
	if !under(cwd, root) {
		fail(fmt.Sprintf("許した場所（%s）の外を渡された: %s", root, cwd))
		return
	}
	if err := validAgentPath(spec.Bin); err != nil {
		fail(err.Error())
		return
	}
	// **向こうでも駆動器で起こす**（M42）。探す名前・置き場・引数は駆動器が持ち、sh には書かない。
	agent, perm := agentOr(m.Agent), permOr(m.Perm)
	d, ok := drivers[agent]
	if !ok || !d.Info().Remote {
		fail(fmt.Sprintf("%s は向こうのホストで起こせない", agent))
		return
	}
	args, err := d.Argv(perm, m.Resume)
	if err != nil {
		fail(err.Error())
		return
	}
	rl := d.RemoteLaunch()

	// **許したときと同じ先か。** 違えば繋がない——繋いだ時点で、向こうの
	// ログインシェルが走る。
	now, err := a.resolve(spec.Alias)
	if err != nil {
		fail(err.Error())
		return
	}
	if !spec.Pin.Pinned() {
		fail(fmt.Sprintf("%s は行き先（ホスト鍵）を固定していない。許し直す", spec.Alias))
		return
	}
	if d := spec.Pin.Diff(now); d != "" {
		fail(fmt.Sprintf("%s の行き先が、許したときと違う（%s）。~/.ssh/config と known_hosts を確かめてから許し直す",
			spec.Alias, d))
		return
	}

	bin, unit := spec.Bin, "-"
	if bin == "" {
		bin = "-"
	}
	if a.Scope {
		unit = scopeName(m.Session)
	}
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	nonce := newID()
	wargs := append([]string{cwd, root, rl.Name, bin, dash(rl.HomeEnv), dash(rl.HomeDefault), unit, nonce,
		m.Session}, args...)
	cmd := exec.Command(a.SSH, a.sshArgs("--", spec.Alias, remoteCommand(wrapperScript, wargs...))...)
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
	tb := &tailBuf{max: 8192}
	cmd.Stderr = io.MultiWriter(os.Stderr, tb)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fail(err.Error())
		return
	}
	pid := cmd.Process.Pid
	st, _ := Starttime(pid)

	// **1行目の名乗りまでは、フレームとして扱わない。** ログインシェルが
	// 何か吐いても、それを子の出力と混ぜない。
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	hdr := make(chan headerResult, 1)
	go func() {
		for i := 0; i < 64 && sc.Scan(); i++ {
			line := sc.Text()
			if o, ok := parseHeader(line, spec.Alias, nonce); ok {
				hdr <- headerResult{owner: o, ok: true}
				return
			}
			if e, ok := parseRemoteErr(line); ok {
				hdr <- headerResult{err: e}
				return
			}
		}
		hdr <- headerResult{}
	}()
	var h headerResult
	select {
	case h = <-hdr:
	case <-time.After(a.HeaderWait):
		h = headerResult{err: fmt.Sprintf("%s が %v 経っても名乗らない", spec.Alias, a.HeaderWait)}
	}
	if !h.ok {
		syscall.Kill(-pid, syscall.SIGTERM)
		cmd.Wait()
		why := h.err
		if why == "" {
			why = explainSSHFailure(spec.Alias, tb.String())
		}
		fail(why)
		return
	}
	o := h.owner
	o.Session = m.Session
	// 向こうの sh が確かめているが、名乗りも照らす。
	if !under(o.Cwd, o.Root) {
		a.reapRemote(&o, spec)
		syscall.Kill(-pid, syscall.SIGTERM)
		cmd.Wait()
		fail(fmt.Sprintf("向こうで降りた先が許した場所の外だった（%s）。止めた", o.Cwd))
		return
	}

	var homes []string
	for _, h := range []string{o.Home, o.HomeReal} {
		if h != "" {
			homes = append(homes, h)
		}
	}
	k := &child{id: m.Session, token: m.Token, cmd: cmd, stdin: stdin,
		pending: map[string]chan []byte{}, remote: &o, spec: spec, name: agent,
		conv: d.Open(OpenOpts{Session: m.Session, Perm: perm, Cwd: o.Cwd, Remote: true,
			RemoteHomes: homes, Resume: m.Resume})}
	if lg, err := OpenLog(a.LogDir, m.Session); err == nil {
		k.log = lg
	} else {
		k.logBroken = true
		fmt.Fprintf(os.Stderr, "camp agent: 落とし先を開けない（%v）。フレームは残らない\n", err)
		a.send(Msg{T: MsgDropped, Session: m.Session, Token: m.Token, Dropped: -1,
			Error: "落とし先を開けない: " + err.Error()})
	}
	// **話し始めるまでに時間を切る**（手元の start と同じ。手順の無い駆動器はすぐ戻る）。
	// 諦めるときは向こうを先に止める（killChild）。
	timer := time.AfterFunc(a.HeaderWait, func() { a.killChild(k) })
	err = k.conv.Begin(BeginOpts{Scanner: sc, W: stdin, Record: func(kind string, line []byte) {
		a.record(k, kind, line)
	}})
	if !timer.Stop() && err != nil {
		err = fmt.Errorf("%v 待っても話し始められない（%v）", a.HeaderWait, err)
	}
	if err != nil {
		a.reapRemote(&o, spec)
		syscall.Kill(-pid, syscall.SIGTERM)
		cmd.Wait()
		if k.log != nil {
			k.log.Close()
		}
		fail(err.Error())
		return
	}
	a.mu.Lock()
	a.kids[m.Session] = k
	wanted, wasAsked := a.stopWanted[m.Session]
	delete(a.stopWanted, m.Session)
	a.mu.Unlock()

	a.send(Msg{T: MsgStarted, Session: m.Session, Token: m.Token, Agent: agent,
		PID: pid, Started: st, BootID: BootID(), RemoteOwner: &o, Perm: perm,
		ClaudeID: k.conv.SessionID()})
	if wasAsked {
		go a.stop(Msg{Session: m.Session, Token: m.Token, Mode: wanted})
	}

	go a.drainConv(k, sc)

	err = cmd.Wait()
	code, reason := exitOf(err)
	k.mu.Lock()
	k.dead = true
	deliberate := k.deliberate
	k.mu.Unlock()

	// **255 だけでは「切れた」と決めない。** 向こうの子がシグナルで死んでも 255。
	lost := false
	if !deliberate {
		if ee, ok := err.(*exec.ExitError); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				lost = true // 手元の ssh が（Camp 以外に）殺された
			}
		}
		if code == 255 && connectionLost(tb.String()) {
			lost = true
		}
	}
	if code != 0 {
		if l := lastLine(tb.String()); l != "" {
			reason += "（" + l + "）"
		}
	}

	// **終わったら必ず向こうを見に行く。** 手元の ssh が終わっても、
	// 向こうの子や孫が残っていることがある（実測）。
	res, detail := a.reapRemote(&o, spec)
	if res == RemoteUnreachable && a.ReapRetry > 0 {
		time.Sleep(a.ReapRetry)
		res, detail = a.reapRemote(&o, spec)
	}
	reason += "。向こう: " + RemoteEndLabel(res, detail)

	if k.log != nil {
		k.log.Close()
	}
	a.mu.Lock()
	delete(a.kids, m.Session)
	a.mu.Unlock()
	a.send(Msg{T: MsgExited, Session: m.Session, Token: m.Token, Code: code,
		Reason: reason, ConnLost: lost, RemoteEnd: res})
}

func exitOf(err error) (int, string) {
	if err == nil {
		return 0, "終わった"
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), err.Error()
	}
	return -1, err.Error()
}

var campScopeRe = regexp.MustCompile(`^camp-session-[0-9a-f]{32}\.scope$`)

// reapRemote は向こうの残りを始末する（reapScript）。
//
// **scope 名は自分で作った形しか通さない。** 向こうの `systemctl --user stop` に
// 渡るので、任意の名前を通すと同じユーザーの無関係な unit を止められる。
//
// **繋ぐ前に行き先を照らす**（spec。M47 で足した）。起こすときは照らしているのに
// ここは飛ばしていたので、`~/.ssh/config` の HostName を書き換えれば、固定と違う先へ
// 繋ぎに行けた（Fable の M47 設計レビュー 4）。照らせないときは**繋がない**——
// 掃除のために、行き先の固定を破らない。残りは「確かめられない」として残る。
func (a *Agent) reapRemote(o *RemoteOwner, spec *RemoteSpec) (string, string) {
	if o == nil || validAlias(o.Host) != nil || o.PID <= 0 {
		return RemoteUnreachable, "向こうの身元が無い"
	}
	if spec == nil || spec.Alias != o.Host || !spec.Pin.Pinned() {
		return RemoteUnreachable, "行き先（ホスト鍵）の固定が無いので、確かめに行かない"
	}
	now, err := a.resolve(o.Host)
	if err != nil {
		return RemoteUnreachable, err.Error()
	}
	if d := spec.Pin.Diff(now); d != "" {
		return RemoteUnreachable, fmt.Sprintf(
			"行き先が、許したときと違う（%s）。確かめに行かない", d)
	}
	st, boot, scope := "-", "-", "-"
	if o.Started != 0 {
		st = strconv.FormatUint(o.Started, 10)
	}
	if o.BootID != "" {
		boot = o.BootID
	}
	if campScopeRe.MatchString(o.Scope) {
		scope = o.Scope
	}
	// しるし（セッション id）。**自分で作った形しか通さない**（向こうの grep に渡る）。
	sess := "-"
	if sessionIDRe.MatchString(o.Session) {
		sess = o.Session
	}
	ctx, cancel := context.WithTimeout(context.Background(), a.ReapWait)
	defer cancel()
	tb := &tailBuf{max: 2048}
	cmd := exec.CommandContext(ctx, a.SSH, a.sshArgs("--", o.Host,
		remoteCommand(reapScript, strconv.Itoa(o.PID), st, boot, scope, sess))...)
	cmd.Stderr = tb
	out, _ := cmd.Output()
	res, detail := parseReap(string(out))
	if res == RemoteUnreachable {
		detail = explainSSHFailure(o.Host, tb.String())
	}
	return res, detail
}

// RemoteEndLabel は向こうを見に行った結果の文。
func RemoteEndLabel(res, detail string) string {
	switch res {
	case RemoteGone:
		return "もう居なかった"
	case RemoteKilled:
		return "残っていたものを止めた（" + detail + "）"
	case RemoteUnsupported:
		return "確かめようがない（" + detail + "）"
	}
	return "確かめられない（" + detail + "）"
}

// killChild は止める。リモートなら**向こうを先に止めてから**手元の ssh を止める。
//
// 手元の ssh を先に止めると、stdin を読んでいない向こうの子（工具を走らせている
// 最中の claude）は生き残る（実測）。
func (a *Agent) killChild(k *child) {
	k.mu.Lock()
	k.deliberate = true
	remote := k.remote
	spec := k.spec
	k.mu.Unlock()
	if remote != nil {
		if res, why := a.reapRemote(remote, spec); res == RemoteUnreachable {
			fmt.Fprintf(os.Stderr, "camp agent: %s の向こうを止められない: %s\n", k.id[:8], why)
		}
	}
	k.kill()
}

// reapOrphanRemote は、見張りが外れていたリモートのセッションを始末する。
//
// 手元の ssh が残っていれば止め（起動時刻を照らしてから）、向こうを見に行く。
// **campd はこの報告を確かめられない。** 向こうの /proc を読めるのは鍵を持つ側だけ。
func (a *Agent) reapOrphanRemote(m Msg) {
	o := Owner{PID: m.PID, Started: m.Started, BootID: m.BootID}
	if alive, known := o.Alive(); alive && known {
		syscall.Kill(-m.PID, syscall.SIGTERM)
	}
	res, detail := a.reapRemote(m.RemoteOwner, m.Remote)
	a.send(Msg{T: MsgReaped, Session: m.Session, Reason: RemoteEndLabel(res, detail),
		RemoteEnd: res})
}
