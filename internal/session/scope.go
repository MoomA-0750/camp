package session

// scope の後始末を**確かめる**（Phase 3.6 の outer gate で codex が指摘）。
//
// `systemctl --user stop` の結果を見ずに「止めた」と言っていた。Codex の子は別のプロセス
// グループ・別のセッションに居る（実測）ので、プロセスグループへのシグナルでは取り逃がす。
// 子の本体が終わっても、scope（cgroup）には残りが居るかもしれない。
//
// そこで、終わったあと・止めたあとに scope の cgroup を数え、残っていれば止め、それでも
// 残れば数を返す。**確かめられないことを「空だった」とは読まない。**

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// defaultScopeProcs は scope の cgroup に居るプロセスを返す。unit が片付いていれば空。
func defaultScopeProcs(scope string) ([]int, error) {
	out, err := exec.Command("systemctl", "--user", "show", "-p", "ControlGroup", "--value",
		"--", scope).Output()
	if err != nil {
		return nil, err
	}
	cg := strings.TrimSpace(string(out))
	if cg == "" {
		return nil, nil // unit が無い＝中にもう何も居ない（--collect で片付く）
	}
	b, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", cg, "cgroup.procs"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var pids []int
	for _, f := range strings.Fields(string(b)) {
		if p, err := strconv.Atoi(f); err == nil {
			pids = append(pids, p)
		}
	}
	return pids, nil
}

func defaultScopeStop(scope string) error {
	return exec.Command("systemctl", "--user", "stop", "--", scope).Run()
}

// sweepScope は scope に残っているものを止め、止められなかった数を返す。
// known が false なら確かめられなかった（systemd に訊けない）。
//
// **撃つのは scope の cgroup に居るものだけ。** scope 名は campd が採番したセッションの
// もので、Camp 以外は作らない（`camp-session-<乱数>.scope`）。
func (a *Agent) sweepScope(scope string) (left int, known bool) {
	if scope == "" {
		return 0, true
	}
	procs, stop := a.ScopeProcs, a.ScopeStop
	if procs == nil {
		procs = defaultScopeProcs
	}
	if stop == nil {
		stop = defaultScopeStop
	}
	for try := 0; try < 4; try++ {
		pids, err := procs(scope)
		if err != nil {
			return 0, false
		}
		if len(pids) == 0 {
			return 0, true
		}
		if try == 0 {
			stop(scope) // 失敗しても次で1つずつ止める。結果は数えて確かめる
		} else {
			for _, p := range pids {
				syscall.Kill(p, syscall.SIGKILL)
			}
		}
		time.Sleep(time.Duration(200*(try+1)) * time.Millisecond)
	}
	pids, err := procs(scope)
	if err != nil {
		return 0, false
	}
	return len(pids), true
}

// leftovers は子が終わったあと、その scope に残ったものを止め、止められなかった数を返す。
// -1 は確かめられなかった。scope を使っていなければ 0。
func (a *Agent) leftovers(k *child) int {
	if k.scope == "" {
		return 0
	}
	left, known := a.sweepScope(k.scope)
	if !known {
		return -1
	}
	return left
}

// leftoverNote は終わった理由に添える一言。
func leftoverNote(n int) string {
	if n < 0 {
		return "（scope に残りが無いか確かめられなかった）"
	}
	return fmt.Sprintf("（scope に %d 個残り、止められなかった）", n)
}
