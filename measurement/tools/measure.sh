#!/usr/bin/env bash
# ベンチを1本回し、アプリVMのプロセス別CPUとスコアを出す。
#
#   ./tools/measure.sh <label>
#
# 構成は環境変数で上書きできる。ベンチをアプリVMに同居させる場合は
# BENCH_VM=<アプリVM名> BENCH_DIR=/home/isucon/bench BENCH_USER=isucon を指定する。
#
#   例) ベンチ別VM  : ./tools/measure.sh label
#   例) ベンチ同居  : BENCH_VM=isucon11q BENCH_DIR=/home/isucon/bench \
#                     BENCH_USER=isucon JIA_IP=127.0.0.1 TARGET_IP=127.0.0.11 \
#                     ./tools/measure.sh label
set -euo pipefail

APP_VM="${APP_VM:-isucon11q}"
BENCH_VM="${BENCH_VM:-isucon11q-bench}"
BENCH_DIR="${BENCH_DIR:-/home/ubuntu/bench}"
BENCH_USER="${BENCH_USER:-}"          # 空ならそのまま実行、値があれば sudo -u で実行
TARGET_IP="${TARGET_IP:-192.168.252.9}"
JIA_IP="${JIA_IP:-192.168.252.10}"
TLS_OPTS="${TLS_OPTS:--tls -tls-skip-verify}"
WINDOW="${WINDOW:-45}"   # CPU を測る窓(秒)。LOAD が始まってから測る
LABEL="${1:-run}"

# /proc/<pid>/stat の utime+stime をコマンド名で合算する
SNAP='awk "{c=\$2; u=\$14; s=\$15; print c, u+s}" /proc/[0-9]*/stat 2>/dev/null \
      | awk "{n=\$1; gsub(/[()]/,\"\",n); t[n]+=\$2} END{for(k in t) print k, t[k]}"'

# 走行ごとにアクセスログを切る。切らないと前回分と混ざって
# 転送量やリクエスト数の集計が壊れる。
multipass exec "$APP_VM" -- sudo truncate -s 0 /var/log/nginx/access.log

# ベンチVMは素のUbuntuなので nofile が 1024 のまま。上げないと
# "too many open files" で panic する（アプリVMは ansible の nofile.yml で緩和済み）。
# BENCH_ENV でベンチ側に環境変数を渡す。
#   例) BENCH_ENV="BENCH_INIT_USERS=14 BENCH_NO_ADD=1"
BENCH_CMD="ulimit -n 1048576; cd $BENCH_DIR && ${BENCH_ENV:-} ./bench \
  -all-addresses $TARGET_IP -target $TARGET_IP:443 $TLS_OPTS \
  -jia-service-url http://$JIA_IP:5000 -score-dump /tmp/score.jsonl \
  2>/dev/null > /tmp/measure.log"
if [ -n "$BENCH_USER" ]; then
  multipass exec "$BENCH_VM" -- sudo -u "$BENCH_USER" bash -c "$BENCH_CMD" &
else
  multipass exec "$BENCH_VM" -- bash -c "$BENCH_CMD" &
fi
BPID=$!

sleep 10   # PREPARE を避ける
multipass exec "$APP_VM" -- bash -c "$SNAP > /tmp/s1; sleep $WINDOW; $SNAP > /tmp/s2"
wait $BPID

echo "### $LABEL"
multipass exec "$BENCH_VM" -- bash -c '
  grep -oE "score: [0-9]+\([0-9]+ - [0-9]+\)" /tmp/measure.log | tail -1
  echo "ユーザー増加: $(grep -cE "ユーザーが[0-9]+人増えました" /tmp/measure.log) 回 / 非増加: $(grep -cE "ユーザーは増えませんでした" /tmp/measure.log) 回"'

APP_CORES=$(multipass exec "$APP_VM" -- nproc)
multipass exec "$APP_VM" -- bash -c "awk -v w=$WINDOW -v cores=$APP_CORES '
  NR==FNR{a[\$1]=\$2; next}
  {d=\$2-(\$1 in a ? a[\$1] : 0); if(d>0){sec=d/100; tot+=sec; r[\$1]=sec}}
  END{
    n=asorti(r, idx, \"@val_num_desc\");
    printf \"%-16s%9s%12s\n\", \"process\", \"CPU秒\", \"コア換算\";
    for(i=1;i<=n && i<=4;i++) printf \"%-16s%9.1f%11.2f\n\", idx[i], r[idx[i]], r[idx[i]]/w;
    printf \"%-16s%9.1f%11.2f  (%dコア中)\n\", \"合計\", tot, tot/w, cores;
  }' /tmp/s1 /tmp/s2"
