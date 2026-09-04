#!/bin/bash
# Phase 3 の実行面を入れる。**sudo は要らない。**
#
#   bash deploy/install-agent.sh
#
# campd（camp ユーザー）は `claude` を起こせないので、起こす役は本人のユーザーで
# 動く必要がある。だからこれは user unit で、system unit にしてはいけない。
#
# 先に `/usr/local/bin/campd` を新しくしてあること（そちらは root が要る）:
#   sudo install -o root -g root -m 0755 ./campd /usr/local/bin/campd
#   sudo systemctl restart camp.service
set -euo pipefail
cd "$(dirname "$0")/.."

UD="$HOME/.config/systemd/user"
mkdir -p "$UD"
install -m 0644 deploy/camp-agent.service "$UD/"
systemctl --user daemon-reload
systemctl --user enable --now camp-agent.service
sleep 2
systemctl --user --no-pager status camp-agent.service | head -12

cat <<'EOS'

== 入った。ただし、このままでは1本も起こせない ==

cwd の許可リストは**既定 deny**。空のままだとどこでもセッションを起こせない。
許すのは camp 側（DBを開ける側）なので sudo が要る:

  sudo -u camp /usr/local/bin/campd allow add ~/Documents/git-cloned/camp
  sudo -u camp /usr/local/bin/campd allow list

画面からも足せる（「駆動」→「許可した場所」）。**変更にはパスワードの再入力が要る。**

確認:
  bash deploy/verify-agent.sh
EOS
