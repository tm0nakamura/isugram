#!/usr/bin/env bash
set -euo pipefail
SITE=/etc/nginx/sites-available/isucon.conf
NGINX=/etc/nginx/nginx.conf
CNF=/etc/mysql/mysql.conf.d/z-isucon.cnf

# nginx: サイト側 access_log を off
sudo sed -i "s#access_log /var/log/nginx/access.log ltsv;#access_log off;#" "$SITE"
# nginx: httpブロックのデフォルト access_log も off（二重記録防止）
sudo sed -i "s#.*access_log /var/log/nginx/access.log;.*#\taccess_log off;#" "$NGINX"

sudo nginx -t && sudo systemctl reload nginx

# mysql: 恒久ファイル書き換え → SET GLOBAL
sudo sed -i "s/^slow_query_log .*/slow_query_log          = 0/; s/^long_query_time .*/long_query_time         = 10/" "$CNF"
sudo mysql -e "SET GLOBAL slow_query_log=0; SET GLOBAL long_query_time=10;"

echo "[logs-off] applied"
