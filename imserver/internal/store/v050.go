package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ---------- 公开用户 DTO（v0.5.0，SPEC §1.2） ----------

// PublicUser 一切「他人视角」响应使用的用户结构：只有 id / nickname / avatar_url，
// 绝不含 email 等隐私字段。适用：用户搜索、好友列表/申请、群成员、会话对端信息。
type PublicUser struct {
	ID        uint64 `json:"id"`
	Nickname  string `json:"nickname"`
	AvatarURL string `json:"avatar_url"`
}

// Public 把完整用户收紧为公开 DTO（avatar_url 已由 ApplyAvatar 拼装）。
func (u *User) Public() *PublicUser {
	if u == nil {
		return nil
	}
	return &PublicUser{ID: u.ID, Nickname: u.Nickname, AvatarURL: u.AvatarURL}
}

// ---------- allow_find 隐私开关（v0.5.0，SPEC §2.1） ----------

// SetAllowFind 设置「允许通过邮箱 / 用户 ID 找到我」。
func (s *Store) SetAllowFind(ctx context.Context, uid uint64, allow bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET allow_find = ? WHERE id = ?`, allow, uid)
	return err
}

// ---------- 退出其他设备（v0.5.0，SPEC §2.1） ----------

// BumpTokenVersion 只递增 token_version（不改密码），返回新版本号。
// 语义与 ChangePassword 的吊销部分相同：旧 token 全部失效，调用方给当前设备换发新 token。
func (s *Store) BumpTokenVersion(ctx context.Context, uid uint64) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE users SET token_version = token_version + 1 WHERE id = ?`, uid); err != nil {
		return 0, err
	}
	var tv int
	if err := tx.QueryRowContext(ctx, `SELECT token_version FROM users WHERE id = ?`, uid).Scan(&tv); err != nil {
		return 0, err
	}
	return tv, tx.Commit()
}

// ---------- 好友系统（v0.5.0，SPEC §1） ----------

// 好友申请状态（friend_requests.status）
const (
	FriendReqPending   = 0
	FriendReqAccepted  = 1
	FriendReqRejected  = 2
	FriendReqWithdrawn = 3
)

var (
	ErrAlreadyFriends = errors.New("already friends")
	ErrRequestPending = errors.New("friend request pending")
	ErrNotFriends     = errors.New("not friends")
	ErrRequestHandled = errors.New("friend request already handled")
)

// FriendRequest 一条好友申请（原始行）。
type FriendRequest struct {
	ID        uint64
	FromID    uint64
	ToID      uint64
	Message   string
	Status    int
	CreatedAt time.Time
	HandledAt *time.Time
}

// FriendRequestView 申请列表项：user 是「对方」（incoming 看 from、outgoing 看 to）。
type FriendRequestView struct {
	ID        uint64      `json:"id"`
	User      *PublicUser `json:"user"`
	Message   string      `json:"message"`
	CreatedAt time.Time   `json:"created_at"`
}

// FriendView 好友列表项：user 是好友本人，Remark 是我给对方的备注（单向）。
type FriendView struct {
	User      *PublicUser `json:"user"`
	Remark    string      `json:"remark"`
	CreatedAt time.Time   `json:"created_at"`
}

// IsFriend 两人之间是否存在好友边（边成对存储，查一条即可）。
func (s *Store) IsFriend(ctx context.Context, a, b uint64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM friend_edges WHERE user_id = ? AND friend_id = ?`, a, b).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// FriendRelation 返回 me 相对 other 的关系：none | friend | pending_out | pending_in。
func (s *Store) FriendRelation(ctx context.Context, me, other uint64) (string, error) {
	if ok, err := s.IsFriend(ctx, me, other); err != nil {
		return "", err
	} else if ok {
		return "friend", nil
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM friend_requests WHERE from_id = ? AND to_id = ? AND status = ?`,
		me, other, FriendReqPending).Scan(&n); err != nil {
		return "", err
	}
	if n > 0 {
		return "pending_out", nil
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM friend_requests WHERE from_id = ? AND to_id = ? AND status = ?`,
		other, me, FriendReqPending).Scan(&n); err != nil {
		return "", err
	}
	if n > 0 {
		return "pending_in", nil
	}
	return "none", nil
}

func scanFriendRequest(row interface{ Scan(...any) error }) (*FriendRequest, error) {
	fr := &FriendRequest{}
	var handled sql.NullTime
	if err := row.Scan(&fr.ID, &fr.FromID, &fr.ToID, &fr.Message, &fr.Status, &fr.CreatedAt, &handled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if handled.Valid {
		t := handled.Time
		fr.HandledAt = &t
	}
	return fr, nil
}

const friendReqCols = `id, from_id, to_id, message, status, created_at, handled_at`

// GetFriendRequest 按 ID 读取申请；不存在返回 ErrNotFound。
func (s *Store) GetFriendRequest(ctx context.Context, id uint64) (*FriendRequest, error) {
	return scanFriendRequest(s.db.QueryRowContext(ctx,
		`SELECT `+friendReqCols+` FROM friend_requests WHERE id = ?`, id))
}

// insertFriendEdgesTx 在事务内成对插入好友边（已存在则忽略）。
func insertFriendEdgesTx(ctx context.Context, tx *sql.Tx, a, b uint64, now time.Time) error {
	for _, p := range [][2]uint64{{a, b}, {b, a}} {
		if _, err := tx.ExecContext(ctx,
			`INSERT IGNORE INTO friend_edges (user_id, friend_id, created_at) VALUES (?, ?, ?)`,
			p[0], p[1], now); err != nil {
			return err
		}
	}
	return nil
}

// SendFriendRequest 发送好友申请（事务内校验，同一对用户最多一条待处理）：
//   - 已是好友 → ErrAlreadyFriends
//   - 同向已有待处理 → ErrRequestPending
//   - 反向已有待处理 → 直接接受那一条、建双边，返回 (该申请 ID, true)
//   - 否则新建待处理申请，返回 (新 ID, false)
//
// 目标用户是否存在由调用方先校验（404 not_found）。
func (s *Store) SendFriendRequest(ctx context.Context, from, to uint64, message string) (uint64, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM friend_edges WHERE user_id = ? AND friend_id = ? FOR UPDATE`, from, to).Scan(&one)
	if err == nil {
		return 0, false, ErrAlreadyFriends
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	var pendingID uint64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM friend_requests WHERE from_id = ? AND to_id = ? AND status = ? FOR UPDATE`,
		from, to, FriendReqPending).Scan(&pendingID)
	if err == nil {
		return 0, false, ErrRequestPending
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	var reverseID uint64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM friend_requests WHERE from_id = ? AND to_id = ? AND status = ? FOR UPDATE`,
		to, from, FriendReqPending).Scan(&reverseID)
	if err == nil {
		// 反向待处理：视为对方已同意与我成友，直接接受。
		if _, err := tx.ExecContext(ctx,
			`UPDATE friend_requests SET status = ?, handled_at = ? WHERE id = ?`,
			FriendReqAccepted, now, reverseID); err != nil {
			return 0, false, err
		}
		if err := insertFriendEdgesTx(ctx, tx, from, to, now); err != nil {
			return 0, false, err
		}
		if err := tx.Commit(); err != nil {
			return 0, false, err
		}
		return reverseID, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO friend_requests (from_id, to_id, message, status, created_at) VALUES (?, ?, ?, ?, ?)`,
		from, to, message, FriendReqPending, now)
	if err != nil {
		return 0, false, err
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return uint64(id), false, nil
}

// HandleFriendRequest 被申请方接受/拒绝一条待处理申请。
// 申请不存在、或与 by 无关（by 不是被申请方）统一 ErrNotFound（不泄露存在性）；
// 已处理过返回 ErrRequestHandled。接受时在同一事务建双边。
func (s *Store) HandleFriendRequest(ctx context.Context, id, by uint64, accept bool) (*FriendRequest, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	fr, err := scanFriendRequest(tx.QueryRowContext(ctx,
		`SELECT `+friendReqCols+` FROM friend_requests WHERE id = ? FOR UPDATE`, id))
	if err != nil {
		return nil, err
	}
	if fr.ToID != by {
		return nil, ErrNotFound
	}
	if fr.Status != FriendReqPending {
		return nil, ErrRequestHandled
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	st := FriendReqRejected
	if accept {
		st = FriendReqAccepted
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE friend_requests SET status = ?, handled_at = ? WHERE id = ?`, st, now, id); err != nil {
		return nil, err
	}
	if accept {
		if err := insertFriendEdgesTx(ctx, tx, fr.FromID, fr.ToID, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	fr.Status = st
	fr.HandledAt = &now
	return fr, nil
}

// WithdrawFriendRequest 申请方撤回自己的待处理申请（置状态 3，列表按 status=0 过滤）。
func (s *Store) WithdrawFriendRequest(ctx context.Context, id, by uint64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	fr, err := scanFriendRequest(tx.QueryRowContext(ctx,
		`SELECT `+friendReqCols+` FROM friend_requests WHERE id = ? FOR UPDATE`, id))
	if err != nil {
		return err
	}
	if fr.FromID != by {
		return ErrNotFound
	}
	if fr.Status != FriendReqPending {
		return ErrRequestHandled
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := tx.ExecContext(ctx,
		`UPDATE friend_requests SET status = ?, handled_at = ? WHERE id = ?`, FriendReqWithdrawn, now, id); err != nil {
		return err
	}
	return tx.Commit()
}

// PendingFriendRequests 返回 uid 的待处理申请（incoming 的 user 是申请方，outgoing 的 user 是被申请方）。
func (s *Store) PendingFriendRequests(ctx context.Context, uid uint64) ([]FriendRequestView, []FriendRequestView, error) {
	incoming := []FriendRequestView{}
	outgoing := []FriendRequestView{}
	q := func(toMe bool) ([]FriendRequestView, error) {
		cond, joinCol := `r.to_id = ?`, "r.from_id"
		if !toMe {
			cond, joinCol = `r.from_id = ?`, "r.to_id"
		}
		rows, err := s.db.QueryContext(ctx, `
			SELECT r.id, r.message, r.created_at,
			       u.id, u.email, u.nickname, u.avatar_url, u.created_at, u.password_hash, u.avatar_key, u.token_version, u.allow_find
			FROM friend_requests r
			JOIN users u ON u.id = `+joinCol+`
			WHERE `+cond+` AND r.status = ?
			ORDER BY r.created_at DESC, r.id DESC`, uid, FriendReqPending)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []FriendRequestView{}
		for rows.Next() {
			var v FriendRequestView
			var msg string
			var created time.Time
			u := &User{}
			if err := rows.Scan(&v.ID, &msg, &created,
				&u.ID, &u.Email, &u.Nickname, &u.AvatarURL, &u.CreatedAt, &u.PasswordHash, &u.AvatarKey, &u.TokenVersion, &u.AllowFind); err != nil {
				return nil, err
			}
			u.ApplyAvatar()
			v.User, v.Message, v.CreatedAt = u.Public(), msg, created
			out = append(out, v)
		}
		return out, rows.Err()
	}
	var err error
	if incoming, err = q(true); err != nil {
		return nil, nil, err
	}
	if outgoing, err = q(false); err != nil {
		return nil, nil, err
	}
	return incoming, outgoing, nil
}

// ListFriends 返回 uid 的好友（按成为好友时间倒序）；备注/昵称排序由客户端做。
func (s *Store) ListFriends(ctx context.Context, uid uint64) ([]FriendView, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.email, u.nickname, u.avatar_url, u.created_at, u.password_hash, u.avatar_key, u.token_version, u.allow_find,
		       e.remark, e.created_at
		FROM friend_edges e JOIN users u ON u.id = e.friend_id
		WHERE e.user_id = ?
		ORDER BY e.created_at DESC, e.friend_id DESC`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FriendView{}
	for rows.Next() {
		u := &User{}
		var v FriendView
		if err := rows.Scan(&u.ID, &u.Email, &u.Nickname, &u.AvatarURL, &u.CreatedAt, &u.PasswordHash, &u.AvatarKey, &u.TokenVersion, &u.AllowFind,
			&v.Remark, &v.CreatedAt); err != nil {
			return nil, err
		}
		u.ApplyAvatar()
		v.User = u.Public()
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetFriend 返回 uid 视角下与 friendID 的好友项；不是好友返回 ErrNotFriends。
func (s *Store) GetFriend(ctx context.Context, uid, friendID uint64) (*FriendView, error) {
	u := &User{}
	var v FriendView
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.email, u.nickname, u.avatar_url, u.created_at, u.password_hash, u.avatar_key, u.token_version, u.allow_find,
		       e.remark, e.created_at
		FROM friend_edges e JOIN users u ON u.id = e.friend_id
		WHERE e.user_id = ? AND e.friend_id = ?`, uid, friendID).
		Scan(&u.ID, &u.Email, &u.Nickname, &u.AvatarURL, &u.CreatedAt, &u.PasswordHash, &u.AvatarKey, &u.TokenVersion, &u.AllowFind,
			&v.Remark, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFriends
	}
	if err != nil {
		return nil, err
	}
	u.ApplyAvatar()
	v.User = u.Public()
	return &v, nil
}

// SetFriendRemark 改自己这条边的备注（单向生效）；不是好友返回 ErrNotFriends。
func (s *Store) SetFriendRemark(ctx context.Context, uid, friendID uint64, remark string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE friend_edges SET remark = ? WHERE user_id = ? AND friend_id = ?`, remark, uid, friendID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 备注未变化时 RowsAffected 也为 0，再确认边是否存在
		if ok, err := s.IsFriend(ctx, uid, friendID); err != nil {
			return err
		} else if !ok {
			return ErrNotFriends
		}
	}
	return nil
}

// RemoveFriend 删除双边好友关系；不删会话与消息记录。不是好友返回 ErrNotFriends。
func (s *Store) RemoveFriend(ctx context.Context, uid, friendID uint64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`DELETE FROM friend_edges WHERE (user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)`,
		uid, friendID, friendID, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFriends
	}
	return tx.Commit()
}
