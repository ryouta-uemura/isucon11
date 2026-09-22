#!/usr/bin/env bash
# trend キャッシュの TTL を掃引する。
#
#   ./tools/sweep-trend-ttl.sh [繰り返し回数]
#
# 条件は「巡回」させる。同じ条件をまとめて先に走らせると、セッション中の
# ドリフト(同一コードで 4.2M -> 3.7M を観測)を条件差と誤認する。
set -euo pipefail

APP_VM="${APP_VM:-isucon11q}"
ROUNDS="${1:-2}"
# ラベル:TTL(ms)  0 = 凍結(期限なし)
CONDITIONS=("frozen:0" "ttl10s:10000" "ttl2s:2000")

RESULT="runs/sweep-trend-ttl-$(date +%Y%m%d-%H%M%S).tsv"
printf "round\tcondition\tttl_ms\tscore\tuser_increase\tpost_avg_ms\tserver_total_s\n" > "$RESULT"

for r in $(seq 1 "$ROUNDS"); do
  for cond in "${CONDITIONS[@]}"; do
    label="${cond%%:*}"
    ttl="${cond##*:}"

    # env.sh の TREND_CACHE_TTL_MS を書き換えてアプリを再起動する
    multipass exec "$APP_VM" -- sudo bash -c "
      sed -i '/^TREND_CACHE_TTL_MS=/d' /home/isucon/env.sh
      echo 'TREND_CACHE_TTL_MS=$ttl' >> /home/isucon/env.sh
      systemctl restart isucondition.go.service
    "
    # 再ビルド/再起動直後は落ち着かせる。2秒では足りず score 0 や異常値が混じる。
    sleep 8

    out=$(BENCH_VM="$APP_VM" BENCH_DIR=/home/isucon/bench BENCH_USER=isucon \
          TARGET_IP=127.0.0.11 JIA_IP=127.0.0.1 TLS_OPTS="-tls" \
          ./tools/measure.sh "r${r}-${label}" 2>&1)
    echo "$out" | grep -E "^###|^score|ユーザー増加"

    score=$(echo "$out" | sed -n 's/^score: \([0-9]*\).*/\1/p')
    inc=$(echo "$out"   | sed -n 's/^ユーザー増加: \([0-9]*\) 回.*/\1/p')

    dir="runs/$(date +%Y%m%d-%H%M%S)-r${r}-${label}"
    mkdir -p "$dir"
    multipass exec "$APP_VM" -- sudo cat /var/log/nginx/access.log > "$dir/access.log"
    multipass exec "$APP_VM" -- sudo -u isucon cat /tmp/score.jsonl > "$dir/score.jsonl"

    read -r post total < <(awk -F'\t' '
      {a=$6; sub(/^apptime:/,"",a); tot+=a;
       if($2=="method:POST" && $3 ~ /^uri:\/api\/condition\//){pc++; ps+=a}}
      END{printf "%.2f %.1f\n", ps/pc*1000, tot}' "$dir/access.log")

    printf "%s\t%s\t%s\t%s\t%s\t%s\t%s\n" \
      "$r" "$label" "$ttl" "${score:-NA}" "${inc:-NA}" "$post" "$total" >> "$RESULT"
  done
done

echo
echo "=== $RESULT ==="
column -t -s $'\t' "$RESULT"
