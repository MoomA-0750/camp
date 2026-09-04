package session

import (
	"fmt"
	"os"
	"testing"
)

// **テストが本人の置き場へ書いていないことを、毎回確かめる。**
//
// 2026-09-04 の outer gate で、テストが 221 個のフレームログを本物の
// `~/.local/state/camp/sessions/` に残しているのを見つけた。`Agent.LogDir`
// の既定値がそこを指していて、差し替えを忘れると静かに漏れる。
// 「気をつける」では再発するので、数えて落とす。
func TestMain(m *testing.M) {
	dir := DefaultLogDir()
	before := countFiles(dir)
	code := m.Run()
	if after := countFiles(dir); after > before {
		fmt.Fprintf(os.Stderr,
			"\nテストが %s へ %d 個のファイルを残した。**LogDir を t.TempDir() にする**\n",
			dir, after-before)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func countFiles(dir string) int {
	e, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	return len(e)
}
