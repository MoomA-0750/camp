// Package session は Camp が起こしたセッションを見張る。
//
// **campd は子を起こせない。** camp ユーザーで動いていて ProtectHome=read-only が
// 掛かっているので、`claude` が書く ~/.claude/ に手が届かない（2026-09-04 実測。
// dev/active/phase3-baseline.md）。だから実行面は本人のユーザーで動く
// `campd agent` に分け、campd 側は決めること・記録すること・出すことに徹する。
//
// このファイルはその両側が使う「そのプロセスは本当にあれか」の判定。
package session

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// procStatPath は差し替え可能にしておく（テストで偽の /proc を使う）。
var procRoot = "/proc"

// BootID は今の起動を識別する。
//
// **起動時刻（下の Starttime）は起動からの経過なので、再起動を跨ぐと比較できない。**
// 「pid 1234 は起動後 9,565,016 tick に始まった」は、再起動後の別プロセスにも
// 同じくらい当てはまってしまう。だから boot_id と組で持つ。
func BootID() string {
	b, err := os.ReadFile(procRoot + "/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Starttime は pid の起動時刻（起動からの clock ticks）を返す。
//
// /proc/<pid>/stat の22番目。**先頭から数えてはいけない**——2番目の comm は
// 実行ファイル名で、空白も括弧も入りうる。`) ` の**最後の**出現で切る。
func Starttime(pid int) (uint64, error) {
	b, err := os.ReadFile(fmt.Sprintf("%s/%d/stat", procRoot, pid))
	if err != nil {
		return 0, err
	}
	s := string(b)
	i := strings.LastIndex(s, ") ")
	if i < 0 {
		return 0, fmt.Errorf("pid %d の stat が読めない形をしている", pid)
	}
	// i+2 からは3番目のフィールド（state）。22番目は数えて 20 個先。
	f := strings.Fields(s[i+2:])
	const idx = 22 - 3
	if len(f) <= idx {
		return 0, fmt.Errorf("pid %d の stat にフィールドが足りない（%d 個）", pid, len(f))
	}
	v, err := strconv.ParseUint(f[idx], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("pid %d の起動時刻が数でない: %w", pid, err)
	}
	return v, nil
}

// Owner は「そのプロセスは、いま自分が知っているあれか」の答え。
type Owner struct {
	PID     int
	Started uint64
	BootID  string
}

// Alive は o の指すプロセスが今も生きているかを返す。
//
// **pid が生きているかだけを見ない。** pid は使い回されるので、
// 死んだ子と同じ番号を取った他人のプロセスを掴む。起動時刻が一致して初めて
// 「あれ」だと言える。
//
// 第2の戻り値は「判定できたか」。/proc が読めない環境では嘘をつくより
// 分からないと言う（**「見ていないから居ない」を「居ないから居ない」と読ませない**）。
func (o Owner) Alive() (alive bool, known bool) {
	if o.PID <= 0 {
		return false, true // そもそも起こしていない
	}
	if o.BootID != "" {
		if now := BootID(); now != "" && now != o.BootID {
			// 再起動を跨いだ。**そのプロセスはもう居ない**と言い切れる。
			return false, true
		}
	}
	st, err := Starttime(o.PID)
	if err != nil {
		if os.IsNotExist(err) {
			return false, true // pid が居ない
		}
		return false, false // 読めなかった。居ないとは限らない
	}
	if o.Started == 0 {
		return false, false // 比べる相手を持っていない
	}
	return st == o.Started, true
}
