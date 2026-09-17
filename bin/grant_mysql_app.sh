#!/bin/bash
set -euo pipefail

MYSQL_APP_HOST=${MYSQL_APP_HOST:?set MYSQL_APP_HOST to the app VM IP or MySQL host pattern}
MYSQL_USER=${MYSQL_USER:-isucon}
MYSQL_PASS=${MYSQL_PASS:-isucon}
MYSQL_DBNAME=${MYSQL_DBNAME:-isucondition}

case "$MYSQL_APP_HOST" in
  *[!0-9A-Za-z._:%-]*)
    echo "MYSQL_APP_HOST contains unsupported characters: $MYSQL_APP_HOST" >&2
    exit 1
    ;;
esac

case "$MYSQL_USER$MYSQL_DBNAME" in
  *[!0-9A-Za-z_]*)
    echo "MYSQL_USER and MYSQL_DBNAME must contain only [0-9A-Za-z_]" >&2
    exit 1
    ;;
esac

escaped_password=${MYSQL_PASS//\\/\\\\}
escaped_password=${escaped_password//\'/\'\'}

sudo mysql --defaults-file=/dev/null <<SQL
CREATE USER IF NOT EXISTS '${MYSQL_USER}'@'${MYSQL_APP_HOST}' IDENTIFIED BY '${escaped_password}';
ALTER USER '${MYSQL_USER}'@'${MYSQL_APP_HOST}' IDENTIFIED BY '${escaped_password}';
GRANT ALL PRIVILEGES ON \`${MYSQL_DBNAME}\`.* TO '${MYSQL_USER}'@'${MYSQL_APP_HOST}';
FLUSH PRIVILEGES;
SQL
