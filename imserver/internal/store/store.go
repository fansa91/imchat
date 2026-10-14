package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"imserver/internal/config"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

var (
	ErrNotFound  = errors.New("not found")
	ErrDuplicate = errors.New("duplicate")
	ErrNotMember = errors.New("not a member of conversation")
	ErrFull      = errors.New("group is full")
)

// 会话类型
const (
	ConvDirect = 1
	ConvGroup  = 2
)

// 消息类型
const (
	MsgText   = 1
	MsgImage  = 2
	MsgVoice  = 3
	MsgVideo  = 4
	MsgFile   = 5
	MsgSystem = 10 // 系统消息（入群/退群/改名/公告），content 为 JSON
)

// 群成员角色
const (
	RoleMember = 0
	RoleAdmin  = 1
	RoleOwner  = 2
)

type Store struct{ db *sql.DB }

func Open(cfg config.MySQLConfig) (*Store, error) {
	db, err := sql.Open("mysql", cfg.DSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(time.Duration(cfg.ConnMaxLifetime) * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("连接 MySQL 失败: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// 已有表在新版本中新增的列与索引。按 information_schema 检查后再 ALTER，
// 因此可重复执行，也能把 v0.1.0 的库原地升级（不丢数据）。
var upgradeColumns = []struct{ table, column, ddl string }{
	{"conversations", "name", "ADD COLUMN name VARCHAR(64) NOT NULL DEFAULT '' AFTER type"},
	{"conversations", "owner_id", "ADD COLUMN owner_id BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER name"},
	{"conversations", "announcement", "ADD COLUMN announcement VARCHAR(2000) NOT NULL DEFAULT '' AFTER owner_id"},
	{"conversations", "announcement_by", "ADD COLUMN announcement_by BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER announcement"},
	{"conversations", "announcement_at", "ADD COLUMN announcement_at DATETIME(3) NULL AFTER announcement_by"},
	{"conversations", "mod_seq", "ADD COLUMN mod_seq BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER last_seq"},
	{"conversation_members", "role", "ADD COLUMN role TINYINT NOT NULL DEFAULT 0 COMMENT '0=成员 1=管理员 2=群主' AFTER user_id"},
	{"conversation_members", "join_seq", "ADD COLUMN join_seq BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '入群时的 last_seq，只能看到之后的消息' AFTER role"},
	{"messages", "reply_to_seq", "ADD COLUMN reply_to_seq BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER content"},
	{"messages", "recalled", "ADD COLUMN recalled TINYINT NOT NULL DEFAULT 0 AFTER reply_to_seq"},
	{"messages", "recalled_at", "ADD COLUMN recalled_at DATETIME(3) NULL AFTER recalled"},
	{"messages", "mod_seq", "ADD COLUMN mod_seq BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '撤回/回应变化时更新，用于离线同步变更' AFTER recalled_at"},
}

var upgradeIndexes = []struct{ table, index, ddl string }{
	{"messages", "idx_conv_mod", "ADD INDEX idx_conv_mod (conversation_id, mod_seq)"},
}

// Migrate 执行内置建表脚本（全部 IF NOT EXISTS）和列升级，可重复执行。
func (s *Store) Migrate(ctx context.Context) error {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		b, _ := migrationFS.ReadFile("migrations/" + name)
		for _, stmt := range strings.Split(string(b), ";") {
			if strings.TrimSpace(stripComments(stmt)) == "" {
				continue
			}
			if _, err := s.db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migration %s: %w", name, err)
			}
		}
	}
	for _, c := range upgradeColumns {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.COLUMNS
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`, c.table, c.column).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := s.db.ExecContext(ctx, "ALTER TABLE "+c.table+" "+c.ddl); err != nil {
				return fmt.Errorf("upgrade %s.%s: %w", c.table, c.column, err)
			}
		}
	}
	for _, ix := range upgradeIndexes {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.STATISTICS
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ?`, ix.table, ix.index).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := s.db.ExecContext(ctx, "ALTER TABLE "+ix.table+" "+ix.ddl); err != nil {
				return fmt.Errorf("upgrade index %s.%s: %w", ix.table, ix.index, err)
			}
		}
	}
	return nil
}

func stripComments(stmt string) string {
	var out []string
	for _, l := range strings.Split(stmt, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "--") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// ---------- 用户 ----------

type User struct {
	ID           uint64    `json:"id"`
	Email        string    `json:"email"`
	Nickname     string    `json:"nickname"`
	AvatarURL    string    `json:"avatar_url"`
	CreatedAt    time.Time `json:"created_at"`
	PasswordHash string    `json:"-"`
}

func isDup(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

func (s *Store) CreateUser(ctx context.Context, email, hash, nickname string) (*User, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (email, password_hash, nickname) VALUES (?, ?, ?)`, email, hash, nickname)
	if err != nil {
		if isDup(err) {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetUser(ctx, uint64(id))
}

const userCols = `id, email, nickname, avatar_url, created_at, password_hash`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	u := &User{}
	if err := row.Scan(&u.ID, &u.Email, &u.Nickname, &u.AvatarURL, &u.CreatedAt, &u.PasswordHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return u, nil
}

func (s *Store) GetUser(ctx context.Context, id uint64) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

func (s *Store) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE email = ?`, email))
}

// GetUsers 批量查询用户，返回 id → User（不存在的忽略）。
func (s *Store) GetUsers(ctx context.Context, ids []uint64) (map[uint64]*User, error) {
	out := map[uint64]*User{}
	if len(ids) == 0 {
		return out, nil
	}
	q, args := inClause(ids)
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users WHERE id IN (`+q+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out[u.ID] = u
	}
	return out, rows.Err()
}

func inClause(ids []uint64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

// ---------- 会话 ----------

// GetOrCreateDirect 返回两人之间唯一的单聊会话，不存在则创建。
func (s *Store) GetOrCreateDirect(ctx context.Context, a, b uint64) (uint64, error) {
	low, high := a, b
	if low > high {
		low, high = high, low
	}
	var cid uint64
	err := s.db.QueryRowContext(ctx,
		`SELECT conversation_id FROM direct_pairs WHERE user_low = ? AND user_high = ?`, low, high).Scan(&cid)
	if err == nil {
		return cid, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO conversations (type) VALUES (1)`)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	cid = uint64(id)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO direct_pairs (user_low, user_high, conversation_id) VALUES (?, ?, ?)`, low, high, cid); err != nil {
		if isDup(err) { // 并发创建，回退读取已存在的
			tx.Rollback()
			err = s.db.QueryRowContext(ctx,
				`SELECT conversation_id FROM direct_pairs WHERE user_low = ? AND user_high = ?`, low, high).Scan(&cid)
			return cid, err
		}
		return 0, err
	}
	members := []uint64{low}
	if high != low {
		members = append(members, high)
	}
	for _, uid := range members {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO conversation_members (conversation_id, user_id) VALUES (?, ?)`, cid, uid); err != nil {
			return 0, err
		}
	}
	return cid, tx.Commit()
}

func (s *Store) Members(ctx context.Context, cid uint64) ([]uint64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id FROM conversation_members WHERE conversation_id = ?`, cid)
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

// Member 是某用户在某会话中的成员信息。
type Member struct {
	ConversationID uint64
	UserID         uint64
	Role           int
	JoinSeq        uint64
	ReadSeq        uint64
	ConvType       int
}

// GetMember 返回成员信息；不是成员返回 ErrNotMember。所有读写操作都应先经过它做权限校验。
func (s *Store) GetMember(ctx context.Context, cid, uid uint64) (*Member, error) {
	m := &Member{ConversationID: cid, UserID: uid}
	err := s.db.QueryRowContext(ctx, `
		SELECT m.role, m.join_seq, m.read_seq, c.type FROM conversation_members m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE m.conversation_id = ? AND m.user_id = ?`, cid, uid).Scan(&m.Role, &m.JoinSeq, &m.ReadSeq, &m.ConvType)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotMember
	}
	return m, err
}

func (s *Store) IsMember(ctx context.Context, cid, uid uint64) (bool, error) {
	_, err := s.GetMember(ctx, cid, uid)
	if errors.Is(err, ErrNotMember) {
		return false, nil
	}
	return err == nil, err
}

type ConversationSummary struct {
	ID             uint64     `json:"id"`
	Type           int        `json:"type"`
	LastSeq        uint64     `json:"last_seq"`
	ModSeq         uint64     `json:"mod_seq"`
	ReadSeq        uint64     `json:"read_seq"`
	JoinSeq        uint64     `json:"join_seq"`
	Unread         uint64     `json:"unread"`
	UpdatedAt      time.Time  `json:"updated_at"`
	Peer           *User      `json:"peer,omitempty"`
	LastMessage    *Message   `json:"last_message,omitempty"`
	LastSenderName string     `json:"last_sender_name,omitempty"` // 群聊列表预览“XX：”用
	Name           string     `json:"name,omitempty"`             // 群名
	OwnerID        uint64     `json:"owner_id,omitempty"`         // 群主
	MyRole         int        `json:"my_role"`                    // 我的角色 0/1/2
	MemberCount    int        `json:"member_count,omitempty"`     // 群成员数
	MembersPreview []*User    `json:"members_preview,omitempty"`  // 群头像用：最早入群的最多 4 人
	Announcement   string     `json:"announcement,omitempty"`
	AnnouncementBy uint64     `json:"announcement_by,omitempty"`
	AnnouncementAt *time.Time `json:"announcement_at,omitempty"`
}

// ListConversations 返回用户所有会话，按最近活跃排序。客户端重连后用 last_seq / mod_seq 判断需要补拉哪些会话。
func (s *Store) ListConversations(ctx context.Context, uid uint64) ([]ConversationSummary, error) {
	return s.listConversations(ctx, uid, 0)
}

// GetConversation 返回单个会话摘要（必须是成员）。
func (s *Store) GetConversation(ctx context.Context, uid, cid uint64) (*ConversationSummary, error) {
	list, err := s.listConversations(ctx, uid, cid)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotMember
	}
	return &list[0], nil
}

func (s *Store) listConversations(ctx context.Context, uid, onlyCID uint64) ([]ConversationSummary, error) {
	q := `
		SELECT c.id, c.type, c.last_seq, c.mod_seq, m.read_seq, m.join_seq, m.role, c.updated_at,
		       c.name, c.owner_id, c.announcement, c.announcement_by, c.announcement_at,
		       u.id, u.email, u.nickname, u.avatar_url, u.created_at
		FROM conversation_members m
		JOIN conversations c ON c.id = m.conversation_id
		LEFT JOIN conversation_members pm ON pm.conversation_id = c.id AND pm.user_id <> m.user_id AND c.type = 1
		LEFT JOIN users u ON u.id = pm.user_id
		WHERE m.user_id = ?`
	args := []any{uid}
	if onlyCID > 0 {
		q += ` AND c.id = ?`
		args = append(args, onlyCID)
	}
	q += ` ORDER BY c.updated_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []ConversationSummary
	for rows.Next() {
		var cs ConversationSummary
		var pid sql.NullInt64
		var pemail, pnick, pavatar sql.NullString
		var pcreated, annAt sql.NullTime
		if err := rows.Scan(&cs.ID, &cs.Type, &cs.LastSeq, &cs.ModSeq, &cs.ReadSeq, &cs.JoinSeq, &cs.MyRole, &cs.UpdatedAt,
			&cs.Name, &cs.OwnerID, &cs.Announcement, &cs.AnnouncementBy, &annAt,
			&pid, &pemail, &pnick, &pavatar, &pcreated); err != nil {
			return nil, err
		}
		if pid.Valid {
			cs.Peer = &User{ID: uint64(pid.Int64), Email: pemail.String, Nickname: pnick.String,
				AvatarURL: pavatar.String, CreatedAt: pcreated.Time}
		}
		if annAt.Valid {
			t := annAt.Time
			cs.AnnouncementAt = &t
		}
		if cs.LastSeq > cs.ReadSeq {
			cs.Unread = cs.LastSeq - cs.ReadSeq
		}
		list = append(list, cs)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range list {
		c := &list[i]
		if c.Type == ConvGroup {
			_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_members WHERE conversation_id = ?`, c.ID).Scan(&c.MemberCount)
			c.MembersPreview, _ = s.membersPreview(ctx, c.ID, 4)
		}
		if c.LastSeq > c.JoinSeq {
			msgs, err := s.Messages(ctx, MsgQuery{Viewer: uid, ConversationID: c.ID, JoinSeq: c.JoinSeq, BeforeSeq: c.LastSeq + 1, Limit: 1})
			if err == nil && len(msgs) == 1 {
				c.LastMessage = &msgs[0]
				if c.Type == ConvGroup {
					if u, err := s.GetUser(ctx, msgs[0].SenderID); err == nil {
						c.LastSenderName = u.Nickname
						if c.LastSenderName == "" {
							c.LastSenderName = strings.Split(u.Email, "@")[0]
						}
					}
				}
			}
		}
	}
	return list, nil
}

func (s *Store) membersPreview(ctx context.Context, cid uint64, n int) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.email, u.nickname, u.avatar_url, u.created_at, u.password_hash
		FROM conversation_members m JOIN users u ON u.id = m.user_id
		WHERE m.conversation_id = ? ORDER BY m.role DESC, m.joined_at, m.user_id LIMIT ?`, cid, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ---------- 消息 ----------

// Reaction 某个表情的聚合结果。
type Reaction struct {
	Emoji   string   `json:"emoji"`
	Count   int      `json:"count"`
	UserIDs []uint64 `json:"user_ids"`
}

// ReplyPreview 被引用消息的预览（读取时实时计算，原消息撤回后 recalled=true、preview 为空）。
type ReplyPreview struct {
	Seq      uint64 `json:"seq"`
	SenderID uint64 `json:"sender_id"`
	MsgType  int    `json:"msg_type"`
	Preview  string `json:"preview"`
	Recalled bool   `json:"recalled"`
	Missing  bool   `json:"missing,omitempty"`
}

type Message struct {
	ConversationID uint64        `json:"conversation_id"`
	Seq            uint64        `json:"seq"`
	SenderID       uint64        `json:"sender_id"`
	ClientMsgID    string        `json:"client_msg_id"`
	MsgType        int           `json:"msg_type"`
	Content        string        `json:"content"`
	CreatedAt      time.Time     `json:"created_at"`
	ReplyToSeq     uint64        `json:"reply_to_seq,omitempty"`
	ReplyTo        *ReplyPreview `json:"reply_to,omitempty"`
	Recalled       bool          `json:"recalled,omitempty"`
	RecalledAt     *time.Time    `json:"recalled_at,omitempty"`
	ModSeq         uint64        `json:"mod_seq,omitempty"`
	Reactions      []Reaction    `json:"reactions,omitempty"`
}

const msgCols = `m.conversation_id, m.seq, m.sender_id, m.client_msg_id, m.msg_type, m.content, m.created_at,
	m.reply_to_seq, m.recalled, m.recalled_at, m.mod_seq`

func scanMessage(row interface{ Scan(...any) error }) (*Message, error) {
	m := &Message{}
	var recalledAt sql.NullTime
	if err := row.Scan(&m.ConversationID, &m.Seq, &m.SenderID, &m.ClientMsgID, &m.MsgType, &m.Content, &m.CreatedAt,
		&m.ReplyToSeq, &m.Recalled, &recalledAt, &m.ModSeq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if recalledAt.Valid {
		t := recalledAt.Time
		m.RecalledAt = &t
	}
	return m, nil
}

// InsertMessage 在事务中为会话分配递增 seq 并写入消息。
// 同一发送者重复提交同一 client_msg_id 时返回已存在的消息（幂等），dup=true。
func (s *Store) InsertMessage(ctx context.Context, m *Message) (dup bool, err error) {
	existing, err := s.messageByClientID(ctx, m.SenderID, m.ClientMsgID)
	if err == nil {
		*m = *existing
		return true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return false, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var seq uint64
	if err := tx.QueryRowContext(ctx,
		`SELECT last_seq FROM conversations WHERE id = ? FOR UPDATE`, m.ConversationID).Scan(&seq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, err
	}
	seq++
	m.Seq = seq
	m.CreatedAt = time.Now().UTC().Truncate(time.Millisecond)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messages (conversation_id, seq, sender_id, client_msg_id, msg_type, content, reply_to_seq, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ConversationID, m.Seq, m.SenderID, m.ClientMsgID, m.MsgType, m.Content, m.ReplyToSeq, m.CreatedAt); err != nil {
		if isDup(err) {
			tx.Rollback()
			existing, err2 := s.messageByClientID(ctx, m.SenderID, m.ClientMsgID)
			if err2 != nil {
				return false, err2
			}
			*m = *existing
			return true, nil
		}
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET last_seq = ? WHERE id = ?`, seq, m.ConversationID); err != nil {
		return false, err
	}
	// 发送者自己发的消息视为已读
	if _, err := tx.ExecContext(ctx,
		`UPDATE conversation_members SET read_seq = GREATEST(read_seq, ?) WHERE conversation_id = ? AND user_id = ?`,
		seq, m.ConversationID, m.SenderID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return false, nil
}

// MessageByClientID 按发送者 + client_msg_id 查找消息（幂等）。
func (s *Store) MessageByClientID(ctx context.Context, sender uint64, clientID string) (*Message, error) {
	return s.messageByClientID(ctx, sender, clientID)
}

func (s *Store) messageByClientID(ctx context.Context, sender uint64, clientID string) (*Message, error) {
	m, err := scanMessage(s.db.QueryRowContext(ctx,
		`SELECT `+msgCols+` FROM messages m WHERE m.sender_id = ? AND m.client_msg_id = ?`, sender, clientID))
	if err != nil {
		return nil, err
	}
	_ = s.enrich(ctx, m.ConversationID, []*Message{m})
	return m, nil
}

// GetMessage 读取单条消息（不做可见性过滤）。
func (s *Store) GetMessage(ctx context.Context, cid, seq uint64) (*Message, error) {
	return scanMessage(s.db.QueryRowContext(ctx,
		`SELECT `+msgCols+` FROM messages m WHERE m.conversation_id = ? AND m.seq = ?`, cid, seq))
}

// Enrich 给消息补充引用预览与表情回应（用于广播新消息）。
func (s *Store) Enrich(ctx context.Context, m *Message) error {
	return s.enrich(ctx, m.ConversationID, []*Message{m})
}

// MsgQuery 拉取消息的条件。
//   - AfterSeq>0：补拉 seq > AfterSeq（正序，离线同步）
//   - ChangedSince>0：拉取 mod_seq > ChangedSince 的消息（撤回、表情回应变化，按 mod_seq 正序）
//   - 否则：seq < BeforeSeq 的最近消息（倒序取再翻转为正序，翻历史）
//
// 总是排除 Viewer 删除（仅自己隐藏）的消息和入群前（seq <= JoinSeq）的消息。
type MsgQuery struct {
	Viewer         uint64
	ConversationID uint64
	JoinSeq        uint64
	AfterSeq       uint64
	BeforeSeq      uint64
	ChangedSince   uint64
	Limit          int
}

func (s *Store) Messages(ctx context.Context, q MsgQuery) ([]Message, error) {
	base := `SELECT ` + msgCols + ` FROM messages m
		LEFT JOIN message_hidden h ON h.user_id = ? AND h.conversation_id = m.conversation_id AND h.seq = m.seq
		WHERE m.conversation_id = ? AND m.seq > ? AND h.seq IS NULL `
	args := []any{q.Viewer, q.ConversationID, q.JoinSeq}
	desc := false
	switch {
	case q.ChangedSince > 0:
		base += `AND m.mod_seq > ? ORDER BY m.mod_seq ASC LIMIT ?`
		args = append(args, q.ChangedSince, q.Limit)
	case q.AfterSeq > 0 || q.BeforeSeq == 0:
		base += `AND m.seq > ? ORDER BY m.seq ASC LIMIT ?`
		args = append(args, q.AfterSeq, q.Limit)
	default:
		base += `AND m.seq < ? ORDER BY m.seq DESC LIMIT ?`
		args = append(args, q.BeforeSeq, q.Limit)
		desc = true
	}
	rows, err := s.db.QueryContext(ctx, base, args...)
	if err != nil {
		return nil, err
	}
	var ptrs []*Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		ptrs = append(ptrs, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if desc {
		for i, j := 0, len(ptrs)-1; i < j; i, j = i+1, j-1 {
			ptrs[i], ptrs[j] = ptrs[j], ptrs[i]
		}
	}
	if err := s.enrich(ctx, q.ConversationID, ptrs); err != nil {
		return nil, err
	}
	out := make([]Message, len(ptrs))
	for i, p := range ptrs {
		out[i] = *p
	}
	return out, nil
}

// enrich 批量补充表情回应与引用预览。
func (s *Store) enrich(ctx context.Context, cid uint64, msgs []*Message) error {
	if len(msgs) == 0 {
		return nil
	}
	seqs := make([]uint64, 0, len(msgs))
	idx := map[uint64]*Message{}
	var replySeqs []uint64
	for _, m := range msgs {
		seqs = append(seqs, m.Seq)
		idx[m.Seq] = m
		if m.ReplyToSeq > 0 {
			replySeqs = append(replySeqs, m.ReplyToSeq)
		}
	}
	reacts, err := s.reactionsFor(ctx, cid, seqs)
	if err != nil {
		return err
	}
	for seq, rs := range reacts {
		idx[seq].Reactions = rs
	}
	if len(replySeqs) > 0 {
		in, args := inClause(replySeqs)
		rows, err := s.db.QueryContext(ctx, `SELECT seq, sender_id, msg_type, content, recalled FROM messages
			WHERE conversation_id = ? AND seq IN (`+in+`)`, append([]any{cid}, args...)...)
		if err != nil {
			return err
		}
		prev := map[uint64]*ReplyPreview{}
		for rows.Next() {
			p := &ReplyPreview{}
			var content string
			if err := rows.Scan(&p.Seq, &p.SenderID, &p.MsgType, &content, &p.Recalled); err != nil {
				rows.Close()
				return err
			}
			if !p.Recalled {
				p.Preview = PreviewText(p.MsgType, content, 80)
			}
			prev[p.Seq] = p
		}
		rows.Close()
		for _, m := range msgs {
			if m.ReplyToSeq == 0 {
				continue
			}
			if p, ok := prev[m.ReplyToSeq]; ok {
				cp := *p
				m.ReplyTo = &cp
			} else {
				m.ReplyTo = &ReplyPreview{Seq: m.ReplyToSeq, Missing: true}
			}
		}
	}
	return nil
}

func (s *Store) reactionsFor(ctx context.Context, cid uint64, seqs []uint64) (map[uint64][]Reaction, error) {
	out := map[uint64][]Reaction{}
	if len(seqs) == 0 {
		return out, nil
	}
	in, args := inClause(seqs)
	rows, err := s.db.QueryContext(ctx, `SELECT seq, emoji, user_id FROM message_reactions
		WHERE conversation_id = ? AND seq IN (`+in+`) ORDER BY seq, created_at, user_id`, append([]any{cid}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq, uid uint64
		var emoji string
		if err := rows.Scan(&seq, &emoji, &uid); err != nil {
			return nil, err
		}
		list := out[seq]
		found := false
		for i := range list {
			if list[i].Emoji == emoji {
				list[i].Count++
				list[i].UserIDs = append(list[i].UserIDs, uid)
				found = true
				break
			}
		}
		if !found {
			list = append(list, Reaction{Emoji: emoji, Count: 1, UserIDs: []uint64{uid}})
		}
		out[seq] = list
	}
	return out, rows.Err()
}

// PreviewText 生成消息的纯文本预览（会话列表、引用、推送用）。
func PreviewText(msgType int, content string, maxRunes int) string {
	switch msgType {
	case MsgText:
		r := []rune(strings.Join(strings.Fields(content), " "))
		if len(r) > maxRunes {
			return string(r[:maxRunes]) + "…"
		}
		return string(r)
	case MsgImage:
		return "[图片]"
	case MsgVoice:
		return "[语音]"
	case MsgVideo:
		return "[视频]"
	case MsgFile:
		return "[文件]"
	case MsgSystem:
		return SystemText(content)
	}
	return "[消息]"
}

// MarkRead 更新已读位置（只增不减），返回实际生效的 read_seq。
func (s *Store) MarkRead(ctx context.Context, cid, uid, seq uint64) (uint64, error) {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE conversation_members m JOIN conversations c ON c.id = m.conversation_id
		 SET m.read_seq = GREATEST(m.read_seq, LEAST(?, c.last_seq))
		 WHERE m.conversation_id = ? AND m.user_id = ?`, seq, cid, uid); err != nil {
		return 0, err
	}
	var rs uint64
	err := s.db.QueryRowContext(ctx,
		`SELECT read_seq FROM conversation_members WHERE conversation_id = ? AND user_id = ?`, cid, uid).Scan(&rs)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotMember
	}
	return rs, err
}

// ReadStates 返回会话中各成员的已读位置（单聊显示“已读”，群聊统计“N人已读”）。
func (s *Store) ReadStates(ctx context.Context, cid uint64) (map[uint64]uint64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_id, read_seq FROM conversation_members WHERE conversation_id = ?`, cid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uint64]uint64{}
	for rows.Next() {
		var uid, rs uint64
		if err := rows.Scan(&uid, &rs); err != nil {
			return nil, err
		}
		out[uid] = rs
	}
	return out, rows.Err()
}

// ---------- 撤回 / 表情回应 / 仅自己删除 ----------

var (
	ErrNotSender       = errors.New("not sender")
	ErrRecallExpired   = errors.New("recall window expired")
	ErrAlreadyRecalled = errors.New("already recalled")
)

// bumpMod 在事务内把会话 mod_seq +1 并返回新值。
func bumpMod(ctx context.Context, tx *sql.Tx, cid uint64) (uint64, error) {
	var mod uint64
	if err := tx.QueryRowContext(ctx, `SELECT mod_seq FROM conversations WHERE id = ? FOR UPDATE`, cid).Scan(&mod); err != nil {
		return 0, err
	}
	mod++
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET mod_seq = ?, updated_at = updated_at WHERE id = ?`, mod, cid); err != nil {
		return 0, err
	}
	return mod, nil
}

// Recall 撤回消息：只能撤回自己的、在时间窗口内的。内容清空，变成墓碑。
// 返回撤回前的消息（调用方据此删除语音文件）和更新后的消息。
func (s *Store) Recall(ctx context.Context, cid, seq, uid uint64, window time.Duration) (before, after *Message, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	mod, err := bumpMod(ctx, tx, cid)
	if err != nil {
		return nil, nil, err
	}
	before, err = scanMessage(tx.QueryRowContext(ctx,
		`SELECT `+msgCols+` FROM messages m WHERE m.conversation_id = ? AND m.seq = ? FOR UPDATE`, cid, seq))
	if err != nil {
		return nil, nil, err
	}
	if before.SenderID != uid || before.MsgType == MsgSystem {
		return nil, nil, ErrNotSender
	}
	if before.Recalled {
		return nil, nil, ErrAlreadyRecalled
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	if now.Sub(before.CreatedAt) > window {
		return nil, nil, ErrRecallExpired
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET recalled = 1, recalled_at = ?, content = '', mod_seq = ?
		WHERE conversation_id = ? AND seq = ?`, now, mod, cid, seq); err != nil {
		return nil, nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM message_reactions WHERE conversation_id = ? AND seq = ?`, cid, seq); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	a := *before
	a.Recalled, a.RecalledAt, a.Content, a.ModSeq, a.Reactions = true, &now, "", mod, nil
	_ = s.enrich(ctx, cid, []*Message{&a})
	return before, &a, nil
}

// MaxReactionKinds 每条消息最多不同表情数。
const MaxReactionKinds = 20

var ErrTooManyReactions = errors.New("too many reactions")

// ToggleReaction 添加/取消表情回应。on=nil 表示切换。返回最终状态、新 mod_seq 和该消息聚合后的回应。
func (s *Store) ToggleReaction(ctx context.Context, cid, seq, uid uint64, emoji string, on *bool) (bool, uint64, []Reaction, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, nil, err
	}
	defer tx.Rollback()
	mod, err := bumpMod(ctx, tx, cid)
	if err != nil {
		return false, 0, nil, err
	}
	var recalled bool
	var mtype int
	if err := tx.QueryRowContext(ctx, `SELECT recalled, msg_type FROM messages WHERE conversation_id = ? AND seq = ? FOR UPDATE`, cid, seq).
		Scan(&recalled, &mtype); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, 0, nil, ErrNotFound
		}
		return false, 0, nil, err
	}
	if recalled || mtype == MsgSystem {
		return false, 0, nil, ErrNotFound
	}
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM message_reactions WHERE conversation_id = ? AND seq = ? AND user_id = ? AND emoji = ?`,
		cid, seq, uid, emoji).Scan(&exists)
	has := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, 0, nil, err
	}
	want := !has
	if on != nil {
		want = *on
	}
	if want && !has {
		var kinds int
		_ = tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT emoji) FROM message_reactions WHERE conversation_id = ? AND seq = ?`, cid, seq).Scan(&kinds)
		var kindExists int
		e2 := tx.QueryRowContext(ctx, `SELECT 1 FROM message_reactions WHERE conversation_id = ? AND seq = ? AND emoji = ? LIMIT 1`, cid, seq, emoji).Scan(&kindExists)
		if kinds >= MaxReactionKinds && e2 != nil {
			return false, 0, nil, ErrTooManyReactions
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO message_reactions (conversation_id, seq, user_id, emoji) VALUES (?, ?, ?, ?)`,
			cid, seq, uid, emoji); err != nil {
			return false, 0, nil, err
		}
	} else if !want && has {
		if _, err := tx.ExecContext(ctx, `DELETE FROM message_reactions WHERE conversation_id = ? AND seq = ? AND user_id = ? AND emoji = ?`,
			cid, seq, uid, emoji); err != nil {
			return false, 0, nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET mod_seq = ? WHERE conversation_id = ? AND seq = ?`, mod, cid, seq); err != nil {
		return false, 0, nil, err
	}
	if err := tx.Commit(); err != nil {
		return false, 0, nil, err
	}
	rs, err := s.reactionsFor(ctx, cid, []uint64{seq})
	if err != nil {
		return want, mod, nil, err
	}
	list := rs[seq]
	if list == nil {
		list = []Reaction{}
	}
	return want, mod, list, nil
}

// HideMessages 仅自己删除（对自己隐藏）。只接受会话中真实存在、且自己可见的 seq。
func (s *Store) HideMessages(ctx context.Context, uid, cid, joinSeq uint64, seqs []uint64) ([]uint64, error) {
	if len(seqs) == 0 {
		return nil, nil
	}
	in, args := inClause(seqs)
	rows, err := s.db.QueryContext(ctx, `SELECT seq FROM messages WHERE conversation_id = ? AND seq > ? AND seq IN (`+in+`)`,
		append([]any{cid, joinSeq}, args...)...)
	if err != nil {
		return nil, err
	}
	var valid []uint64
	for rows.Next() {
		var seq uint64
		if err := rows.Scan(&seq); err != nil {
			rows.Close()
			return nil, err
		}
		valid = append(valid, seq)
	}
	rows.Close()
	for _, seq := range valid {
		if _, err := s.db.ExecContext(ctx, `INSERT IGNORE INTO message_hidden (user_id, conversation_id, seq) VALUES (?, ?, ?)`,
			uid, cid, seq); err != nil {
			return nil, err
		}
	}
	return valid, nil
}

// HiddenSeqs 返回用户在会话中删除的 seq（>= minSeq），用于多设备同步。
func (s *Store) HiddenSeqs(ctx context.Context, uid, cid, minSeq uint64) ([]uint64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq FROM message_hidden WHERE user_id = ? AND conversation_id = ? AND seq >= ?
		ORDER BY seq LIMIT 5000`, uid, cid, minSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []uint64{}
	for rows.Next() {
		var seq uint64
		if err := rows.Scan(&seq); err != nil {
			return nil, err
		}
		out = append(out, seq)
	}
	return out, rows.Err()
}
