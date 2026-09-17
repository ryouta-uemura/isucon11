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
  sudo cp nginx/nginx.conf /etc/nginx/nginx.conf
  sudo cp -r nginx/sites-available /etc/nginx/
  sudo cp -r nginx/sites-enabled /etc/nginx/
  sudo cp -r nginx/snippets /etc/nginx/
  sudo cp nginx/*.params /etc/nginx/
  sudo cp nginx/fastcgi.conf /etc/nginx/fastcgi.conf
  sudo cp nginx/mime.types /etc/nginx/mime.types
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
  if [ -n "${MYSQL_APP_HOST:-}" ]; then
    ./bin/grant_mysql_app.sh
  fi
fi

if [ "$APP_ROLE" = "all" ] || [ "$APP_ROLE" = "app" ]; then
  sudo systemctl restart isucondition.go.service  # アプリは後の方が良いかも, アプリが持つDBへの接続が切れる可能性
  sudo systemctl restart nginx
fi
