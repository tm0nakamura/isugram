-- #1 comment_count 非正規化カラム (B: app/SQL)
-- 表示時の GROUP BY COUNT を廃止し posts.comment_count を読むだけにする。
-- コメント投稿時に postComment が同一Txで +1、/initialize(getInitialize/dbInitialize) で実件数に再構築する。
-- 適用: mysql -u isuconp -pisuconp isuconp < webapp/sql/99_add_comment_count.sql
ALTER TABLE posts ADD COLUMN comment_count INT NOT NULL DEFAULT 0;
UPDATE posts SET comment_count = (SELECT COUNT(*) FROM comments WHERE comments.post_id = posts.id);
