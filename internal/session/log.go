package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Log は1セッションぶんのフレームを、子の隣に落とす場所。
//
// **なぜ子の隣なのか。** 読み手（ブラウザ）が遅いと、stdout のパイプが詰まって
// 子が書けなくなり、そこで止まる。だから読み手とは関係なく、常時 drain して
// ここへ落とす。読むほうはあとから追いつく。
//
// **なぜ上限があるのか。** セッションは何時間も走りうるし、フレームには
// 工具の出力がそのまま入る。上限が無ければディスクが埋まる。溢れたぶんは
// 古いほうから落とし、**落としたことを数えて外へ出す**——黙って消さない。
type Log struct {
	dir string
	id  string

	mu      sync.Mutex
	f       *os.File
	size    int64
	seq     int64 // 最後に振った番号
	dropped int64 // これまでに落とした件数

	curFirst, curCount   int64 // いま書いているファイル
	prevFirst, prevCount int64 // 1つ前の世代

	// marks は「この seq はこのバイト位置から始まる」の間引いた目印。
	//
	// **無いと、末尾を1行引くたびにファイル全体を舐める。** 2026-09-04 の
	// outer gate で実測: 満杯（約17,000行）の落とし先で 59ms/回。SSE は
	// 300ms ごとに叩くので、読み手1人でコアの2割を使う計算になる。
	curMarks, prevMarks []mark
}

// mark は seq とその行の先頭バイト位置。
type mark struct {
	seq int64
	off int64
}

// markEvery は何行ごとに目印を置くか。8MB / 1KB ≒ 8,000行に対して 125 個。
const markEvery = 64

// seekTo は since の次の行を含みうる位置を返す。**必ず行頭。**
func seekTo(marks []mark, since int64) int64 {
	var off int64
	for _, m := range marks {
		if m.seq > since {
			break
		}
		off = m.off
	}
	return off
}

// Line は落としたフレーム1つ。
type Line struct {
	Seq   int64           `json:"seq"`
	At    string          `json:"at"`
	Kind  string          `json:"kind"`
	Frame json.RawMessage `json:"frame"`
}

// 1世代あたりの上限。2世代持つので、最大でこの倍が残る。
// テストで小さくするので var。
var maxLogBytes int64 = 8 << 20

const (
	// 1回の tail で返す上限。**境界を越える量に必ず天井を置く。**
	maxTailLines = 500
	maxTailBytes = 1 << 20
)

// OpenLog は落とし先を開く。既にあれば続きから書く。
func OpenLog(dir, id string) (*Log, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	l := &Log{dir: dir, id: id}
	// 前回の続きを知るために、両方の世代を数え直す。
	//
	// **番号を振り直さない。** 振り直すと、読み手のカーソルが黙ってずれる。
	// だから「読めなかった」を「空だった」に畳まない——畳むと seq が 1 に
	// 戻り、画面は同じ番号の別のフレームを受け取る。
	var err error
	if l.prevFirst, l.prevCount, _, l.prevMarks, err = scanFile(l.path(1)); err != nil {
		return nil, err
	}
	first, n, last, marks, err := scanFile(l.path(0))
	if err != nil {
		return nil, err
	}
	l.curFirst, l.curCount, l.curMarks = first, n, marks
	if last > l.seq {
		l.seq = last
	}
	if fi, err := os.Stat(l.path(0)); err == nil {
		l.size = fi.Size()
	}
	f, err := os.OpenFile(l.path(0), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	l.f = f
	return l, nil
}

func (l *Log) path(gen int) string {
	if gen == 0 {
		return filepath.Join(l.dir, l.id+".jsonl")
	}
	return filepath.Join(l.dir, fmt.Sprintf("%s.%d.jsonl", l.id, gen))
}

// scanFile はそのファイルの最初の seq・件数・最後の seq を返す。
//
// **無いのと読めないのを区別する。** 無いなら 0 件でよいが、読めないのを
// 0 件と答えると、番号の付け直しが黙って起きる。
func scanFile(p string) (first, count, last int64, marks []mark, err error) {
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, 0, nil, nil
		}
		return 0, 0, 0, nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	var off int64
	for sc.Scan() {
		lineLen := int64(len(sc.Bytes())) + 1 // 改行ぶん
		var ln Line
		if json.Unmarshal(sc.Bytes(), &ln) != nil {
			off += lineLen
			continue
		}
		if count == 0 {
			first = ln.Seq
		}
		if count%markEvery == 0 {
			marks = append(marks, mark{seq: ln.Seq, off: off})
		}
		count++
		last = ln.Seq
		off += lineLen
	}
	return first, count, last, marks, sc.Err()
}

// Append は1フレーム落とす。**読み手を待たない。**
func (l *Log) Append(kind string, frame []byte) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.seq++
	ln := Line{Seq: l.seq, At: time.Now().UTC().Format(time.RFC3339Nano), Kind: kind}
	if len(frame) > 0 {
		ln.Frame = json.RawMessage(frame)
	}
	b, err := json.Marshal(ln)
	if err != nil {
		return 0, err
	}
	b = append(b, '\n')

	if l.size+int64(len(b)) > maxLogBytes && l.curCount > 0 {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}
	if l.curCount%markEvery == 0 {
		l.curMarks = append(l.curMarks, mark{seq: ln.Seq, off: l.size})
	}
	n, err := l.f.Write(b)
	l.size += int64(n)
	if l.curCount == 0 {
		l.curFirst = ln.Seq
	}
	l.curCount++
	return ln.Seq, err
}

// rotate は世代を1つ進める。**溢れたぶんは数えてから捨てる。**
func (l *Log) rotate() error {
	if err := l.f.Close(); err != nil {
		return err
	}
	l.dropped += l.prevCount
	os.Remove(l.path(1))
	if err := os.Rename(l.path(0), l.path(1)); err != nil && !os.IsNotExist(err) {
		return err
	}
	l.prevFirst, l.prevCount, l.prevMarks = l.curFirst, l.curCount, l.curMarks
	l.curFirst, l.curCount, l.size, l.curMarks = 0, 0, 0, nil
	f, err := os.OpenFile(l.path(0), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	l.f = f
	return nil
}

// Stats はいま持っている範囲。
func (l *Log) Stats() (oldest, newest, dropped int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	oldest = l.curFirst
	if l.prevCount > 0 {
		oldest = l.prevFirst
	}
	if oldest == 0 {
		oldest = l.seq + 1
	}
	return oldest, l.seq, l.dropped
}

// Tail は since より後ろを返す。
//
// 第2の戻り値は「読み手のカーソルより前に落としたものがあるか」。
// **黙って飛ばさない。** 飛んだことが分かれば、画面は「ここが抜けている」と出せる。
func (l *Log) Tail(since int64, limit int) (lines []Line, gap bool, err error) {
	oldest, newest, _ := l.Stats()
	if limit <= 0 || limit > maxTailLines {
		limit = maxTailLines
	}
	if since+1 < oldest {
		gap = true
	}
	// **何も無いなら、ファイルを開かない。**
	// SSE は 300ms ごとに聞きに来る。その大半は「まだ無い」の答えになる。
	if since >= newest && !gap {
		return nil, false, nil
	}

	l.mu.Lock()
	marks := [2][]mark{l.prevMarks, l.curMarks}
	l.mu.Unlock()

	var bytes int
	for gen := 1; gen >= 0; gen-- {
		f, e := os.Open(l.path(gen))
		if e != nil {
			if os.IsNotExist(e) {
				continue // その世代はまだ無い。ふつうのこと
			}
			// **開けなかったことを「そこには無かった」と読ませない。**
			return nil, gap, fmt.Errorf("落とし先の第%d世代が読めない: %w", gen, e)
		}
		// 目印まで飛ぶ。**全部舐めない。**
		if off := seekTo(marks[1-gen], since); off > 0 {
			if _, e := f.Seek(off, 0); e != nil {
				f.Close()
				return nil, gap, e
			}
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), maxLine)
		for sc.Scan() {
			if len(lines) >= limit || bytes >= maxTailBytes {
				break
			}
			var ln Line
			if json.Unmarshal(sc.Bytes(), &ln) != nil {
				continue
			}
			if ln.Seq <= since {
				continue
			}
			bytes += len(sc.Bytes())
			lines = append(lines, ln)
		}
		f.Close()
		if len(lines) >= limit || bytes >= maxTailBytes {
			break
		}
	}
	return lines, gap, nil
}

// Close は閉じる。中身は消さない（あとから読める）。
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Remove は落とし先ごと消す。セッションが終わって、もう要らないときだけ。
func (l *Log) Remove() {
	l.Close()
	os.Remove(l.path(0))
	os.Remove(l.path(1))
}

// DefaultLogDir は落とし先。**本人のユーザーの領域**に置く（実行面が書く）。
func DefaultLogDir() string {
	if d := os.Getenv("CAMP_SESSION_LOG_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "camp", "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "camp-sessions")
	}
	return filepath.Join(home, ".local", "state", "camp", "sessions")
}

// logExists は落とし先が既にあるか。**開かずに見る**（開くと作ってしまう）。
func logExists(dir, id string) bool {
	l := &Log{dir: dir, id: id}
	for _, gen := range []int{0, 1} {
		if _, err := os.Stat(l.path(gen)); err == nil {
			return true
		}
	}
	return false
}
