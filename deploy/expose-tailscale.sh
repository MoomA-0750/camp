#!/bin/bash
# Camp を tailnet から見えるようにする。**インターネットには出さない。**
#
#   sudo bash deploy/expose-tailscale.sh
#   sudo bash deploy/expose-tailscale.sh --off     # やめる
#
# 何をするか:
#   Tailscale が :443 で TLS を終端し、127.0.0.1:8785 の campd へ渡す。
#   **campd の待ち受けは 127.0.0.1 のまま**なので、LAN や外のネットワークからは
#   相変わらず触れない。増えるのは tailnet の中からの経路だけ。
#
# 何をしないか:
#   `tailscale funnel`（インターネット公開）は使わない。使うと、Camp の
#   ログイン画面が世界中から叩かれる。会話履歴と Vault の索引が入っている
#   ものに対して、それは割に合わない。
set -euo pipefail

if [ "${1:-}" = "--off" ]; then
	tailscale serve reset
	echo "やめた。tailnet からも見えなくなった（127.0.0.1:8785 はそのまま）。"
	exit 0
fi

if [ "$(id -u)" != "0" ]; then
	echo "sudo が要る: sudo bash $0" >&2
	echo "（毎回 sudo を避けるなら: sudo tailscale set --operator=\$USER を一度だけ）" >&2
	exit 1
fi

# 先に確かめる。**出す先が生きていないのに口だけ開けない。**
if ! curl -sf -o /dev/null http://127.0.0.1:8785/healthz; then
	echo "127.0.0.1:8785 が応答しない。先に campd を確かめる: systemctl status camp.service" >&2
	exit 1
fi

NAME="$(tailscale status --json | python3 -c 'import sys,json;print(json.load(sys.stdin)["Self"]["DNSName"].rstrip("."))')"
tailscale serve --bg --https=443 http://127.0.0.1:8785

cat <<EOS

== 出した（tailnet の中だけ）==

  https://$NAME/

  ・TLS は Tailscale が終端する（本物の証明書。ブラウザが警告しない）
  ・campd の待ち受けは 127.0.0.1 のまま。**LAN からは触れない**
  ・インターネットには出していない（funnel は使っていない）

やめるとき:  sudo bash $0 --off
今の状態  :  tailscale serve status

== うまく入れなかったら ==

「オリジンが違う」と出たら、そのメッセージが**何を見て断ったか**を言う。
Origin のホストと、campd に届いた Host が食い違っているなら、そのオリジンを
明示的に許す。camp.service の ExecStart をこうする:

  ExecStart=/usr/local/bin/campd serve -secure-cookie -origin https://$NAME

（メッセージが出ないほど古い campd なら、先に新しいものを入れる:
  sudo install -o root -g root -m 0755 ./campd /usr/local/bin/campd
  sudo systemctl daemon-reload && sudo systemctl restart camp.service）

== まだ残っていること ==

Cookie に Secure を付けるなら、camp.service の ExecStart に -secure-cookie を
足して再起動する（deploy/camp.service には入れてある）。HTTPS 越しでしか
Cookie を出さなくなる。

  **付けても 127.0.0.1 からは入れる。** 実測で確かめた（2026-09-06、
  -secure-cookie を付けた campd に Chrome で http://127.0.0.1 から入り直し、
  セッションが続くことを確認）。127.0.0.1 はブラウザが「安全な文脈」として
  扱うため。

この serve 設定は tailscaled が覚えるので、再起動しても残る。
EOS
