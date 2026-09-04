#!/bin/bash
# M25.5 の inner gate。境界が効いていること、移設で何も失われていないことを見る。
#   sudo bash deploy/verify-boundary.sh
set -uo pipefail
DB=/var/lib/camp/camp.sqlite
C="/usr/local/bin/campd"
HUMAN="${SUDO_USER:-mooma-0750}"
ng=0
ok()  { echo "  ok    $*"; }
bad() { echo "  NG    $*"; ng=$((ng+1)); }

echo "== 1. 境界（$HUMAN から届くか）=="
sudo -u "$HUMAN" cat $DB >/dev/null 2>&1 && bad "DBが読めてしまう" || ok "DBに届かない"
sudo -u "$HUMAN" test -w $C && bad "実体を差し替えられる" || ok "実体を差し替えられない"
# **ファイルだけ見ても足りない。** 親ディレクトリが書けるなら rename で差し替えられる。
for d in /usr/local/bin /usr/local /usr; do
	sudo -u "$HUMAN" test -w "$d" && bad "$d が書けるので実体を rename で差し替えられる" \
		|| ok "$d は書けない"
done
loginctl show-user "$HUMAN" -p Linger 2>/dev/null | grep -q "Linger=yes" \
	&& ok "linger 有効（ログアウト中も ACL を配り直す）" \
	|| bad "linger が無効。ログアウト中に ACL の配り直しが止まる"
# sqlite3 で開こうとする検査は置かない。**通る理由が権限とは限らない**
# （このDBは fts5 の仮想表を持つので、fts5 の無いビルドでは権限に関係なく落ちる）。
# 代わりに、権限だけを見る。
sudo -u "$HUMAN" test -r $DB && bad "DBに読み権限がある" || ok "DBに読み権限が無い"
sudo -u "$HUMAN" test -x /var/lib/camp && bad "置き場を辿れる" || ok "置き場を辿れない"

echo "== 2. 移設で失われていないか（移設前の実測値と比べる）=="
# **sqlite3 の CLI は使わない。** このDBは fts5 の仮想表を持っており、
# fts5 を含まないビルドの sqlite3 はスキーマ解析の時点で落ちる（実測）。
# campd は自前のドライバなので開ける。
COUNTS=$(sudo -u camp $C doctor -db $DB -v 2>/dev/null)
num() { echo "$COUNTS" | awk -v k="$1" '$1==k {print $2}' | head -1; }
for row in "messages 38578" "message_blocks 18951" "tombstones 3691" "notes 4186" "sessions 62"; do
	set -- $row
	got=$(num "$1")
	if [ -z "$got" ]; then
		bad "$1 を数えられない"
	elif [ "$got" -ge "$2" ]; then
		ok "$1 が減っていない（$got ≧ 移設前 $2）"
	else
		bad "$1 が $got。移設前は $2"
	fi
done

echo "== 3. camp が会話記録を読めるか（ACLの本番確認）=="
# 取り込みが実際に進んだかで見る。読めていなければ messages は増えない。
sup=$(sudo -u camp $C doctor -db $DB 2>/dev/null | grep -c "旧世代")
[ "${sup:-0}" -eq 0 ] && ok "世代交代の警告なし（二重取り込みなし）" || bad "世代交代が起きている"
one=$(sudo -u camp $C files 2>/dev/null | head -1)
vault=$(getent passwd "$HUMAN" | cut -d: -f6)/Documents/git-cloned/Obsidian-Vault
sudo -u camp test -r "$vault/AGENTS.md" && ok "Vault を読める" || bad "Vault を読めない"
# 生の JSONL を1本、camp として開いてみる。
j=$(sudo -u "$HUMAN" sh -c 'ls -1 ~/.claude/projects/*/*.jsonl 2>/dev/null | head -1')
if [ -n "$j" ]; then
	sudo -u camp head -c 1 "$j" >/dev/null 2>&1 		&& ok "JSONL を読める: $(basename "$j")" || bad "JSONL を読めない: $j"
else
	bad "JSONL が見つからない"
fi

echo "== 4. サービスと取り込み =="
for u in camp.service camp-ingest.timer; do
  systemctl is-active --quiet $u && ok "$u" || bad "$u が動いていない"
done
echo "     直近の取り込み:"
journalctl -u camp-ingest.service -n 6 --no-pager -o cat 2>/dev/null | sed 's/^/       /'

echo "== 5. doctor =="
sudo -u camp $C doctor -db $DB 2>&1 | grep -E "境界|NG|^ng" | sed 's/^/     /'
sudo -u camp $C doctor -db $DB 2>&1 | grep -c "^ok" | sed 's/^/     ok の数: /'
sudo -u camp $C doctor -db $DB 2>&1 | grep -v "^ok" | grep -v "^ *$" | sed 's/^/     落ちた: /'

echo
[ $ng -eq 0 ] && echo "全部通った。" || echo "$ng 件おかしい。"
echo "（報告口 campd report は再ログイン後に別途確認する）"
