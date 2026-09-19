#!/usr/bin/env bash
# 直近の走行のアクセスログから、接続構造と転送量を出す。
#   ./tools/conninfo.sh [app-vm]
set -euo pipefail
VM="${1:-isucon11q}"
multipass exec "$VM" -- sudo python3 -c "
import collections
conns = collections.defaultdict(list); tot=0; sz=0; gz=0
for line in open('/var/log/nginx/access.log', errors='replace'):
    r = {}
    for f in line.rstrip('\n').split('\t'):
        k, _, v = f.partition(':'); r[k] = v
    if 'conn' not in r or 'time' not in r: continue
    try:
        end = float(r['time']); rt = float(r['reqtime'] or 0); sz += int(r.get('size') or 0)
    except ValueError: continue
    if r.get('gzip'): gz += 1
    tot += 1
    conns[r['conn']].append((end - rt, end))

best = 0; one = 0
for c, evs in conns.items():
    pts = sorted([(s,1) for s,e in evs] + [(e,-1) for s,e in evs])
    cur = mx = 0
    for _, d in pts:
        cur += d
        if cur > mx: mx = cur
    best = max(best, mx)
    if len(evs) == 1: one += 1
print('リクエスト        : %d' % tot)
print('TCP接続           : %d  (1本だけ: %d, %.1f%%)' % (len(conns), one, 100*one/len(conns)))
print('接続あたり平均    : %.0f' % (tot/len(conns)))
print('同時ストリーム最大: %d' % best)
print('送信量            : %.1f MB (%.0f Mbps) / gzip適用 %d' % (sz/1048576, sz*8/60/1e6, gz))
"
