#!/bin/bash
set -e


cd /home/isucon/webapp

sudo cp -r nginx/* /etc/nginx
sudo cp -r mysql/* /etc/mysql

sudo chown -R root:root /etc/mysql
sudo chmod -R 644 /etc/mysql/mariadb.conf.d/*
# This leads to diff deletion or sort of stuff, IDK well...
# sudo chown -R isucon:isucon /home/isucon/webapp
# => Turned out to be due to the symlink

cd go
go build -o isucondition .


# validations
sudo nginx -t

sudo systemctl restart mysql
sudo systemctl restart isucondition.go.service  # アプリは後の方が良いかも, アプリが持つDBへの接続が切れる可能性
sudo systemctl restart nginx


