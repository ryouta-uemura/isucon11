#!/usr/bin/env bash
# ベンチを1本回し、アプリVMのプロセス別CPUとスコアを出す。
#
#   ./tools/measure.sh <label>
#
# アプリVM/ベンチVMは下の定数で固定。構成を変えたらここを直す。
set -euo pipefail

APP_VM=isucon11q
BENCH_VM=isucon11q-bench
APP_IP=192.168.252.9
BENCH_IP=192.168.252.10
WINDOW=45          # CPU を測る窓(秒)。LOAD が始まってから測る
LABEL="${1:-run}"

# /proc/<pid>/stat の utime+stime をコマンド名で合算する
SNAP='awk "{c=\$2; u=\$14; s=\$15; print c, u+s}" /proc/[0-9]*/stat 2>/dev/null \
      | awk "{n=\$1; gsub(/[()]/,\"\",n); t[n]+=\$2} END{for(k in t) print k, t[k]}"'

# 走行ごとにアクセスログを切る。切らないと前回分と混ざって
# 転送量やリクエスト数の集計が壊れる。
multipass exec "$APP_VM" -- sudo truncate -s 0 /var/log/nginx/access.log

# ベンチVMは素のUbuntuなので nofile が 1024 のまま。上げないと
# "too many open files" で panic する（アプリVMは ansible の nofile.yml で緩和済み）。
multipass exec "$BENCH_VM" -- bash -c "ulimit -n 1048576; cd /home/ubuntu/bench && ./bench \
  -all-addresses $APP_IP -target $APP_IP:443 -tls -tls-skip-verify \
  -jia-service-url http://$BENCH_IP:5000 -score-dump /tmp/score.jsonl \
  2>/dev/null > /tmp/measure.log" &
BPID=$!

sleep 10   # PREPARE を避ける
multipass exec "$APP_VM" -- bash -c "$SNAP > /tmp/s1; sleep $WINDOW; $SNAP > /tmp/s2"
wait $BPID

echo "### $LABEL"
multipass exec "$BENCH_VM" -- bash -c '
  grep -oE "score: [0-9]+\([0-9]+ - [0-9]+\)" /tmp/measure.log | tail -1
  echo "ユーザー増加: $(grep -cE "ユーザーが[0-9]+人増えました" /tmp/measure.log) 回 / 非増加: $(grep -cE "ユーザーは増えませんでした" /tmp/measure.log) 回"'

multipass exec "$APP_VM" -- bash -c "awk -v w=$WINDOW '
  NR==FNR{a[\$1]=\$2; next}
  {d=\$2-(\$1 in a ? a[\$1] : 0); if(d>0){sec=d/100; tot+=sec; r[\$1]=sec}}
  END{
    n=asorti(r, idx, \"@val_num_desc\");
    printf \"%-16s%9s%12s\n\", \"process\", \"CPU秒\", \"コア換算\";
    for(i=1;i<=n && i<=4;i++) printf \"%-16s%9.1f%11.2f\n\", idx[i], r[idx[i]], r[idx[i]]/w;
    printf \"%-16s%9.1f%11.2f  (2コア中)\n\", \"合計\", tot, tot/w;
  }' /tmp/s1 /tmp/s2"
