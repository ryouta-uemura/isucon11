#!/usr/bin/env bash
# gzip の on/off を「ベンチ同居」「ベンチ分離」の両方で掃引する。
#
#   ./tools/sweep-gzip.sh [繰り返し回数]
#
# 同居は loopback なので転送量がタダで gzip は純コストになるが、分離では
# TX オフロードが全滅している（tx-checksum / TSO が off [fixed]）ため、
# バイト数がそのままソフトウェア処理するパケット数になる。
# つまり gzip が自分の代金を払い返す可能性があり、符号が逆転しうる。
set -euo pipefail

APP_VM="${APP_VM:-isucon11q}"
BENCH_VM_REMOTE="${BENCH_VM_REMOTE:-isucon11q-db}"
ROUNDS="${1:-2}"

RESULT="runs/sweep-gzip-$(date +%Y%m%d-%H%M%S).tsv"
printf "round\tplacement\tgzip\tscore\tnginx_cores\tgraph_req\tbytes_mb\tserver_s\n" > "$RESULT"

set_gzip() {
  multipass exec "$APP_VM" -- sudo bash -c "
    sed -i 's/^\( *\)gzip  *\(on\|off\);/\1gzip  $1;/' /etc/nginx/nginx.conf
    nginx -t >/dev/null 2>&1 && systemctl reload nginx"
  # 反映確認。ここを黙って通すと、設定が変わっていない走行を比較してしまう。
  local got
  got=$(multipass exec "$APP_VM" -- sudo grep -oP '^\s*gzip\s+\K(on|off)' /etc/nginx/nginx.conf | head -1)
  [ "$got" = "$1" ] || { echo "gzip の設定に失敗 (期待 $1 / 実際 $got)"; exit 1; }
}

for r in $(seq 1 "$ROUNDS"); do
  for placement in colo split; do
    for gz in on off; do
      set_gzip "$gz"

      if [ "$placement" = "colo" ]; then
        env_args=(BENCH_VM="$APP_VM" BENCH_DIR=/home/isucon/bench BENCH_USER=isucon
                  TARGET_IP=127.0.0.11 JIA_IP=127.0.0.1 TLS_OPTS="-tls")
      else
        env_args=(BENCH_VM="$BENCH_VM_REMOTE" BENCH_DIR=/home/isucon/bench BENCH_USER=isucon
                  TARGET_IP=192.168.252.9 JIA_IP=192.168.252.11 TLS_OPTS="-tls -tls-skip-verify")
      fi

      # env を噛ませること。"${arr[@]}" で展開された VAR=value は、パース後の
      # 展開なので bash が変数代入と見なさず、コマンド名として扱われて落ちる。
      out=$(env APP_VM="$APP_VM" "${env_args[@]}" ./tools/measure.sh "gz-${placement}-${gz}-r${r}" 2>&1)
      echo "$out" | grep -E "^###|^score"

      score=$(echo "$out"  | sed -n 's/^score: \([0-9]*\).*/\1/p')
      nginx=$(echo "$out"  | awk '$1=="nginx"{print $3}')

      d="runs/$(date +%Y%m%d-%H%M%S)-gz-${placement}-${gz}-r${r}"
      mkdir -p "$d"
      multipass exec "$APP_VM" -- sudo cat /var/log/nginx/access.log > "$d/access.log"

      read -r gq mb tot < <(awk -F'\t' '
        {for(i=1;i<=NF;i++) if($i ~ /^size:/) sz=substr($i,6)
         a=$6; sub(/^apptime:/,"",a); tot+=a; byt+=sz
         if($3 ~ /graph/) q++}
        END{printf "%d %.1f %.1f\n", q, byt/1048576, tot}' "$d/access.log")

      printf "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n" \
        "$r" "$placement" "$gz" "${score:-NA}" "${nginx:-NA}" "$gq" "$mb" "$tot" >> "$RESULT"
    done
  done
done

set_gzip on   # 既定に戻す
echo
echo "=== $RESULT ==="
column -t -s $'\t' "$RESULT"
