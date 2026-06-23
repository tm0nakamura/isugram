#!/usr/bin/env bash
set -euo pipefail
SITE=/etc/nginx/sites-available/isucon.conf
CNF=/etc/mysql/mysql.conf.d/z-isucon.cnf
sudo sed -i "s#access_log off;#access_log /var/log/nginx/access.log ltsv;#" "$SITE"
sudo nginx -t && sudo systemctl reload nginx
sudo sed -i "s/^slow_query_log .*/slow_query_log          = 1/; s/^long_query_time .*/long_query_time         = 0/" "$CNF"
sudo mysql -e "SET GLOBAL slow_query_log=1; SET GLOBAL long_query_time=0;"
echo "[logs-on] applied"
