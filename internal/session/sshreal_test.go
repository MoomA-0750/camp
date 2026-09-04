package session

import (
	"os"
	"testing"
)

// 本物の `~/.ssh/config` で読めるかを見る。**中身は出さない。**
// 数と、どの欄が埋まったかだけを出す。
func TestTheRealSSHConfigParses(t *testing.T) {
	p := DefaultSSHConfig()
	if _, err := os.Stat(p); err != nil {
		t.Skip("~/.ssh/config が無い")
	}
	hosts, err := ReadSSHConfig(p)
	if err != nil {
		t.Fatalf("本物が読めない: %v", err)
	}
	if len(hosts) == 0 {
		t.Skip("接続先が1件も書かれていない")
	}
	var withHost, withUser, withKey int
	for _, h := range hosts {
		if h.HostName != "" {
			withHost++
		}
		if h.User != "" {
			withUser++
		}
		if h.Identity != "" {
			withKey++
		}
	}
	t.Logf("%d 件（HostName あり %d / User あり %d / 鍵あり %d）",
		len(hosts), withHost, withUser, withKey)
}
