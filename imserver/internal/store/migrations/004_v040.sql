-- v0.4.0：密码重置 token（只存 SHA-256 哈希；明文只出现在邮件链接里）。
-- users.token_version / users.avatar_key 两列在 store.go 的 upgradeColumns 中
-- 按 information_schema 检查后添加（可重复执行）。

CREATE TABLE IF NOT EXISTS password_reset_tokens (
  token_hash  CHAR(64)        NOT NULL COMMENT 'token 明文的 SHA-256（hex）',
  user_id     BIGINT UNSIGNED NOT NULL,
  expires_at  DATETIME(3)     NOT NULL,
  used_at     DATETIME(3)     NULL,
  created_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  request_ip  VARCHAR(64)     NOT NULL DEFAULT '',
  PRIMARY KEY (token_hash),
  KEY idx_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
