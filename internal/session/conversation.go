package session

import (
	"bufio"
	"encoding/json"
	"io"
)

// Conversation は子1本との会話。**実行面の本流は、これを通してしか子と話さない**
// （D-031。dev/active/phase3.7-design.md の駆動器）。駆動器の Open が作る。
//
// 返す []byte は子の stdin へそのまま書く1行（改行は本流が足す）。
type Conversation interface {
	// Begin は話し始めるまでの手順。Codex は initialize → thread/start と照合、Claude は無し。
	// 途中で流れた行は o.Record で落とし先へ残す。
	Begin(o BeginOpts) error
	// Fold は流れてくる1行を Camp の言葉に畳む。**知らないものは種類だけにして通す。**
	Fold(line []byte) Event
	// Input は1ターン分の入力。
	Input(text string) ([]byte, error)
	// Answer は待っている承認への答え。**見た id にだけ答える**——campd から来た文字列を
	// そのまま埋めると、別の中身を差し込める。
	Answer(reqID, behavior, reason string) ([]byte, error)
	// Interrupt は中断。nil なら今は投げられない（あとで Event.Replies に出る）。
	Interrupt() []byte
	// Query は残量の問い合わせ。req が nil なら手元の控え now をそのまま返す。
	// req を投げたなら、答えは Fold が Event.Deliver = key で渡す。
	Query(kind string) (req []byte, key string, now json.RawMessage, err error)
	// Waiting はまだ答えていない承認。引き取り直しで campd へ名乗る。
	Waiting() []HeldAsk
	// SessionID はエージェント自身のセッション id（分からなければ空）。
	SessionID() string
}

// BeginOpts は Begin に渡すもの。**再開（Phase 3.8 以降）で増えるので構造体で受ける。**
type BeginOpts struct {
	Scanner *bufio.Scanner
	W       io.Writer
	Record  func(kind string, line []byte)
}

// OpenOpts は駆動器が会話を作るときに要るもの。
type OpenOpts struct {
	Session string // Camp が採番した id
	Perm    string
	Cwd     string // 起こした場所（実パス）
	// Home はエージェントの置き場。照らす駆動器だけが使う（Codex は本人の置き場で起きたかを見る）。
	Home string
	// Remote は向こうのホストの子か。RemoteHomes は向こうの sh が名乗った置き場（直す前と実パス）。
	// 向こうのパスは手元で実パスに直せないので、照らす駆動器はこれと文字の上で比べる。
	Remote      bool
	RemoteHomes []string
}

// Event は1行を Camp の言葉に直したもの。campd へは Msg の欄として渡る。
type Event struct {
	// Drop は読めない行。落とし先にも残さず、campd へも渡さない（Claude の今までの振る舞い）。
	Drop        bool
	Kind        string
	TurnEnd     bool
	Interrupted bool   // 中断でターンが終わった
	Err         string // 監査に残す一言（断った・ターンが失敗した）
	Note        string // 監査に残す記録（止めない。設定が途中で変わった等）
	// Halt は止める理由。頼んだ確認の度合いで起きていなかった（起こしたときに1回だけ照らす）。
	Halt      string
	Ask       *HeldAsk        // 画面へ出す承認
	Replies   [][]byte        // 実行面がすぐ子へ返すもの（断り・後回しの中断）
	Withdrawn []string        // もう答えを待たなくなった承認の id
	Deliver   string          // 待っている者へ渡す応答の鍵（残量の問い合わせ）
	Payload   json.RawMessage // その中身
	SessionID string          // エージェント自身のセッション id（この行で分かったなら）
}
