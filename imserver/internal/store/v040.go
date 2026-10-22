package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ---------- 头像 / 昵称（v0.4.0） ----------

// UpdateUserProfile 修改昵称和/或头像 key（nil 表示不改动该字段），返回更新后的用户。
func (s *Store) UpdateUserProfile(ctx context.Context, uid uint64, nickname, avatarKey *string) (*User, error) {
	if nickname != nil {
		if _, err := s.db.ExecContext(ctx, `UPDATE users SET nickname = ? WHERE id = ?`, *nickname, uid); err != nil {
			return nil, err
		}
	}
	if avatarKey != nil {
		if _, err := s.db.ExecContext(ctx, `UPDATE users SET avatar_key = ? WHERE id = ?`, *avatarKey, uid); err != nil {
			return nil, err
		}
	}
	return s.GetUser(ctx, uid)
}

// ---------- token_version / 改密（v0.4.0） ----------

// GetTokenVersion 读取用户当前的登录态版本（JWT tv 比对用）。
func (s *Store) GetTokenVersion(ctx context.Context, uid uint64) (int, error) {
	var tv int
	err := s.db.QueryRowContext(ctx, `SELECT token_version FROM users WHERE id = ?`, uid).Scan(&tv)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return tv, err
}

// ChangePassword 更新密码哈希并递增 token_version（旧 token 全部失效），返回新版本号。
func (s *Store) ChangePassword(ctx context.Context, uid uint64, hash string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ?, token_version = token_version + 1 WHERE id = ?`, hash, uid); err != nil {
		return 0, err
	}
	var tv int
	if err := tx.QueryRowContext(ctx, `SELECT token_version FROM users WHERE id = ?`, uid).Scan(&tv); err != nil {
		return 0, err
	}
	return tv, tx.Commit()
}

// ---------- 重置密码 token（v0.4.0） ----------
// 库里只存 token 的 SHA-256 哈希（token_hash），明文只出现在邮件链接里。

// CreateResetToken 保存新 token 的哈希；同一用户旧的未用 token 一并删除（一人同时只有一个有效链接）。
func (s *Store) CreateResetToken(ctx context.Context, uid uint64, tokenHash string, expiresAt time.Time, requestIP string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM password_reset_tokens WHERE user_id = ? AND used_at IS NULL`, uid); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO password_reset_tokens (token_hash, user_id, expires_at, request_ip) VALUES (?, ?, ?, ?)`,
		tokenHash, uid, expiresAt.UTC(), requestIP); err != nil {
		return err
	}
	return tx.Commit()
}

// ResetPasswordWithToken 在一个事务内完成重置：校验 token（存在、未用、未过期）→
// 更新密码哈希 → 标记 token 已用 → 删除该用户其他 token → token_version + 1。
// token 无效（不存在/过期/已用）统一返回 ErrNotFound，调用方回 bad_token 不区分原因。
func (s *Store) ResetPasswordWithToken(ctx context.Context, tokenHash, pwHash string) (uint64, int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	var uid uint64
	var expiresAt time.Time
	var usedAt sql.NullTime
	err = tx.QueryRowContext(ctx,
		`SELECT user_id, expires_at, used_at FROM password_reset_tokens WHERE token_hash = ? FOR UPDATE`, tokenHash).
		Scan(&uid, &expiresAt, &usedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, ErrNotFound
	}
	if err != nil {
		return 0, 0, err
	}
	if usedAt.Valid || time.Now().After(expiresAt) {
		return 0, 0, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ?, token_version = token_version + 1 WHERE id = ?`, pwHash, uid); err != nil {
		return 0, 0, err
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := tx.ExecContext(ctx, `UPDATE password_reset_tokens SET used_at = ? WHERE token_hash = ?`, now, tokenHash); err != nil {
		return 0, 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM password_reset_tokens WHERE user_id = ? AND token_hash <> ?`, uid, tokenHash); err != nil {
		return 0, 0, err
	}
	var tv int
	if err := tx.QueryRowContext(ctx, `SELECT token_version FROM users WHERE id = ?`, uid).Scan(&tv); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return uid, tv, nil
}

// ---------- 通话记录（v0.4.0） ----------

// CallContent 通话记录消息（msg_type=6）的 content。
type CallContent struct {
	CallID     string `json:"call_id"`
	Kind       string `json:"kind"`    // audio | video
	Outcome    string `json:"outcome"` // completed | missed | rejected | cancelled | busy | failed
	DurationMs int64  `json:"duration_ms"`
	CallerID   uint64 `json:"caller_id"`
}

// InsertCallRecord 以主叫为 sender 插入一条通话记录消息（复用 InsertMessage 的 seq 分配）。
// client_msg_id 用 call_id 派生，保证同一通话重复调用幂等。
func (s *Store) InsertCallRecord(ctx context.Context, cid uint64, cc CallContent) (*Message, error) {
	b, _ := json.Marshal(cc)
	msg := &Message{
		ConversationID: cid, SenderID: cc.CallerID,
		ClientMsgID: "call-" + cc.CallID, MsgType: MsgCall, Content: string(b),
	}
	if _, err := s.InsertMessage(ctx, msg); err != nil {
		return nil, err
	}
	return msg, nil
}
