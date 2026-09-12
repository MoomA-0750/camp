package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// 実行面が向こうのホストの記録を読む（M47）。**読むだけ。向こうへは書かない。**
//
// campd は ssh しない（D-025）。鍵を持つのは実行面だけなので、記録を読むのもここを通す。
//
// **繋ぐ前に行き先を照らす。** `checkRemote` が見るのは台帳の可否と「固定があるか」だけで、
// 「いまの行き先が固定と同じか」は実行面が `ssh -G` で照らしている（startRemote）。
// 繋いだ時点で向こうのログインシェルが走るので、照らさずに繋がない。

const (
	recListWait = 90 * time.Second
	recReadWait = 3 * time.Minute

	// recHardCap は1回の返事で運ぶ上限。campd が頼む蓋より大きくても、ここで切る。
	// **1つの Msg が大きくなりすぎないため**（受け取る側に1行の蓋がある）。
	recHardCap = 512 << 10

	// recScanMax は向こうから来る1行の上限。断片は base64 なので中身より 1.3 倍になる。
	recScanMax = 1 << 20

	// recWinDefault は再開点の窓。**手元と同じ値を campd が渡す**ので、ここは保険。
	recWinDefault = 256
)

// recList は向こうの記録を数えて返す。
func (a *Agent) recList(m Msg) {
	out := Msg{T: MsgRecListRes, ReqID: m.ReqID}
	ctx, cancel := context.WithTimeout(context.Background(), recListWait)
	defer cancel()

	win := m.RecWin
	if win <= 0 {
		win = recWinDefault
	}
	cmd, err := a.recDial(ctx, m, recListScript,
		recDash(m.RecHomeEnv), recDash(m.RecHomeDefault), m.RecSub, strconv.FormatInt(win, 10))
	if err != nil {
		out.Error = err.Error()
		a.send(out)
		return
	}

	// 頼む窓は「パス<TAB>位置」。**改行やタブを含むパスは頼まない**（向こうの読み口が
	// タブ区切りなので、通すと行を偽装できる）。
	var in bytes.Buffer
	for p, off := range m.RecAt {
		if off <= 0 || strings.ContainsAny(p, "\n\t") {
			continue
		}
		fmt.Fprintf(&in, "%s\t%d\n", p, off)
	}
	cmd.Stdin = &in

	lines, tag, err := a.recRun(cmd, m.Remote.Alias)
	if err != nil {
		out.Error = err.Error()
		a.send(out)
		return
	}
	out.RecRoot = tag

	for _, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "F\t"):
			// F<TAB>大きさ<TAB>更新時刻<TAB>パス
			f := strings.SplitN(ln, "\t", 4)
			if len(f) != 4 {
				continue
			}
			size, err1 := strconv.ParseInt(f[1], 10, 64)
			mtime, err2 := strconv.ParseInt(strings.SplitN(f[2], ".", 2)[0], 10, 64)
			if err1 != nil || err2 != nil {
				continue
			}
			out.RecFiles = append(out.RecFiles, RecFile{Path: f[3], Size: size, MTime: mtime})
		case strings.HasPrefix(ln, "W\t"):
			// W<TAB>パス<TAB>中身（base64）
			f := strings.SplitN(ln, "\t", 3)
			if len(f) != 3 {
				continue
			}
			b, err := base64.StdEncoding.DecodeString(f[2])
			if err != nil {
				continue
			}
			if out.RecWindow == nil {
				out.RecWindow = map[string][]byte{}
			}
			out.RecWindow[f[1]] = b
		}
	}
	a.send(out)
}

// recRead は頼まれた範囲だけ返す。**まとめて頼む**——1本ずつ繋ぎ直さないため。
func (a *Agent) recRead(m Msg) {
	out := Msg{T: MsgRecReadRes, ReqID: m.ReqID}
	ctx, cancel := context.WithTimeout(context.Background(), recReadWait)
	defer cancel()

	cap := m.RecCap
	if cap <= 0 || cap > recHardCap {
		cap = recHardCap
	}
	cmd, err := a.recDial(ctx, m, recReadScript,
		recDash(m.RecHomeEnv), recDash(m.RecHomeDefault), m.RecSub, strconv.FormatInt(cap, 10))
	if err != nil {
		out.Error = err.Error()
		a.send(out)
		return
	}

	var in bytes.Buffer
	for _, r := range m.RecWant {
		if r.N <= 0 || r.Off < 0 || strings.ContainsAny(r.Path, "\n\t") {
			continue
		}
		fmt.Fprintf(&in, "%s\t%d\t%d\n", r.Path, r.Off, r.N)
	}
	cmd.Stdin = &in

	lines, tag, err := a.recRun(cmd, m.Remote.Alias)
	if err != nil {
		out.Error = err.Error()
		a.send(out)
		return
	}
	out.RecRoot = tag

	var total int64
	for _, ln := range lines {
		switch {
		case ln == "CAP":
			out.RecMore = true
		case strings.HasPrefix(ln, "D\t"):
			// D<TAB>パス<TAB>位置<TAB>中身（base64）
			f := strings.SplitN(ln, "\t", 4)
			if len(f) != 4 {
				continue
			}
			b, err := base64.StdEncoding.DecodeString(f[3])
			if err != nil {
				continue
			}
			if total+int64(len(b)) > recHardCap {
				out.RecMore = true
				continue
			}
			total += int64(len(b))
			if out.RecData == nil {
				out.RecData = map[string][]byte{}
			}
			// **1回の頼みでは1つのパスにつき1つの範囲**（campd がそう頼む）。
			out.RecData[f[1]] = b
		}
	}
	a.send(out)
}

// recDial は接続先を照らしてから ssh のコマンドを作る。**照らすまで繋がない。**
func (a *Agent) recDial(ctx context.Context, m Msg, script string, args ...string) (*exec.Cmd, error) {
	if m.Remote == nil {
		return nil, fmt.Errorf("接続先が無い")
	}
	alias := m.Remote.Alias
	if err := validAlias(alias); err != nil {
		return nil, err
	}
	if m.RecSub == "" || strings.ContainsAny(m.RecSub, "\n\t") {
		return nil, fmt.Errorf("記録の置き場の指定がおかしい")
	}
	if !m.Remote.Pin.Pinned() {
		return nil, fmt.Errorf("%s は行き先（ホスト鍵）を固定していない。許し直す", alias)
	}
	now, err := a.resolve(alias)
	if err != nil {
		return nil, err
	}
	if d := m.Remote.Pin.Diff(now); d != "" {
		return nil, fmt.Errorf("%s の行き先が、許したときと違う（%s）。~/.ssh/config と known_hosts を確かめてから許し直す",
			alias, d)
	}
	return exec.CommandContext(ctx, a.SSH, a.sshArgs("--", alias, remoteCommand(script, args...))...), nil
}

// recRun は走らせて、1行目のタグを確かめ、残りの行を返す。
//
// **1行目を読むまで中身として扱わない**（ログインシェルが何か吐いても混ざらない）。
func (a *Agent) recRun(cmd *exec.Cmd, alias string) (lines []string, root string, err error) {
	var so bytes.Buffer
	tb := &tailBuf{max: 2048}
	cmd.Stdout = &so
	cmd.Stderr = tb
	runErr := cmd.Run()

	sc := bufio.NewScanner(&so)
	sc.Buffer(make([]byte, 0, 64<<10), recScanMax)
	if !sc.Scan() {
		if runErr != nil {
			return nil, "", fmt.Errorf("%s", explainSSHFailure(alias, tb.String()))
		}
		return nil, "", fmt.Errorf("向こうが何も返さない")
	}
	head := strings.SplitN(sc.Text(), "\t", 3)
	switch {
	case head[0] == "CAMP-ERR" && len(head) == 3:
		return nil, "", fmt.Errorf("%s", head[2])
	case head[0] == "CAMP-REC" && len(head) == 3:
		root = head[2]
	default:
		if runErr != nil {
			return nil, "", fmt.Errorf("%s", explainSSHFailure(alias, tb.String()))
		}
		return nil, "", fmt.Errorf("向こうの返事が読めない")
	}
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	// **終わりの印が無ければ、途中で切れている。**
	//
	// 名乗りだけ読めて中身が来ないのを「読めたが0件」と読まない——2026-09-12、
	// macOS で実際にそうなった（向こうに GNU の道具が無く、名乗った直後に落ちていた）。
	// 蓋で切ったときは向こうが最後まで走って印を出すので、ここでは捨てない。
	if len(lines) == 0 || lines[len(lines)-1] != "END" {
		if runErr != nil {
			return nil, "", fmt.Errorf("%s", explainSSHFailure(alias, tb.String()))
		}
		return nil, "", fmt.Errorf("向こうの返事が途中で切れた（%s の記録を読めていない）", alias)
	}
	return lines[:len(lines)-1], root, nil
}

func recDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
