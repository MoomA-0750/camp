package session

import (
	"fmt"
	"time"
)

// campd 側から、向こうのホストの記録を読む頼み（M47）。
//
// **campd 自身は ssh しない。** 鍵を持つのは実行面だけ（D-025）。ここがやるのは
// 「頼んで待つ」だけで、行き先を照らすのも向こうの sh を走らせるのも実行面。

// RecReq は「どのホストの、どのエージェントの記録か」。
//
// **パスは運ばない。** 置き場は向こうが $HOME と環境変数から解決して名乗る
// （本人の決定 2026-09-12。自由なパスを打ち込む口を作らない）。
type RecReq struct {
	Remote RemoteSpec
	// HomeEnv・HomeDefault は駆動器の RemoteLaunch と同じ（置き場の環境変数名と、
	// 無いときの $HOME からの相対パス）。Sub はその下の記録の置き場。
	HomeEnv     string
	HomeDefault string
	Sub         string
}

// ReadRecords は「このホストの、このエージェントの記録を読め」。**campd が外から挿す。**
//
// 取り込みそのものはここではできない——`internal/session` は `internal/ingest` を
// 取り込まない（向きを一方通行に保つ。繋ぐのは `internal/remoteingest` だけ）。
// そこで差し込み口だけ置いて、`cmd/campd` で繋ぐ。nil なら何もしない。
//
// 呼ばれるのは別の goroutine。**終わりの処理を待たせない。**
type ReadRecords func(host, agent string)

// SetReadRecords は読み手を挿す。走り出す前に1回だけ呼ぶ。
func (s *Supervisor) SetReadRecords(f ReadRecords) {
	s.mu.Lock()
	s.readRecords = f
	s.mu.Unlock()
}

// ReadRecordsNow は「いま読め」（画面から押したとき）。**待たない**——
// 携帯の回線では1周に何分もかかる。結果は台帳の跡と、取り込んだ行に出る。
//
// 読み手が挿さっていなければ（campd が組み立てていない）false を返す。
func (s *Supervisor) ReadRecordsNow(host, agent string) bool {
	s.mu.Lock()
	f := s.readRecords
	s.mu.Unlock()
	if f == nil {
		return false
	}
	go f(host, agent)
	return true
}

// afterSession は、そのホストのセッションが終わった直後の1回。
func (s *Supervisor) afterSession(host, agent string) {
	s.mu.Lock()
	f := s.readRecords
	s.mu.Unlock()
	if f == nil {
		return
	}
	go f(host, agent)
}

// RecordCapEvery は「見に行く間隔」を伸ばすときの上限。
//
// 続けて失敗した接続先は 2 倍ずつ伸びる（DueRecordRoots）。**寝ている携帯を
// 叩き続けない**ため。成功したら戻る。
const RecordCapEvery = 4 * time.Hour

// RunRecords は番が来た行を読みに行かせ続ける（本人の決定 2026-09-12: 定期にも読む）。
//
// every は既定の間隔（`serve` の `-record-every`）。**0 なら定期をやめる**——
// そのときは押したときと、そのホストのセッションが終わった直後だけになる。
func (s *Supervisor) RunRecords(done <-chan struct{}, every time.Duration) {
	if every <= 0 {
		return
	}
	t := time.NewTicker(recordTick(every))
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			s.readDue(every)
		}
	}
}

// recordTick は見回りの刻み。間隔そのもので刻むと、伸びた行の番が来たことに
// 気づくのが遅れる。細かすぎても台帳を読むだけなので安い。
func recordTick(every time.Duration) time.Duration {
	const max = 5 * time.Minute
	if every > max {
		return max
	}
	return every
}

// readDue は番が来た行を読みに行かせる。**重ならないように1周ずつ。**
func (s *Supervisor) readDue(every time.Duration) {
	s.mu.Lock()
	f, busy := s.readRecords, s.reading
	if f != nil && !busy {
		s.reading = true
	}
	s.mu.Unlock()
	if f == nil || busy {
		return // 前の周がまだ終わっていない。次の刻みで
	}
	defer func() {
		s.mu.Lock()
		s.reading = false
		s.mu.Unlock()
	}()

	due, err := DueRecordRoots(s.db, s.Now(), every, RecordCapEvery)
	if err != nil {
		return // **静かに次回へ。** 台帳が読めないことは画面のエラーにしない
	}
	for _, r := range due {
		f(r.Host, r.Agent) // 失敗の扱いは読み手の側（台帳に跡を残す）
	}
}

// RecordReq は「このホストの、このエージェントの記録を読む」頼みを組み立てる。
//
// **置き場の決め方だけを運ぶ。** パスは向こうが $HOME と環境変数から解決して名乗る
// （本人の決定 2026-09-12）。置き場そのもの（$CLAUDE_CONFIG_DIR / $CODEX_HOME と既定）は
// 駆動器が持ち、その下の名前（projects / sessions）は取り込み器が言う。
//
// 許していない接続先・固定の無い接続先・台帳の行が無いか止めてある組み合わせは、ここで断る。
func (s *Supervisor) RecordReq(agent, host, sub string) (RecReq, error) {
	spec, err := checkRecord(s.db, agent, host)
	if err != nil {
		return RecReq{}, err
	}
	d, ok := drivers[agentOr(agent)]
	if !ok {
		return RecReq{}, fmt.Errorf("知らないエージェント: %s", agent)
	}
	rl := d.RemoteLaunch()
	if rl.HomeEnv == "" && rl.HomeDefault == "" {
		return RecReq{}, fmt.Errorf("%s は置き場を名乗らないので、向こうの記録を読めない", agent)
	}
	if sub == "" {
		return RecReq{}, fmt.Errorf("%s の記録の置き場が決まっていない", agent)
	}
	return RecReq{Remote: *spec, HomeEnv: rl.HomeEnv, HomeDefault: rl.HomeDefault, Sub: sub}, nil
}

// RecListing は一覧の結果。
type RecListing struct {
	// Root は**向こうが解決して名乗った置き場の実パス**。
	Root  string
	Files []RecFile
	// Windows は頼んだ位置の直前の中身。**ハッシュは手元で取る**——向こうで
	// 計算させると、取り方がずれても気づけない（向こうに sha256sum があるとも限らない）。
	Windows map[string][]byte
}

// RecList は実行面に向こうの記録を数えさせる。
func (s *Supervisor) RecList(r RecReq, at map[string]int64, win int64) (*RecListing, error) {
	m := Msg{
		T:              MsgRecList,
		Remote:         &r.Remote,
		RecHomeEnv:     r.HomeEnv,
		RecHomeDefault: r.HomeDefault,
		RecSub:         r.Sub,
		RecAt:          at,
		RecWin:         win,
	}
	got, err := s.recAsk(m, recListDeadline)
	if err != nil {
		return nil, err
	}
	return &RecListing{Root: got.RecRoot, Files: got.RecFiles, Windows: got.RecWindow}, nil
}

// RecRead は実行面に範囲をまとめて取り寄せさせる。more は「蓋で切った。続きがある」。
func (s *Supervisor) RecRead(r RecReq, want []RecRange, capBytes int64) (data map[string][]byte, more bool, err error) {
	m := Msg{
		T:              MsgRecRead,
		Remote:         &r.Remote,
		RecHomeEnv:     r.HomeEnv,
		RecHomeDefault: r.HomeDefault,
		RecSub:         r.Sub,
		RecWant:        want,
		RecCap:         capBytes,
	}
	got, err := s.recAsk(m, recReadDeadline)
	if err != nil {
		return nil, false, err
	}
	return got.RecData, got.RecMore, nil
}

const (
	// 実行面の側の待ち（recListWait・recReadWait）より長くしておく。
	// **先に諦めるのは向こう側**——こちらが先に諦めると、返事の行き先が消える。
	recListDeadline = 2 * time.Minute
	recReadDeadline = 4 * time.Minute
)

// recAsk は実行面へ1つ頼んで、答えを待つ（ScanSSH と同じ作法）。
func (s *Supervisor) recAsk(m Msg, wait time.Duration) (Msg, error) {
	s.mu.Lock()
	agent := s.agent
	s.mu.Unlock()
	if agent == nil {
		return Msg{}, ErrNoAgent
	}
	req := newID()
	ch := make(chan Msg, 1)
	s.mu.Lock()
	s.waits[req] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.waits, req)
		s.mu.Unlock()
	}()
	m.ReqID = req
	if err := agent.send(m); err != nil {
		return Msg{}, err
	}
	select {
	case got := <-ch:
		if got.Error != "" {
			return Msg{}, fmt.Errorf("%s", got.Error)
		}
		return got, nil
	case <-time.After(wait):
		return Msg{}, fmt.Errorf("実行面が返事をしない")
	}
}
