#!/bin/bash
# isucon11q standalone プロビジョニング（Go 実装のみ）
#
# 元の cloud-init runcmd からの変更点:
#  - github release からの curl を、ホストから転送済みの /tmp/assets/* に差し替え
#  - HOME 未設定で失敗していた ~/.curlrc の行を削除
#  - クローン先を実行ごとの新規ディレクトリにし、既存ディレクトリの再帰削除をなくした
#  - langs を Go だけに絞る（/tmp/patch-langs.py。単体で検証済み）
#  - 複数ロールが /tmp/isucon11-qualify をハードコード参照するので、そこへソースを配置
#  - playbook が isucondition.go.service を enable/start しないので最後に自分で行う
set -eux

export GIT_SSL_NO_VERIFY=true
GITDIR="/opt/isucon11-qualify-src/$(date +%Y%m%d-%H%M%S)"
ASSETS="/tmp/assets"
HARDCODED="/tmp/isucon11-qualify"   # ansible ロールが直接参照するパス

mkdir -p "${GITDIR}"
git clone --depth=1 -b aarch64 https://github.com/matsuu/isucon11-qualify.git "${GITDIR}"
ln -sfn "${GITDIR}" /opt/isucon11-qualify-src/current

cd "${GITDIR}/provisioning/ansible"

# common: multipass 環境では timezone 設定と Deploy タスクが邪魔になる
sed -i -e '/timezone/d' roles/common/tasks/main.yml
sed -i -e '/name.*Deploy/,/dest/d' -e 's/^$/    recurse: yes/' roles/common/tasks/isucon11-qualify.yml

# 使うのは Go 実装だけ。xbuild による perl/python/ruby 等のソースビルドを省く
python3 /tmp/patch-langs.py

# bench / contestant 用アセット（ダウンロード済みのものを使う）
cp "${ASSETS}/initialize.json"  roles/bench/files/initialize.json
cp "${ASSETS}/images.tgz"       roles/bench/files/images.tgz
cp "${ASSETS}/1_InitData.sql"   roles/contestant/files/initial-data.sql

# 自己署名 TLS 証明書
CERTDIR=roles/contestant/files/etc/nginx/certificates
openssl req -subj '/CN=*.t.isucon.dev' -nodes -newkey rsa:2048 \
  -keyout "${CERTDIR}/tls-key.pem" -out "${CERTDIR}/tls-csr.pem"
printf 'basicConstraints=critical,CA:true,pathlen:0\nsubjectAltName=DNS.1:*.t.isucon.dev\n' \
  > "${CERTDIR}/tls-extfile.txt"
openssl x509 -in "${CERTDIR}/tls-csr.pem" -req -signkey "${CERTDIR}/tls-key.pem" \
  -sha256 -days 3650 -out "${CERTDIR}/tls-cert.pem" -extfile "${CERTDIR}/tls-extfile.txt"

# standalone(1台構成)向けの調整
mkdir -p /var/lib/cloud/scripts/per-instance
sed -i -e '/^index=/s/=.*/=1/' roles/contestant/files/var/lib/cloud/scripts/per-instance/generate-env_aws.sh
sed -i -e 's/192\.168\.0/127.0.0/' roles/contestant/files/etc/hosts

# ロールが /tmp/isucon11-qualify を直接参照する（webapp / bench / extra のソース）
mkdir -p "${HARDCODED}"
cp -a "${GITDIR}/." "${HARDCODED}/"
chown -R isucon:isucon "${HARDCODED}" 2>/dev/null || true

ansible-playbook -i standalone.hosts --connection=local site.yml
/var/lib/cloud/scripts/per-instance/generate-env.sh

# ベンチが自己署名証明書を信頼できるようにする
mkdir -p /usr/share/ca-certificates/isucon
cp /etc/nginx/certificates/tls-cert.pem /usr/share/ca-certificates/isucon
grep -q '^isucon/tls-cert.pem$' /etc/ca-certificates.conf || echo "isucon/tls-cert.pem" >> /etc/ca-certificates.conf
update-ca-certificates

# playbook はサービスファイルを置くだけで有効化しないので、ここで起動する
systemctl daemon-reload
systemctl enable --now isucondition.go.service

# jiaapi-mock は起動してはいけない。
# ベンチは自前の ISU協会サービスを :5000 に立てるため、常駐していると
#   panic: ISU協会サービスが異常終了しました:
#          listen tcp 0.0.0.0:5000: bind: address already in use
# でベンチが落ちる。手動で疎通確認したいとき「だけ」起動する。
systemctl disable --now jiaapi-mock.service || true

echo "=== PROVISION DONE ==="
