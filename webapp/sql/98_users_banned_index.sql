ALTER TABLE users ADD INDEX idx_banned (authority, del_flg, created_at);
