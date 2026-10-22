package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"imserver/internal/store"
	"imserver/internal/ws"
)

// ---------- 多人通话（v0.5.0，SPEC §3）：群聊 + Cloudflare Realtime SFU ----------
// 房间状态存 Redis（多实例一致）：
//   im:gcall:{call_id}          hash：conversation_id/kind/creator/created_at/state/alone_since
//   im:gcall:{call_id}:members  当前成员 set
//   im:gcall:{call_id}:ever     曾加入过的成员 set（写通话记录 joined_count 用）
//   im:gcall:{call_id}:tracks   hash："{uid}/{kind}" → {session_id, track_name, mid}
//   im:gcall:{call_id}:sessions hash：uid → 该用户在 SFU 侧的 session_id
//   im:gcall:conv:{cid}         群会话当前 open 房间的 call_id
// 用户通话占用与 1 对 1 共用同一键 im:call:user:{uid}（值都是 call_id），busy 语义一致。
// SFU 的 App Secret 只在服务端使用，绝不下发客户端、不写日志。

const (
	gcallPrefix   = "im:gcall:"
	gcallConvPfx  = "im:gcall:conv:"
	gcallUserPfx  = "im:call:user:" // 与 ws 包 1 对 1 通话共用（ws.callUserPrefix）
	gcallRoomTTL  = 24 * time.Hour  // 房间键安全 TTL（正常结束时主动删除）
	gcallAloneMax = 120 * time.Second
	gcallIDPrefix = "gc-"
)

var errSFUFailed = errors.New("sfu call failed")

func gcallRoomKey(id string) string     { return gcallPrefix + id }
func gcallMembersKey(id string) string  { return gcallPrefix + id + ":members" }
func gcallEverKey(id string) string     { return gcallPrefix + id + ":ever" }
func gcallTracksKey(id string) string   { return gcallPrefix + id + ":tracks" }
func gcallSessionsKey(id string) string { return gcallPrefix + id + ":sessions" }
func gcallConvKey(cid uint64) string    { return gcallConvPfx + strconv.FormatUint(cid, 10) }
func gcallUserKey(uid uint64) string    { return gcallUserPfx + strconv.FormatUint(uid, 10) }

func newGroupCallID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return gcallIDPrefix + hex.EncodeToString(b)
}

func pathCallID(r *http.Request) string { return r.PathValue("call_id") }

type gcallRoom struct {
	CallID         string
	ConversationID uint64
	Kind           string // audio | video
	Creator        uint64
	CreatedAt      int64 // Unix 毫秒
	State          string
	AloneSince     int64 // Unix 毫秒，房间只剩 1 人时的时间；否则 0
}

func (s *Server) groupCallEnabled() bool {
	return s.cfg.Calls.GroupEnabled && s.cfg.Calls.GroupConfigured()
}

func (s *Server) loadGroupRoom(ctx context.Context, callID string) (*gcallRoom, error) {
	m, err := s.rdb.HGetAll(ctx, gcallRoomKey(callID)).Result()
	if err != nil {
		return nil, err
	}
	if len(m) == 0 {
		return nil, nil
	}
	room := &gcallRoom{CallID: callID, Kind: m["kind"], State: m["state"]}
	room.ConversationID, _ = strconv.ParseUint(m["conversation_id"], 10, 64)
	room.Creator, _ = strconv.ParseUint(m["creator"], 10, 64)
	room.CreatedAt, _ = strconv.ParseInt(m["created_at"], 10, 64)
	room.AloneSince, _ = strconv.ParseInt(m["alone_since"], 10, 64)
	if room.ConversationID == 0 || room.Creator == 0 {
		return nil, nil
	}
	return room, nil
}

// checkDelScript 仅当键值等于期望值时才删除（占用键/会话→房间映射的归属校验）。
var checkDelScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0`)

func (s *Server) checkDel(ctx context.Context, key, want string) {
	if err := checkDelScript.Run(ctx, s.rdb, []string{key}, want).Err(); err != nil {
		slog.Warn("gcall checkdel", "key", key, "err", err)
	}
}

// createRoomScript 原子建房：0=成功 2=发起人忙（在别的通话里）3=房间已存在 4=该群已有 open 房间。
var createRoomScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then return 3 end
if redis.call('EXISTS', KEYS[7]) == 1 then return 2 end
if redis.call('SET', KEYS[6], ARGV[5], 'NX') == false then return 4 end
redis.call('HSET', KEYS[1],
  'conversation_id', ARGV[1], 'kind', ARGV[2], 'creator', ARGV[3],
  'created_at', ARGV[4], 'state', 'open', 'alone_since', ARGV[4])
redis.call('SADD', KEYS[2], ARGV[3])
redis.call('SADD', KEYS[3], ARGV[3])
redis.call('SET', KEYS[7], ARGV[5], 'EX', ARGV[6])
for i = 1, 5 do redis.call('EXPIRE', KEYS[i], ARGV[6]) end
return 0`)

// joinRoomScript 原子加入：0=新加入 1=已是成员 2=忙 3=房间已满 4=房间不存在/已结束。
var joinRoomScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'state') ~= 'open' then return 4 end
local occ = redis.call('GET', KEYS[4])
if occ and occ ~= ARGV[2] then return 2 end
local isMember = redis.call('SISMEMBER', KEYS[2], ARGV[1]) == 1
if not isMember and redis.call('SCARD', KEYS[2]) >= tonumber(ARGV[3]) then return 3 end
redis.call('SADD', KEYS[2], ARGV[1])
redis.call('SADD', KEYS[3], ARGV[1])
redis.call('SET', KEYS[4], ARGV[2], 'EX', ARGV[5])
local n = redis.call('SCARD', KEYS[2])
if n == 1 then redis.call('HSET', KEYS[1], 'alone_since', ARGV[4])
else redis.call('HSET', KEYS[1], 'alone_since', '0') end
if isMember then return 1 end
return 0`)

func (s *Server) joinRoomRedis(ctx context.Context, room *gcallRoom, uid uint64) (int, error) {
	n, err := joinRoomScript.Run(ctx, s.rdb, []string{
		gcallRoomKey(room.CallID), gcallMembersKey(room.CallID), gcallEverKey(room.CallID), gcallUserKey(uid),
	}, uid, room.CallID, s.cfg.Calls.GroupMaxParticipants, time.Now().UnixMilli(), int(gcallRoomTTL/time.Second)).Int64()
	return int(n), err
}

// ---------- SFU HTTP 客户端 ----------

func (s *Server) sfuRequest(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.cfg.Calls.BaseURL+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.Calls.SFUAppSecret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return resp.StatusCode, raw, err
}

func (s *Server) sfuSessionPath() string { return "/apps/" + s.cfg.Calls.SFUAppID + "/sessions" }

// sfuNewSession 在 Cloudflare Calls 侧为某参与者新建独立 session。
func (s *Server) sfuNewSession(ctx context.Context) (string, error) {
	status, raw, err := s.sfuRequest(ctx, "POST", s.sfuSessionPath()+"/new", []byte("{}"))
	if err != nil {
		slog.Warn("sfu new session", "err", err)
		return "", errSFUFailed
	}
	if status != 200 && status != 201 {
		slog.Warn("sfu new session bad status", "status", status)
		return "", errSFUFailed
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.SessionID == "" {
		slog.Warn("sfu new session bad response")
		return "", errSFUFailed
	}
	return out.SessionID, nil
}

// attachSession 为已在成员集中的用户建 SFU session 并登记；失败时调用方负责回滚成员身份。
func (s *Server) attachSession(ctx context.Context, room *gcallRoom, uid uint64) (string, error) {
	sid, err := s.sfuNewSession(ctx)
	if err != nil {
		return "", err
	}
	if err := s.rdb.HSet(ctx, gcallSessionsKey(room.CallID), strconv.FormatUint(uid, 10), sid).Err(); err != nil {
		return "", err
	}
	return sid, nil
}

// detachMember 把用户移出房间（session 创建失败回滚 / leave 共用），返回剩余成员数。
func (s *Server) detachMember(ctx context.Context, room *gcallRoom, uid uint64, wasEver bool) int64 {
	pipe := s.rdb.Pipeline()
	pipe.SRem(ctx, gcallMembersKey(room.CallID), uid)
	if wasEver {
		pipe.SRem(ctx, gcallEverKey(room.CallID), uid)
	}
	pipe.HDel(ctx, gcallSessionsKey(room.CallID), strconv.FormatUint(uid, 10))
	_, _ = pipe.Exec(ctx)
	s.checkDel(ctx, gcallUserKey(uid), room.CallID)
	n, _ := s.rdb.SCard(ctx, gcallMembersKey(room.CallID)).Result()
	switch {
	case n == 1:
		s.rdb.HSet(ctx, gcallRoomKey(room.CallID), "alone_since", time.Now().UnixMilli())
	case n > 1:
		s.rdb.HSet(ctx, gcallRoomKey(room.CallID), "alone_since", 0)
	}
	return n
}

// roomParticipants 当前成员的 PublicUser 列表（按 user_id 升序，稳定输出）。
func (s *Server) roomParticipants(ctx context.Context, room *gcallRoom) []*store.PublicUser {
	ids, err := s.rdb.SMembers(ctx, gcallMembersKey(room.CallID)).Result()
	if err != nil || len(ids) == 0 {
		return []*store.PublicUser{}
	}
	uids := make([]uint64, 0, len(ids))
	for _, x := range ids {
		if id, err := strconv.ParseUint(x, 10, 64); err == nil && id > 0 {
			uids = append(uids, id)
		}
	}
	users, err := s.store.GetUsers(ctx, uids)
	if err != nil {
		return []*store.PublicUser{}
	}
	out := []*store.PublicUser{}
	for _, id := range uids {
		if u := users[id]; u != nil {
			out = append(out, u.Public())
		}
	}
	return out
}

// broadcastRoom 把帧发给群会话全体成员（在线的才收得到），exclude 为不发的用户（0 = 都发）。
func (s *Server) broadcastRoom(ctx context.Context, room *gcallRoom, exclude uint64, frame []byte) {
	members, err := s.store.Members(ctx, room.ConversationID)
	if err != nil {
		slog.Warn("gcall broadcast members", "call", room.CallID, "err", err)
		return
	}
	targets := make([]uint64, 0, len(members))
	for _, id := range members {
		if id != exclude {
			targets = append(targets, id)
		}
	}
	s.hub.Publish(ctx, targets, "", frame)
}

// respondRoom 写建房 / join 的统一响应。
func (s *Server) respondRoom(w http.ResponseWriter, r *http.Request, room *gcallRoom, uid uint64, sessionID string) {
	out := map[string]any{
		"call_id":      room.CallID,
		"session_id":   sessionID,
		"participants": s.roomParticipants(r.Context(), room),
	}
	if raw, err := s.turnICEServers(r.Context(), uid); err == nil {
		out["ice_servers"] = json.RawMessage(raw)
	}
	writeJSON(w, 200, out)
}

// requireGroupMemberForCall 校验会话存在、是群聊、且 uid 是成员；失败已写响应。
// 返回 false 时：会话不存在 404、非群聊 400 not_group、非成员 403 not_member。
func (s *Server) requireGroupMemberForCall(w http.ResponseWriter, ctx context.Context, cid, uid uint64) bool {
	meta, err := s.store.GetGroupMeta(ctx, cid)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, 404, "not_found", "会话不存在")
		return false
	}
	if err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return false
	}
	if meta.Type != store.ConvGroup {
		writeErr(w, 400, "not_group", "多人通话只能在群聊里发起")
		return false
	}
	if _, err := s.store.GetMember(ctx, cid, uid); err != nil {
		if errors.Is(err, store.ErrNotMember) {
			writeErr(w, 403, "not_member", "不是该群成员")
			return false
		}
		writeErr(w, 500, "internal", "服务器错误")
		return false
	}
	return true
}

// createGroupCall POST /api/calls/group {conversation_id, kind}：建房或加入已有 open 房间。
func (s *Server) createGroupCall(w http.ResponseWriter, r *http.Request, uid uint64) {
	if !s.groupCallEnabled() {
		writeErr(w, 501, "group_call_disabled", "服务器未启用多人通话")
		return
	}
	var req struct {
		ConversationID uint64 `json:"conversation_id"`
		Kind           string `json:"kind"`
	}
	if err := decode(r, &req); err != nil || req.ConversationID == 0 ||
		(req.Kind != "audio" && req.Kind != "video") {
		writeErr(w, 400, "bad_request", "conversation_id 与 kind(audio|video) 必填")
		return
	}
	ctx := r.Context()
	if !s.requireGroupMemberForCall(w, ctx, req.ConversationID, uid) {
		return
	}
	// 该群已有 open 房间 → 等同 join
	if existing, err := s.rdb.Get(ctx, gcallConvKey(req.ConversationID)).Result(); err == nil && existing != "" {
		if room, err := s.loadGroupRoom(ctx, existing); err == nil && room != nil && room.State == "open" {
			s.joinExistingRoom(w, r, room, uid)
			return
		}
	}
	callID := newGroupCallID()
	now := time.Now()
	room := &gcallRoom{CallID: callID, ConversationID: req.ConversationID, Kind: req.Kind,
		Creator: uid, CreatedAt: now.UnixMilli(), State: "open", AloneSince: now.UnixMilli()}
	code, err := createRoomScript.Run(ctx, s.rdb, []string{
		gcallRoomKey(callID), gcallMembersKey(callID), gcallEverKey(callID), gcallTracksKey(callID),
		gcallSessionsKey(callID), gcallConvKey(req.ConversationID), gcallUserKey(uid),
	}, req.ConversationID, req.Kind, uid, room.CreatedAt, callID, int(gcallRoomTTL/time.Second)).Int64()
	if err != nil {
		slog.Error("gcall create room", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	switch code {
	case 2:
		writeErr(w, 409, "call_unavailable", "你正在另一通通话中")
		return
	case 3, 4: // 竞态：房间/该群房间刚被别人建好，转 join
		if code == 4 {
			existing, err := s.rdb.Get(ctx, gcallConvKey(req.ConversationID)).Result()
			if err != nil || existing == "" {
				writeErr(w, 500, "internal", "服务器错误")
				return
			}
			callID = existing
		}
		room2, err := s.loadGroupRoom(ctx, callID)
		if err != nil || room2 == nil || room2.State != "open" {
			writeErr(w, 404, "room_gone", "房间不存在或已结束")
			return
		}
		s.joinExistingRoom(w, r, room2, uid)
		return
	}
	sessionID, err := s.attachSession(ctx, room, uid)
	if err != nil {
		s.hardDeleteRoom(ctx, room) // 房间还没对外公布，不写通话记录
		writeErr(w, 502, "sfu_failed", "创建通话失败，请稍后重试")
		return
	}
	s.broadcastRoom(ctx, room, uid, ws.Encode("group_call_started", map[string]any{
		"call_id": callID, "conversation_id": req.ConversationID, "kind": req.Kind, "creator": uid,
	}))
	s.respondRoom(w, r, room, uid, sessionID)
}

// joinExistingRoom join 语义的公共实现（POST /calls/group 命中已有房间与 /join 共用）。
func (s *Server) joinExistingRoom(w http.ResponseWriter, r *http.Request, room *gcallRoom, uid uint64) {
	ctx := r.Context()
	code, err := s.joinRoomRedis(ctx, room, uid)
	if err != nil {
		slog.Error("gcall join room", "call", room.CallID, "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	switch code {
	case 2:
		writeErr(w, 409, "call_unavailable", "你正在另一通通话中")
		return
	case 3:
		writeErr(w, 403, "room_full", "通话人数已满")
		return
	case 4:
		writeErr(w, 404, "room_gone", "房间不存在或已结束")
		return
	}
	sessionID, err := s.attachSession(ctx, room, uid)
	if err != nil {
		if code == 0 {
			if s.detachMember(ctx, room, uid, true) == 0 {
				s.endGroupRoom(ctx, room.CallID, false)
			}
		}
		writeErr(w, 502, "sfu_failed", "加入通话失败，请稍后重试")
		return
	}
	if code == 0 {
		if me, err := s.store.GetUser(ctx, uid); err == nil {
			s.broadcastRoom(ctx, room, uid, ws.Encode("group_call_joined", map[string]any{
				"call_id": room.CallID, "user": me.Public(),
			}))
		}
	}
	s.respondRoom(w, r, room, uid, sessionID)
}

// joinGroupCall POST /api/calls/group/{call_id}/join。
func (s *Server) joinGroupCall(w http.ResponseWriter, r *http.Request, uid uint64) {
	if !s.groupCallEnabled() {
		writeErr(w, 501, "group_call_disabled", "服务器未启用多人通话")
		return
	}
	ctx := r.Context()
	room, err := s.loadGroupRoom(ctx, pathCallID(r))
	if err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	if room == nil || room.State != "open" {
		writeErr(w, 404, "room_gone", "房间不存在或已结束")
		return
	}
	if _, err := s.store.GetMember(ctx, room.ConversationID, uid); err != nil {
		if errors.Is(err, store.ErrNotMember) {
			writeErr(w, 403, "not_member", "不是该群成员")
			return
		}
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	s.joinExistingRoom(w, r, room, uid)
}

// roomAndSession 取房间与调用方自己的 SFU session（publish/subscribe/renegotiate/close 共用校验）。
func (s *Server) roomAndSession(w http.ResponseWriter, r *http.Request, uid uint64) (*gcallRoom, string, bool) {
	if !s.groupCallEnabled() {
		writeErr(w, 501, "group_call_disabled", "服务器未启用多人通话")
		return nil, "", false
	}
	ctx := r.Context()
	room, err := s.loadGroupRoom(ctx, pathCallID(r))
	if err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return nil, "", false
	}
	if room == nil || room.State != "open" {
		writeErr(w, 404, "room_gone", "房间不存在或已结束")
		return nil, "", false
	}
	sid, err := s.rdb.HGet(ctx, gcallSessionsKey(room.CallID), strconv.FormatUint(uid, 10)).Result()
	if err != nil || sid == "" {
		writeErr(w, 400, "bad_request", "尚未加入该通话")
		return nil, "", false
	}
	return room, sid, true
}

type sfuSessionDesc struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
}

func (d *sfuSessionDesc) valid(typ string) bool {
	return d != nil && d.Type == typ && d.SDP != "" && len(d.SDP) <= 128*1024
}

// gcallTrackInfo 登记在 Redis tracks hash 里的轨道信息。
type gcallTrackInfo struct {
	SessionID string `json:"session_id"`
	TrackName string `json:"track_name"`
	MID       string `json:"mid"`
}

type cfTrackResult struct {
	TrackName string `json:"trackName"`
	MID       string `json:"mid"`
	ErrorCode string `json:"errorCode,omitempty"`
}

type cfTracksResponse struct {
	ErrorCode string          `json:"errorCode,omitempty"`
	Tracks    []cfTrackResult `json:"tracks"`
}

// sfuTracksNew 代理 Cloudflare tracks/new 并校验响应（错误统一 errSFUFailed）。
func (s *Server) sfuTracksNew(ctx context.Context, sessionID string, tracks []map[string]any, desc *sfuSessionDesc) ([]byte, *cfTracksResponse, error) {
	body := map[string]any{"tracks": tracks}
	if desc != nil {
		body["sessionDescription"] = map[string]string{"type": desc.Type, "sdp": desc.SDP}
	}
	raw, _ := json.Marshal(body)
	status, respRaw, err := s.sfuRequest(ctx, "POST", s.sfuSessionPath()+"/"+sessionID+"/tracks/new", raw)
	if err != nil {
		slog.Warn("sfu tracks/new", "err", err)
		return nil, nil, errSFUFailed
	}
	if status != 200 {
		slog.Warn("sfu tracks/new bad status", "status", status)
		return nil, nil, errSFUFailed
	}
	var parsed cfTracksResponse
	if err := json.Unmarshal(respRaw, &parsed); err != nil || parsed.ErrorCode != "" {
		slog.Warn("sfu tracks/new bad response")
		return nil, nil, errSFUFailed
	}
	for _, t := range parsed.Tracks {
		if t.ErrorCode != "" {
			slog.Warn("sfu tracks/new track error", "track", t.TrackName, "code", t.ErrorCode)
			return nil, nil, errSFUFailed
		}
	}
	return respRaw, &parsed, nil
}

func writeSFURaw(w http.ResponseWriter, raw []byte) {
	if len(raw) == 0 {
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(200)
	_, _ = w.Write(raw)
}

// publishGroupTracks POST /api/calls/group/{call_id}/tracks/publish：发布本人轨道。
// track_name 必须等于服务端分配的 {uid}/audio|video，防冒名。
func (s *Server) publishGroupTracks(w http.ResponseWriter, r *http.Request, uid uint64) {
	room, sid, ok := s.roomAndSession(w, r, uid)
	if !ok {
		return
	}
	var req struct {
		SessionDescription *sfuSessionDesc `json:"session_description"`
		Tracks             []struct {
			TrackName string `json:"track_name"`
			Kind      string `json:"kind"`
		} `json:"tracks"`
	}
	if err := decode(r, &req); err != nil || !req.SessionDescription.valid("offer") ||
		len(req.Tracks) == 0 || len(req.Tracks) > 2 {
		writeErr(w, 400, "bad_request", "参数错误")
		return
	}
	seen := map[string]bool{}
	cfTracks := make([]map[string]any, 0, len(req.Tracks))
	for _, t := range req.Tracks {
		if (t.Kind != "audio" && t.Kind != "video") || seen[t.Kind] ||
			t.TrackName != fmt.Sprintf("%d/%s", uid, t.Kind) {
			writeErr(w, 400, "bad_track", "轨道名与服务端分配的不一致")
			return
		}
		seen[t.Kind] = true
		cfTracks = append(cfTracks, map[string]any{"location": "local", "trackName": t.TrackName})
	}
	ctx := r.Context()
	raw, parsed, err := s.sfuTracksNew(ctx, sid, cfTracks, req.SessionDescription)
	if err != nil {
		writeErr(w, 502, "sfu_failed", "发布轨道失败，请稍后重试")
		return
	}
	mids := map[string]string{}
	for _, t := range parsed.Tracks {
		mids[t.TrackName] = t.MID
	}
	out := make([]map[string]any, 0, len(req.Tracks))
	pipe := s.rdb.Pipeline()
	for _, t := range req.Tracks {
		info, _ := json.Marshal(gcallTrackInfo{SessionID: sid, TrackName: t.TrackName, MID: mids[t.TrackName]})
		pipe.HSet(ctx, gcallTracksKey(room.CallID), fmt.Sprintf("%d/%s", uid, t.Kind), info)
		out = append(out, map[string]any{"track_name": t.TrackName, "kind": t.Kind, "mid": mids[t.TrackName]})
	}
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Error("gcall register tracks", "call", room.CallID, "err", err)
	}
	s.broadcastRoom(ctx, room, uid, ws.Encode("group_call_tracks", map[string]any{
		"call_id": room.CallID, "user_id": uid, "tracks": out,
	}))
	writeSFURaw(w, raw)
}

// subscribeGroupTracks POST /api/calls/group/{call_id}/tracks/subscribe：订阅他人轨道。
func (s *Server) subscribeGroupTracks(w http.ResponseWriter, r *http.Request, uid uint64) {
	room, sid, ok := s.roomAndSession(w, r, uid)
	if !ok {
		return
	}
	var req struct {
		SessionDescription *sfuSessionDesc `json:"session_description"`
		Tracks             []struct {
			OwnerID uint64 `json:"owner_id"`
			Kind    string `json:"kind"`
		} `json:"tracks"`
	}
	if err := decode(r, &req); err != nil || len(req.Tracks) == 0 || len(req.Tracks) > 32 {
		writeErr(w, 400, "bad_request", "参数错误")
		return
	}
	if req.SessionDescription != nil && !req.SessionDescription.valid("offer") {
		writeErr(w, 400, "bad_request", "session_description 无效")
		return
	}
	ctx := r.Context()
	cfTracks := make([]map[string]any, 0, len(req.Tracks))
	for _, t := range req.Tracks {
		if (t.Kind != "audio" && t.Kind != "video") || t.OwnerID == 0 || t.OwnerID == uid {
			writeErr(w, 400, "bad_track", "轨道参数无效")
			return
		}
		rawInfo, err := s.rdb.HGet(ctx, gcallTracksKey(room.CallID), fmt.Sprintf("%d/%s", t.OwnerID, t.Kind)).Result()
		if err != nil || rawInfo == "" {
			writeErr(w, 400, "bad_track", "轨道不存在或已关闭")
			return
		}
		var info gcallTrackInfo
		if err := json.Unmarshal([]byte(rawInfo), &info); err != nil || info.SessionID == "" {
			writeErr(w, 400, "bad_track", "轨道不存在或已关闭")
			return
		}
		cfTracks = append(cfTracks, map[string]any{
			"location": "remote", "trackName": info.TrackName, "sessionId": info.SessionID,
		})
	}
	raw, _, err := s.sfuTracksNew(ctx, sid, cfTracks, req.SessionDescription)
	if err != nil {
		writeErr(w, 502, "sfu_failed", "订阅轨道失败，请稍后重试")
		return
	}
	writeSFURaw(w, raw)
}

// renegotiateGroupCall POST /api/calls/group/{call_id}/renegotiate：透传 answer 给 Cloudflare。
func (s *Server) renegotiateGroupCall(w http.ResponseWriter, r *http.Request, uid uint64) {
	room, sid, ok := s.roomAndSession(w, r, uid)
	if !ok {
		return
	}
	var req struct {
		SessionDescription *sfuSessionDesc `json:"session_description"`
	}
	if err := decode(r, &req); err != nil || !req.SessionDescription.valid("answer") {
		writeErr(w, 400, "bad_request", "session_description 无效")
		return
	}
	body, _ := json.Marshal(map[string]any{
		"sessionDescription": map[string]string{"type": req.SessionDescription.Type, "sdp": req.SessionDescription.SDP},
	})
	status, raw, err := s.sfuRequest(r.Context(), "PUT", s.sfuSessionPath()+"/"+sid+"/renegotiate", body)
	if err != nil || status != 200 {
		if err != nil {
			slog.Warn("sfu renegotiate", "err", err)
		} else {
			slog.Warn("sfu renegotiate bad status", "status", status)
		}
		writeErr(w, 502, "sfu_failed", "重协商失败，请稍后重试")
		return
	}
	_ = room
	writeSFURaw(w, raw)
}

// closeGroupTracks POST /api/calls/group/{call_id}/tracks/close {kinds:[...]}：关本人轨道。
func (s *Server) closeGroupTracks(w http.ResponseWriter, r *http.Request, uid uint64) {
	room, sid, ok := s.roomAndSession(w, r, uid)
	if !ok {
		return
	}
	var req struct {
		SessionDescription *sfuSessionDesc `json:"session_description"`
		Kinds              []string        `json:"kinds"`
	}
	if err := decode(r, &req); err != nil || len(req.Kinds) == 0 {
		writeErr(w, 400, "bad_request", "kinds 必填")
		return
	}
	if req.SessionDescription != nil && !req.SessionDescription.valid("answer") {
		writeErr(w, 400, "bad_request", "session_description 无效")
		return
	}
	ctx := r.Context()
	type pending struct {
		kind  string
		field string
		info  gcallTrackInfo
	}
	var pend []pending
	for _, kind := range req.Kinds {
		if kind != "audio" && kind != "video" {
			writeErr(w, 400, "bad_track", "轨道类型无效")
			return
		}
		field := fmt.Sprintf("%d/%s", uid, kind)
		rawInfo, err := s.rdb.HGet(ctx, gcallTracksKey(room.CallID), field).Result()
		if err != nil || rawInfo == "" {
			continue // 未发布过该轨道：幂等跳过
		}
		var info gcallTrackInfo
		if err := json.Unmarshal([]byte(rawInfo), &info); err != nil {
			continue
		}
		pend = append(pend, pending{kind: kind, field: field, info: info})
	}
	if len(pend) == 0 {
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	cfTracks := make([]map[string]any, 0, len(pend))
	for _, p := range pend {
		cfTracks = append(cfTracks, map[string]any{"mid": p.info.MID})
	}
	body := map[string]any{"tracks": cfTracks}
	if req.SessionDescription != nil {
		body["sessionDescription"] = map[string]string{"type": req.SessionDescription.Type, "sdp": req.SessionDescription.SDP}
	}
	rawBody, _ := json.Marshal(body)
	status, raw, err := s.sfuRequest(ctx, "PUT", s.sfuSessionPath()+"/"+sid+"/tracks/close", rawBody)
	if err != nil || status != 200 {
		if err != nil {
			slog.Warn("sfu tracks/close", "err", err)
		} else {
			slog.Warn("sfu tracks/close bad status", "status", status)
		}
		writeErr(w, 502, "sfu_failed", "关闭轨道失败，请稍后重试")
		return
	}
	fields := make([]string, 0, len(pend))
	kinds := make([]string, 0, len(pend))
	for _, p := range pend {
		fields = append(fields, p.field)
		kinds = append(kinds, p.kind)
	}
	s.rdb.HDel(ctx, gcallTracksKey(room.CallID), fields...)
	s.broadcastRoom(ctx, room, uid, ws.Encode("group_call_tracks_closed", map[string]any{
		"call_id": room.CallID, "user_id": uid, "kinds": kinds,
	}))
	writeSFURaw(w, raw)
}

// leaveGroupCall POST /api/calls/group/{call_id}/leave：关本人轨道并退出房间。
func (s *Server) leaveGroupCall(w http.ResponseWriter, r *http.Request, uid uint64) {
	if !s.groupCallEnabled() {
		writeErr(w, 501, "group_call_disabled", "服务器未启用多人通话")
		return
	}
	ctx := r.Context()
	room, err := s.loadGroupRoom(ctx, pathCallID(r))
	if err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	if room == nil || room.State != "open" {
		writeErr(w, 404, "room_gone", "房间不存在或已结束")
		return
	}
	isMember, _ := s.rdb.SIsMember(ctx, gcallMembersKey(room.CallID), uid).Result()
	if !isMember {
		writeErr(w, 404, "room_gone", "房间不存在或已结束")
		return
	}
	s.closeOwnTracksBestEffort(ctx, room, uid)
	if n := s.detachMember(ctx, room, uid, false); n == 0 {
		s.endGroupRoom(ctx, room.CallID, false)
	}
	s.broadcastRoom(ctx, room, uid, ws.Encode("group_call_left", map[string]any{
		"call_id": room.CallID, "user_id": uid,
	}))
	writeJSON(w, 200, map[string]any{"ok": true})
}

// closeOwnTracksBestEffort 离开/异常时尽力在 SFU 侧关掉本人全部轨道并清登记（失败只记日志）。
func (s *Server) closeOwnTracksBestEffort(ctx context.Context, room *gcallRoom, uid uint64) {
	all, err := s.rdb.HGetAll(ctx, gcallTracksKey(room.CallID)).Result()
	if err != nil {
		return
	}
	prefix := strconv.FormatUint(uid, 10) + "/"
	var fields []string
	var mids []map[string]any
	for field, rawInfo := range all {
		if !strings.HasPrefix(field, prefix) {
			continue
		}
		fields = append(fields, field)
		var info gcallTrackInfo
		if err := json.Unmarshal([]byte(rawInfo), &info); err == nil && info.MID != "" {
			mids = append(mids, map[string]any{"mid": info.MID})
		}
	}
	if len(fields) > 0 {
		s.rdb.HDel(ctx, gcallTracksKey(room.CallID), fields...)
	}
	if len(mids) == 0 {
		return
	}
	sid, err := s.rdb.HGet(ctx, gcallSessionsKey(room.CallID), strconv.FormatUint(uid, 10)).Result()
	if err != nil || sid == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{"tracks": mids})
	if status, _, err := s.sfuRequest(ctx, "PUT", s.sfuSessionPath()+"/"+sid+"/tracks/close", body); err != nil || status != 200 {
		slog.Warn("gcall close tracks on leave", "call", room.CallID, "status", status, "err", err)
	}
}

// getGroupCall GET /api/calls/group/{call_id}：断线重连恢复用（状态/成员/轨道清单）。
func (s *Server) getGroupCall(w http.ResponseWriter, r *http.Request, uid uint64) {
	if !s.groupCallEnabled() {
		writeErr(w, 501, "group_call_disabled", "服务器未启用多人通话")
		return
	}
	ctx := r.Context()
	room, err := s.loadGroupRoom(ctx, pathCallID(r))
	if err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	if room == nil || room.State != "open" {
		writeErr(w, 404, "room_gone", "房间不存在或已结束")
		return
	}
	if _, err := s.store.GetMember(ctx, room.ConversationID, uid); err != nil {
		if errors.Is(err, store.ErrNotMember) {
			writeErr(w, 403, "not_member", "不是该群成员")
			return
		}
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	tracks := []map[string]any{}
	if all, err := s.rdb.HGetAll(ctx, gcallTracksKey(room.CallID)).Result(); err == nil {
		for field, rawInfo := range all {
			parts := strings.SplitN(field, "/", 2)
			if len(parts) != 2 {
				continue
			}
			owner, _ := strconv.ParseUint(parts[0], 10, 64)
			var info gcallTrackInfo
			_ = json.Unmarshal([]byte(rawInfo), &info)
			tracks = append(tracks, map[string]any{
				"user_id": owner, "track_name": info.TrackName, "kind": parts[1], "mid": info.MID,
			})
		}
	}
	writeJSON(w, 200, map[string]any{
		"call_id":         room.CallID,
		"conversation_id": room.ConversationID,
		"kind":            room.Kind,
		"state":           room.State,
		"creator":         room.Creator,
		"created_at":      time.UnixMilli(room.CreatedAt).UTC().Format(time.RFC3339),
		"participants":    s.roomParticipants(ctx, room),
		"tracks":          tracks,
	})
}

// ---------- 房间结束与清扫 ----------

// claimEndScript 抢房间终结权：只有把 state 从 open 改成 ended 的一方写通话记录。
var claimEndScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'state') ~= 'open' then return 0 end
redis.call('HSET', KEYS[1], 'state', 'ended')
return 1`)

// endGroupRoom 终结房间：清状态、释放占用、写通话记录消息、广播 group_call_ended。
func (s *Server) endGroupRoom(ctx context.Context, callID string, failed bool) {
	room, err := s.loadGroupRoom(ctx, callID)
	if err != nil || room == nil {
		return
	}
	won, err := claimEndScript.Run(ctx, s.rdb, []string{gcallRoomKey(callID)}).Int64()
	if err != nil || won != 1 {
		return
	}
	memberIDs, _ := s.rdb.SMembers(ctx, gcallMembersKey(callID)).Result()
	everIDs, _ := s.rdb.SMembers(ctx, gcallEverKey(callID)).Result()
	pipe := s.rdb.Pipeline()
	pipe.Del(ctx, gcallRoomKey(callID), gcallMembersKey(callID), gcallEverKey(callID),
		gcallTracksKey(callID), gcallSessionsKey(callID))
	_, _ = pipe.Exec(ctx)
	s.checkDel(ctx, gcallConvKey(room.ConversationID), callID)
	for _, x := range memberIDs {
		if id, err := strconv.ParseUint(x, 10, 64); err == nil && id > 0 {
			s.checkDel(ctx, gcallUserKey(id), callID)
		}
	}
	joined := len(everIDs)
	outcome := "missed"
	var dur int64
	switch {
	case failed:
		outcome = "failed"
	case joined >= 2:
		outcome = "completed"
		dur = time.Now().UnixMilli() - room.CreatedAt
		if dur < 0 {
			dur = 0
		}
	}
	participantCount := joined
	if members, err := s.store.Members(ctx, room.ConversationID); err == nil && len(members) > 0 {
		participantCount = len(members)
	}
	msg, err := s.store.InsertCallRecord(ctx, room.ConversationID, store.CallContent{
		CallID: callID, Kind: room.Kind, Outcome: outcome, DurationMs: dur, CallerID: room.Creator,
		Mode: "group", ParticipantCount: participantCount, JoinedCount: joined,
	})
	if err != nil {
		slog.Error("gcall insert record", "call", callID, "err", err)
		return
	}
	s.broadcastRoom(ctx, room, 0, ws.Encode("message", map[string]any{"message": msg}))
	s.broadcastRoom(ctx, room, 0, ws.Encode("group_call_ended", map[string]any{
		"call_id": callID, "conversation_id": room.ConversationID,
	}))
}

// hardDeleteRoom 删除从未对外公布的房间（建房后 SFU session 创建失败时用，不写记录）。
func (s *Server) hardDeleteRoom(ctx context.Context, room *gcallRoom) {
	pipe := s.rdb.Pipeline()
	pipe.Del(ctx, gcallRoomKey(room.CallID), gcallMembersKey(room.CallID), gcallEverKey(room.CallID),
		gcallTracksKey(room.CallID), gcallSessionsKey(room.CallID))
	_, _ = pipe.Exec(ctx)
	s.checkDel(ctx, gcallConvKey(room.ConversationID), room.CallID)
	s.checkDel(ctx, gcallUserKey(room.Creator), room.CallID)
}

// gcallSweeper 巡检：成员为空的房间结束；只剩 1 人超过 120 秒的房间结束（SPEC §3.2）。
func (s *Server) gcallSweeper() {
	ctx := context.Background()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		s.sweepGroupRooms(ctx)
	}
}

func (s *Server) sweepGroupRooms(ctx context.Context) {
	iter := s.rdb.Scan(ctx, 0, gcallPrefix+gcallIDPrefix+"*", 100).Iterator()
	now := time.Now().UnixMilli()
	for iter.Next(ctx) {
		key := iter.Val()
		callID := strings.TrimPrefix(key, gcallPrefix)
		if strings.Contains(callID, ":") {
			continue // :members/:tracks 等子键
		}
		room, err := s.loadGroupRoom(ctx, callID)
		if err != nil || room == nil || room.State != "open" {
			continue
		}
		n, err := s.rdb.SCard(ctx, gcallMembersKey(callID)).Result()
		if err != nil {
			continue
		}
		if n == 0 || (n == 1 && room.AloneSince > 0 && now-room.AloneSince >= gcallAloneMax.Milliseconds()) {
			s.endGroupRoom(ctx, callID, false)
		}
	}
	if err := iter.Err(); err != nil {
		slog.Warn("gcall sweep scan", "err", err)
	}
}
