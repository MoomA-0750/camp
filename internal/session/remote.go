package session

import (
	"bufio"
	"bytes"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// リモート起動（2026-09-11）。**向こうに常駐するものは置かない**（D-003）。
//
// 実行面が `ssh <alias> <小さな sh>` を起こし、その sh が場所を確かめてから
// `claude` を子として起こし、終わるまで待つ。stream-json はそのまま ssh の上を流れる。
//
// 実測（2026-09-11、ssh localhost。dev/active/phase3.5-plan.md）で決まったこと:
//
//   - sshd は遠隔コマンドごとに setsid する。向こうの sh は pid = pgid = sid
//   - 手元の ssh が死んでも、**stdin を読んでいない向こうの子は生き残る**
//   - stdin が閉じると子は終わるが、**孫は生き残る**
//   - 向こうの子がシグナルで死んでも ssh の出口は 255。**接続の失敗と同じ番号**
//   - `systemd-run --scope` は ssh の中でも pid を変えずに成り代わる
//
// だから向こうの sh は `claude` に成り代わらず、**親として残る**。子が終わったら
// 同じセッションの残りを止めてから抜ける——残りが stdout を握っていると、
// sshd はチャネルを閉じず、手元の ssh も終わらない。手元からも、終わったら
// 必ず見に行って残りを始末する（接続が切れた場合はこちらしか効かない）。

// Resolved は `ssh -G` が決めた行き先。**許したときの値を固定し、起こすたびに照らす。**
//
// 本体は HostKeys。hostname・user・port だけを固定しても、`UserKnownHostsFile` や
// `HostKeyAlias` を書き換えれば同じ名前のまま別の鍵を信じさせられる（2026-09-11 の
// outer gate で codex が指摘）。**ssh が信じるホスト鍵の指紋を全部固定する**——
// ホスト鍵は経路（DNS・ProxyCommand）に関係なく向こうに証明させるものなので、
// これが同じなら繋がる相手は許したときと同じ。
type Resolved struct {
	HostName     string `json:"hostname"`
	User         string `json:"user"`
	Port         string `json:"port"`
	ProxyJump    string `json:"proxyjump,omitempty"`
	ProxyCommand string `json:"proxycommand,omitempty"`
	HostKeyAlias string `json:"hostkeyalias,omitempty"`
	// HostKeys は、この行き先について known_hosts が信じる鍵の指紋（SHA256:…、並べ替え済み）。
	HostKeys []string `json:"hostkeys,omitempty"`
}

// Pinned は起こしてよい固定か。**鍵の無い固定は固定ではない。**
func (r *Resolved) Pinned() bool {
	return r != nil && r.HostName != "" && len(r.HostKeys) > 0
}

// parseSSHG は `ssh -G` の出力から、行き先を決める欄と、known_hosts の在り処を拾う。
func parseSSHG(out []byte) (Resolved, []string) {
	var r Resolved
	var files []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if v == "none" {
			v = ""
		}
		switch strings.ToLower(k) {
		case "hostname":
			r.HostName = v
		case "user":
			r.User = v
		case "port":
			r.Port = v
		case "proxyjump":
			r.ProxyJump = v
		case "proxycommand":
			r.ProxyCommand = v
		case "hostkeyalias":
			r.HostKeyAlias = v
		case "userknownhostsfile", "globalknownhostsfile":
			files = append(files, strings.Fields(v)...)
		}
	}
	return r, files
}

// Diff は違いを人の読める形で返す。同じなら空。
func (r Resolved) Diff(now Resolved) string {
	var d []string
	add := func(name, was, is string) {
		if was != is {
			d = append(d, fmt.Sprintf("%s %q → %q", name, was, is))
		}
	}
	add("hostname", r.HostName, now.HostName)
	add("user", r.User, now.User)
	add("port", r.Port, now.Port)
	add("proxyjump", r.ProxyJump, now.ProxyJump)
	add("proxycommand", r.ProxyCommand, now.ProxyCommand)
	add("hostkeyalias", r.HostKeyAlias, now.HostKeyAlias)
	if strings.Join(r.HostKeys, ",") != strings.Join(now.HostKeys, ",") {
		d = append(d, fmt.Sprintf("信じるホスト鍵 %d 個 → %d 個（中身が違う）",
			len(r.HostKeys), len(now.HostKeys)))
	}
	return strings.Join(d, "、")
}

// String は画面と CLI 用。
func (r Resolved) String() string {
	s := r.HostName
	if r.User != "" {
		s = r.User + "@" + s
	}
	if r.Port != "" && r.Port != "22" {
		s += ":" + r.Port
	}
	if r.ProxyJump != "" {
		s += "（" + r.ProxyJump + " 経由）"
	}
	return s
}

// RecFile は向こうの記録1本（一覧の1件）。
//
// **dev・inode は持たない。** 同じ実体かは大きさと再開点の窓だけで決める
// （2026-09-03 の実測で、再起動で dev が変わっただけで 72 ファイル全部が
// 「別の実体」と判定された）。
type RecFile struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // Unix 秒
}

// RecRange は取り寄せる範囲。N は最大バイト数。
type RecRange struct {
	Path string `json:"path"`
	Off  int64  `json:"off"`
	N    int64  `json:"n"`
}

// RemoteSpec は campd が実行面へ渡す「どこへ繋ぐか」。
type RemoteSpec struct {
	Alias string `json:"alias"`
	// Pin は許したときの行き先。実行面は `ssh -G` と照らし、違えば起こさない。
	Pin Resolved `json:"pin"`
	// Bin は向こうでの、起こすエージェントの実体（台帳の場所）。空なら向こうで探す。
	Bin string `json:"bin,omitempty"`
}

// RemoteOwner は向こうで起きたセッションの身元。**向こうの sh が名乗ったもの。**
//
// PID は向こうの sh（sshd が setsid したので、セッション番号でもある）。
// `claude` はその子。止めるときはセッションごと止める。
//
// campd はこれを確かめられない（camp ユーザーは鍵を持たないし、持たせない）。
// 手元の子なら /proc で起動時刻を見られるが、向こうの子は実行面の報告しか無い。
type RemoteOwner struct {
	Host    string `json:"host"`
	PID     int    `json:"pid"`
	Started uint64 `json:"proc_started,omitempty"`
	BootID  string `json:"boot_id,omitempty"`
	Scope   string `json:"scope,omitempty"`
	// Root は許した行を向こうで実パスに直したもの。Cwd は実際に降りた場所。
	Root string `json:"root"`
	Cwd  string `json:"cwd"`
	// Session はしるし（CAMP_SESSION）の値 = Camp のセッション id。後始末で、別のセッションへ逃げた
	// 残りを探すのに使う（台帳にあるので、campd の再起動後も渡せる）。
	Session string `json:"session,omitempty"`
	// Home・HomeReal はエージェントの置き場（向こうの sh が名乗る。照らす駆動器だけ）。
	Home     string `json:"home,omitempty"`
	HomeReal string `json:"home_real,omitempty"`
}

var sessionIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// 向こうを見に行った結果。
const (
	RemoteGone        = "gone"        // もう居なかった
	RemoteKilled      = "killed"      // 残っていたので止めた
	RemoteUnsupported = "unsupported" // 向こうに /proc が無く、確かめようがない
	RemoteUnreachable = "unreachable" // 繋がらなかった。**あとでもう一度見に行く**
)

var aliasRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+-]{0,127}$`)

// validAlias は ssh に渡すエイリアスを確かめる。
//
// **`-` で始まる名前を通さない。** ssh の引数として読まれて、
// `-oProxyCommand=…` のような形で手元のコマンドを走らせられる。
// 引数の前に `--` も置くが、二重に塞ぐ。
func validAlias(a string) error {
	if !aliasRe.MatchString(a) {
		return fmt.Errorf("接続先の名前として使えない: %q", a)
	}
	return nil
}

// cleanRemotePath は向こうのパスを確かめて、正規化した形を返す。
//
// **campd は向こうのパスを実パスに直せない。** だから文字の上で決められることは
// ここで全部決める——絶対パスであること、`..` を含まないこと、制御文字が無いこと。
// symlink は向こうで起こすときに解く（下の wrapperScript）。
func cleanRemotePath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("場所が要る")
	}
	if len(p) > 4096 {
		return "", fmt.Errorf("パスが長すぎる")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("パスに制御文字がある")
		}
	}
	if !path.IsAbs(p) {
		return "", fmt.Errorf("向こうの場所は絶対パスで指す: %s", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("パスに .. を使わない: %s", p)
		}
	}
	c := path.Clean(p)
	if c == "/" {
		return "", fmt.Errorf("/ そのものは許さない（広すぎる）")
	}
	return c, nil
}

// validAgentPath は台帳に書く、向こうのエージェントの実体の場所を確かめる。空は「向こうで探す」。
//
// **`$` を通さない。** systemd-run は引数の `$VAR` を展開する（systemd 254 以降。
// 2026-09-11 に `$$` が `$` になるのを実測した）。
func validAgentPath(p string) error {
	if p == "" {
		return nil
	}
	c, err := cleanRemotePath(p)
	if err != nil {
		return err
	}
	if c != p {
		return fmt.Errorf("実体の場所は正規化した形で書く: %s", c)
	}
	if strings.ContainsAny(p, "$`\\\"'") {
		return fmt.Errorf("実体の場所に使えない文字がある: %s", p)
	}
	return nil
}

// shQuote は sh の単引用符で包む。**中身は一切解釈されない。**
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// remoteCommand は ssh に渡す1本の文字列を作る。
//
// 向こうのログインシェル（bash / zsh / sh）がこれを1回読み、`sh` に成り代わる。
// **スクリプトも引数も全部単引用符で包む**ので、パスに何が入っていても
// 向こうのシェルに解釈されない。
func remoteCommand(script string, args ...string) string {
	var b strings.Builder
	b.WriteString("exec sh -c ")
	b.WriteString(shQuote(script))
	b.WriteString(" camp")
	for _, a := range args {
		b.WriteString(" ")
		b.WriteString(shQuote(a))
	}
	return b.String()
}

// sshSafeOpts は Camp が起こす ssh に必ず付ける。**config より優先される**
// （ssh は最初に見た値を採り、コマンドラインが先に読まれる）。
//
// **転送は config のまま**（D-030、本人の決定 2026-09-12）。以前は `-a`・`-x`・ForwardAgent=no・
// ForwardX11=no・ClearAllForwardings=yes を必ず付けていたが、そうすると `ssh <host>` してから CLI を
// 起こすのと違い、向こうから手元の鍵を使えない（git push など）。答える人が居ないこと（BatchMode）と
// ホスト鍵の固定は、Camp が答えられない・行き先を固定するための縛りなので残す。
var sshSafeOpts = []string{
	"-T",
	// 答える人が居ない。パスワードも、ホスト鍵の確認も訊かせない。
	"-o", "BatchMode=yes",
	// **ホスト鍵を Camp が受け入れない。** 知らない鍵・変わった鍵では繋がない。
	// 画面の1クリックで鍵を信じられると、行き先の固定が意味を失う。
	"-o", "StrictHostKeyChecking=yes",
	// known_hosts に書き足させない（D-026: ~/.ssh へ書かない）。
	"-o", "UpdateHostKeys=no",
	// 鍵を外のコマンドに答えさせない。信じる鍵は known_hosts にあるものだけにして、
	// それを固定する（Resolved.HostKeys）。
	"-o", "KnownHostsCommand=none",
	// 相乗りしない。他の接続を巻き込んで止めたり、止め損ねたりしない。
	"-o", "ControlMaster=no", "-o", "ControlPath=none",
	// 繋いだときに手元でコマンドを走らせない。tty も要らない（転送は config のまま）。
	"-o", "PermitLocalCommand=no", "-o", "RequestTTY=no",
	// 黙って切れた接続に1分で気づく。
	"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=4",
	"-o", "ConnectTimeout=20",
	"-o", "LogLevel=ERROR",
}

// procFns は両方のスクリプトが使う、/proc を読む関数。
//
// **`set -f` を掛けない**——/proc/[0-9]* を展開する。stat の欄（comm を
// 除いたもの）は数と状態の1文字だけなので、分割しても展開は起きない。
// 読むのは組み込みの read で、プロセスごとに cat を起こさない。
//
// **statof は state・ppid・sess・start を上書きする**（sh の関数に局所変数は無い）。marked は
// me・smark・sself・unread を、stopall は left・alive・was を使う。スクリプトの側でこれらの名前を
// 使わない（2026-09-12、しるしを sess と名付けて取り逃がした）。
//
// hasmark は「同じユーザーで、このセッションのしるしを持つ」プロセスか。**uid を読める側でも見る**
// ——root で入ると、しるしを持つ別のユーザーのプロセスまで拾ってしまう（codex exec のレビュー）。
// **pid の使い回しは残る穴**: 走査から撃つまでの間にしるし持ちが終わり、同じ番号が別人に渡ると、
// その別人へ TERM が飛ぶ（窓は数秒。pid_max が小さいホストでは起こりうる）。
//
// members は、セッション番号が $1 の者と、そこから親を辿れる者を集める（$2 は除く）。
// **セッション番号で数えてよいのは、番号を使っている者が居る間はカーネルが
// その pid を使い回さないから。** 子が死んでいても、孫がその番号を持っている
// 限り、同じ番号の別人は生まれない。
const procFns = `statof() {
  { IFS= read -r s < "/proc/$1/stat"; } 2>/dev/null || return 1
  s=${s##*") "}
  set -- $s
  state=$1 ppid=$2 sess=$4 start=${20:-}
}
members() {
  list=' '
  for f in /proc/[0-9]*; do
    p=${f#/proc/}
    [ "$p" = "$2" ] && continue
    statof "$p" || continue
    [ "$sess" = "$1" ] && list="$list$p "
  done
  round=0
  while [ $round -lt 8 ]; do
    grew=0
    for f in /proc/[0-9]*; do
      p=${f#/proc/}
      [ "$p" = "$2" ] && continue
      case "$list" in *" $p "*) continue ;; esac
      statof "$p" || continue
      case " $1$list" in *" $ppid "*) list="$list$p "; grew=1 ;; esac
    done
    [ $grew = 0 ] && break
    round=$((round+1))
  done
  n=0
  for p in $list; do n=$((n+1)); done
}
hasmark() {
  [ -r "/proc/$1/environ" ] || return 1
  hu=
  { while read -r k v rest; do [ "$k" = Uid: ] && { hu=$v; break; }; done < "/proc/$1/status"; } 2>/dev/null
  [ -n "$me" ] && [ "$hu" = "$me" ] || return 1
  tr '\0' '\n' < "/proc/$1/environ" 2>/dev/null | grep -qx "CAMP_SESSION=$2"
}
marked() {
  me=$(id -u 2>/dev/null) || me=
  smark=$1 sself=$2
  unread=0
  for f in /proc/[0-9]*; do
    p=${f#/proc/}
    [ "$p" = "$2" ] && continue
    case "$list" in *" $p "*) continue ;; esac
    if [ -r "$f/environ" ]; then
      hasmark "$p" "$1" && list="$list$p "
    else
      uid=
      { while read -r k v rest; do [ "$k" = Uid: ] && { uid=$v; break; }; done < "$f/status"; } 2>/dev/null
      [ -n "$me" ] && [ "$uid" = "$me" ] && unread=$((unread+1))
    fi
  done
  n=0
  for p in $list; do n=$((n+1)); done
}
stopall() {
  left=0
  [ "$n" = 0 ] && return 0
  kill -TERM $list 2>/dev/null
  i=0
  while [ $i -lt 10 ]; do
    alive=
    for p in $list; do
      if statof "$p" && [ "$state" != Z ]; then alive="$alive $p"; fi
    done
    [ -z "$alive" ] && break
    sleep 0.3 2>/dev/null || sleep 1
    i=$((i+1))
  done
  # **止めている間に生まれたしるし持ちを拾う。** 最初の走査は1回きりなので、
  # TERM を受けた子がその後に起こしたものが残る。
  if [ -n "${smark:-}" ]; then
    was=$n
    marked "$smark" "$sself"
    if [ "$n" != "$was" ]; then
      kill -TERM $list 2>/dev/null
      sleep 0.3 2>/dev/null || sleep 1
    fi
  fi
  alive=
  for p in $list; do
    if statof "$p" && [ "$state" != Z ]; then alive="$alive $p"; fi
  done
  [ -n "$alive" ] && kill -KILL $alive 2>/dev/null
  # **撃ったあとに数え直す。** 「止めた」と言い切らない。
  sleep 0.2 2>/dev/null || true
  for p in $list; do
    if statof "$p" && [ "$state" != Z ]; then left=$((left+1)); fi
  done
  [ "$left" = 0 ] && return 0
  return 1
}
`

// wrapperScript は向こうで走る sh。**確かめてからエージェントを起こし、親として残る。**
//
// 引数: 場所、許した行、探す名前、実体（- なら探す）、置き場の環境変数名と既定の相対パス（- なら
// 照らさない）、scope 名（- なら使わない）、合言葉、しるし（セッション id）。残りはエージェントの
// 引数（駆動器の Argv）。**この sh にエージェントの名前を書かない**（D-031）。
//
// **しるし**: エージェントを `CAMP_SESSION=<セッション id>` の環境で起こす。エージェントが別の
// セッションへ逃がしたもの（Codex のコマンド）は、エージェントが異常終了すると親を辿れなくなる
// （2026-09-12、`rp` で実測）。終わりの後始末と reapScript は、同じユーザーのプロセスのうち
// このしるしを持つものも止める。切り離して起こしたもの（nohup のサーバなど）も止まる（本人の決定。
// 手元の scope と同じ）。残したいものは `env -u CAMP_SESSION` で起こす。
//
// 1行目に必ず `CAMP-REMOTE` か `CAMP-ERR` を出す。実行面はそれを読むまで
// フレームとして扱わない（ログインシェルが何か吐いても混ざらない）。
// 名乗りには実行面が起動ごとに作った合言葉を入れる。**合言葉の合わない名乗りは
// 採らない**——採ると、あとで始末するときに名乗られた別の pid を撃つ。
// ただし向こうのアカウントそのものが悪意を持てば、自分のコマンド行から
// 合言葉を読める。防ぐのは取り違えで、偽造ではない。
//
// **/proc の無いホストでは起こさない。** 接続が切れたあとに向こうを確かめる手段が
// 無く、確かめられないものを「終わった」とも「走っている」とも書けない。
//
// stdin は fd 3 に写してから子へ渡す。非対話の sh は、裏で起こした子の stdin を
// /dev/null にする（POSIX）。明示の付け替えはその後に効く。
const wrapperScript = procFns + `cwd=$1 root=$2 name=$3 bin=$4 henv=$5 hdef=$6 unit=$7 nonce=$8 mark=$9
shift 9
nl='
'
tab=$(printf '\t')
bad() { printf 'CAMP-ERR\t%s\t%s\n' "$2" "$1"; exit "$2"; }
[ -r /proc/self/stat ] || bad '/proc が無いホストでは起こさない（切れたあとに向こうを確かめられない）' 95
case "$cwd$root$bin$hdef" in *"$nl"*|*"$tab"*) bad 'パスに改行かタブがある' 90 ;; esac
case "$name" in ''|*[!a-z0-9_-]*) bad "探す名前として使えない: $name" 90 ;; esac
case "$henv" in -) ;; ''|[0-9]*|*[!A-Z0-9_]*) bad "置き場の環境変数名として使えない: $henv" 90 ;; esac
case "$mark" in ''|*[!0-9a-f]*) bad 'しるしとして使えない' 90 ;; esac
rootreal=$(cd -- "$root" 2>/dev/null && pwd -P) || bad "許した場所が向こうに無い: $root" 91
[ "$rootreal" = / ] && bad "許した場所を辿ると / になる: $root" 91
cd -- "$cwd" 2>/dev/null || bad "場所を辿れない: $cwd" 92
real=$(pwd -P)
case "$real/" in "$rootreal"/*) ;; *) bad "許した場所の外: $real" 93 ;; esac
case "$real$rootreal" in *"$nl"*|*"$tab"*) bad '実パスに改行かタブがある' 90 ;; esac
if [ "$bin" = - ]; then
  bin=$(command -v "$name" 2>/dev/null) || bin=
  case "$bin" in /*) ;; *) bin= ;; esac
  if [ -z "$bin" ]; then
    for p in "$HOME/.local/bin/$name" "/opt/homebrew/bin/$name" "/usr/local/bin/$name"; do
      if [ -x "$p" ]; then bin=$p; break; fi
    done
  fi
  [ -n "$bin" ] || bad "$name が見つからない（台帳で場所を指せる）" 94
fi
[ -x "$bin" ] || bad "$name を実行できない: $bin" 94
PATH=${bin%/*}:$PATH
export PATH
home=- homereal=-
if [ "$henv" != - ]; then
  eval "home=\${$henv:-}"
  [ -n "$home" ] || home=$HOME/$hdef
  homereal=$(cd -- "$home" 2>/dev/null && pwd -P) || homereal=-
  case "$home$homereal" in *"$nl"*|*"$tab"*) bad '置き場のパスに改行かタブがある' 90 ;; esac
fi
boot=$(cat /proc/sys/kernel/random/boot_id 2>/dev/null) || boot=
st=
statof $$ && st=$start
[ -n "$st" ] || bad '自分の起動時刻が読めない（止めるときに照らせない）' 95
if [ "$unit" != - ]; then
  if command -v systemd-run >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1; then :; else unit=-; fi
fi
printf 'CAMP-REMOTE\t3\t%s\t%s\t%s\t%s\t%s\t%s\t%s\thome=%s\thomereal=%s\n' "$nonce" "$$" "$st" "${boot:--}" "$unit" "$rootreal" "$real" "$home" "$homereal"
CAMP_SESSION=$mark
export CAMP_SESSION
exec 3<&0
if [ "$unit" != - ]; then
  systemd-run --user --scope --quiet --collect --unit "$unit" -- "$bin" "$@" 0<&3 3<&- &
else
  "$bin" "$@" 0<&3 3<&- &
fi
kid=$!
exec 0</dev/null 3<&-
wait "$kid"
rc=$?
trap '' TERM HUP
unset CAMP_SESSION
members $$ $$
marked "$mark" $$
stopall
exit $rc
`

// recListScript は向こうの記録を**数えて返す**（M47）。**向こうへは書かない。**
//
// 引数: 置き場の環境変数名（- なら見ない）、無いときの $HOME からの相対パス、
// その下の記録の置き場、窓の大きさ。標準入力に `パス<TAB>位置` を並べると、
// その位置の直前の窓も返す（同じ実体かを見るため）。
//
// **ハッシュは向こうで計算させない。** `sha256sum` があるとは限らず、あっても
// 手元と同じ計算だと保証できない。窓の中身そのものを運び、手元で同じ関数にかける
// （256 バイト × 40 本で 14KB 程度）。
//
// 1行目に必ず `CAMP-REC` か `CAMP-ERR` を出す（wrapperScript と同じ約束）。
// 読むのは `*.jsonl` だけ・置き場の下だけ・通常ファイルだけ。symlink は辿らない。
const recListScript = `henv=$1 hdef=$2 sub=$3 win=$4
nl='
'
tab=$(printf '\t')
bad() { printf 'CAMP-ERR\t%s\t%s\n' "$2" "$1"; exit "$2"; }
case "$henv" in -) ;; ''|[0-9]*|*[!A-Z0-9_]*) bad "置き場の環境変数名として使えない: $henv" 90 ;; esac
case "$hdef$sub" in ''|*"$nl"*|*"$tab"*) bad '置き場の指定に改行かタブがある' 90 ;; esac
case "$win" in ''|*[!0-9]*) bad '窓の大きさが数でない' 90 ;; esac
home=
if [ "$henv" != - ]; then eval "home=\${$henv:-}"; fi
[ -n "$home" ] || home=$HOME/$hdef
root=$home/$sub
rootreal=$(cd -- "$root" 2>/dev/null && pwd -P) || bad "記録の置き場が向こうに無い: $root" 91
[ "$rootreal" = / ] && bad '記録の置き場を辿ると / になる' 91
case "$rootreal" in *"$nl"*|*"$tab"*) bad '実パスに改行かタブがある' 90 ;; esac
printf 'CAMP-REC\t1\t%s\n' "$rootreal"
if find "$rootreal" -maxdepth 0 -printf '' 2>/dev/null; then
  find "$rootreal" -type f -name '*.jsonl' -printf 'F\t%s\t%Ts\t%p\n'
elif stat -c '%s' -- "$rootreal" >/dev/null 2>&1; then
  find "$rootreal" -type f -name '*.jsonl' -print | while IFS= read -r p; do
    case "$p" in *"$tab"*) continue ;; esac
    set -- $(stat -c '%s %Y' -- "$p" 2>/dev/null) || continue
    [ -n "$2" ] || continue
    printf 'F\t%s\t%s\t%s\n' "$1" "$2" "$p"
  done
else
  bad '向こうの find も stat も大きさを出せない' 96
fi
while IFS="$tab" read -r p off; do
  case "$p" in "$rootreal"/*) ;; *) continue ;; esac
  case "$p" in *'/../'*|*'/..') continue ;; esac
  case "$p" in *.jsonl) ;; *) continue ;; esac
  [ -f "$p" ] || continue
  case "$off" in ''|*[!0-9]*) continue ;; esac
  [ "$off" -gt 0 ] || continue
  st=$((off - win))
  [ "$st" -lt 0 ] && st=0
  n=$((off - st))
  printf 'W\t%s\t' "$p"
  tail -c +$((st + 1)) -- "$p" 2>/dev/null | head -c "$n" | base64 | tr -d '\n'
  printf '\n'
done
`

// recReadScript は頼まれた範囲だけ返す（M47）。**向こうへは書かない。**
//
// 引数は recListScript と同じ（置き場の決め方）＋1回の合計の蓋。標準入力に
// `パス<TAB>位置<TAB>最大バイト数` を並べる。**まとめて頼むのは、1本ずつ
// 繋ぎ直さないため。** 蓋で切ったら最後に `CAP` を出す。
const recReadScript = `henv=$1 hdef=$2 sub=$3 cap=$4
nl='
'
tab=$(printf '\t')
bad() { printf 'CAMP-ERR\t%s\t%s\n' "$2" "$1"; exit "$2"; }
case "$henv" in -) ;; ''|[0-9]*|*[!A-Z0-9_]*) bad "置き場の環境変数名として使えない: $henv" 90 ;; esac
case "$hdef$sub" in ''|*"$nl"*|*"$tab"*) bad '置き場の指定に改行かタブがある' 90 ;; esac
case "$cap" in ''|*[!0-9]*) bad '蓋が数でない' 90 ;; esac
home=
if [ "$henv" != - ]; then eval "home=\${$henv:-}"; fi
[ -n "$home" ] || home=$HOME/$hdef
root=$home/$sub
rootreal=$(cd -- "$root" 2>/dev/null && pwd -P) || bad "記録の置き場が向こうに無い: $root" 91
[ "$rootreal" = / ] && bad '記録の置き場を辿ると / になる' 91
printf 'CAMP-REC\t1\t%s\n' "$rootreal"
total=0
while IFS="$tab" read -r p off n; do
  case "$p" in "$rootreal"/*) ;; *) continue ;; esac
  case "$p" in *'/../'*|*'/..') continue ;; esac
  case "$p" in *.jsonl) ;; *) continue ;; esac
  [ -f "$p" ] || continue
  case "$off$n" in ''|*[!0-9]*) continue ;; esac
  [ "$n" -gt 0 ] || continue
  if [ "$total" -ge "$cap" ]; then printf 'CAP\n'; break; fi
  rem=$((cap - total))
  [ "$n" -gt "$rem" ] && n=$rem
  printf 'D\t%s\t%s\t' "$p" "$off"
  tail -c +$((off + 1)) -- "$p" 2>/dev/null | head -c "$n" | base64 | tr -d '\n'
  printf '\n'
  total=$((total + n))
done
`

// reapScript は向こうの残りを始末する。
//
// 引数: pid（向こうの sh）、起動時刻、boot_id、scope 名（どれも - なら無い）。
//
// **pid をそのまま撃たない。** 起動時刻が違えば別人で、何もしない。
// sh が居なくなっていても孫が残っていることがある（stdin が閉じると子は
// 終わるが孫は残る。実測）ので、セッション番号が同じもの（sshd が setsid
// しているので、それが sh の pid）と、そこから親を辿れるものを全部止める。
//
// **確かめ終えるまで、scope にも触らない。** boot_id と起動時刻を照らしてから止める。
// 以前は scope を先に止めていた（2026-09-11 の outer gate で codex が指摘。
// 名前はセッションごとに乱数なので実害は無かったが、「照らしてから撃つ」と逆だった）。
const reapScript = procFns + `pid=$1 st=$2 boot=$3 unit=$4 mark=$5
out() { printf 'CAMP-REAP\t%s\t%s\n' "$1" "$2"; exit 0; }
[ -r /proc/self/stat ] || out unsupported '/proc が無い'
case "$mark" in ''|*[!0-9a-f]*) mark=- ;; esac
now=$(cat /proc/sys/kernel/random/boot_id 2>/dev/null) || now=
if [ "$boot" != - ] && [ -n "$now" ] && [ "$now" != "$boot" ]; then out gone '再起動を跨いだ'; fi
same=1
if statof "$pid"; then
  [ "$st" = - ] && out unsupported '起動時刻を控えていない'
  [ "$start" != "$st" ] && same=0
fi
stopped=0
if [ $same = 1 ] && [ "$unit" != - ] && command -v systemctl >/dev/null 2>&1; then
  systemctl --user stop "$unit" >/dev/null 2>&1 && stopped=1
fi
list=' ' n=0 unread=0
[ $same = 1 ] && members "$pid" $$
[ "$mark" != - ] && marked "$mark" $$
note=
[ "$unread" -gt 0 ] && note="（environ を読めない同じユーザーのプロセスが $unread）"
if [ $n = 0 ]; then
  [ $stopped = 1 ] && out killed "scope$note"
  [ $same = 0 ] && out gone "pid が使い回されている$note"
  out gone "残っていなかった$note"
fi
stopall
[ "${left:-0}" -gt 0 ] && note="$note（撃っても $left 残った）"
out killed "$n$note"
`

// parseHeader は向こうの sh の1行目を読む（版 3）。**合言葉と起動時刻の無い名乗りは採らない。**
// 9欄のあとは `key=value` を足せる（版を上げずに名乗りを増やすため。Fable の M42 レビュー）。
func parseHeader(line, host, nonce string) (RemoteOwner, bool) {
	f := strings.Split(line, "\t")
	if len(f) < 9 || f[0] != "CAMP-REMOTE" || f[1] != "3" || nonce == "" || f[2] != nonce {
		return RemoteOwner{}, false
	}
	extra := map[string]string{}
	for _, kv := range f[9:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return RemoteOwner{}, false
		}
		if v != "-" {
			extra[k] = v
		}
	}
	pid, err := strconv.Atoi(f[3])
	if err != nil || pid <= 0 {
		return RemoteOwner{}, false
	}
	st, err := strconv.ParseUint(f[4], 10, 64)
	if err != nil || st == 0 {
		return RemoteOwner{}, false
	}
	o := RemoteOwner{Host: host, PID: pid, Started: st, Root: f[7], Cwd: f[8],
		Home: extra["home"], HomeReal: extra["homereal"]}
	if f[5] != "-" {
		o.BootID = f[5]
	}
	if f[6] != "-" {
		o.Scope = f[6]
	}
	return o, true
}

// parseRemoteErr は `CAMP-ERR\t<code>\t<文>` を読む。
func parseRemoteErr(line string) (string, bool) {
	f := strings.SplitN(line, "\t", 3)
	if len(f) != 3 || f[0] != "CAMP-ERR" {
		return "", false
	}
	return f[2], true
}

// parseReap は reapScript の最後の行を読む。
func parseReap(out string) (result, detail string) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		f := strings.SplitN(lines[i], "\t", 3)
		if len(f) == 3 && f[0] == "CAMP-REAP" {
			switch f[1] {
			case RemoteGone, RemoteKilled, RemoteUnsupported:
				return f[1], f[2]
			}
		}
	}
	return RemoteUnreachable, ""
}

// explainSSHFailure は、向こうの sh が名乗る前に ssh が終わった理由を人の言葉にする。
func explainSSHFailure(alias, stderr string) string {
	last := lastLine(stderr)
	switch {
	case strings.Contains(stderr, "REMOTE HOST IDENTIFICATION HAS CHANGED"):
		return fmt.Sprintf("%s のホスト鍵が変わっている。Camp は鍵を受け入れない。端末で確かめてから known_hosts を直す", alias)
	case strings.Contains(stderr, "Host key verification failed"),
		strings.Contains(stderr, "host key is known"):
		return fmt.Sprintf("%s のホスト鍵をまだ確かめていない（known_hosts に無い）。Camp は鍵を受け入れないので、端末で一度 ssh %s して確かめる", alias, alias)
	case strings.Contains(stderr, "Permission denied"):
		return fmt.Sprintf("%s に鍵で入れない（Permission denied）", alias)
	case strings.Contains(stderr, "Could not resolve hostname"):
		return fmt.Sprintf("%s の名前が引けない", alias)
	case strings.Contains(stderr, "timed out"):
		return fmt.Sprintf("%s に繋がらない（時間切れ）", alias)
	case strings.Contains(stderr, "Connection refused"):
		return fmt.Sprintf("%s に繋がらない（断られた）", alias)
	case strings.Contains(stderr, "No route to host"):
		return fmt.Sprintf("%s への経路が無い", alias)
	case strings.Contains(stderr, "ParserError"), strings.Contains(stderr, "is not recognized"):
		return fmt.Sprintf("%s のシェルが sh と互換でない（Windows など）。起こせない", alias)
	case last != "":
		return fmt.Sprintf("%s で起こせなかった: %s", alias, last)
	}
	return fmt.Sprintf("%s で起こせなかった（向こうが何も名乗らないまま終わった）", alias)
}

// connectionLost は、名乗ったあとで ssh が 255 で終わったとき、それが
// 接続の喪失かどうかを stderr から見る。**255 だけでは決めない**——
// 向こうの sh がシグナルで死んでも 255 になる（実測）。
func connectionLost(stderr string) bool {
	for _, s := range []string{
		"closed by remote host", "not responding", "Broken pipe",
		"Connection reset", "client_loop", "Connection closed", "Connection timed out",
	} {
		if strings.Contains(stderr, s) {
			return true
		}
	}
	return false
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// tailBuf は stderr の最後の数 KB だけを覚える。**理由を画面に出すため。**
type tailBuf struct {
	mu  sync.Mutex
	b   []byte
	max int
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if over := len(t.b) - t.max; over > 0 {
		t.b = append([]byte(nil), t.b[over:]...)
	}
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
