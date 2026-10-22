-- v0.5.0：好友系统（申请 + 有向边）。
-- users.allow_find 一列在 store.go 的 upgradeColumns 中按 information_schema
-- 检查后添加（可重复执行），不写进本迁移的 ALTER。

CREATE TABLE IF NOT EXISTS friend_requests (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  from_id    BIGINT UNSIGNED NOT NULL,
  to_id      BIGINT UNSIGNED NOT NULL,
  message    VARCHAR(128)    NOT NULL DEFAULT '' COMMENT '申请附言',
  status     TINYINT         NOT NULL DEFAULT 0 COMMENT '0 待处理 1 已接受 2 已拒绝 3 已撤回',
  created_at DATETIME(3)     NOT NULL,
  handled_at DATETIME(3)     NULL,
  PRIMARY KEY (id),
  KEY idx_pair (from_id, to_id),
  KEY idx_to_status (to_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS friend_edges (
  user_id    BIGINT UNSIGNED NOT NULL,
  friend_id  BIGINT UNSIGNED NOT NULL,
  remark     VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '备注名：单向，只对 user_id 本人可见',
  created_at DATETIME(3)     NOT NULL,
  PRIMARY KEY (user_id, friend_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
