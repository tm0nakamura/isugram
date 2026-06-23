.RECIPEPREFIX = >
APP      := isu-go
SLOW     := /var/log/mysql/mysql-slow.log
ACCESS   := /var/log/nginx/access.log
ERRLOG   := /var/log/nginx/error.log
BENCH    := ./benchmarker/bin/benchmarker
USERDATA := ./benchmarker/userdata
ALP_M    := /image/[0-9]+\.(jpg|png|gif),/posts/[0-9]+,/@[0-9a-zA-Z_]+
.PHONY: help b bench logclean slow alp restart reload df reboot-check enable
b: logclean bench
bench:
> $(BENCH) -u $(USERDATA) -t http://localhost
logclean:
> sudo truncate -s 0 $(SLOW) $(ACCESS) $(ERRLOG)
slow:
> sudo mysqldumpslow -s t -t 10 $(SLOW)
alp:
> alp ltsv --file $(ACCESS) -m '$(ALP_M)' --sort sum -r
restart:
> cd webapp/golang && make
> sudo systemctl restart $(APP)
reload:
> sudo nginx -t && sudo systemctl reload nginx
df:
> df -h /
reboot-check:
> systemctl is-enabled $(APP) nginx mysql
enable:
> sudo systemctl enable $(APP) nginx mysql
