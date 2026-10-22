package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"imserver/internal/store"
)

// ---------- 1 对 1 通话信令（v0.4.0，SPEC §1.3） ----------
// 状态存 Redis（多实例一致）：im:call:{call_id} hash、im:call:user:{uid} 占用、
// im:call:ring:{call_id} 响铃标记（TTL）、im:call:ringing ZSET（score=响铃截止，巡检用）。
// 终结时用 Lua 原子删除状态，只有删成功的一方写通话记录，防多节点重复写。

const (
	callStatePrefix = "im:call:"
	callUserPrefix  = "im:call:user:"
	callRingPrefix  = "im:call:ring:"
	callRingingZSet = "im:call:ringing"
)

// callRecordStore 通话记录写入所需的 store 能力（由 api 层把 *store.Store 注入 Hub）。
type callRecordStore interface {
	InsertCallRecord(ctx context.Context, cid uint64, cc store.CallContent) (*store.Message, error)
	Members(ctx context.Context, cid uint64) ([]uint64, error)
}

// SetCallStore 注入通话记录存储（api.New 调用；未注入时通话信令可用但不写记录）。
func (h *Hub) SetCallStore(s callRecordStore) { h.callStore = s }

type callState struct {
	ID             string
	Caller         uint64
	Callee         uint64
	ConversationID uint64
	Kind           string // audio | video
	State          string // ringing | connected
	CreatedAt      int64  // Unix 毫秒
	ConnectedAt    int64  // Unix 毫秒，未接通为 0
}

func (st *callState) other(uid uint64) uint64 {
	if uid == st.Caller {
		return st.Callee
	}
	return st.Caller
}

func parseCallState(id string, m map[string]string) *callState {
	if len(m) == 0 {
		return nil
	}
	st := &callState{ID: id, Kind: m["kind"], State: m["state"]}
	st.Caller, _ = strconv.ParseUint(m["caller"], 10, 64)
	st.Callee, _ = strconv.ParseUint(m["callee"], 10, 64)
	st.ConversationID, _ = strconv.ParseUint(m["conversation_id"], 10, 64)
	st.CreatedAt, _ = strconv.ParseInt(m["created_at"], 10, 64)
	st.ConnectedAt, _ = strconv.ParseInt(m["connected_at"], 10, 64)
	if st.Caller == 0 || st.Callee == 0 {
		return nil
	}
	return st
}

func (h *Hub) loadCall(ctx context.Context, id string) (*callState, error) {
	m, err := h.rdb.HGetAll(ctx, callStatePrefix+id).Result()
	if err != nil {
		return nil, err
	}
	return parseCallState(id, m), nil
}

// createCallScript 原子地创建通话：0=成功 1=主叫忙 2=被叫忙 3=call_id 已存在。
var createCallScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then return 3 end
if redis.call('EXISTS', KEYS[2]) == 1 then return 1 end
if redis.call('EXISTS', KEYS[3]) == 1 then return 2 end
redis.call('HSET', KEYS[1],
  'caller', ARGV[1], 'callee', ARGV[2], 'conversation_id', ARGV[3], 'kind', ARGV[4],
  'state', 'ringing', 'created_at', ARGV[5], 'connected_at', 0)
redis.call('EXPIRE', KEYS[1], ARGV[6])
redis.call('SET', KEYS[2], ARGV[8], 'EX', ARGV[6])
redis.call('SET', KEYS[3], ARGV[8], 'EX', ARGV[6])
redis.call('SET', KEYS[5], '1', 'EX', ARGV[9])
redis.call('ZADD', KEYS[4], ARGV[7], ARGV[8])
return 0`)

func (h *Hub) createCall(ctx context.Context, st *callState, ringTimeout time.Duration) (int, error) {
	ttlSec := int(ringTimeout/time.Second) + 7200
	deadline := time.Now().Add(ringTimeout).Unix()
	n, err := createCallScript.Run(ctx, h.rdb, []string{
		callStatePrefix + st.ID, callUserPrefix + strconv.FormatUint(st.Caller, 10),
		callUserPrefix + strconv.FormatUint(st.Callee, 10), callRingingZSet, callRingPrefix + st.ID,
	}, st.Caller, st.Callee, st.ConversationID, st.Kind, st.CreatedAt, ttlSec, deadline, st.ID, int(ringTimeout/time.Second)).Int64()
	return int(n), err
}

// acceptCallScript ringing → connected，返回 1 表示流转成功。
var acceptCallScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'state') ~= 'ringing' then return 0 end
redis.call('HSET', KEYS[1], 'state', 'connected', 'connected_at', ARGV[1])
redis.call('ZREM', KEYS[2], ARGV[2])
redis.call('DEL', KEYS[3])
return 1`)

func (h *Hub) acceptCall(ctx context.Context, id string) bool {
	n, err := acceptCallScript.Run(ctx, h.rdb, []string{
		callStatePrefix + id, callRingingZSet, callRingPrefix + id,
	}, time.Now().UnixMilli(), id).Int64()
	return err == nil && n == 1
}

// endCallScript 原子删除通话全部状态并返回删除前的 hash 字段（空表示已被别人终结）。
var endCallScript = redis.NewScript(`
local f = redis.call('HGETALL', KEYS[1])
if #f == 0 then return f end
redis.call('DEL', KEYS[1], KEYS[2], KEYS[3], KEYS[5])
redis.call('ZREM', KEYS[4], ARGV[1])
return f`)

// sweepEndScript 巡检专用：仅当通话仍在 ringing 时才删除并返回字段；
// 已被接通（connected）的通话绝不由巡检终结（否则通话状态被删后无人写 completed 记录）。
var sweepEndScript = redis.NewScript(`
local f = redis.call('HGETALL', KEYS[1])
if #f == 0 then return f end
local m = {}
for i = 1, #f, 2 do m[f[i]] = f[i+1] end
if m['state'] ~= 'ringing' then return {} end
redis.call('DEL', KEYS[1], KEYS[2], KEYS[3], KEYS[5])
redis.call('ZREM', KEYS[4], ARGV[1])
return f`)

// endCall 终结通话。won=true 的调用方是唯一的终结者（负责写通话记录与通知）。
func (h *Hub) endCall(ctx context.Context, id string) (st *callState, won bool) {
	return h.endCallWith(ctx, id, endCallScript)
}

func (h *Hub) endCallRinging(ctx context.Context, id string) (st *callState, won bool) {
	return h.endCallWith(ctx, id, sweepEndScript)
}

func (h *Hub) endCallWith(ctx context.Context, id string, script *redis.Script) (st *callState, won bool) {
	// 先读参与方以拼 user key（状态被删后就读不到了）；真正的竞态由脚本的删除结果裁决。
	pre, err := h.loadCall(ctx, id)
	if err != nil || pre == nil {
		return nil, false
	}
	vals, err := script.Run(ctx, h.rdb, []string{
		callStatePrefix + id,
		callUserPrefix + strconv.FormatUint(pre.Caller, 10),
		callUserPrefix + strconv.FormatUint(pre.Callee, 10),
		callRingingZSet, callRingPrefix + id,
	}, id).StringSlice()
	if err != nil || len(vals) == 0 {
		return nil, false
	}
	m := make(map[string]string, len(vals)/2)
	for i := 0; i+1 < len(vals); i += 2 {
		m[vals[i]] = vals[i+1]
	}
	st = parseCallState(id, m)
	if st == nil {
		return nil, false
	}
	return st, true
}

// writeCallRecord 由终结者调用：写通话记录消息并广播给会话成员。
func (h *Hub) writeCallRecord(ctx context.Context, st *callState, outcome string) {
	if h.callStore == nil {
		slog.Warn("call store not set, call record skipped", "call", st.ID)
		return
	}
	var dur int64
	if outcome == "completed" && st.ConnectedAt > 0 {
		dur = time.Now().UnixMilli() - st.ConnectedAt
		if dur < 0 {
			dur = 0
		}
	}
	msg, err := h.callStore.InsertCallRecord(ctx, st.ConversationID, store.CallContent{
		CallID: st.ID, Kind: st.Kind, Outcome: outcome, DurationMs: dur, CallerID: st.Caller,
	})
	if err != nil {
		slog.Error("insert call record", "call", st.ID, "err", err)
		return
	}
	members, err := h.callStore.Members(ctx, st.ConversationID)
	if err != nil {
		return
	}
	h.Publish(ctx, members, "", encode("message", "", map[string]any{"message": msg}))
}

// callSweeper 巡检响铃超时：截止时间已过且仍在 ringing 的通话按未接听终结。
func (h *Hub) callSweeper(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := strconv.FormatInt(time.Now().Unix(), 10)
		ids, err := h.rdb.ZRangeByScore(ctx, callRingingZSet, &redis.ZRangeBy{Min: "-inf", Max: now, Count: 100}).Result()
		if err != nil {
			continue
		}
		for _, id := range ids {
			st, won := h.endCallRinging(ctx, id)
			if !won {
				continue
			}
			h.Publish(ctx, []uint64{st.Caller, st.Callee}, "", encode("call_hangup", "", map[string]any{
				"call_id": id, "conversation_id": st.ConversationID, "reason": "timeout",
			}))
			h.writeCallRecord(ctx, st, "missed")
		}
	}
}

// ---------- 客户端信令处理 ----------

type callReq struct {
	CallID         string          `json:"call_id"`
	ConversationID uint64          `json:"conversation_id"`
	Kind           string          `json:"kind"`
	Reason         string          `json:"reason"`
	Signal         json.RawMessage `json:"signal"`
}

func (c *Client) ringTimeout() time.Duration {
	if c.deps.CallRingTimeout > 0 {
		return c.deps.CallRingTimeout
	}
	return 60 * time.Second
}

func (c *Client) handleCallInvite(ctx context.Context, env Envelope) {
	var req callReq
	if err := json.Unmarshal(env.Data, &req); err != nil || req.CallID == "" || len(req.CallID) > 64 ||
		(req.Kind != "audio" && req.Kind != "video") {
		c.fail(env.ID, "bad_request", "参数错误")
		return
	}
	mem, ok := c.member(ctx, env.ID, req.ConversationID)
	if !ok {
		return
	}
	if mem.ConvType != store.ConvDirect {
		c.fail(env.ID, "forbidden", "v0.4.0 仅支持单聊通话")
		return
	}
	members, err := c.store.Members(ctx, req.ConversationID)
	if err != nil || len(members) != 2 {
		c.fail(env.ID, "internal", "服务器错误")
		return
	}
	callee := members[0]
	if callee == c.userID {
		callee = members[1]
	}
	if callee == c.userID {
		c.fail(env.ID, "bad_request", "不能给自己打电话")
		return
	}
	// 主叫自己已在另一通通话中：先拦下，避免给离线被叫误写未接听记录。
	// （映射指向本 call_id 时是同通话重试，继续走 createCall 的去重分支。）
	if cur, err := c.hub.rdb.Get(ctx, callUserPrefix+strconv.FormatUint(c.userID, 10)).Result(); err == nil && cur != req.CallID {
		c.fail(env.ID, "call_busy", "你正在另一通通话中")
		return
	}
	now := time.Now()
	st := &callState{ID: req.CallID, Caller: c.userID, Callee: callee,
		ConversationID: req.ConversationID, Kind: req.Kind, State: "ringing", CreatedAt: now.UnixMilli()}
	if !c.hub.IsOnline(ctx, callee) {
		c.hub.writeCallRecord(ctx, st, "missed")
		c.reply("call_unavailable", env.ID, map[string]any{
			"call_id": req.CallID, "conversation_id": req.ConversationID, "reason": "offline"})
		return
	}
	code, err := c.hub.createCall(ctx, st, c.ringTimeout())
	if err != nil {
		slog.Error("create call", "err", err)
		c.fail(env.ID, "internal", "服务器错误")
		return
	}
	inviteFrame := encode("call_invite", "", map[string]any{
		"call_id": req.CallID, "conversation_id": req.ConversationID, "from_user_id": c.userID,
		"kind": req.Kind, "created_at": now.UTC().Format(time.RFC3339),
	})
	switch code {
	case 0:
		c.hub.Publish(ctx, []uint64{callee}, "", inviteFrame)
	case 3: // 同 call_id 重试：已存在的通话且主叫是本人时把来电帧补发给被叫
		if cur, err := c.hub.loadCall(ctx, req.CallID); err == nil && cur != nil && cur.Caller == c.userID {
			c.hub.Publish(ctx, []uint64{callee}, "", inviteFrame)
			return
		}
		c.fail(env.ID, "bad_request", "call_id 已被占用")
	case 1:
		c.fail(env.ID, "call_busy", "你正在另一通通话中")
	case 2:
		c.hub.writeCallRecord(ctx, st, "busy")
		c.reply("call_unavailable", env.ID, map[string]any{
			"call_id": req.CallID, "conversation_id": req.ConversationID, "reason": "busy"})
	}
}

// loadCallFor 校验通话存在且当前用户是参与方，失败时已回复 error。
func (c *Client) loadCallFor(ctx context.Context, id, callID string) (*callState, bool) {
	st, err := c.hub.loadCall(ctx, callID)
	if err != nil {
		slog.Error("load call", "err", err)
		c.fail(id, "internal", "服务器错误")
		return nil, false
	}
	if st == nil {
		c.fail(id, "not_found", "通话不存在或已结束")
		return nil, false
	}
	if c.userID != st.Caller && c.userID != st.Callee {
		c.fail(id, "forbidden", "不是该通话的参与方")
		return nil, false
	}
	return st, true
}

func (c *Client) handleCallAccept(ctx context.Context, env Envelope) {
	var req callReq
	if err := json.Unmarshal(env.Data, &req); err != nil || req.CallID == "" {
		c.fail(env.ID, "bad_request", "参数错误")
		return
	}
	st, ok := c.loadCallFor(ctx, env.ID, req.CallID)
	if !ok {
		return
	}
	if c.userID != st.Callee {
		c.fail(env.ID, "forbidden", "只有被叫可以接听")
		return
	}
	if !c.hub.acceptCall(ctx, req.CallID) {
		c.fail(env.ID, "bad_request", "通话状态已变化")
		return
	}
	c.hub.Publish(ctx, []uint64{st.Caller}, "", encode("call_accept", "", map[string]any{
		"call_id": req.CallID, "conversation_id": st.ConversationID, "user_id": c.userID}))
	// 接听方的其他设备停止响铃
	c.hub.Publish(ctx, []uint64{st.Callee}, c.connID, encode("call_hangup", "", map[string]any{
		"call_id": req.CallID, "conversation_id": st.ConversationID, "user_id": c.userID, "reason": "answered_elsewhere"}))
}

// endAndNotify 终结通话（当前客户端已校验过参与方身份），赢得终结权后转发事件并写记录。
// ringingOnly=true 时仅当通话仍在响铃才终结（拒接/取消与接通竞态时以接通为准）。
func (c *Client) endAndNotify(ctx context.Context, req callReq, eventType string, outcome string, ringingOnly bool) {
	var st *callState
	var won bool
	if ringingOnly {
		st, won = c.hub.endCallRinging(ctx, req.CallID)
	} else {
		st, won = c.hub.endCall(ctx, req.CallID)
	}
	if !won {
		return // 已被对方/超时巡检终结，或响铃期间已被接通
	}
	data := map[string]any{"call_id": req.CallID, "conversation_id": st.ConversationID, "user_id": c.userID}
	if req.Reason != "" {
		data["reason"] = req.Reason
	}
	other := st.other(c.userID)
	c.hub.Publish(ctx, []uint64{other}, "", encode(eventType, "", data))
	// 自己其他设备同步状态
	c.hub.Publish(ctx, []uint64{c.userID}, c.connID, encode(eventType, "", data))
	c.hub.writeCallRecord(ctx, st, outcome)
}

func (c *Client) handleCallReject(ctx context.Context, env Envelope) {
	var req callReq
	if err := json.Unmarshal(env.Data, &req); err != nil || req.CallID == "" {
		c.fail(env.ID, "bad_request", "参数错误")
		return
	}
	st, ok := c.loadCallFor(ctx, env.ID, req.CallID)
	if !ok {
		return
	}
	if c.userID != st.Callee {
		c.fail(env.ID, "forbidden", "只有被叫可以拒绝")
		return
	}
	if st.State != "ringing" {
		c.fail(env.ID, "bad_request", "通话已接通，请挂断")
		return
	}
	c.endAndNotify(ctx, req, "call_reject", "rejected", true)
}

func (c *Client) handleCallCancel(ctx context.Context, env Envelope) {
	var req callReq
	if err := json.Unmarshal(env.Data, &req); err != nil || req.CallID == "" {
		c.fail(env.ID, "bad_request", "参数错误")
		return
	}
	st, ok := c.loadCallFor(ctx, env.ID, req.CallID)
	if !ok {
		return
	}
	if c.userID != st.Caller {
		c.fail(env.ID, "forbidden", "只有主叫可以取消")
		return
	}
	if st.State != "ringing" {
		c.fail(env.ID, "bad_request", "通话已接通，请挂断")
		return
	}
	c.endAndNotify(ctx, req, "call_cancel", "cancelled", true)
}

func (c *Client) handleCallHangup(ctx context.Context, env Envelope) {
	var req callReq
	if err := json.Unmarshal(env.Data, &req); err != nil || req.CallID == "" {
		c.fail(env.ID, "bad_request", "参数错误")
		return
	}
	st, ok := c.loadCallFor(ctx, env.ID, req.CallID)
	if !ok {
		return
	}
	outcome := "completed"
	if req.Reason == "failed" {
		outcome = "failed"
	} else if st.State == "ringing" {
		if c.userID == st.Caller {
			outcome = "cancelled"
		} else {
			outcome = "rejected"
		}
	}
	c.endAndNotify(ctx, req, "call_hangup", outcome, false)
}

const maxSignalBytes = 64 * 1024

func (c *Client) handleCallSignal(ctx context.Context, env Envelope) {
	if len(env.Data) > maxSignalBytes {
		c.fail(env.ID, "too_large", "signal 过大")
		return
	}
	var req callReq
	if err := json.Unmarshal(env.Data, &req); err != nil || req.CallID == "" || len(req.Signal) == 0 {
		c.fail(env.ID, "bad_request", "参数错误")
		return
	}
	st, ok := c.loadCallFor(ctx, env.ID, req.CallID)
	if !ok {
		return
	}
	c.hub.Publish(ctx, []uint64{st.other(c.userID)}, "", encode("call_signal", "", map[string]any{
		"call_id": req.CallID, "from_user_id": c.userID, "signal": req.Signal}))
}
