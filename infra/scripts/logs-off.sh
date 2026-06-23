#!/usr/bin/env bash
set -euo pipefail
SITE=/etc/nginx/sites-available/isucon.conf
CNF=/etc/mysql/mysql.conf.d/z-isucon.cnf
sudo sed -i "s#access_log /var/log/nginx/access.log ltsv;#access_log off;#" "$SITE"
sudo nginx -t && sudo systemctl reload nginx
sudo sed -i "s/^slow_query_log .*/slow_query_log          = 0/; s/^long_query_time .*/long_query_time         = 10/" "$CNF"
sudo mysql -e "SET GLOBAL slow_query_log=0; SET GLOBAL long_query_time=10;"
echo "[logs-off] applied"
