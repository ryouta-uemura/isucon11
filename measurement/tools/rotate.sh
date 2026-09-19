#!/usr/bin/env bash
# ベンチ走行の直前に VM 上でログを空にする。
# 走行ごとにログを切っておかないと時系列が前回の走行と混ざる。
#
#   ./tools/rotate.sh <vm-name>
set -euo pipefail

VM="${1:?usage: rotate.sh <vm-name>}"

multipass exec "$VM" -- sudo truncate -s 0 /var/log/nginx/access.log
multipass exec "$VM" -- sudo truncate -s 0 /var/log/nginx/error.log
echo "==> truncated nginx logs on $VM"
