package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ---------- 在线状态（v0.3.0） ----------

// SetLastSeen 记录用户最后在线时间（最后一个连接断开时调用）。
func (s *Store) SetLastSeen(ctx context.Context, uid uint64, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET last_seen_at = ? WHERE id = ?`, t.UTC(), uid)
	return err
}

// CoMembers 返回与 uid 同在至少一个会话中的其他用户（不含 uid 自己）。
func (s *Store) CoMembers(ctx context.Context, uid uint64) ([]uint64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT m2.user_id FROM conversation_members m1
		JOIN conversation_members m2 ON m2.conversation_id = m1.conversation_id
		WHERE m1.user_id = ? AND m2.user_id <> ?`, uid, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SharedLastSeen 在 ids 中筛出与 viewer 共享至少一个会话的用户（viewer 自己只要在任一会话中也算），
// 返回 id → last_seen_at（从未断开过为 nil）。
func (s *Store) SharedLastSeen(ctx context.Context, viewer uint64, ids []uint64) (map[uint64]*time.Time, error) {
	out := map[uint64]*time.Time{}
	if len(ids) == 0 {
		return out, nil
	}
	q, args := inClause(ids)
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.last_seen_at FROM users u WHERE u.id IN (`+q+`) AND EXISTS (
			SELECT 1 FROM conversation_members m1
			JOIN conversation_members m2 ON m2.conversation_id = m1.conversation_id
			WHERE m1.user_id = ? AND m2.user_id = u.id)`, append(args, viewer)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uint64
		var ls sql.NullTime
		if err := rows.Scan(&id, &ls); err != nil {
			return nil, err
		}
		if ls.Valid {
			t := ls.Time.UTC()
			out[id] = &t
		} else {
			out[id] = nil
		}
	}
	return out, rows.Err()
}

// ---------- 语音转文字（v0.3.0） ----------

// GetTranscript 返回已缓存的转写结果；没有返回 ErrNotFound。
func (s *Store) GetTranscript(ctx context.Context, cid, seq uint64) (string, error) {
	var text string
	err := s.db.QueryRowContext(ctx,
		`SELECT text FROM message_transcripts WHERE conversation_id = ? AND seq = ?`, cid, seq).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return text, err
}

// SaveTranscript 保存转写结果（并发重复写入时保留先写入的）。
func (s *Store) SaveTranscript(ctx context.Context, cid, seq uint64, text, provider string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT IGNORE INTO message_transcripts (conversation_id, seq, text, provider) VALUES (?, ?, ?, ?)`,
		cid, seq, text, provider)
	return err
}

// DeleteTranscript 删除转写结果（撤回语音时调用）。
func (s *Store) DeleteTranscript(ctx context.Context, cid, seq uint64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM message_transcripts WHERE conversation_id = ? AND seq = ?`, cid, seq)
	return err
}
