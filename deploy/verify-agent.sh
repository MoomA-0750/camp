#!/bin/bash
# M26 の inner gate。**実行面を分けたことが、実際に効いているか。**
#
#   bash deploy/verify-agent.sh        （本人のユーザーで走らせる。sudo は要らない）
#
# 見るのは3つ。実行面が動いていること、制御口が正しく絞られていること、そして
# **実行面が DB を1つも開いていないこと**。3つ目がこの分割の全部。
set -uo pipefail
DB=/var/lib/camp/camp.sqlite
SOCK=/run/camp/agent.sock
C=/usr/local/bin/campd
ng=0
ok()  { echo "  ok    $*"; }
bad() { echo "  NG    $*"; ng=$((ng+1)); }

echo "== 1. 実行面が動いているか =="
if systemctl --user is-active --quiet camp-agent.service; then
	ok "camp-agent.service（user unit）"
else
	bad "camp-agent.service が動いていない"
fi
systemctl is-active --quiet camp.service && ok "camp.service" || bad "camp.service が動いていない"

echo "== 2. 制御口 =="
if [ -S "$SOCK" ]; then
	ok "制御口がある: $SOCK"
	perm=$(stat -c '%a %U:%G' "$SOCK")
	case "$perm" in
		"660 camp:campreport") ok "$perm" ;;
		*) bad "権限が違う: $perm（660 camp:campreport のはず）" ;;
	esac
else
	bad "制御口が無い: $SOCK"
fi

echo "== 3. 実行面は DB を開いていないか（この分割の全部）=="
pid=$(systemctl --user show camp-agent.service -p MainPID --value 2>/dev/null)
if [ -z "$pid" ] || [ "$pid" = "0" ]; then
	bad "実行面の pid が分からない"
else
	ok "実行面 pid=$pid"
	# **開いているファイルを全部見る。** 「DB を開く口が無い」は設計の主張なので、
	# 実際に開いていないことを毎回測る。
	opened=$(ls -l /proc/"$pid"/fd 2>/dev/null | grep -c "camp.sqlite")
	[ "${opened:-1}" -eq 0 ] && ok "camp.sqlite を1つも開いていない" \
		|| bad "camp.sqlite を $opened 個開いている"
	# そもそも届かないことも見る（境界が効いていれば読めない）。
	test -r "$DB" && bad "DBに読み権限がある" || ok "DBに読み権限が無い"
fi

echo "== 4. 台帳が読めるか（camp 側から）=="
if sudo -n -u camp $C runtime -db $DB -n 3 >/dev/null 2>&1; then
	ok "campd runtime が通る"
	sudo -n -u camp $C runtime -db $DB -n 3 2>/dev/null | sed 's/^/       /'
else
	echo "  --    台帳の確認は sudo が要る: sudo -u camp $C runtime -db $DB"
fi

echo "== 5. 実行面を騙れるか =="
# **騙れる。** 同じユーザーで動く以上、2つ目を繋ごうとすることは誰でもできる。
# 守れるのは「先に繋いだほうが本物」だけ。それが効いているかを見る。
out=$($C agent -sock "$SOCK" 2>&1 <<<"" | head -3)
if echo "$out" | grep -q "既に繋がっている"; then
	ok "2つ目の実行面は断られる（先に繋いだほうが本物）"
else
	bad "2つ目が断られていない: $out"
fi

echo
[ $ng -eq 0 ] && echo "全部通った。" || echo "$ng 件おかしい。"
echo "（**内容の偽造は防げない。** 実行面は人間と同じユーザーで動く。"
echo "  守れるのは、すでに書かれた監査ログが書き換わらないことまで）"
