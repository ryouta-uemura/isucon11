#!/bin/bash
set -e


cd /home/isucon/webapp

sudo cp -r nginx/* /etc/nginx
sudo cp -r mysql/* /etc/mysql

sudo chown -R isucon:isucon /home/isucon/webapp

cd go
go build -o isucondition .

sudo systemctl restart isucondition.go.service
sudo systemctl restart nginx
sudo systemctl restart mysql


