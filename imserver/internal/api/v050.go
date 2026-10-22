package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"imserver/internal/store"
	"imserver/internal/ws"
)

// ---------- 本人视角响应（v0.5.0，SPEC §1.2/§2.1） ----------

// meResponse 是 /api/me 的响应：完整用户对象 + allow_find。
// store.User 的 AllowFind 本身 json:"-"，只经这个本人视角结构下发。
type meResponse struct {
	*store.User
	AllowFind bool `json:"allow_find"`
}

func meResponseOf(u *store.User) meResponse { return meResponse{User: u, AllowFind: u.AllowFind} }

// ---------- 用户搜索（v0.5.0，SPEC §1.3） ----------

// searchRateOK 搜索限流：每用户 30 次/分钟（Redis 计数；Redis 异常时放行）。
func (s *Server) searchRateOK(ctx context.Context, uid uint64) bool {
	key := "im:search:" + strconv.FormatUint(uid, 10)
	n, err := s.rdb.Incr(ctx, key).Result()
	if err != nil {
		return true
	}
	if n == 1 {
		s.rdb.Expire(ctx, key, time.Minute)
	}
	return n <= 30
}

type searchResult struct {
	*store.PublicUser
	Relation string `json:"relation"`
}

// searchUser GET /api/users/search?email= 或 ?id= → PublicUser + relation。
// 目标 allow_find=0 时与「不存在」同样回 404（防枚举）；搜自己 400。
func (s *Server) searchUser(w http.ResponseWriter, r *http.Request, uid uint64) {
	ctx := r.Context()
	if !s.searchRateOK(ctx, uid) {
		writeErr(w, 429, "rate_limited", "搜索过于频繁，请稍后再试")
		return
	}
	q := r.URL.Query()
	var target *store.User
	switch {
	case q.Get("id") != "":
		id, err := strconv.ParseUint(q.Get("id"), 10, 64)
		if err != nil || id == 0 {
			writeErr(w, 400, "bad_request", "用户 ID 无效")
			return
		}
		if id == uid {
			writeErr(w, 400, "bad_request", "不能搜索自己")
			return
		}
		u, err := s.store.GetUser(ctx, id)
		if err != nil {
			writeErr(w, 404, "not_found", "用户不存在")
			return
		}
		target = u
	case q.Has("email"):
		email, ok := normalizeEmail(q.Get("email"))
		if !ok {
			writeErr(w, 400, "bad_email", "邮箱格式不正确")
			return
		}
		u, err := s.store.GetUserByEmail(ctx, email)
		if err != nil {
			writeErr(w, 404, "not_found", "用户不存在")
			return
		}
		if u.ID == uid {
			writeErr(w, 400, "bad_request", "不能搜索自己")
			return
		}
		target = u
	default:
		writeErr(w, 400, "bad_request", "需要提供 email 或 id")
		return
	}
	if !target.AllowFind {
		writeErr(w, 404, "not_found", "用户不存在")
		return
	}
	rel, err := s.store.FriendRelation(ctx, uid, target.ID)
	if err != nil {
		slog.Error("friend relation", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	writeJSON(w, 200, searchResult{PublicUser: target.Public(), Relation: rel})
}

// ---------- 退出其他设备（v0.5.0，SPEC §2.1） ----------

// revokeOtherSessions POST /api/me/revoke-other-sessions → {token, expires_at}。
// token_version +1 后给当前设备换发新 token；其余连接按既有流程收 session_revoked 下线。
func (s *Server) revokeOtherSessions(w http.ResponseWriter, r *http.Request, uid uint64) {
	ctx := r.Context()
	newTV, err := s.store.BumpTokenVersion(ctx, uid)
	if err != nil {
		slog.Error("bump token_version", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	s.setTVCache(ctx, uid, newTV)
	tok, exp, err := s.auth.Issue(uid, newTV)
	if err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	// 与改密相同：延迟 2 秒再断连接，先让本响应把新 token 送达当前设备，
	// 当前设备收到 session_revoked 时用新 token 校验即可无感重连，不会被误踢。
	go func() {
		time.Sleep(2 * time.Second)
		s.revokeUserSessions(context.Background(), uid, "sessions_revoked")
	}()
	writeJSON(w, 200, map[string]any{"token": tok, "expires_at": exp})
}

// ---------- 好友系统（v0.5.0，SPEC §1.3） ----------

func (s *Server) friendEvent(ctx context.Context, target uint64, typ string, data map[string]any) {
	s.hub.Publish(ctx, []uint64{target}, "", ws.Encode(typ, data))
}

// createFriendRequest POST /api/friends/requests {user_id, message?}。
func (s *Server) createFriendRequest(w http.ResponseWriter, r *http.Request, uid uint64) {
	var req struct {
		UserID  uint64 `json:"user_id"`
		Message string `json:"message"`
	}
	if err := decode(r, &req); err != nil || req.UserID == 0 {
		writeErr(w, 400, "bad_request", "user_id 必填")
		return
	}
	if req.UserID == uid {
		writeErr(w, 400, "self", "不能给自己发好友申请")
		return
	}
	msg := strings.TrimSpace(req.Message)
	if utf8.RuneCountInString(msg) > 128 {
		writeErr(w, 400, "bad_request", "申请附言不超过 128 个字符")
		return
	}
	ctx := r.Context()
	target, err := s.store.GetUser(ctx, req.UserID)
	if err != nil {
		writeErr(w, 404, "not_found", "用户不存在")
		return
	}
	id, autoAccepted, err := s.store.SendFriendRequest(ctx, uid, target.ID, msg)
	if errors.Is(err, store.ErrAlreadyFriends) {
		writeErr(w, 409, "already_friends", "对方已经是你的好友")
		return
	}
	if errors.Is(err, store.ErrRequestPending) {
		writeErr(w, 409, "request_pending", "已有一条待处理的申请")
		return
	}
	if err != nil {
		slog.Error("send friend request", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	me, err := s.store.GetUser(ctx, uid)
	if err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	if autoAccepted {
		// 反向已有申请 → 直接成友，通知对方（含触发其申请的那一端刷新列表）
		s.friendEvent(ctx, target.ID, "friend_request_accepted", map[string]any{"by": me.Public()})
		writeJSON(w, 200, map[string]any{"status": "accepted"})
		return
	}
	view := store.FriendRequestView{ID: id, User: me.Public(), Message: msg, CreatedAt: time.Now().UTC()}
	if fr, err := s.store.GetFriendRequest(ctx, id); err == nil {
		view.CreatedAt = fr.CreatedAt
	}
	s.friendEvent(ctx, target.ID, "friend_request", map[string]any{"request": view})
	writeJSON(w, 201, map[string]any{"id": id, "status": "pending"})
}

// listFriendRequests GET /api/friends/requests → {incoming, outgoing}（只含待处理）。
func (s *Server) listFriendRequests(w http.ResponseWriter, r *http.Request, uid uint64) {
	incoming, outgoing, err := s.store.PendingFriendRequests(r.Context(), uid)
	if err != nil {
		slog.Error("list friend requests", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	writeJSON(w, 200, map[string]any{"incoming": incoming, "outgoing": outgoing})
}

func (s *Server) handleFriendRequest(accept bool) func(http.ResponseWriter, *http.Request, uint64) {
	return func(w http.ResponseWriter, r *http.Request, uid uint64) {
		id, ok := pathID(r)
		if !ok {
			writeErr(w, 400, "bad_request", "申请 ID 无效")
			return
		}
		ctx := r.Context()
		fr, err := s.store.HandleFriendRequest(ctx, id, uid, accept)
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, 404, "not_found", "申请不存在")
			return
		}
		if errors.Is(err, store.ErrRequestHandled) {
			writeErr(w, 400, "request_handled", "申请已处理")
			return
		}
		if err != nil {
			slog.Error("handle friend request", "err", err)
			writeErr(w, 500, "internal", "服务器错误")
			return
		}
		me, err := s.store.GetUser(ctx, uid)
		if err != nil {
			writeErr(w, 500, "internal", "服务器错误")
			return
		}
		typ := "friend_request_rejected"
		if accept {
			typ = "friend_request_accepted"
		}
		s.friendEvent(ctx, fr.FromID, typ, map[string]any{"by": me.Public()})
		writeJSON(w, 200, map[string]any{"ok": true})
	}
}

func (s *Server) acceptFriendRequest(w http.ResponseWriter, r *http.Request, uid uint64) {
	s.handleFriendRequest(true)(w, r, uid)
}

func (s *Server) rejectFriendRequest(w http.ResponseWriter, r *http.Request, uid uint64) {
	s.handleFriendRequest(false)(w, r, uid)
}

// withdrawFriendRequest DELETE /api/friends/requests/{id}（仅申请方，置状态 3）。
func (s *Server) withdrawFriendRequest(w http.ResponseWriter, r *http.Request, uid uint64) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad_request", "申请 ID 无效")
		return
	}
	err := s.store.WithdrawFriendRequest(r.Context(), id, uid)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, 404, "not_found", "申请不存在")
		return
	}
	if errors.Is(err, store.ErrRequestHandled) {
		writeErr(w, 400, "request_handled", "申请已处理")
		return
	}
	if err != nil {
		slog.Error("withdraw friend request", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

type friendOut struct {
	User      *store.PublicUser `json:"user"`
	Remark    string            `json:"remark"`
	Online    bool              `json:"online"`
	CreatedAt time.Time         `json:"created_at"`
}

// listFriends GET /api/friends → [{user, remark, online, created_at}]（created_at 倒序）。
func (s *Server) listFriends(w http.ResponseWriter, r *http.Request, uid uint64) {
	ctx := r.Context()
	friends, err := s.store.ListFriends(ctx, uid)
	if err != nil {
		slog.Error("list friends", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	ids := make([]uint64, 0, len(friends))
	for _, f := range friends {
		ids = append(ids, f.User.ID)
	}
	online, err := s.hub.OnlineMany(ctx, ids)
	if err != nil {
		slog.Warn("friends online", "err", err)
		online = map[uint64]bool{}
	}
	out := make([]friendOut, 0, len(friends))
	for _, f := range friends {
		out = append(out, friendOut{User: f.User, Remark: f.Remark, Online: online[f.User.ID], CreatedAt: f.CreatedAt})
	}
	writeJSON(w, 200, out)
}

// updateFriend PATCH /api/friends/{user_id} {remark} → 改自己这条边的备注。
func (s *Server) updateFriend(w http.ResponseWriter, r *http.Request, uid uint64) {
	friendID, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad_request", "用户 ID 无效")
		return
	}
	var req struct {
		Remark *string `json:"remark"`
	}
	if err := decode(r, &req); err != nil || req.Remark == nil {
		writeErr(w, 400, "bad_request", "remark 必填")
		return
	}
	remark := *req.Remark
	if utf8.RuneCountInString(remark) > 64 {
		writeErr(w, 400, "bad_remark", "备注不超过 64 个字符")
		return
	}
	ctx := r.Context()
	if err := s.store.SetFriendRemark(ctx, uid, friendID, remark); err != nil {
		if errors.Is(err, store.ErrNotFriends) {
			writeErr(w, 404, "not_friends", "对方不是你的好友")
			return
		}
		slog.Error("set friend remark", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	fv, err := s.store.GetFriend(ctx, uid, friendID)
	if err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	online, _ := s.hub.OnlineMany(ctx, []uint64{friendID})
	writeJSON(w, 200, friendOut{User: fv.User, Remark: fv.Remark, Online: online[friendID], CreatedAt: fv.CreatedAt})
}

// deleteFriend DELETE /api/friends/{user_id} → 删双边；不删会话与消息记录。
func (s *Server) deleteFriend(w http.ResponseWriter, r *http.Request, uid uint64) {
	friendID, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad_request", "用户 ID 无效")
		return
	}
	ctx := r.Context()
	if err := s.store.RemoveFriend(ctx, uid, friendID); err != nil {
		if errors.Is(err, store.ErrNotFriends) {
			writeErr(w, 404, "not_friends", "对方不是你的好友")
			return
		}
		slog.Error("remove friend", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	s.friendEvent(ctx, friendID, "friend_removed", map[string]any{"by": uid})
	writeJSON(w, 200, map[string]any{"ok": true})
}
