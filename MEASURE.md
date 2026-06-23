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
| 1 | 2026-06-23 | #2 index + #3 N+1解消 + #6 GET/最適化 + nginx(#4準備) | 18748 | 17738 | 0 | B/C | Ruby稼働。fail=0、login/register timeout解消 |

## レギュレーション（厳守）
**変更禁止:** URI(ポート/パス) / HTML DOM構造 / JS・CSS内容 / 画像・メディア内容
**許可:** DBスキーマ・index変更、キャッシュ/jobqueue/遅延書込、他言語再実装、設定変更（外部リソース委譲は禁止、1台のみ）
**必須:** /initialize等メンテコマンド互換 / 任意タイミングの再起動に耐える / 再起動後も全コード正常動作 / ベンチ書込データは再起動後も取得可能（投稿・コメント・画像は永続化必須）
**採点:** ベンチ1分、POSTは即座に関連GETへ反映、DOM不変、成功リクエスト数ベース（種類別配点）・エラー減点

### 最適化時の遵守ポイント
- N+1/テンプレ最適化は **出力HTMLを完全一致** させる（DOM不変）
- キャッシュは **書込時に必ず無効化**（POST→GET即反映）。一次データを揮発ストア(memcached)に置かない
- 画像ファイル出し(#4)後も /initialize を壊さない。投稿画像はFS永続化

## AMI既存状態（2026-06-23 確認）
- nginx: 静的配信(/css,/js,/img,favicon)・`/image/`のtry_files・LTSVログ → **設定済み**（#5完了、#4はnginx側完了＝Bの画像ファイル出し待ち）
- MySQL: slow_query_log=1, flush_log_at_trx_commit=2 → 設定済み。buffer_pool=1024MB
- **posts=1204MB（imgdata BLOB）でbuffer_poolに乗り切らない** → #4でDBが激減し全乗り。最重要
- unicorn worker_processes=1（2vCPU）→ 機会損失

## 追加対策バックログ
| # | 施策 | 担当 | 効果/メモ |
|---|------|------|----------|
| A | unicorn worker_processes 1→4程度 | B | 簡単・高効果。webapp/ruby/unicorn_config.rb |
| B | comment_count 非正規化/キャッシュ(COUNT(*)毎post廃止) | B | 書込時に無効化 |
| C | prepared statement 再prepare回避 | B | mysql2 |
| D | nginx→app keepalive(upstream+http1.1+Connection"") | C | TCP張り直し削減 |
| E | gzip(css/js)/sendfile/tcp_nopush/open_file_cache | C | |
| F | OS: somaxconn, nginx worker_connections/auto, fd上限 | C | 終盤 |
| G | Go実装へ切替(systemctl) | 戦略 | Rubyの2〜5倍。#1-9流用可 |

注: flush_log_at_trx_commit は再起動データ保全のため 1 or 2 を維持（0は不可）。

## 言語方針: Go採用（2026-06-23 決定）
稼働実装をRuby→**Go(webapp/golang/)**に切替える。以降Bはwebapp/golang/を編集、CはアプリをisugoとしてrestartするService=isu-go。
切替: `sudo systemctl stop isu-ruby; sudo systemctl disable isu-ruby; cd webapp/golang && make; sudo systemctl enable isu-go; sudo systemctl start isu-go`
ビルド: `cd webapp/golang && make`（go build -o app）。コード変更後は `make && sudo systemctl restart isu-go`

### Go版ボトルネック（app.go、行番号確認済み）
| # | 施策 | 箇所 |
|---|------|------|
| 1 | digest()のopenssl shell-out廃止→crypto/sha512 | app.go:122-139 |
| G1 | テンプレを毎req ParseFiles→起動時1回パース(global) | app.go:281,321,413,498,554,595,768 (template.Must×7) |
| G2 | DB接続プール設定 SetMaxOpenConns/Idle/ConnMaxLifetime | main() app.go:847付近 |
| 2 | index追加(posts/comments) ※SQLをrepoに保存し適用 | DB |
| 3 | makePosts N+1解消(comments/usersをIN一括) | app.go:178-227 |
| 6 | getIndex 全件→JOINでdel_flg絞り+LIMIT | app.go:391-401 |
| 7 | SELECT * のimgdata不要ロード回避(一覧/詳細) | app.go:570,696 |
| 4 | 画像ファイル出し(upload時+既存dump)→nginx配信(設定済) | app.go:663,686 |
| 9 | getSessionUserのcache | app.go:147-163 |
| B | comment_count非正規化/cache | app.go:182 |
