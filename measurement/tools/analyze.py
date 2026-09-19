#!/usr/bin/env python3
"""nginx の LTSV アクセスログを 1 秒バケットに畳んで、時系列レポート(単一HTML)を吐く。

    python3 tools/analyze.py runs/xxx/access.log --out runs/xxx/report.html

alp や kataribe は全区間の集計しか出さないので「いつ何が起きたか」が消える。
このスクリプトは時間軸を保ったまま、ISUCON11予選のベンチが投げてくる 3 系統
(normalユーザ / conditionポスター / trendビューア) を分離して見えるようにする。

依存は標準ライブラリのみ。描画は Plotly.js を CDN から読む。
"""

from __future__ import annotations

import argparse
import html
import json
import math
import re
import sys
from collections import defaultdict
from dataclasses import dataclass, field

# ---------------------------------------------------------------- パス正規化

UUID_RE = re.compile(
    r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}"
)
NUM_SEG_RE = re.compile(r"/\d+(?=/|$)")


def normalize(method: str, uri: str) -> str:
    """URI をエンドポイント単位にまとめる。クエリは落とす。"""
    path = uri.split("?", 1)[0]
    path = UUID_RE.sub(":uuid", path)
    path = NUM_SEG_RE.sub("/:id", path)
    # 静的ファイルは 1 本にまとめないとヒートマップが埋まる
    if path.startswith("/assets/"):
        path = "/assets/*"
    elif re.search(r"\.(js|css|png|jpg|jpeg|svg|ico|woff2?|map)$", path):
        path = "/static/*"
    return f"{method} {path}"


# ---------------------------------------------------------------- パース


@dataclass
class Req:
    start: float  # リクエスト受信時刻(epoch秒)
    end: float  # 応答完了時刻(epoch秒)
    endpoint: str
    status: int
    reqtime: float  # 秒
    apptime: float  # 秒 (upstream。未設定なら reqtime と同値扱い)
    uid: str


def _f(v: str, default: float = 0.0) -> float:
    """"-" や空文字、複数 upstream の "0.1, 0.2" を吸収する。"""
    if not v or v == "-":
        return default
    if "," in v:  # upstream が複数ある場合は合算
        total = 0.0
        for part in v.split(","):
            part = part.strip()
            if part and part != "-":
                total += float(part)
        return total
    try:
        return float(v)
    except ValueError:
        return default


def parse(paths: list[str]) -> tuple[list[Req], int]:
    reqs: list[Req] = []
    skipped = 0
    for p in paths:
        with open(p, encoding="utf-8", errors="replace") as fh:
            for line in fh:
                line = line.rstrip("\n")
                if not line:
                    continue
                rec = {}
                for field_str in line.split("\t"):
                    k, _, v = field_str.partition(":")
                    rec[k] = v
                if "time" not in rec or "uri" not in rec:
                    skipped += 1
                    continue
                try:
                    end = float(rec["time"])
                except ValueError:
                    skipped += 1
                    continue
                reqtime = _f(rec.get("reqtime", ""))
                apptime = _f(rec.get("apptime", ""), reqtime)
                try:
                    status = int(rec.get("status", "0") or 0)
                except ValueError:
                    status = 0
                reqs.append(
                    Req(
                        start=end - reqtime,
                        end=end,
                        endpoint=normalize(rec.get("method", "?"), rec["uri"]),
                        status=status,
                        reqtime=reqtime,
                        apptime=apptime,
                        uid=rec.get("uid", ""),
                    )
                )
    reqs.sort(key=lambda r: r.start)
    return reqs, skipped


# ---------------------------------------------------------------- 集計


def pct(sorted_vals: list[float], q: float) -> float:
    """nearest-rank パーセンタイル。sorted_vals は昇順前提。"""
    if not sorted_vals:
        return 0.0
    k = max(0, math.ceil(q * len(sorted_vals)) - 1)
    return sorted_vals[min(k, len(sorted_vals) - 1)]


def status_class(code: int) -> str:
    if code == 499:
        return "499 (client abort)"
    if code >= 500:
        return "5xx"
    if code >= 400:
        return "4xx"
    if code >= 300:
        return "3xx"
    if code >= 200:
        return "2xx"
    return "other"


@dataclass
class Agg:
    n_buckets: int
    bucket: float
    endpoints: list[str]
    rps: dict[str, list[float]]  # endpoint -> per-bucket rps
    lat: dict[str, list[list[float]]]  # endpoint -> per-bucket latency samples(ms)
    status_rps: dict[str, list[float]]
    inflight: list[float]
    totals: dict[str, int] = field(default_factory=dict)


def aggregate(reqs: list[Req], t0: float, bucket: float) -> Agg:
    last = max(r.end for r in reqs)
    n = int((last - t0) / bucket) + 1

    rps: dict[str, list[float]] = defaultdict(lambda: [0.0] * n)
    lat: dict[str, list[list[float]]] = defaultdict(lambda: [[] for _ in range(n)])
    status_rps: dict[str, list[float]] = defaultdict(lambda: [0.0] * n)
    inflight = [0.0] * n
    totals: dict[str, int] = defaultdict(int)

    for r in reqs:
        b = int((r.start - t0) / bucket)
        if b < 0 or b >= n:
            continue
        rps[r.endpoint][b] += 1
        lat[r.endpoint][b].append(r.reqtime * 1000.0)
        status_rps[status_class(r.status)][b] += 1
        totals[r.endpoint] += 1
        # 開始〜終了にまたがるバケットすべてに在庫としてカウント（同時実行数の近似）
        eb = min(n - 1, int((r.end - t0) / bucket))
        for i in range(max(b, 0), eb + 1):
            inflight[i] += 1

    # バケット幅で割って毎秒あたりに直す
    for series in (rps, status_rps):
        for key in series:
            series[key] = [v / bucket for v in series[key]]

    endpoints = sorted(totals, key=lambda e: totals[e], reverse=True)
    return Agg(n, bucket, endpoints, dict(rps), dict(lat), dict(status_rps), inflight, dict(totals))


# ---------------------------------------------------------------- スコア

# bench/scenario/prepare.go の Score.Set(...) と同じ値。
# "_" で始まるタグは Set されないので 0 点（計上はされるが加点しない情報タグ）。
SCORE_MAGNITUDE = {
    "00.StartBenchmark": 1000,
    "01.GraphGood": 150,
    "02.GraphNormal": 100,
    "03.GraphBad": 60,
    "04.GraphWorst": 10,
    "05.TodayGraphGood": 60,
    "06.TodayGraphNormal": 40,
    "07.TodayGraphBad": 24,
    "08.TodayGraphWorst": 4,
    "09.ReadInfoCondition": 20,
    "10.ReadWarningCondition": 8,
    "11.ReadCriticalCondition": 4,
}


@dataclass
class ScoreSeries:
    t: list[float]                    # 経過秒(t0 基準)
    ts: list[float]                   # epoch秒(生)
    score: list[int]
    raw: list[int]
    deduction: list[int]
    timeout: list[int]
    tags: dict[str, list[int]]        # タグ -> 累積カウント


def parse_score(path: str) -> ScoreSeries:
    """bench の -score-dump が吐いた JSONL を読む。"""
    rows = []
    with open(path, encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if line:
                rows.append(json.loads(line))
    rows.sort(key=lambda r: r["ts"])

    all_tags: list[str] = []
    for r in rows:
        for k in r.get("tags", {}):
            if k not in all_tags:
                all_tags.append(k)
    all_tags.sort()

    return ScoreSeries(
        t=[],  # あとで t0 を引いて埋める
        ts=[r["ts"] for r in rows],
        score=[r["score"] for r in rows],
        raw=[r["raw"] for r in rows],
        deduction=[r["deduction"] for r in rows],
        timeout=[r["timeout"] for r in rows],
        tags={k: [r.get("tags", {}).get(k, 0) for r in rows] for k in all_tags},
    )


def check_magnitudes(sc: ScoreSeries) -> str | None:
    """倍率表が bench の実装とズレていないかを最終スナップショットで検算する。

    上流が点数を変えたら黙って間違ったグラフを描くことになるので、
    合わなければ警告を返す。
    """
    if not sc.ts:
        return None
    total = sum(SCORE_MAGNITUDE.get(tag, 0) * vals[-1] for tag, vals in sc.tags.items())
    if total != sc.raw[-1]:
        return (f"SCORE_MAGNITUDE が bench と一致しません "
                f"(計算 {total} != raw {sc.raw[-1]})。"
                f"bench/scenario/prepare.go の Score.Set を確認してください。")
    return None


def score_gain_per_sec(sc: ScoreSeries) -> dict[str, list[float | None]]:
    """タグ別の「その秒に稼いだ点数」を返す。累積カウントの差分 × 倍率。"""
    out: dict[str, list[float | None]] = {}
    for tag, counts in sc.tags.items():
        mag = SCORE_MAGNITUDE.get(tag, 0)
        if mag == 0:
            continue  # 0 点のタグは加点グラフに出しても意味がない
        series: list[float | None] = [None]
        for i in range(1, len(counts)):
            dt = sc.ts[i] - sc.ts[i - 1]
            if dt <= 0:
                series.append(None)
            else:
                series.append((counts[i] - counts[i - 1]) * mag / dt)
        out[tag] = series
    return out


# ---------------------------------------------------------------- 描画


# ベンチの 3 系統が一目で分かるよう、系統ごとに色相を固定する。
def endpoint_color(ep: str, idx: int) -> str:
    palette = [
        "#4C78A8", "#F58518", "#54A24B", "#E45756", "#72B7B2",
        "#EECA3B", "#B279A2", "#FF9DA6", "#9D755D", "#BAB0AC",
        "#1B9E77", "#D95F02", "#7570B3", "#E7298A", "#66A61E",
    ]
    if ep.startswith("POST /api/condition"):
        return "#E45756"  # ポスター(書き込み)
    if ep.endswith("/api/trend"):
        return "#F58518"  # ビューア(スコアの律速)
    return palette[idx % len(palette)]


@dataclass
class Panel:
    """縦に積むサブプロット1枚。weight は高さの比率。"""
    title: str
    weight: float
    traces: list = field(default_factory=list)


# スコアタグの系統ごとに色を固定する（Graph系 / TodayGraph系 / Read系）
TAG_COLOR = {
    "00.StartBenchmark": "#9D755D",
    "01.GraphGood": "#1B6E3F", "02.GraphNormal": "#2E9E5B",
    "03.GraphBad": "#6BBF8A", "04.GraphWorst": "#B7E0C5",
    "05.TodayGraphGood": "#1F4E79", "06.TodayGraphNormal": "#3A7CB8",
    "07.TodayGraphBad": "#7FB2DC", "08.TodayGraphWorst": "#C3DCF0",
    "09.ReadInfoCondition": "#B35A00", "10.ReadWarningCondition": "#E8871A",
    "11.ReadCriticalCondition": "#F7BE7C",
}


def build_figure(agg: Agg, top: int, lat_targets: list[str],
                 sc: "ScoreSeries | None" = None) -> dict:
    x = [round(i * agg.bucket, 3) for i in range(agg.n_buckets)]
    top_eps = agg.endpoints[:top]
    panels: list[Panel] = []

    # ---- スコア（-score-dump がある場合のみ）--------------------------------
    if sc is not None and sc.t:
        pnl = Panel("score (cumulative)", 1.1)
        pnl.traces.append({
            "type": "scatter", "mode": "lines", "name": "score",
            "x": sc.t, "y": sc.score,
            "line": {"width": 2.4, "color": "#1F77B4"},
            "hovertemplate": "score %{y}<extra></extra>",
        })
        pnl.traces.append({
            "type": "scatter", "mode": "lines", "name": "raw (減点前)",
            "x": sc.t, "y": sc.raw,
            "line": {"width": 1.2, "color": "#AEC7E8", "dash": "dot"},
            "hovertemplate": "raw %{y}<extra></extra>",
        })
        panels.append(pnl)

        pnl = Panel("score gain / sec  (タグ別)", 1.2)
        gains = score_gain_per_sec(sc)
        for tag in sorted(gains):
            pnl.traces.append({
                "type": "scatter", "mode": "none", "stackgroup": "gain",
                "name": tag, "legendgroup": "gain",
                "x": sc.t, "y": [round(v, 1) if v is not None else None
                                 for v in gains[tag]],
                "fillcolor": TAG_COLOR.get(tag, "#BAB0AC"),
                "hovertemplate": tag + "<br>%{y} pt/s<extra></extra>",
            })
        panels.append(pnl)

        pnl = Panel("deduction / timeout", 0.6)
        pnl.traces.append({
            "type": "scatter", "mode": "lines", "name": "timeout (累積)",
            "x": sc.t, "y": sc.timeout,
            "line": {"width": 1.6, "color": "#B279A2"},
            "hovertemplate": "timeout %{y}<extra></extra>",
        })
        pnl.traces.append({
            "type": "scatter", "mode": "lines", "name": "deduction (累積)",
            "x": sc.t, "y": sc.deduction,
            "line": {"width": 1.6, "color": "#E45756"},
            "hovertemplate": "deduction %{y}<extra></extra>",
        })
        panels.append(pnl)

    # ---- エンドポイント別 RPS ----------------------------------------------
    pnl = Panel("rps by endpoint", 1.2)
    for i, ep in enumerate(top_eps):
        pnl.traces.append({
            "type": "scatter", "mode": "none", "stackgroup": "rps",
            "name": ep, "legendgroup": ep,
            "x": x, "y": [round(v, 2) for v in agg.rps[ep]],
            "fillcolor": endpoint_color(ep, i),
            "hovertemplate": html.escape(ep) + "<br>%{y} rps<extra></extra>",
        })
    panels.append(pnl)

    # ---- ステータス別 RPS ---------------------------------------------------
    pnl = Panel("rps by status", 0.9)
    status_colors = {
        "2xx": "#54A24B", "3xx": "#4C78A8", "4xx": "#EECA3B",
        "5xx": "#E45756", "499 (client abort)": "#B279A2", "other": "#BAB0AC",
    }
    for name, color in status_colors.items():
        if name not in agg.status_rps:
            continue
        pnl.traces.append({
            "type": "scatter", "mode": "none", "stackgroup": "status",
            "name": name, "legendgroup": "status",
            "x": x, "y": [round(v, 2) for v in agg.status_rps[name]],
            "fillcolor": color,
            "hovertemplate": name + "<br>%{y} rps<extra></extra>",
        })
    panels.append(pnl)

    # ---- レイテンシ分位 -----------------------------------------------------
    pnl = Panel("latency p50/p95/p99 (ms)", 1.0)
    dashes = ["dot", "dash", "solid"]
    for ti, ep in enumerate(lat_targets):
        if ep not in agg.lat:
            continue
        per_bucket = [sorted(v) for v in agg.lat[ep]]
        for di, q in enumerate((0.5, 0.95, 0.99)):
            pnl.traces.append({
                "type": "scatter", "mode": "lines",
                "name": f"{ep} p{int(q * 100)}",
                "legendgroup": f"lat-{ep}",
                "x": x, "y": [round(pct(v, q), 1) if v else None for v in per_bucket],
                "line": {"width": 1.6, "dash": dashes[di],
                         "color": endpoint_color(ep, ti)},
                "connectgaps": False,
                "hovertemplate": html.escape(ep) + f" p{int(q * 100)}"
                                 + "<br>%{y} ms<extra></extra>",
            })
    panels.append(pnl)

    # ---- 同時実行数 ---------------------------------------------------------
    pnl = Panel("in-flight", 0.8)
    pnl.traces.append({
        "type": "scatter", "mode": "lines", "name": "in-flight",
        "x": x, "y": [round(v, 1) for v in agg.inflight],
        "line": {"width": 1.6, "color": "#333"},
        "hovertemplate": "in-flight %{y}<extra></extra>", "showlegend": False,
    })
    panels.append(pnl)

    # ---- p95 ヒートマップ ---------------------------------------------------
    heat_eps = list(reversed(top_eps))  # Plotly の y は下から積む
    z, all_p95 = [], []
    for ep in heat_eps:
        row = []
        for samples in agg.lat[ep]:
            if samples:
                v = pct(sorted(samples), 0.95)
                row.append(round(v, 1))
                all_p95.append(v)
            else:
                row.append(None)
        z.append(row)
    zmax = pct(sorted(all_p95), 0.95) if all_p95 else 1.0
    pnl = Panel("", 1.6)
    pnl.traces.append({
        "type": "heatmap", "z": z, "x": x, "y": heat_eps,
        "colorscale": "YlOrRd", "zmin": 0, "zmax": max(zmax, 1.0),
        "hovertemplate": "%{y}<br>%{x}s  p95 %{z} ms<extra></extra>",
        "colorbar": {"title": {"text": "p95 ms"}, "len": 0.16,
                     "y": 0.08, "yanchor": "middle"},
    })
    panels.append(pnl)

    # ---- パネルを縦に割り付ける ---------------------------------------------
    gap = 0.030
    total_w = sum(p.weight for p in panels)
    avail = 1.0 - gap * (len(panels) - 1)
    traces: list[dict] = []
    layout: dict = {
        "height": int(165 * total_w) + 140,
        "margin": {"l": 215, "r": 40, "t": 60, "b": 50},
        "hovermode": "x unified",
        "legend": {"orientation": "v", "x": 1.01, "y": 1.0, "font": {"size": 10}},
        "title": {"text": "ISUCON11q benchmark timeline"},
    }

    top_edge = 1.0
    for i, pnl in enumerate(panels):
        suffix = "" if i == 0 else str(i + 1)
        yaxis_id = "y" + suffix
        h = avail * pnl.weight / total_w
        domain = [max(0.0, round(top_edge - h, 4)), round(top_edge, 4)]
        top_edge -= h + gap

        axis = {"domain": domain, "title": {"text": pnl.title}}
        if pnl.traces[0]["type"] == "heatmap":
            axis["tickfont"] = {"size": 9}
        else:
            axis["rangemode"] = "tozero"
        layout["yaxis" + suffix] = axis

        for tr in pnl.traces:
            tr["xaxis"] = "x"
            tr["yaxis"] = yaxis_id
            traces.append(tr)

    layout["xaxis"] = {
        "title": {"text": "elapsed (s)  —  t=0 は POST /initialize"},
        "anchor": "y" + ("" if len(panels) == 1 else str(len(panels))),
        "showspikes": True, "spikemode": "across",
        "spikethickness": 1, "spikedash": "dot",
    }
    return {"data": traces, "layout": layout}


HTML_TMPL = """<!doctype html>
<meta charset="utf-8">
<title>ISUCON11q benchmark timeline</title>
<script src="https://cdn.plot.ly/plotly-2.32.0.min.js" charset="utf-8"></script>
<style>
  body {{ font-family: -apple-system, "Helvetica Neue", sans-serif; margin: 20px; }}
  pre  {{ background:#f6f8fa; padding:12px; border-radius:6px; font-size:12px;
         overflow-x:auto; line-height:1.5; }}
  h2   {{ font-size:15px; margin-top:28px; }}
</style>
<div id="chart"></div>
<h2>summary</h2>
<pre>{summary}</pre>
<script>
  const fig = {fig};
  Plotly.newPlot("chart", fig.data, fig.layout, {{responsive:true, displaylogo:false}});
</script>
"""


# ---------------------------------------------------------------- サマリ


def summarize(reqs: list[Req], agg: Agg, t0: float,
              sc: "ScoreSeries | None" = None) -> str:
    lines = []
    total = len(reqs)
    dur = agg.n_buckets * agg.bucket
    lines.append(f"requests={total}  duration={dur:.1f}s  mean={total / dur:.1f} rps")
    lines.append("")
    header = (f"{'endpoint':<42}{'count':>8}{'rps':>8}{'p50':>9}{'p95':>9}{'p99':>9}"
              f"{'max':>9}{'5xx':>6}{'499':>8}{'499%':>7}")
    lines.append(header)
    lines.append("-" * len(header))

    by_ep_lat: dict[str, list[float]] = defaultdict(list)
    by_ep_5xx: dict[str, int] = defaultdict(int)
    # 499 = クライアント(ベンチ)側のタイムアウト切断。ISUCON では 5xx より
    # こちらが本命の失敗信号になる（アプリは正常に処理中でも間に合っていない）。
    by_ep_499: dict[str, int] = defaultdict(int)
    for r in reqs:
        by_ep_lat[r.endpoint].append(r.reqtime * 1000.0)
        if r.status >= 500:
            by_ep_5xx[r.endpoint] += 1
        if r.status == 499:
            by_ep_499[r.endpoint] += 1

    # 合計レイテンシ（= そのエンドポイントが占有した総時間）順。ここが改善の優先順位。
    order = sorted(by_ep_lat, key=lambda e: sum(by_ep_lat[e]), reverse=True)
    for ep in order[:25]:
        s = sorted(by_ep_lat[ep])
        n499 = by_ep_499.get(ep, 0)
        # 件数だけでなく比率も出す。少数のエンドポイントで全滅しているのか、
        # 全体に薄く散っているのかで打ち手が変わるため。
        rate = f"{n499 / len(s) * 100:.1f}%" if n499 else "-"
        lines.append(
            f"{ep[:42]:<42}{len(s):>8}{len(s) / dur:>8.1f}"
            f"{pct(s, 0.5):>9.1f}{pct(s, 0.95):>9.1f}{pct(s, 0.99):>9.1f}{s[-1]:>9.1f}"
            f"{by_ep_5xx.get(ep, 0):>6}{n499:>8}{rate:>7}"
        )

    lines.append("")
    lines.append("※ 並び順は「総所要時間(count × 平均レイテンシ)」の降順。")
    lines.append("   単発が遅いものより、ここの上位を削る方がスコアに効く。")
    lines.append("※ 499 はベンチ側がタイムアウトして切断した数（既定 1s）。")
    lines.append("   5xx が 0 でも 499 が多いなら「落ちている」のではなく「間に合っていない」。")

    # 負荷が頭打ちになった時刻を機械的に拾っておく
    trend = next((e for e in agg.endpoints if e.endswith("/api/trend")), None)
    if trend:
        worst_b, worst_v = -1, 0.0
        for b, samples in enumerate(agg.lat[trend]):
            if samples:
                v = pct(sorted(samples), 0.95)
                if v > worst_v:
                    worst_b, worst_v = b, v
        if worst_b >= 0:
            lines.append("")
            lines.append(
                f"GET /api/trend の p95 最悪値: {worst_v:.0f} ms @ {worst_b * agg.bucket:.0f}s"
            )
            lines.append("   trend が遅いとベンチはユーザを増やさない＝スコアの上限がここで決まる。")

    if sc is not None and sc.t:
        lines.append("")
        lines.append("=" * 40 + " SCORE " + "=" * 40)
        lines.append(f"final: score={sc.score[-1]}  raw={sc.raw[-1]}  "
                     f"deduction={sc.deduction[-1]}  timeout={sc.timeout[-1]}")

        # 秒間加点のピークと終盤を比べる。頭打ちになっていればここに出る。
        gains = score_gain_per_sec(sc)
        per_sec = []
        for i in range(len(sc.t)):
            tot = 0.0
            for series in gains.values():
                v = series[i] if i < len(series) else None
                if v:
                    tot += v
            per_sec.append(tot)
        if len(per_sec) > 1:
            peak = max(per_sec)
            peak_at = sc.t[per_sec.index(peak)]
            tail = per_sec[-10:]
            tail_avg = sum(tail) / len(tail)
            lines.append(f"加点レート: peak {peak:.0f} pt/s @ {peak_at:.0f}s  /  "
                         f"終盤10秒平均 {tail_avg:.0f} pt/s")
            if peak > 0 and tail_avg < peak * 0.7:
                lines.append("   → 終盤で加点レートが落ちている。負荷を上げきれていない。")

        lines.append("")
        lines.append(f"{'tag':<26}{'count':>8}{'x pt':>7}{'total':>9}{'share':>8}")
        lines.append("-" * 58)
        rows = []
        for tag, counts in sc.tags.items():
            mag = SCORE_MAGNITUDE.get(tag, 0)
            rows.append((tag, counts[-1], mag, counts[-1] * mag))
        rows.sort(key=lambda r: (-r[3], r[0]))
        for tag, cnt, mag, tot in rows:
            share = f"{tot / sc.raw[-1] * 100:.1f}%" if sc.raw[-1] and tot else "-"
            lines.append(f"{tag:<26}{cnt:>8}{mag:>7}{tot:>9}{share:>8}")
        lines.append("※ x pt が 0 のタグは加点しない情報タグ(_ 始まり)。")
        lines.append("   ViewerDropout はビューアがタイムアウトで離脱した数。")

    return "\n".join(lines)


# ---------------------------------------------------------------- main


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("logs", nargs="+", help="LTSV 形式の access.log")
    ap.add_argument("--out", default="report.html")
    ap.add_argument("--bucket", type=float, default=1.0, help="バケット幅(秒)")
    ap.add_argument("--top", type=int, default=15, help="表示するエンドポイント数")
    ap.add_argument("--latency", action="append", default=[],
                    help="レイテンシ分位を描くエンドポイント(既定: GET /api/trend)")
    ap.add_argument("--t0", choices=("initialize", "first"), default="initialize",
                    help="時刻 0 の基準。initialize は POST /initialize を探す")
    ap.add_argument("--score", help="bench の -score-dump が吐いた JSONL")
    args = ap.parse_args()

    reqs, skipped = parse(args.logs)
    if not reqs:
        print("no parsable lines. log_format が ltsv になっているか確認してください。",
              file=sys.stderr)
        return 1
    if skipped:
        print(f"warning: skipped {skipped} unparsable lines", file=sys.stderr)

    t0 = reqs[0].start
    if args.t0 == "initialize":
        init = next((r for r in reqs if r.endpoint == "POST /initialize"), None)
        if init:
            t0 = init.start
        else:
            print("warning: POST /initialize が見つからないので最初の行を t=0 にします",
                  file=sys.stderr)
    # initialize より前の行(前回走行の残骸など)は捨てる
    reqs = [r for r in reqs if r.end >= t0]

    agg = aggregate(reqs, t0, args.bucket)

    targets = args.latency or [e for e in ("GET /api/trend",) if e in agg.lat]
    if not targets:
        targets = agg.endpoints[:2]

    sc = None
    if args.score:
        sc = parse_score(args.score)
        # access.log と同じ t0（POST /initialize）に揃える
        sc.t = [round(ts - t0, 3) for ts in sc.ts]
        warn = check_magnitudes(sc)
        if warn:
            print("warning: " + warn, file=sys.stderr)

    fig = build_figure(agg, args.top, targets, sc)
    summary = summarize(reqs, agg, t0, sc)

    fig_json = json.dumps(fig)
    with open(args.out, "w", encoding="utf-8") as fh:
        fh.write(HTML_TMPL.format(fig=fig_json, summary=html.escape(summary)))

    print(summary)
    print(f"\nwrote {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
