# isucon11 deployment notes

## 2VM構成

このブランチはApp VMとDB VMを分けて動かせる。

- App VM: nginx + Go app
- DB VM: MySQL/MariaDB

### DB VM

`MYSQL_APP_HOST` にApp VMのprivate IPを入れて、DB roleで反映する。

```bash
cd /home/isucon/webapp
MYSQL_APP_HOST=<APP_VM_PRIVATE_IP> APP_ROLE=db ./bin/run.sh
```

`MYSQL_APP_HOST` はMySQLの接続許可を作るために使う。

```sql
GRANT ALL PRIVILEGES ON isucondition.* TO 'isucon'@'<APP_VM_PRIVATE_IP>';
```

MySQL接続情報はデフォルトだと以下。

```bash
MYSQL_USER=isucon
MYSQL_PASS=isucon
MYSQL_DBNAME=isucondition
```

変える場合は環境変数で上書きする。

### App VM

アプリはデフォルトだとローカルのMySQL socketを見る。DB VMにつなぐ場合は `MYSQL_HOST` を指定する。

```bash
cd /home/isucon/webapp
export MYSQL_HOST=<DB_VM_PRIVATE_IP>
export MYSQL_PORT=3306
export MYSQL_USER=isucon
export MYSQL_PASS=isucon
export MYSQL_DBNAME=isucondition
export POST_ISUCONDITION_TARGET_BASE_URL=https://isucondition.t.isucon.dev
APP_ROLE=app ./bin/run.sh
```

毎回exportする代わりに、VM上のローカルファイルとして `env.sh` に書いておいてもよい。

```bash
cat > env.sh <<'EOF'
export MYSQL_HOST=<DB_VM_PRIVATE_IP>
export MYSQL_PORT=3306
export MYSQL_USER=isucon
export MYSQL_PASS=isucon
export MYSQL_DBNAME=isucondition
export POST_ISUCONDITION_TARGET_BASE_URL=https://isucondition.t.isucon.dev
EOF

. ./env.sh
APP_ROLE=app ./bin/run.sh
```

実IPやcredentialが入った `env.sh` はコミットしない。

### Benchmark

ISUCON11のbenchは `-target` にIPアドレスを指定する。TLSのServerNameは `-all-addresses` の順番から以下のように内部で対応付けられる。

- 1番目: `isucondition-1.t.isucon.dev`
- 2番目: `isucondition-2.t.isucon.dev`
- 3番目: `isucondition-3.t.isucon.dev`

```bash
cd /home/isucon/webapp
BENCH_TARGET_ADDR=<APP_VM_PRIVATE_IP> ./bin/bench
```

必要なら全VMのIPやJIA URLも環境変数で上書きできる。

```bash
BENCH_TARGET_ADDR=<APP_VM_PRIVATE_IP> \
BENCH_ALL_ADDRESSES=<APP_VM_PRIVATE_IP>,<DB_VM_PRIVATE_IP> \
JIA_SERVICE_URL=http://127.0.0.1:4999 \
./bin/bench
```
