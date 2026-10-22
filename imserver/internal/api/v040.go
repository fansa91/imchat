package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"imserver/internal/auth"
	"imserver/internal/storage"
	"imserver/internal/store"
	"imserver/internal/ws"
)

// ---------- 登录态版本 token_version（v0.4.0，SPEC §4.1） ----------

func tvKey(uid uint64) string { return "im:tv:" + strconv.FormatUint(uid, 10) }

// currentTV 读用户当前 token_version：先 Redis 缓存，未命中查 DB 回填（TTL 24h）。
// Redis/DB 出错时返回 -1（调用方据此放行还是拒绝：tvValid 出错时拒绝，DB 是权威源）。
func (s *Server) currentTV(ctx context.Context, uid uint64) (int, error) {
	if v, err := s.rdb.Get(ctx, tvKey(uid)).Int(); err == nil {
		return v, nil
	}
	tv, err := s.store.GetTokenVersion(ctx, uid)
	if err != nil {
		return -1, err
	}
	s.rdb.Set(ctx, tvKey(uid), tv, 24*time.Hour)
	return tv, nil
}

// tvValid token 中的 tv 是否与用户当前版本一致。
func (s *Server) tvValid(ctx context.Context, uid uint64, tv int) bool {
	cur, err := s.currentTV(ctx, uid)
	if err != nil {
		slog.Warn("token_version check failed", "user", uid, "err", err)
		return false
	}
	return cur == tv
}

// setTVCache 改密/重置后覆写缓存为新版本（多实例一致）。
func (s *Server) setTVCache(ctx context.Context, uid uint64, tv int) {
	if err := s.rdb.Set(ctx, tvKey(uid), tv, 24*time.Hour).Err(); err != nil {
		slog.Warn("set token_version cache", "user", uid, "err", err)
	}
}

// revokeUserSessions 版本变更后的吊销动作：先推 session_revoked 说明原因，再断开该用户全部连接（跨节点）。
func (s *Server) revokeUserSessions(ctx context.Context, uid uint64, reason string) {
	s.hub.DisconnectUser(ctx, uid, ws.Encode("session_revoked", map[string]any{"reason": reason}))
}

// ---------- 通话 ICE 配置（v0.4.0，SPEC §1.2） ----------

func (s *Server) iceServers(w http.ResponseWriter, r *http.Request, uid uint64) {
	cfg := s.cfg.Calls
	if !cfg.Enabled || !cfg.Configured() {
		writeErr(w, 501, "call_disabled", "服务器未启用通话")
		return
	}
	ctx := r.Context()
	cacheKey := "im:turn:" + strconv.FormatUint(uid, 10)
	if cached, err := s.rdb.Get(ctx, cacheKey).Bytes(); err == nil && len(cached) > 0 {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(200)
		_, _ = w.Write(cached)
		return
	}
	body, _ := json.Marshal(map[string]int{"ttl": cfg.CredentialTTLSeconds})
	url := cfg.BaseURL + "/turn/keys/" + cfg.TurnKeyID + "/credentials/generate-ice-servers"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		writeErr(w, 502, "turn_failed", "获取 TURN 凭证失败")
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfg.TurnAPIToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		slog.Warn("turn generate-ice-servers", "err", err)
		writeErr(w, 502, "turn_failed", "获取 TURN 凭证失败")
		return
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || resp.StatusCode != 201 {
		slog.Warn("turn generate-ice-servers bad status", "status", resp.StatusCode)
		writeErr(w, 502, "turn_failed", "获取 TURN 凭证失败")
		return
	}
	var cf struct {
		IceServers json.RawMessage `json:"iceServers"`
	}
	if err := json.Unmarshal(raw, &cf); err != nil || len(cf.IceServers) == 0 {
		writeErr(w, 502, "turn_failed", "获取 TURN 凭证失败")
		return
	}
	expires := time.Now().Add(time.Duration(cfg.CredentialTTLSeconds) * time.Second)
	out, _ := json.Marshal(map[string]any{
		"iceServers": cf.IceServers,
		"expires_at": expires.UTC().Format(time.RFC3339),
	})
	if ttl := time.Duration(cfg.CredentialTTLSeconds-300) * time.Second; ttl > 0 {
		if err := s.rdb.Set(ctx, cacheKey, out, ttl).Err(); err != nil {
			slog.Warn("cache turn credentials", "user", uid, "err", err)
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(200)
	_, _ = w.Write(out)
}

// ---------- 头像 / 昵称（v0.4.0，SPEC §3） ----------

// updateMe PATCH /api/me {nickname?, avatar_key?} → 更新后的完整用户对象。
func (s *Server) updateMe(w http.ResponseWriter, r *http.Request, uid uint64) {
	var req struct {
		Nickname  *string `json:"nickname"`
		AvatarKey *string `json:"avatar_key"`
	}
	if err := decode(r, &req); err != nil || (req.Nickname == nil && req.AvatarKey == nil) {
		writeErr(w, 400, "bad_request", "至少提供 nickname 或 avatar_key")
		return
	}
	ctx := r.Context()
	var nickname *string
	if req.Nickname != nil {
		n := strings.TrimSpace(*req.Nickname)
		if c := utf8.RuneCountInString(n); c < 1 || c > 64 {
			writeErr(w, 400, "bad_nickname", "昵称为 1~64 个字符")
			return
		}
		nickname = &n
	}
	var avatarKey *string
	if req.AvatarKey != nil {
		k := strings.TrimSpace(*req.AvatarKey)
		if k != "" {
			kind, owner, ok := storage.ParseKey(k)
			if !ok || kind != "avatar" || owner != uid {
				writeErr(w, 400, "bad_avatar", "头像文件无效")
				return
			}
			if s.storage == nil {
				writeErr(w, 503, "storage_disabled", "服务器未启用文件存储")
				return
			}
			if s.storage.Config().VerifyUpload {
				size, err := s.storage.Size(ctx, k)
				if errors.Is(err, storage.ErrNotFound) {
					writeErr(w, 400, "bad_avatar", "头像文件未上传")
					return
				}
				if err != nil {
					slog.Error("stat avatar", "err", err)
					writeErr(w, 500, "internal", "服务器错误")
					return
				}
				if size <= 0 || size > s.storage.Config().AvatarMaxBytes {
					writeErr(w, 400, "bad_avatar", "头像文件大小无效")
					return
				}
			}
		}
		avatarKey = &k
	}
	old, err := s.store.GetUser(ctx, uid)
	if err != nil {
		writeErr(w, 404, "not_found", "用户不存在")
		return
	}
	u, err := s.store.UpdateUserProfile(ctx, uid, nickname, avatarKey)
	if err != nil {
		slog.Error("update profile", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	// 头像换新后尽力删除旧对象（失败只记日志）
	if avatarKey != nil && old.AvatarKey != "" && old.AvatarKey != *avatarKey && s.storage != nil {
		oldKey := old.AvatarKey
		go func() {
			rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := s.storage.Remove(rctx, oldKey); err != nil {
				slog.Warn("remove old avatar", "key", oldKey, "err", err)
			}
		}()
	}
	// 通知共享会话成员 + 本人所有设备刷新昵称/头像（范围同 presence：CoMembers + 本人）
	targets, err := s.store.CoMembers(ctx, uid)
	if err == nil {
		s.hub.Publish(ctx, append(targets, uid), "", ws.Encode("user_updated", map[string]any{"user": u}))
	} else {
		slog.Warn("user_updated co-members", "user", uid, "err", err)
	}
	writeJSON(w, 200, u)
}

// userAvatar GET /api/users/{id}/avatar → 302 跳转到短期预签名 GET。
func (s *Server) userAvatar(w http.ResponseWriter, r *http.Request, uid uint64) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad_request", "用户 ID 无效")
		return
	}
	u, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		writeErr(w, 404, "not_found", "用户不存在")
		return
	}
	if u.AvatarKey == "" {
		writeErr(w, 404, "no_avatar", "该用户没有头像")
		return
	}
	if s.storage == nil {
		writeErr(w, 503, "storage_disabled", "服务器未启用文件存储")
		return
	}
	url, _, err := s.storage.PresignGet(r.Context(), u.AvatarKey, "")
	if err != nil {
		slog.Error("presign avatar", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
}

// ---------- 重置密码 / 修改密码（v0.4.0，SPEC §4.3） ----------

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// passwordForgot POST /api/password/forgot：无论账号是否存在都回 200（防枚举），存在才发信。
func (s *Server) passwordForgot(w http.ResponseWriter, r *http.Request) {
	if s.mailer == nil {
		writeErr(w, 501, "reset_disabled", "服务器未启用重置密码")
		return
	}
	var req struct {
		Email string `json:"email"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "bad_request", "参数错误")
		return
	}
	email, ok := normalizeEmail(req.Email)
	if !ok {
		writeErr(w, 400, "bad_email", "邮箱格式不正确")
		return
	}
	ctx := r.Context()
	ip := clientIP(r)
	// 同 IP 每小时最多 20 次
	ipKey := "im:pwreset:ip:" + ip
	if n, err := s.rdb.Incr(ctx, ipKey).Result(); err == nil {
		if n == 1 {
			s.rdb.Expire(ctx, ipKey, time.Hour)
		}
		if n > 20 {
			writeErr(w, 429, "rate_limited", "请求过于频繁，请稍后再试")
			return
		}
	}
	u, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		// 账号不存在：恒定 200，不发信（防账号枚举）
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	// 同邮箱每小时限流：超限静默不发信、仍回 200
	cfg := s.mailer.Config()
	if cfg.RateLimitPerHour > 0 {
		eKey := "im:pwreset:email:" + strconv.FormatUint(u.ID, 10)
		if n, err := s.rdb.Incr(ctx, eKey).Result(); err == nil {
			if n == 1 {
				s.rdb.Expire(ctx, eKey, time.Hour)
			}
			if n > int64(cfg.RateLimitPerHour) {
				writeJSON(w, 200, map[string]any{"ok": true})
				return
			}
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	expires := time.Now().Add(time.Duration(cfg.TokenTTLMinutes) * time.Minute)
	if err := s.store.CreateResetToken(ctx, u.ID, hex.EncodeToString(sum[:]), expires, ip); err != nil {
		slog.Error("create reset token", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	link := strings.TrimRight(cfg.ResetURLBase, "/") + "/reset-password?token=" + token
	mailer := s.mailer
	go func() {
		if err := mailer.SendResetPassword(email, link); err != nil {
			slog.Warn("send reset mail", "user", u.ID, "err", err)
		}
	}()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// passwordReset POST /api/password/reset {token, new_password}：成功后旧登录态全部失效。
func (s *Server) passwordReset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token       string `json:"token"`
		NewPassword string `json:"new_password"`
	}
	if err := decode(r, &req); err != nil || strings.TrimSpace(req.Token) == "" {
		writeErr(w, 400, "bad_token", "链接无效或已过期")
		return
	}
	if l := utf8.RuneCountInString(req.NewPassword); l < 8 || l > 72 {
		writeErr(w, 400, "bad_password", "密码长度需为 8 到 72 位")
		return
	}
	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(req.Token)))
	uid, newTV, err := s.store.ResetPasswordWithToken(r.Context(), hex.EncodeToString(sum[:]), hash)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, 400, "bad_token", "链接无效或已过期")
		return
	}
	if err != nil {
		slog.Error("reset password", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	ctx := r.Context()
	s.setTVCache(ctx, uid, newTV)
	s.revokeUserSessions(ctx, uid, "password_reset")
	writeJSON(w, 200, map[string]any{"ok": true})
}

// changePassword POST /api/me/password {old_password, new_password} → 新 token（当前设备无感续用）。
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, uid uint64) {
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "bad_request", "参数错误")
		return
	}
	if l := utf8.RuneCountInString(req.NewPassword); l < 8 || l > 72 {
		writeErr(w, 400, "bad_password", "密码长度需为 8 到 72 位")
		return
	}
	ctx := r.Context()
	u, err := s.store.GetUser(ctx, uid)
	if err != nil {
		writeErr(w, 404, "not_found", "用户不存在")
		return
	}
	if !auth.CheckPassword(u.PasswordHash, req.OldPassword) {
		writeErr(w, 403, "wrong_password", "旧密码不正确")
		return
	}
	if req.NewPassword == req.OldPassword {
		writeErr(w, 400, "same_password", "新密码不能与旧密码相同")
		return
	}
	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	newTV, err := s.store.ChangePassword(ctx, uid, hash)
	if err != nil {
		slog.Error("change password", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	s.setTVCache(ctx, uid, newTV)
	// 断开该用户全部 WS 连接（含当前设备的连接：服务端无法识别 HTTP 请求对应的 WS 连接，
	// 客户端用响应里的新 token 重连即可）；其他 HTTP 会话靠 tv 比对立即失效。
	// 延迟 2 秒再断：先让本响应把新 token 送达改密设备，客户端收到 session_revoked 时
	// 用已保存的新 token 校验即可无感重连，不会被误踢下线。
	go func() {
		time.Sleep(2 * time.Second)
		s.revokeUserSessions(context.Background(), uid, "password_changed")
	}()
	u2, err := s.store.GetUser(ctx, uid)
	if err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	tok, exp, err := s.auth.Issue(uid, newTV)
	if err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	writeJSON(w, 200, map[string]any{"token": tok, "expires_at": exp, "user": u2})
}
