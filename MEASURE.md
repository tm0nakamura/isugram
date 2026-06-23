# MEASURE.md — 計測・司令塔ログ（セッションA専用）

private-isu (isugram) ISUCON 練習の司令塔。**Aだけが編集する。** アプリ/インフラのコードは触らない。

## 実行環境（実サーバー）
- ホスト: ip-192-168-1-10 / Ubuntu 24.04 / **2 vCPU / 3.7GiB RAM**（AWS performance-tuning workshop）
- アプリ配置: `/home/isucon/private_isu`（git管理 → origin: github.com/tm0nakamura/isugram, SSH deploy key）
- 稼働実装: **Ruby**（`isu-ruby.service`）。go/node/python も導入済み
- レギュレーション: `public_manual.md`（違反で pass:false）

### サービス操作
| 用途 | コマンド |
|------|---------|
| アプリ再起動 | `sudo systemctl restart isu-ruby` |
| 言語切替(例:Go) | `sudo systemctl stop isu-ruby; sudo systemctl disable isu-ruby; sudo systemctl enable isu-go; sudo systemctl start isu-go` |
| nginx | `sudo systemctl reload nginx`（設定: /etc/nginx/sites-available/isucon.conf） |
| MySQL | `sudo systemctl restart mysql`（接続: `mysql -u isuconp -pisuconp isuconp`） |
| memcached | localhost:11211 |

サービス名: Ruby=isu-ruby / Go=isu-go / PHP=php8.3-fpm(+nginx差替) / Python=isu-python / Node=isu-node

### ベンチ実行（Aだけが叩く）
```sh
/home/isucon/private_isu/benchmarker/bin/benchmarker \
  -u /home/isucon/private_isu/benchmarker/userdata \
  -t http://localhost
```
出力例: `{"pass":true,"score":1710,"success":1434,"fail":0,"messages":[]}`

## セッション構成（ソロ3分割）
| | 役割 | 触る場所 |
|--|------|---------|
| **A 計測/司令塔** | ベンチ→alp/slow log解析→次の一手 | ログ設定・本ファイル（実コードは触らない） |
| **B アプリ/SQL** | N+1・index・キャッシュ | webapp/<lang>/, 初期化SQL |
| **C ミドルウェア** | nginx/my.cnf/静的配信/画像ファイル出し | /etc/nginx/, /etc/mysql/ |

### 運用ルール
1. 1施策=1コミット、毎回ベンチ。 2. ベンチはAだけ。 3. B/CをAがマージ→ベンチ→記録。 4. 迷ったらB優先。 5. 終盤Cで再起動試験＋ログOFF。

## 計測ツール
### alp（nginx）: log_formatをLTSV化し request_time/upstream_response_time を出す（Cが/etc/nginxに反映）
```
alp ltsv --file /var/log/nginx/access.log -m "/image/[0-9]+,/posts/[0-9]+,/@\w+" --sort sum -r
```
### pt-query-digest（MySQL）: slow_query_log=1 / long_query_time=0（Cが/etc/mysqlに反映）
```
sudo pt-query-digest /var/log/mysql/slow.log
```

## ボトルネック分析（静的解析 2026-06-23）
スキーマ: posts/comments はPKのみ、users は account_name UNIQUEのみ。セカンダリindexゼロ。

| # | 施策 | 担当 | 急所 |
|---|------|------|------|
| 1 | パスワードhashのopenssl shell-out廃止→Digest::SHA512 | B | app.rb:79-82 毎ログインでプロセスfork×2 |
| 2 | index追加 posts(created_at)/posts(user_id)/comments(post_id,created_at)/comments(user_id) | B | 全テーブルindex無し |
| 3 | make_posts のN+1解消→IN句一括 or JOIN | B | app.rb:102-132 |
| 4 | 画像をファイル出し→nginx静的配信 | B→C | DB BLOB配信撤廃 |
| 5 | 静的アセットをnginx直配信 | C | 今はlocation /で全部appにproxy |
| 6 | GET / の全件取得→JOINでdel_flg絞り+LIMIT | B | app.rb:228-229 |
| 7 | /posts/:id,/image の SELECT * でimgdata不要ロード回避 | B | app.rb:281,347 |
| 8 | MySQL innodb_buffer_pool_size 等 / slow log | C | /etc/mysql |
| 9 | get_session_user の毎回users引き→cache | B | app.rb:92-100 |

## ベンチ記録
| # | 日時 | 施策 | スコア | success | fail | 担当 | 備考 |
|---|------|------|--------|---------|------|------|------|
| 0 | 2026-06-23 | 初期状態(Ruby) | 565 | 562 | 4 | A | POST /login,/register timeout → #1 passhash裏付け |
