package ws

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"

	"imserver/internal/push"
	"imserver/internal/storage"
	"imserver/internal/store"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = 25 * time.Second
	maxFrameSize   = 64 * 1024
	maxTextRunes   = 5000
	sendBufferSize = 256
)

// Deps 是连接处理需要的外部依赖。
type Deps struct {
	Store           *store.Store
	Push            push.Notifier
	Storage         *storage.Storage // 为 nil 时不支持语音
	RecallWindow    time.Duration
	CallRingTimeout time.Duration // v0.4.0：通话响铃超时；<=0 时用 60s
}

type Client struct {
	hub    *Hub
	deps   *Deps
	store  *store.Store
	push   push.Notifier
	conn   *websocket.Conn
	userID uint64
	connID string
	send   chan []byte
	limit  *rate.Limiter
	done   chan struct{}
	once   sync.Once
}

func newConnID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func NewClient(h *Hub, d *Deps, conn *websocket.Conn, uid uint64) *Client {
	return &Client{
		hub: h, deps: d, store: d.Store, push: d.Push, conn: conn, userID: uid,
		connID: h.nodeID + ":" + newConnID(),
		send:   make(chan []byte, sendBufferSize),
		limit:  rate.NewLimiter(rate.Limit(20), 40), // 每连接每秒 20 帧，突发 40
		done:   make(chan struct{}),
	}
}

// enqueue 非阻塞写入发送队列；队列满说明客户端太慢，直接断开，客户端重连后会补拉。
func (c *Client) enqueue(frame []byte) {
	select {
	case c.send <- frame:
	default:
		slog.Warn("send buffer full, closing slow client", "user", c.userID, "conn", c.connID)
		c.closeOnce()
	}
}

func (c *Client) closeOnce() { c.once.Do(func() { close(c.done) }) }

// Serve 启动读写循环，阻塞直到连接关闭。
func (c *Client) Serve() {
	if c.hub.register(c) {
		c.publishPresence(true)
	}
	defer func() {
		if c.hub.unregister(c) {
			c.publishPresence(false)
		}
	}()
	go c.writeLoop()
	c.readLoop()
	c.closeOnce()
}

// PresenceData 在线状态（WS presence 事件 data 与 GET /api/presence 的元素同结构）。
// last_seen 为 RFC3339（UTC）；在线或从未断开过为 null。
type PresenceData struct {
	UserID   uint64  `json:"user_id"`
	Online   bool    `json:"online"`
	LastSeen *string `json:"last_seen"`
}

// FormatLastSeen 把时间格式化为 presence 用的 RFC3339 字符串。
func FormatLastSeen(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// publishPresence 在线状态翻转时（第一个连接建立 / 最后一个连接断开）推给共享会话的成员。
func (c *Client) publishPresence(online bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data := PresenceData{UserID: c.userID, Online: online}
	if !online {
		now := time.Now().UTC().Truncate(time.Millisecond)
		if err := c.store.SetLastSeen(ctx, c.userID, now); err != nil {
			slog.Warn("set last_seen failed", "user", c.userID, "err", err)
		}
		data.LastSeen = FormatLastSeen(&now)
	}
	targets, err := c.store.CoMembers(ctx, c.userID)
	if err != nil {
		slog.Warn("presence co-members failed", "user", c.userID, "err", err)
		return
	}
	c.hub.Publish(ctx, targets, "", encode("presence", "", data))
}

func (c *Client) readLoop() {
	c.conn.SetReadLimit(maxFrameSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
		c.hub.touchPresence(context.Background(), c)
		return nil
	})
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
		if !c.limit.Allow() {
			c.reply("error", "", errorData{"rate_limited", "发送过于频繁"})
			continue
		}
		var env Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			c.reply("error", "", errorData{"bad_request", "无效的 JSON"})
			continue
		}
		c.handle(env)
	}
}

func (c *Client) writeLoop() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()
	for {
		select {
		case <-c.done:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
			return
		case frame := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, frame); err != nil {
				c.closeOnce()
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				c.closeOnce()
				return
			}
		}
	}
}

func (c *Client) reply(typ, id string, data any) { c.enqueue(encode(typ, id, data)) }

func (c *Client) handle(env Envelope) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	switch env.Type {
	case "ping":
		c.hub.touchPresence(ctx, c)
		c.reply("pong", env.ID, struct{}{})
	case "send":
		c.handleSend(ctx, env)
	case "read":
		c.handleRead(ctx, env)
	case "typing":
		c.handleTyping(ctx, env)
	case "recall":
		c.handleRecall(ctx, env)
	case "react":
		c.handleReact(ctx, env)
	case "delete":
		c.handleDelete(ctx, env)
	case "call_invite":
		c.handleCallInvite(ctx, env)
	case "call_accept":
		c.handleCallAccept(ctx, env)
	case "call_reject":
		c.handleCallReject(ctx, env)
	case "call_cancel":
		c.handleCallCancel(ctx, env)
	case "call_hangup":
		c.handleCallHangup(ctx, env)
	case "call_signal":
		c.handleCallSignal(ctx, env)
	default:
		c.reply("error", env.ID, errorData{"unknown_type", "未知的消息类型: " + env.Type})
	}
}

func (c *Client) handleRead(ctx context.Context, env Envelope) {
	var req readReq
	if err := json.Unmarshal(env.Data, &req); err != nil || req.ConversationID == 0 {
		c.reply("error", env.ID, errorData{"bad_request", "参数错误"})
		return
	}
	rs, err := c.store.MarkRead(ctx, req.ConversationID, c.userID, req.Seq)
	if err != nil {
		if errors.Is(err, store.ErrNotMember) {
			c.reply("error", env.ID, errorData{"forbidden", "不是该会话成员"})
			return
		}
		c.reply("error", env.ID, errorData{"internal", "服务器错误"})
		return
	}
	members, err := c.store.Members(ctx, req.ConversationID)
	if err != nil {
		return
	}
	// 通知对方（显示“已读”）以及自己的其他设备（同步未读数）
	c.hub.Publish(ctx, members, c.connID, encode("read", "", map[string]any{
		"conversation_id": req.ConversationID, "user_id": c.userID, "read_seq": rs,
	}))
}

func (c *Client) handleTyping(ctx context.Context, env Envelope) {
	var req typingReq
	if err := json.Unmarshal(env.Data, &req); err != nil || req.ConversationID == 0 {
		return
	}
	members, err := c.store.Members(ctx, req.ConversationID)
	if err != nil || !contains(members, c.userID) {
		return
	}
	others := make([]uint64, 0, len(members))
	for _, m := range members {
		if m != c.userID {
			others = append(others, m)
		}
	}
	c.hub.Publish(ctx, others, "", encode("typing", "", map[string]any{
		"conversation_id": req.ConversationID, "user_id": c.userID,
	}))
}

func contains(ids []uint64, id uint64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func jsonNum(n uint64) string { b, _ := json.Marshal(n); return string(b) }
