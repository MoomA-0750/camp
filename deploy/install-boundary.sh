#!/bin/bash
# M25.5 — 権限境界を引く。**root で1回だけ実行する。**
#
# 何をするか:
#   1. campd 専用のOSユーザー `camp` を作る（ログインできない）
#   2. `campreport` グループを作り、人間のユーザーを入れる（追記専用の口を使うため）
#   3. campd の実体を root のものとして /usr/local/bin へ置く
#   4. 会話記録と Vault に、camp への**読み取りだけ**の既定ACLを付ける
#   5. systemd の unit を入れる
#
# 何をしないか:
#   - DB の移設（`campd snapshot` を取ってから、この後で別に行う）
#   - `campd` への NOPASSWD sudoers 規則（**置かない。** 置いた瞬間、
#     エージェントも campd と同じ力を得る）
set -euo pipefail

HUMAN="${CAMP_HUMAN_USER:-mooma-0750}"
VAULT="${CAMP_VAULT_DIR:-/home/$HUMAN/Documents/git-cloned/Obsidian-Vault}"
REPO="${CAMP_REPO_DIR:-/home/$HUMAN/Documents/git-cloned/camp}"

if [ "$(id -u)" -ne 0 ]; then
	echo "root で実行する（sudo bash $0）" >&2
	exit 1
fi
if [ ! -x "$REPO/campd" ]; then
	echo "$REPO/campd が無い。先に make でビルドする" >&2
	exit 1
fi

echo "== 1. 専用ユーザー =="
if ! id camp >/dev/null 2>&1; then
	useradd --system --home-dir /var/lib/camp --create-home \
		--shell /usr/sbin/nologin --comment "Camp daemon" camp
	echo "  camp を作った"
else
	echo "  camp は既にある"
fi
install -d -o camp -g camp -m 0700 /var/lib/camp

echo "== 2. 報告用グループ =="
groupadd -f campreport
usermod -aG campreport "$HUMAN"
usermod -aG campreport camp
echo "  $HUMAN と camp を campreport に入れた（反映には再ログインが要る）"

echo "== 3. 実体を root のものにする =="
install -o root -g root -m 0755 "$REPO/campd" /usr/local/bin/campd
echo "  /usr/local/bin/campd（$HUMAN からは差し替えられない）"

echo "== 4. 読み取りだけを与える =="
# 既定ACLなので、Claude Code が新しく作る 0600 のファイルにも自動で乗る。
for d in "/home/$HUMAN/.claude/projects" "/home/$HUMAN/.claude/file-history" "$VAULT"; do
	[ -d "$d" ] || { echo "  飛ばす（無い）: $d"; continue; }
	setfacl -R  -m u:camp:rX "$d"
	setfacl -R -d -m u:camp:rX "$d"
	echo "  読み取りACL: $d"
done
# 途中のディレクトリを通り抜けられるように x だけ足す。中身は見せない。
setfacl -m u:camp:x "/home/$HUMAN" "/home/$HUMAN/.claude" \
	"/home/$HUMAN/Documents" "/home/$HUMAN/Documents/git-cloned" 2>/dev/null || true

echo "== 5. systemd =="
install -o root -g root -m 0644 "$REPO/deploy/camp.service" /etc/systemd/system/
install -o root -g root -m 0644 "$REPO/deploy/camp-ingest.service" /etc/systemd/system/
install -o root -g root -m 0644 "$REPO/deploy/camp-ingest.timer" /etc/systemd/system/
systemctl daemon-reload
echo "  入れた。まだ起動していない（DB を移してから）"

cat <<'NEXT'

次にやること（DB の移設）:

  sudo -u camp /usr/local/bin/campd migrate -db /var/lib/camp/camp.sqlite
  ./campd snapshot -out ~/camp-before-move.snapshot   # 鍵は標準入力から
  sudo install -o camp -g camp -m 0600 data/camp.sqlite /var/lib/camp/camp.sqlite
  sudo -u camp /usr/local/bin/campd doctor -db /var/lib/camp/camp.sqlite -fix
  sudo systemctl enable --now camp.service camp-ingest.timer

確かめること:

  cat /var/lib/camp/camp.sqlite            # Permission denied になる
  echo x >> /usr/local/bin/campd           # Permission denied になる
  campd report -action test.boundary       # 通る（追記だけ）
NEXT
