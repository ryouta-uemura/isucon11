# isucon11q ベンチ解像度ツール（レイヤ1: アクセスログ時系列 / レイヤ2: スコア推移）

> **計測結果の結論は [FINDINGS.md](FINDINGS.md) にまとめてある。**
> このファイルはツールの使い方。
>
> 以下のコマンドは **`measurement/` を作業ディレクトリとして**実行する想定
> （`cd measurement` してから `./tools/...`）。

alp / kataribe は全区間の集計しか出さないので「どのリクエストが、どの順序で降ってきて、
いつ壊れたか」が消える。ここでは nginx のアクセスログを **1秒バケット × エンドポイント**
に畳んで、時間軸を保ったまま 1 枚の HTML に落とす。

## なぜこれで足りるのか

ISUCON11 予選のベンチは、3 系統のワーカーが別々のリズムで並行して叩いてくる。

| 系統 | 主なリクエスト | 性質 |
|---|---|---|
| normal ユーザ | `GET /api/isu`, `GET /api/condition/:uuid`, `GET /api/isu/:uuid/graph` | 成功し続けるとユーザ数が増え、加点源が増える |
| poster | `POST /api/condition/:uuid` | ISU 数に比例して書き込み RPS が線形に増える |
| viewer | `GET /api/trend` | **これが遅いと負荷レベルが上がらない = スコアの上限を決める** |

集計で混ぜると全部平均化されて消えるので、系統を分離したまま時間軸に並べるのが目的。

## VM のプロビジョニング

`tools/provision.sh` は cloud-init の `runcmd` を再実行可能にしたもの。素の Ubuntu 20.04 の
multipass VM に対して standalone 構成（アプリ + DB + ベンチが1台）を作る。

```bash
# アセットを VM に置く（github release から落とす代わりにローカルのものを使う）
multipass exec isucon11q -- mkdir -p /tmp/assets
for f in initialize.json images.tgz 1_InitData.sql; do
  multipass transfer ~/isucon11-files/$f isucon11q:/tmp/assets/$f
done
multipass transfer tools/patch-langs.py isucon11q:/tmp/patch-langs.py
multipass transfer tools/provision.sh   isucon11q:/tmp/provision.sh

multipass exec isucon11q -- sudo bash -c 'chmod +x /tmp/provision.sh && /tmp/provision.sh'
```

**所要 1 分程度**（`patch-langs.py` で Go 以外の言語実装を落としているため。
上流のまま全言語を xbuild でソースビルドすると 1 時間近くかかる）。

ハマりどころとして踏んだもの:

- **ansible ロールが `/tmp/isucon11-qualify` をハードコード参照する**
  （`bench.yml` / `isucondition.yml` / `jiaapi_mock.yml`）。クローン先を変えるなら
  このパスにもソースを置く必要がある。
- **`isucondition.go.service` は配置されるだけで enable も start もされない。**
  `nginx` / `mariadb` は enable される。
- **`jiaapi-mock` は起動してはいけない。** ベンチが自前の ISU協会サービスを :5000 に
  立てるので、常駐していると `bind: address already in use` でベンチが panic する。

## 計測構成（2VM）

```
isucon11q        2コア / 192.168.252.9    アプリ + MariaDB
isucon11q-bench  4コア / 192.168.252.10   ベンチ専用
```

ベンチを同居させると**ベンチ自身が2コア中0.75コアを食う**ため、アプリの性能ではなく
ベンチとの奪い合いを測ることになる。詳細は FINDINGS.md の 1章。

ベンチVMには Go を入れず、アプリVMでビルドしたものを tar で転送している。
`cd` をベンチのディレクトリにしないと `./images` を開けずに落ちる。
素の Ubuntu は `nofile` が 1024 なので上げておくこと（`tools/measure.sh` は明示的に上げる）。

## ベンチの実行

ベンチVM から:

```bash
multipass exec isucon11q-bench -- bash -c \
  'ulimit -n 1048576; cd /home/ubuntu/bench && ./bench \
     -all-addresses 192.168.252.9 -target 192.168.252.9:443 \
     -tls -tls-skip-verify -jia-service-url http://192.168.252.10:5000 \
     -score-dump /tmp/score.jsonl'
```

ベンチVMの `/etc/hosts` で `isucondition-{1,2,3}.t.isucon.dev` が
アプリVM(192.168.252.9)に向いている。`-tls-skip-verify` は自作フラグ（後述）。

CPU内訳も一緒に見たいなら `./tools/measure.sh <label>` が同じことをして
プロセス別CPUとスコアを出す。

## ベンチへのスコア出力パッチ（レイヤ2）

`-score-dump` は上流には無いフラグ。`tools/bench-patch/` を当てて生やす。

```bash
multipass transfer tools/bench-patch/scoredump.go  isucon11q:/tmp/scoredump.go
multipass transfer tools/bench-patch/patch-main.py isucon11q:/tmp/patch-main.py
multipass exec isucon11q -- sudo -u isucon bash -c '
  cd /home/isucon/bench && cp /tmp/scoredump.go ./scoredump.go &&
  [ -e main.go.orig ] || cp main.go main.go.orig
  python3 /tmp/patch-main.py &&
  PATH=/home/isucon/local/go/bin:$PATH go build -o bench .'
```

やっていること:

- `scoredump.go`（新規）が1行1スナップショットの JSONL を書く。スコア算出は
  `sendResult` と同じ手順・同じ優先順位（critical > timeout > deduction）を踏む。
- `main.go` の途中経過ループを **3秒 → 1秒** に変え、`sendResult` は3回に1回だけ呼ぶ。
  **ポータルへの送信頻度は元の3秒間隔のまま**で、スナップショットだけ1秒刻みになる。
  新しい goroutine を足していないので、既存の並行アクセスを増やさない。
- `patch-main.py` は冪等。適用前の `main.go.orig` を残す。

ベンチを別VMに置く場合はさらに2つ:

- `patch-tls.py` — `-tls-skip-verify` を足す。アプリVMの自己署名証明書を
  ベンチVMへ配らずに済ませるため。
- `patch-tls-poster.py` — **必須**。ポスター(`scenario/posting.go`)は
  `agent.DefaultTLSConfig` を使わず自前の `http.Transport` を組むので、
  上だけでは `POST /api/condition` が証明書検証で全滅する。
  **エラーログが出ず「加点だけゼロ」という分かりにくい症状**になる。

出力はこういう行が毎秒1本:

```json
{"ts":1789746096.91,"raw":12126,"deduction":2,"timeout":27,"score":12124,
 "tags":{"04.GraphWorst":383,"09.ReadInfoCondition":162,...}}
```

`ts` は epoch 秒なので、access.log の `time` と同じ軸。`analyze.py` が
`POST /initialize` を t=0 として両方を揃える。

## セットアップ（multipass VM 側）

```bash
./tools/setup-nginx.sh isucon11q
```

これだけ。中でやっているのは 2 つ:

1. `log_format ltsv` を `/etc/nginx/conf.d/ltsv.conf` に配置（`log_format` は http コンテキスト専用）
2. `isucondition.conf` の `server {` 直後に `access_log /var/log/nginx/access.log ltsv;` を挿入

**2 が必要な理由**: isucon11q の `nginx.conf` は http レベルで既に
`access_log /var/log/nginx/access.log main;` を定義している。nginx は同一レベルの
`access_log` を「すべて」出力するので、conf.d 側に `access_log` を足すと同じファイルに
main 形式と ltsv 形式が混ざって二重書き込みされる。server レベルで定義すれば
http レベルの指定は継承されず、ltsv だけが残る。

初回実行時に `isucondition.conf.orig` としてバックアップを取る（2回目以降は上書きしない）。
元に戻すときはこれを書き戻して `systemctl reload nginx`。

## 走行ごとの手順

```bash
./tools/rotate.sh isucon11q                                  # 前回のログを消す
#   ここでベンチを回す（上のコマンド）
./tools/collect.sh isucon11q before-index isucon11q-bench    # 回収 + report.html 生成
```

結果は `runs/<timestamp>-<label>/` に残る。改善前後でラベルを変えれば並べて比較できる。
第3引数を省くとアプリVMから `score.jsonl` を探す（ベンチ同居構成用）。

**走行ごとに必ずローテートすること。** 切らないと前回分と混ざって転送量や
リクエスト数の集計が壊れる（一度やらかした）。

## レポートの読み方

上から下まで **x 軸（経過秒）が共通**。`POST /initialize` が t=0。
`--score` を渡すと上3段のスコアパネルが増える（無ければ下5段のみ）。

1. **score (cumulative)** — 累積スコアと raw(減点前)。**線が寝た秒が頭打ちの瞬間**。
2. **score gain / sec (タグ別)** — その秒に稼いだ点数の積み上げ。
   どのタグの加点が止まったかが分かる。倍率は `bench/scenario/prepare.go` の
   `Score.Set` と同じ値を使い、最終スナップショットで検算している（ズレたら警告が出る）。
3. **deduction / timeout** — 累積の減点とタイムアウト数。立ち上がった秒が失点の瞬間。
4. **rps by endpoint**（積み上げ） — 3 系統の動きがそのまま出る。
   **伸びが止まった秒が、負荷を上げきれなくなった秒**。
5. **rps by status** — 5xx / 499（クライアント側タイムアウト切断）の発生タイミング。
6. **latency p50/p95/p99** — 既定は `GET /api/trend`。`--latency` で追加できる。
7. **in-flight** — 同時処理中リクエスト数。天井に張り付いていたら
   worker 数 / DB コネクション数 / MySQL 側で詰まっている。
8. **p95 ヒートマップ** — 縦がエンドポイント、色が p95。悪化が
   「特定 1 本から始まって周りに伝播した」のか「全体が一斉に落ちた」のかが区別できる。

読む順番としては 1 で頭打ちの秒を見つけ、2 でどの加点が止まったかを特定し、
6・8 でその秒に何が遅かったかを見る、という流れになる。

末尾の summary は **総所要時間（count × 平均レイテンシ）の降順**。
単発が遅いものより、ここの上位を削る方がスコアに効く。
スコアの内訳表は「count × 倍率 = 寄与点」で、share 列が全体に占める割合。

## オプション

```bash
python3 tools/analyze.py runs/*/access.log \
  --bucket 0.5 \                      # バケット幅。細かくすると瞬間的な詰まりが見える
  --top 20 \                          # 表示エンドポイント数
  --latency 'GET /api/trend' \
  --latency 'GET /api/isu/:uuid/graph' \
  --t0 initialize \                   # first にすると最初の行を 0 秒にする
  --score runs/xxx/score.jsonl \      # スコア推移を重ねる
  --out report.html
```

依存は標準ライブラリのみ。描画は Plotly.js を CDN から読む（オフラインでは描画されない）。

## セッション単位で順序を追いたいとき

`uid`（セッション cookie）フィールドを入れてあるので、1 ユーザのシナリオ順序を
そのまま列として取り出せる。

```bash
grep 'uid:<対象のcookie値>' runs/xxx/access.log | \
  awk -F'\t' '{print $1, $3}' | sort
```

ベンチが「設計上どういう順序で投げるか」は `bench/scenario/` を読むのが確実。
実測と突き合わせると、想定どおりに回っていないシナリオが見つかる。

## ツール一覧

| ファイル | 用途 |
|---|---|
| `provision.sh` / `patch-langs.py` | VM プロビジョニング（Go限定、約1分） |
| `setup-nginx.sh` / `nginx-ltsv.conf` | LTSV ログを有効化 |
| `rotate.sh` | 走行前にログを切る |
| `collect.sh <app-vm> <label> [bench-vm]` | ログ回収 + レポート生成 |
| `analyze.py` | LTSV(+score.jsonl) → 単一HTML |
| `measure.sh <label>` | 1走行してプロセス別CPUとスコアを出す |
| `conninfo.sh` | 接続構造（接続数・同時ストリーム・転送量） |
| `bench-patch/` | ベンチへのパッチ（score-dump / TLS） |

## 次の層（未着手）

- **レイヤ3**: `reqid`（`$request_id`）を `X-Request-Id` でアプリへ渡し、
  ハンドラと SQL のログに同じ ID を出して 1 リクエストを縦断トレースする。
  レイヤ1・2 で「どの秒のどのエンドポイントが原因か」までは絞れるので、
  そこから先（どのクエリか）が要るときに作る。
