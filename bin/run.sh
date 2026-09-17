#!/bin/bash
set -e

APP_ROLE=${APP_ROLE:-all}

cd /home/isucon/webapp

case "$APP_ROLE" in
  all|app|db) ;;
  *)
    echo "APP_ROLE must be one of: all, app, db" >&2
    exit 1
    ;;
esac

if [ "$APP_ROLE" = "all" ] || [ "$APP_ROLE" = "app" ]; then
  sudo cp -r nginx/* /etc/nginx
fi

if [ "$APP_ROLE" = "all" ] || [ "$APP_ROLE" = "db" ]; then
  sudo cp -r mysql/* /etc/mysql
  sudo chown -R root:root /etc/mysql
  sudo chmod -R 644 /etc/mysql/mariadb.conf.d/*
fi
# This leads to diff deletion or sort of stuff, IDK well...
# sudo chown -R isucon:isucon /home/isucon/webapp
# => Turned out to be due to the symlink

if [ "$APP_ROLE" = "all" ] || [ "$APP_ROLE" = "app" ]; then
  cd go
  go build -o isucondition .
  cd ..
fi


# validations
if [ "$APP_ROLE" = "all" ] || [ "$APP_ROLE" = "app" ]; then
  sudo nginx -t
fi

if [ "$APP_ROLE" = "all" ] || [ "$APP_ROLE" = "db" ]; then
  sudo systemctl restart mysql
fi

if [ "$APP_ROLE" = "all" ] || [ "$APP_ROLE" = "app" ]; then
  sudo systemctl restart isucondition.go.service  # アプリは後の方が良いかも, アプリが持つDBへの接続が切れる可能性
  sudo systemctl restart nginx
fi
