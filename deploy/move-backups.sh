#!/bin/bash
# 残っているバックアップを境界の内側へ移す。root で1回。
#
# 境界は「他に平文の複製が無い」ことが前提になる。DBを camp のものにしても、
# 同じ中身がエージェントから読める場所に置いてあれば意味が薄い。
set -euo pipefail
HUMAN="${SUDO_USER:-mooma-0750}"
SRC="$(getent passwd "$HUMAN" | cut -d: -f6)/Documents/git-cloned/camp/data"
DST=/var/lib/camp/backups

[ "$(id -u)" -eq 0 ] || { echo "root で実行する" >&2; exit 1; }
install -d -o camp -g camp -m 0700 "$DST"

n=0
for f in "$SRC"/camp.sqlite.pre-*; do
	[ -e "$f" ] || continue
	base=$(basename "$f")
	install -o camp -g camp -m 0600 "$f" "$DST/$base"
	# 同じ中身が入ったことを確かめてから消す。
	if cmp -s "$f" "$DST/$base"; then
		rm -f "$f"
		echo "  移した  $base"
		n=$((n+1))
	else
		rm -f "$DST/$base"
		echo "  NG      $base（中身が一致しない。元は消していない）" >&2
	fi
done
echo "$n 本を $DST へ移した。"

echo
echo "== 確かめる =="
sudo -u "$HUMAN" ls "$DST" >/dev/null 2>&1 \
	&& echo "  NG    $HUMAN から中が見える" \
	|| echo "  ok    $HUMAN からは辿れない"
ls -la "$DST" | tail -n +2 | awk '{printf "  %s %s\n", $1, $9}'
left=$(sudo -u "$HUMAN" sh -c "ls -1 $SRC 2>/dev/null | wc -l")
echo "  境界の外に残っているファイル: $left"
