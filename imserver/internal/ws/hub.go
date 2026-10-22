package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const eventsChannel = "im:events"

// event 通过 Redis Pub/Sub 广播到所有节点，各节点只投递给本机在线的目标用户。
type event struct {
	Targets       []uint64        `json:"t"`
	ExcludeConnID string          `json:"x,omitempty"`
	Frame         json.RawMessage `json:"f"`
	Disconnect    bool            `json:"d,omitempty"` // v0.4.0：投递后断开目标用户的全部连接（session_revoked 用）
}

type Hub struct {
	rdb    *redis.Client
	nodeID string

	mu    sync.RWMutex
	conns map[uint64]map[*Client]struct{}

	callStore callRecordStore // v0.4.0：通话巡检写通话记录用，由 api 层注入（可为 nil）
}

func NewHub(rdb *redis.Client, nodeID string) *Hub {
	return &Hub{rdb: rdb, nodeID: nodeID, conns: map[uint64]map[*Client]struct{}{}}
}

func onlineKey(uid uint64) string { return "im:online:" + strconv.FormatUint(uid, 10) }

const presenceTTL = 90 * time.Second

// presenceAdd 原子地：清掉过期连接 → 加入本连接 → 返回加入前的有效连接数。
// 返回 0 表示用户刚从离线变为在线（跨实例正确：所有实例共用同一个 ZSET）。
var presenceAdd = redis.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
local before = redis.call('ZCARD', KEYS[1])
if redis.call('ZSCORE', KEYS[1], ARGV[3]) then before = before - 1 end
redis.call('ZADD', KEYS[1], ARGV[2], ARGV[3])
redis.call('EXPIRE', KEYS[1], ARGV[4])
return before`)

// presenceDel 原子地：移除本连接 → 清掉过期连接 → 返回 {是否移除了, 剩余有效连接数}。
var presenceDel = redis.NewScript(`
local removed = redis.call('ZREM', KEYS[1], ARGV[2])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
return {removed, redis.call('ZCARD', KEYS[1])}`)

// register 登记连接，返回该用户是否刚变为在线（第一个连接）。
func (h *Hub) register(c *Client) (becameOnline bool) {
	h.mu.Lock()
	set, ok := h.conns[c.userID]
	if !ok {
		set = map[*Client]struct{}{}
		h.conns[c.userID] = set
	}
	set[c] = struct{}{}
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	now := time.Now()
	n, err := presenceAdd.Run(ctx, h.rdb, []string{onlineKey(c.userID)},
		now.Unix(), now.Add(presenceTTL).Unix(), c.connID, int(presenceTTL/time.Second)).Int64()
	if err != nil {
		slog.Warn("presence register failed", "err", err)
		return false
	}
	return n == 0
}

// unregister 注销连接，返回该用户是否刚变为离线（最后一个连接断开）。
func (h *Hub) unregister(c *Client) (becameOffline bool) {
	h.mu.Lock()
	if set, ok := h.conns[c.userID]; ok {
		delete(set, c)
		if len(set) == 0 {
			delete(h.conns, c.userID)
		}
	}
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := presenceDel.Run(ctx, h.rdb, []string{onlineKey(c.userID)}, time.Now().Unix(), c.connID).Int64Slice()
	if err != nil || len(res) != 2 {
		slog.Warn("presence unregister failed", "err", err)
		return false
	}
	return res[0] == 1 && res[1] == 0
}

// touchPresence 记录连接在线，score 为过期时间戳；用 ZSET 支持多设备同时在线。
func (h *Hub) touchPresence(ctx context.Context, c *Client) {
	key := onlineKey(c.userID)
	exp := time.Now().Add(presenceTTL).Unix()
	pipe := h.rdb.Pipeline()
	pipe.ZAdd(ctx, key, redis.Z{Score: float64(exp), Member: c.connID})
	pipe.ZRemRangeByScore(ctx, key, "-inf", strconv.FormatInt(time.Now().Unix(), 10))
	pipe.Expire(ctx, key, presenceTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Warn("presence update failed", "err", err)
	}
}

// OnlineMany 批量判断用户是否在线（跨节点）。
func (h *Hub) OnlineMany(ctx context.Context, uids []uint64) (map[uint64]bool, error) {
	out := make(map[uint64]bool, len(uids))
	if len(uids) == 0 {
		return out, nil
	}
	now := strconv.FormatInt(time.Now().Unix(), 10)
	pipe := h.rdb.Pipeline()
	cmds := make([]*redis.IntCmd, len(uids))
	for i, uid := range uids {
		cmds[i] = pipe.ZCount(ctx, onlineKey(uid), now, "+inf")
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, err
	}
	for i, uid := range uids {
		out[uid] = cmds[i].Val() > 0
	}
	return out, nil
}

// IsOnline 判断用户是否有任一设备在线（跨节点）。
func (h *Hub) IsOnline(ctx context.Context, uid uint64) bool {
	n, err := h.rdb.ZCount(ctx, onlineKey(uid), strconv.FormatInt(time.Now().Unix(), 10), "+inf").Result()
	return err == nil && n > 0
}

// Publish 把一帧数据发给目标用户的所有在线设备（跨节点）。
func (h *Hub) Publish(ctx context.Context, targets []uint64, excludeConnID string, frame []byte) {
	if len(targets) == 0 {
		return
	}
	b, _ := json.Marshal(event{Targets: targets, ExcludeConnID: excludeConnID, Frame: frame})
	if err := h.rdb.Publish(ctx, eventsChannel, b).Err(); err != nil {
		slog.Error("redis publish failed, delivering locally only", "err", err)
		h.deliverLocal(targets, excludeConnID, frame)
	}
}

func (h *Hub) deliverLocal(targets []uint64, exclude string, frame []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, uid := range targets {
		for c := range h.conns[uid] {
			if c.connID == exclude {
				continue
			}
			c.enqueue(frame)
		}
	}
}

// DisconnectUser 先投递 frame，再断开目标用户的全部连接（跨节点走 im:events 通道）。
// 用于 token_version 变更后的 session_revoked：先推帧说明原因，再断开要求重新登录。
func (h *Hub) DisconnectUser(ctx context.Context, uid uint64, frame []byte) {
	b, _ := json.Marshal(event{Targets: []uint64{uid}, Frame: frame, Disconnect: true})
	if err := h.rdb.Publish(ctx, eventsChannel, b).Err(); err != nil {
		slog.Error("redis publish failed, disconnecting locally only", "err", err)
		h.deliverLocal([]uint64{uid}, "", frame)
		h.disconnectLocal([]uint64{uid})
	}
}

// disconnectLocal 断开本节点上目标用户的全部连接。
func (h *Hub) disconnectLocal(targets []uint64) {
	h.mu.RLock()
	var cs []*Client
	for _, uid := range targets {
		for c := range h.conns[uid] {
			cs = append(cs, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range cs {
		c.closeOnce()
	}
}

// Run 订阅 Redis 事件通道，直到 ctx 结束。
func (h *Hub) Run(ctx context.Context) error {
	go h.callSweeper(ctx) // v0.4.0：响铃超时巡检（每个节点都跑，终结的原子删除保证只有一个写者）
	sub := h.rdb.Subscribe(ctx, eventsChannel)
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil {
		return fmt.Errorf("订阅 Redis 失败: %w", err)
	}
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			var ev event
			if err := json.Unmarshal([]byte(msg.Payload), &ev); err != nil {
				slog.Warn("bad event", "err", err)
				continue
			}
			h.deliverLocal(ev.Targets, ev.ExcludeConnID, ev.Frame)
			if ev.Disconnect {
				h.disconnectLocal(ev.Targets)
			}
		}
	}
}

// LocalConnCount 本节点当前连接数。
func (h *Hub) LocalConnCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for _, s := range h.conns {
		n += len(s)
	}
	return n
}
