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
export POST_ISUCONDITION_TARGET_BASE_URL=https://<APP_VM_PUBLIC_HOST_OR_IP>
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
export POST_ISUCONDITION_TARGET_BASE_URL=https://<APP_VM_PUBLIC_HOST_OR_IP>
EOF

. ./env.sh
APP_ROLE=app ./bin/run.sh
```

実IPやcredentialが入った `env.sh` はコミットしない。
