#!/usr/bin/env bash
set -euo pipefail
SITE=/etc/nginx/sites-available/isucon.conf
NGINX=/etc/nginx/nginx.conf
CNF=/etc/mysql/mysql.conf.d/z-isucon.cnf

# nginx: サイト側 access_log を ltsv で復元
sudo sed -i "s#access_log off;#access_log /var/log/nginx/access.log ltsv;#" "$SITE"
# nginx: httpブロックのデフォルト access_log もコメントアウトに戻す
sudo sed -i "s#^\taccess_log off;#\t#access_log /var/log/nginx/access.log;#" "$NGINX"

sudo nginx -t && sudo systemctl reload nginx

# mysql
sudo sed -i "s/^slow_query_log .*/slow_query_log          = 1/; s/^long_query_time .*/long_query_time         = 0/" "$CNF"
sudo mysql -e "SET GLOBAL slow_query_log=1; SET GLOBAL long_query_time=0;"

echo "[logs-on] applied"
