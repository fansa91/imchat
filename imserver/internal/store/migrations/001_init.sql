CREATE TABLE IF NOT EXISTS users (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  email         VARCHAR(255)    NOT NULL,
  password_hash VARCHAR(100)    NOT NULL,
  nickname      VARCHAR(64)     NOT NULL DEFAULT '',
  avatar_url    VARCHAR(512)    NOT NULL DEFAULT '',
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_email (email)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS conversations (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  type       TINYINT         NOT NULL COMMENT '1=单聊 2=群聊',
  last_seq   BIGINT UNSIGNED NOT NULL DEFAULT 0,
  created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS direct_pairs (
  user_low        BIGINT UNSIGNED NOT NULL,
  user_high       BIGINT UNSIGNED NOT NULL,
  conversation_id BIGINT UNSIGNED NOT NULL,
  PRIMARY KEY (user_low, user_high)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS conversation_members (
  conversation_id BIGINT UNSIGNED NOT NULL,
  user_id         BIGINT UNSIGNED NOT NULL,
  read_seq        BIGINT UNSIGNED NOT NULL DEFAULT 0,
  joined_at       DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (conversation_id, user_id),
  KEY idx_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS messages (
  conversation_id BIGINT UNSIGNED NOT NULL,
  seq             BIGINT UNSIGNED NOT NULL,
  sender_id       BIGINT UNSIGNED NOT NULL,
  client_msg_id   VARCHAR(64)     NOT NULL,
  msg_type        TINYINT         NOT NULL DEFAULT 1 COMMENT '1=文本 2=图片 3=语音 4=视频 5=文件',
  content         TEXT            NOT NULL,
  created_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (conversation_id, seq),
  UNIQUE KEY uk_sender_client (sender_id, client_msg_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
