-- v0.2.0：表情回应、仅自己删除。
-- 已有表新增的列/索引在 store.go 的 upgrades 中按 information_schema 检查后添加（兼容 MySQL 8 与 MariaDB，可重复执行）。

CREATE TABLE IF NOT EXISTS message_reactions (
  conversation_id BIGINT UNSIGNED NOT NULL,
  seq             BIGINT UNSIGNED NOT NULL,
  user_id         BIGINT UNSIGNED NOT NULL,
  emoji           VARCHAR(32)     NOT NULL,
  created_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (conversation_id, seq, user_id, emoji)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS message_hidden (
  user_id         BIGINT UNSIGNED NOT NULL,
  conversation_id BIGINT UNSIGNED NOT NULL,
  seq             BIGINT UNSIGNED NOT NULL,
  created_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (user_id, conversation_id, seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
