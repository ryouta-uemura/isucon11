#!/usr/bin/env bash
# multipass VM から nginx のアクセスログを回収して解析する。
#
#   ./tools/collect.sh <app-vm> [run-label] [bench-vm]
#
# 例: ./tools/collect.sh isucon11q before-index isucon11q-bench
#
# bench-vm を省くとアプリVMから score.jsonl を探す（ベンチ同居構成）。
# ベンチを別VMに分けている場合は3番目の引数でそのVM名を渡す。
#
# ログは runs/<timestamp>-<label>/access.log に保存され、同ディレクトリに
# report.html が生成される。走行ごとにディレクトリが分かれるので改善前後を比較できる。
set -euo pipefail

VM="${1:?usage: collect.sh <app-vm> [run-label] [bench-vm]}"
LABEL="${2:-run}"
BENCH_VM="${3:-$VM}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEST="$ROOT/runs/$(date +%Y%m%d-%H%M%S)-$LABEL"

mkdir -p "$DEST"

echo "==> fetching /var/log/nginx/access.log from $VM"
multipass transfer "$VM:/var/log/nginx/access.log" "$DEST/access.log"

# ベンチを -score-dump 付きで回していればスコアの時系列も回収する（無ければ飛ばす）
ARGS=("$DEST/access.log")
if multipass transfer "$BENCH_VM:/tmp/score.jsonl" "$DEST/score.jsonl" 2>/dev/null; then
  echo "==> score.jsonl: $(wc -l < "$DEST/score.jsonl") snapshots"
  ARGS+=(--score "$DEST/score.jsonl")
else
  echo "==> score.jsonl なし（bench に -score-dump /tmp/score.jsonl を付けると取れる）"
fi

echo "==> $(wc -l < "$DEST/access.log") lines"
python3 "$ROOT/tools/analyze.py" "${ARGS[@]}" --out "$DEST/report.html"

echo "==> $DEST/report.html"
command -v open >/dev/null && open "$DEST/report.html" || true
