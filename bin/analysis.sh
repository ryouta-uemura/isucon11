#!/usr/bin/env bash
set -euo pipefail

NGINX_LOG="/var/log/nginx/access.log"
MYSQL_LOG="/var/log/mysql/slow.log"
OUTPUT_DIR="./log_reports"

mkdir -p "$OUTPUT_DIR"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)

echo "==== Analyzing Nginx Access Log (alp) ===="
if [ -f "$NGINX_LOG" ]; then
    # alp の正規化マッチングパターンは環境に合わせて調整してください
    #alp ltsv \
    #    --file "$NGINX_LOG" \
    #    -m "/api/isu/[0-9]+,/api/condition/[a-z0-9-]+,/image/[0-9]+" \
    #    --sort avg -r \
    #    | tee "$OUTPUT_DIR/alp_$TIMESTAMP.txt"
    alp ltsv \
        --file "$NGINX_LOG" \
        -m "/api/isu/[0-9]+,/api/condition/[a-z0-9-]+,/image/[0-9]+" \
        --sort avg -r \
        | tee "$OUTPUT_DIR/alp_$TIMESTAMP.txt"
else
    echo "Nginx log not found."
fi

echo -e "\n==== Analyzing MySQL Slow Query Log ===="
if [ -f "$MYSQL_LOG" ]; then
    # pt-query-digest が入っている場合
    if command -v pt-query-digest &> /dev/null; then
        sudo pt-query-digest "$MYSQL_LOG" | tee "$OUTPUT_DIR/slow_$TIMESTAMP.txt"
    else
        # pt-query-digest がない場合の代替（末尾を表示）
        sudo tail -n 100 "$MYSQL_LOG"
    fi
else
    echo "MySQL slow log not found."
fi

