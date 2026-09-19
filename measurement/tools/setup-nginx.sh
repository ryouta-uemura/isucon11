#!/usr/bin/env bash
# VM の nginx を LTSV ログに切り替える。冪等なので何度流しても安全。
#
#   ./tools/setup-nginx.sh <vm-name>
#
# やること:
#   1. log_format ltsv を /etc/nginx/conf.d/ltsv.conf に配置（http コンテキスト）
#   2. isucondition.conf の server ブロックに access_log ... ltsv を追記
#      → server レベルで定義すると http レベルの `access_log ... main` は継承されず、
#        ログが ltsv 単独になる（二重書き込みを避けるため）
#   3. nginx -t で検証してから reload
set -euo pipefail

VM="${1:?usage: setup-nginx.sh <vm-name>}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SITE=/etc/nginx/sites-enabled/isucondition.conf

echo "==> installing log_format to $VM"
multipass transfer "$ROOT/tools/nginx-ltsv.conf" "$VM:/tmp/ltsv.conf"
multipass exec "$VM" -- sudo install -m 0644 /tmp/ltsv.conf /etc/nginx/conf.d/ltsv.conf

echo "==> patching server block"
multipass exec "$VM" -- sudo bash -s <<'REMOTE'
set -euo pipefail
SITE=/etc/nginx/sites-enabled/isucondition.conf
[ -e "$SITE" ] || SITE=/etc/nginx/sites-available/isucondition.conf

if grep -q 'access_log .*ltsv_ts' "$SITE"; then
  echo "    already patched"
else
  # 初回のみバックアップを残す（上書きしない）
  [ -e "${SITE}.orig" ] || cp -p "$SITE" "${SITE}.orig"
  # server { の直後に access_log を挿入する
  awk '
    /^[[:space:]]*server[[:space:]]*\{/ && !done {
      print
      print "    access_log /var/log/nginx/access.log ltsv_ts;"
      done = 1
      next
    }
    { print }
  ' "$SITE" > "${SITE}.new"
  mv "${SITE}.new" "$SITE"
  echo "    inserted access_log (backup: ${SITE}.orig)"
fi

nginx -t
systemctl reload nginx
echo "    reloaded"
REMOTE

echo "==> done. 現在の server ブロック:"
multipass exec "$VM" -- sudo cat "$SITE"
