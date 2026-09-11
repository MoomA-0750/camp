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

// RemoteSpec は campd が実行面へ渡す「どこへ繋ぐか」。
type RemoteSpec struct {
	Alias string `json:"alias"`
	// Pin は許したときの行き先。実行面は `ssh -G` と照らし、違えば起こさない。
	Pin Resolved `json:"pin"`
	// Claude は向こうの `claude` の実体。空なら向こうで探す。
	Claude string `json:"claude,omitempty"`
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
}

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

// validClaudePath は台帳に書く `claude` の場所を確かめる。空は「向こうで探す」。
//
// **`$` を通さない。** systemd-run は引数の `$VAR` を展開する（systemd 254 以降。
// 2026-09-11 に `$$` が `$` になるのを実測した）。
func validClaudePath(p string) error {
	if p == "" {
		return nil
	}
	c, err := cleanRemotePath(p)
	if err != nil {
		return err
	}
	if c != p {
		return fmt.Errorf("claude の場所は正規化した形で書く: %s", c)
	}
	if strings.ContainsAny(p, "$`\\\"'") {
		return fmt.Errorf("claude の場所に使えない文字がある: %s", p)
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
var sshSafeOpts = []string{
	"-T", "-a", "-x",
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
	// 鍵も画面も向こうへ渡さない。
	"-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ClearAllForwardings=yes",
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
stopall() {
  [ "$n" = 0 ] && return 0
  kill -TERM $list 2>/dev/null
  i=0
  while [ $i -lt 10 ]; do
    alive=
    for p in $list; do
      if statof "$p" && [ "$state" != Z ]; then alive="$alive $p"; fi
    done
    [ -z "$alive" ] && return 0
    sleep 0.3 2>/dev/null || sleep 1
    i=$((i+1))
  done
  kill -KILL $alive 2>/dev/null
}
`

// wrapperScript は向こうで走る sh。**確かめてから `claude` を起こし、親として残る。**
//
// 引数: 場所、許した行、claude の実体（- なら探す）、scope 名（- なら使わない）、合言葉。
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
const wrapperScript = procFns + `cwd=$1 root=$2 claude=$3 unit=$4 nonce=$5
nl='
'
tab=$(printf '\t')
bad() { printf 'CAMP-ERR\t%s\t%s\n' "$2" "$1"; exit "$2"; }
[ -r /proc/self/stat ] || bad '/proc が無いホストでは起こさない（切れたあとに向こうを確かめられない）' 95
case "$cwd$root$claude" in *"$nl"*|*"$tab"*) bad 'パスに改行かタブがある' 90 ;; esac
rootreal=$(cd -- "$root" 2>/dev/null && pwd -P) || bad "許した場所が向こうに無い: $root" 91
[ "$rootreal" = / ] && bad "許した場所を辿ると / になる: $root" 91
cd -- "$cwd" 2>/dev/null || bad "場所を辿れない: $cwd" 92
real=$(pwd -P)
case "$real/" in "$rootreal"/*) ;; *) bad "許した場所の外: $real" 93 ;; esac
case "$real$rootreal" in *"$nl"*|*"$tab"*) bad '実パスに改行かタブがある' 90 ;; esac
if [ "$claude" = - ]; then
  claude=$(command -v claude 2>/dev/null) || claude=
  case "$claude" in /*) ;; *) claude= ;; esac
  if [ -z "$claude" ]; then
    for p in "$HOME/.local/bin/claude" /opt/homebrew/bin/claude /usr/local/bin/claude; do
      if [ -x "$p" ]; then claude=$p; break; fi
    done
  fi
  [ -n "$claude" ] || bad 'claude が見つからない（台帳で場所を指せる）' 94
fi
[ -x "$claude" ] || bad "claude を実行できない: $claude" 94
PATH=${claude%/*}:$PATH
export PATH
boot=$(cat /proc/sys/kernel/random/boot_id 2>/dev/null) || boot=
st=
statof $$ && st=$start
[ -n "$st" ] || bad '自分の起動時刻が読めない（止めるときに照らせない）' 95
if [ "$unit" != - ]; then
  if command -v systemd-run >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1; then :; else unit=-; fi
fi
printf 'CAMP-REMOTE\t2\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$nonce" "$$" "$st" "${boot:--}" "$unit" "$rootreal" "$real"
set -- -p --input-format stream-json --output-format stream-json --verbose --permission-prompt-tool stdio
exec 3<&0
if [ "$unit" != - ]; then
  systemd-run --user --scope --quiet --collect --unit "$unit" -- "$claude" "$@" 0<&3 3<&- &
else
  "$claude" "$@" 0<&3 3<&- &
fi
kid=$!
exec 0</dev/null 3<&-
wait "$kid"
rc=$?
trap '' TERM HUP
members $$ $$
stopall
exit $rc
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
const reapScript = procFns + `pid=$1 st=$2 boot=$3 unit=$4
out() { printf 'CAMP-REAP\t%s\t%s\n' "$1" "$2"; exit 0; }
[ -r /proc/self/stat ] || out unsupported '/proc が無い'
now=$(cat /proc/sys/kernel/random/boot_id 2>/dev/null) || now=
if [ "$boot" != - ] && [ -n "$now" ] && [ "$now" != "$boot" ]; then out gone '再起動を跨いだ'; fi
if statof "$pid"; then
  [ "$st" = - ] && out unsupported '起動時刻を控えていない'
  [ "$start" != "$st" ] && out gone 'pid が使い回されている'
fi
stopped=0
if [ "$unit" != - ] && command -v systemctl >/dev/null 2>&1; then
  systemctl --user stop "$unit" >/dev/null 2>&1 && stopped=1
fi
members "$pid" $$
if [ $n = 0 ]; then
  [ $stopped = 1 ] && out killed scope
  out gone '残っていなかった'
fi
stopall
out killed "$n"
`

// parseHeader は向こうの sh の1行目を読む。**合言葉と起動時刻の無い名乗りは採らない。**
func parseHeader(line, host, nonce string) (RemoteOwner, bool) {
	f := strings.Split(line, "\t")
	if len(f) != 9 || f[0] != "CAMP-REMOTE" || f[1] != "2" || nonce == "" || f[2] != nonce {
		return RemoteOwner{}, false
	}
	pid, err := strconv.Atoi(f[3])
	if err != nil || pid <= 0 {
		return RemoteOwner{}, false
	}
	st, err := strconv.ParseUint(f[4], 10, 64)
	if err != nil || st == 0 {
		return RemoteOwner{}, false
	}
	o := RemoteOwner{Host: host, PID: pid, Started: st, Root: f[7], Cwd: f[8]}
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
