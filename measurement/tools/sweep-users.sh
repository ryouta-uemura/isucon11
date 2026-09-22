#!/usr/bin/env bash
# ユーザー数 N を固定して score(N) の曲線を引く。
#
#   ./tools/sweep-users.sh [繰り返し回数]
#
# TTL 経由の増加では「何人になったか」を制御できないので、ベンチに
# BENCH_INIT_USERS / BENCH_NO_ADD を生やして直接 N を指定する
# （tools/bench-patch/patch-fixed-users.py）。
#
# 1人増えると何が増えるのかを分解して記録する:
#   ユーザー自身のループ（得点源） / ISU とポスター（0点） / viewer（0点）
set -euo pipefail

APP_VM="${APP_VM:-isucon11q}"
ROUNDS="${1:-2}"
NS="${NS:-7 10 14 20 28}"

RESULT="runs/sweep-users-$(date +%Y%m%d-%H%M%S).tsv"
printf "round\tN\tscore\tdeduct\tusers\tisus\tviewers\tgraphgood\tgraph_req\titers\tapp\tnginx\tbench\n" > "$RESULT"

for r in $(seq 1 "$ROUNDS"); do
  for n in $NS; do
    # PLACEMENT=split でベンチを別VMに出す。本番のベンチは専有機なので、
    # 同居だとベンチ自身のCPU不足が曲線を歪める（ダウンサイドの9割がこれ）。
    if [ "${PLACEMENT:-colo}" = "split" ]; then
      place=(BENCH_VM=isucon11q-db TARGET_IP=192.168.252.9 JIA_IP=192.168.252.11
             TLS_OPTS="-tls -tls-skip-verify")
      BENCH_HOST=isucon11q-db
    else
      place=(BENCH_VM="$APP_VM" TARGET_IP=127.0.0.11 JIA_IP=127.0.0.1 TLS_OPTS="-tls")
      BENCH_HOST=isucon11q
    fi

    out=$(env BENCH_ENV="BENCH_INIT_USERS=$n BENCH_NO_ADD=1" \
          APP_VM="$APP_VM" "${place[@]}" BENCH_DIR=/home/isucon/bench BENCH_USER=isucon \
          ./tools/measure.sh "n${n}-${PLACEMENT:-colo}-r${r}" 2>&1)
    echo "$out" | grep -E "^###|^score"

    read -r score deduct < <(echo "$out" | sed -n 's/^score: \([0-9]*\)(\([0-9]*\) - \([0-9]*\)).*/\1 \3/p')
    app=$(echo   "$out" | awk '$1=="isucondition"{print $3}')
    nginx=$(echo "$out" | awk '$1=="nginx"{print $3}')
    bench=$(echo "$out" | awk '$1=="bench"{print $3}')

    d="runs/$(date +%Y%m%d-%H%M%S)-n${n}-${PLACEMENT:-colo}-r${r}"
    mkdir -p "$d"
    multipass exec isucon11q -- sudo cat /var/log/nginx/access.log > "$d/access.log"
    multipass exec "${BENCH_HOST:-isucon11q}" -- sudo -u isucon cat /tmp/score.jsonl > "$d/score.jsonl"

    # ベンチが実際に何人・何ISU・何viewerで走ったかは score.jsonl のタグに出る
    read -r users isus viewers gg < <(python3 -c "
import json,sys
t=json.loads(open('$d/score.jsonl').readlines()[-1])['tags']
print(t.get('_2.NormalUserInitialize',0), t.get('_1.IsuInitialize',0),
      t.get('_3.ViewerInitialize',0), t.get('01.GraphGood',0))
")
    # イテレーション数は GET /api/isu が1周に1回なのでその件数で代理する
    read -r gq it < <(awk -F'\t' '
      {u=$3; sub(/\?.*/,"",u)
       if(u ~ /graph/) q++
       if(u ~ /^uri:\/api\/isu$/) i++}
      END{printf "%d %d\n", q, i}' "$d/access.log")

    printf "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n" \
      "$r" "$n" "$score" "$deduct" "$users" "$isus" "$viewers" "$gg" "$gq" "$it" \
      "${app:-NA}" "${nginx:-NA}" "${bench:-NA}" >> "$RESULT"
  done
done

echo
echo "=== $RESULT ==="
column -t -s $'\t' "$RESULT"
