package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// SystemPayload 是系统消息（msg_type=10）的 content，JSON 编码。
// 客户端优先用 op/actor/targets 自行渲染（把自己显示为“你”），渲染不了时显示 text。
type SystemPayload struct {
	Op      string      `json:"op"` // create | join | leave | remove | rename | announce | role
	Actor   SysUser     `json:"actor"`
	Targets []SysUser   `json:"targets,omitempty"`
	Name    string      `json:"name,omitempty"`  // rename/create：新群名
	Value   string      `json:"value,omitempty"` // announce：公告内容；role：admin|member
	Text    string      `json:"text"`
	Extra   interface{} `json:"extra,omitempty"`
}

type SysUser struct {
	ID       uint64 `json:"id"`
	Nickname string `json:"nickname"`
}

// SystemText 从系统消息 content 取出可读文本。
func SystemText(content string) string {
	var p SystemPayload
	if json.Unmarshal([]byte(content), &p) == nil && p.Text != "" {
		return p.Text
	}
	return "[系统消息]"
}

// MemberInfo 群成员列表项。User 在 v0.5.0 起收紧为 PublicUser（不含 email）。
type MemberInfo struct {
	User     *PublicUser `json:"user"`
	Role     int         `json:"role"`
	ReadSeq  uint64      `json:"read_seq"`
	JoinedAt time.Time   `json:"joined_at"`
}

// CreateGroup 创建群聊，owner 为群主，members 为其他成员（已去重、不含 owner）。
func (s *Store) CreateGroup(ctx context.Context, owner uint64, name string, members []uint64) (uint64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO conversations (type, name, owner_id) VALUES (2, ?, ?)`, name, owner)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	cid := uint64(id)
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_members (conversation_id, user_id, role) VALUES (?, ?, 2)`, cid, owner); err != nil {
		return 0, err
	}
	for _, uid := range members {
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_members (conversation_id, user_id, role) VALUES (?, ?, 0)`, cid, uid); err != nil {
			return 0, err
		}
	}
	return cid, tx.Commit()
}

// GroupMeta 群基本信息。
type GroupMeta struct {
	ID      uint64
	Type    int
	Name    string
	OwnerID uint64
	LastSeq uint64
}

func (s *Store) GetGroupMeta(ctx context.Context, cid uint64) (*GroupMeta, error) {
	g := &GroupMeta{ID: cid}
	err := s.db.QueryRowContext(ctx, `SELECT type, name, owner_id, last_seq FROM conversations WHERE id = ?`, cid).
		Scan(&g.Type, &g.Name, &g.OwnerID, &g.LastSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return g, err
}

// ListMembers 返回群成员（群主、管理员在前）。
func (s *Store) ListMembers(ctx context.Context, cid uint64) ([]MemberInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.email, u.nickname, u.avatar_url, u.created_at, u.password_hash, u.avatar_key, m.role, m.read_seq, m.joined_at
		FROM conversation_members m JOIN users u ON u.id = m.user_id
		WHERE m.conversation_id = ? ORDER BY m.role DESC, m.joined_at, m.user_id`, cid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MemberInfo{}
	for rows.Next() {
		u := &User{}
		var mi MemberInfo
		if err := rows.Scan(&u.ID, &u.Email, &u.Nickname, &u.AvatarURL, &u.CreatedAt, &u.PasswordHash, &u.AvatarKey, &mi.Role, &mi.ReadSeq, &mi.JoinedAt); err != nil {
			return nil, err
		}
		u.ApplyAvatar()
		mi.User = u.Public()
		out = append(out, mi)
	}
	return out, rows.Err()
}

// AddMembers 拉人进群。新成员 join_seq = 当前 last_seq（看不到入群前的消息）。
// 返回实际新加入的用户（已在群里的忽略）。超过 max 返回 ErrFull。
func (s *Store) AddMembers(ctx context.Context, cid uint64, uids []uint64, max int) ([]uint64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var typ int
	var last uint64
	if err := tx.QueryRowContext(ctx, `SELECT type, last_seq FROM conversations WHERE id = ? FOR UPDATE`, cid).Scan(&typ, &last); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if typ != ConvGroup {
		return nil, ErrNotFound
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_members WHERE conversation_id = ?`, cid).Scan(&count); err != nil {
		return nil, err
	}
	var added []uint64
	for _, uid := range uids {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM conversation_members WHERE conversation_id = ? AND user_id = ?`, cid, uid).Scan(&one)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if count+1 > max {
			return nil, ErrFull
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_members (conversation_id, user_id, role, join_seq, read_seq)
			VALUES (?, ?, 0, ?, ?)`, cid, uid, last, last); err != nil {
			return nil, err
		}
		count++
		added = append(added, uid)
	}
	return added, tx.Commit()
}

// RemoveMember 把成员移出会话（退群或被移出）。如果移出的是群主，自动转让给
// 最早入群的管理员，没有管理员则转给最早入群的成员。返回新群主 ID（未转让为 0）。
func (s *Store) RemoveMember(ctx context.Context, cid, uid uint64) (newOwner uint64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var owner uint64
	if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM conversations WHERE id = ? FOR UPDATE`, cid).Scan(&owner); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM conversation_members WHERE conversation_id = ? AND user_id = ?`, cid, uid)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotMember
	}
	if owner == uid {
		var next uint64
		err := tx.QueryRowContext(ctx, `SELECT user_id FROM conversation_members WHERE conversation_id = ?
			ORDER BY role DESC, joined_at, user_id LIMIT 1`, cid).Scan(&next)
		if err == nil {
			if _, err := tx.ExecContext(ctx, `UPDATE conversation_members SET role = 2 WHERE conversation_id = ? AND user_id = ?`, cid, next); err != nil {
				return 0, err
			}
			newOwner = next
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE conversations SET owner_id = ?, updated_at = updated_at WHERE id = ?`, newOwner, cid); err != nil {
			return 0, err
		}
	}
	return newOwner, tx.Commit()
}

// SetRole 设置成员角色（只用于 admin/member 互换）。
func (s *Store) SetRole(ctx context.Context, cid, uid uint64, role int) error {
	res, err := s.db.ExecContext(ctx, `UPDATE conversation_members SET role = ? WHERE conversation_id = ? AND user_id = ? AND role <> 2`, role, cid, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var one int
		if e := s.db.QueryRowContext(ctx, `SELECT 1 FROM conversation_members WHERE conversation_id = ? AND user_id = ?`, cid, uid).Scan(&one); e != nil {
			return ErrNotMember
		}
	}
	return nil
}

// RenameGroup 修改群名。
func (s *Store) RenameGroup(ctx context.Context, cid uint64, name string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE conversations SET name = ?, updated_at = updated_at WHERE id = ? AND type = 2`, name, cid)
	return err
}

// SetAnnouncement 修改群公告。
func (s *Store) SetAnnouncement(ctx context.Context, cid, by uint64, text string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE conversations SET announcement = ?, announcement_by = ?, announcement_at = ?, updated_at = updated_at
		WHERE id = ? AND type = 2`, text, by, time.Now().UTC().Truncate(time.Millisecond), cid)
	return err
}
