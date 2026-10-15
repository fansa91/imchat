-- v0.3.0：语音转文字结果缓存。
-- users.last_seen_at 列在 store.go 的 upgradeColumns 中按 information_schema 检查后添加（可重复执行）。

CREATE TABLE IF NOT EXISTS message_transcripts (
  conversation_id BIGINT UNSIGNED NOT NULL,
  seq             BIGINT UNSIGNED NOT NULL,
  text            TEXT            NOT NULL,
  provider        VARCHAR(32)     NOT NULL DEFAULT '',
  created_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (conversation_id, seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
