package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"imserver/internal/storage"
	"imserver/internal/store"
	"imserver/internal/ws"
)

const (
	maxGroupNameRunes    = 32
	maxAnnouncementRunes = 1000
)

// DefaultReactions 客户端快捷表情（iMessage 风格），也可以发任意 emoji。
var DefaultReactions = []string{"❤️", "👍", "👎", "😂", "‼️", "❓"}

func (s *Server) clientConfig(w http.ResponseWriter, r *http.Request, uid uint64) {
	out := map[string]any{
		"recall_seconds":    s.cfg.Message.RecallSeconds,
		"group_max_members": s.cfg.Group.MaxMembers,
		"reactions":         DefaultReactions,
		"voice_enabled":     s.storage != nil,
		// v0.3.0
		"image_enabled":    s.storage != nil,
		"image_max_bytes":  s.cfg.Storage.ImageMaxBytes,
		"presence_enabled": true,
		"stt_enabled":      s.stt != nil,
	}
	if s.storage != nil {
		c := s.storage.Config()
		out["voice_max_seconds"] = c.VoiceMaxSeconds
		out["voice_max_bytes"] = c.VoiceMaxBytes
		out["voice_ttl_days"] = c.VoiceTTLDays
		out["image_max_bytes"] = c.ImageMaxBytes
		out["image_ttl_days"] = c.ImageTTLDays
	}
	writeJSON(w, 200, out)
}

func (s *Server) getConversation(w http.ResponseWriter, r *http.Request, uid uint64) {
	cid, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad_request", "会话 ID 无效")
		return
	}
	c, err := s.store.GetConversation(r.Context(), uid, cid)
	if errors.Is(err, store.ErrNotMember) {
		writeErr(w, 403, "forbidden", "不是该会话成员")
		return
	}
	if err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	writeJSON(w, 200, c)
}

func pathUID(r *http.Request) (uint64, bool) {
	id, err := strconv.ParseUint(r.PathValue("uid"), 10, 64)
	return id, err == nil && id > 0
}

func randID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "sys-" + hex.EncodeToString(b)
}

func (s *Server) sysUsers(ctx context.Context, ids []uint64) []store.SysUser {
	users, _ := s.store.GetUsers(ctx, ids)
	out := make([]store.SysUser, 0, len(ids))
	for _, id := range ids {
		su := store.SysUser{ID: id}
		if u := users[id]; u != nil {
			su.Nickname = nickOf(u)
		}
		out = append(out, su)
	}
	return out
}

func nickOf(u *store.User) string {
	if u.Nickname != "" {
		return u.Nickname
	}
	return strings.Split(u.Email, "@")[0]
}

func joinNames(us []store.SysUser) string {
	names := make([]string, 0, len(us))
	for _, u := range us {
		names = append(names, u.Nickname)
	}
	if len(names) > 10 {
		return strings.Join(names[:10], "、") + fmt.Sprintf(" 等 %d 人", len(names))
	}
	return strings.Join(names, "、")
}

// postSystem 写入一条系统消息并推送给当前成员（以及 extra 中已离开的人），
// 同时推送 conversation_updated 让客户端刷新群信息。
func (s *Server) postSystem(ctx context.Context, cid, actor uint64, p store.SystemPayload, extra []uint64) {
	if p.Actor.ID == 0 {
		p.Actor = s.sysUsers(ctx, []uint64{actor})[0]
	}
	b, _ := json.Marshal(p)
	msg := &store.Message{ConversationID: cid, SenderID: actor, ClientMsgID: randID(), MsgType: store.MsgSystem, Content: string(b)}
	if _, err := s.store.InsertMessage(ctx, msg); err != nil {
		slog.Error("insert system message", "err", err)
		return
	}
	members, err := s.store.Members(ctx, cid)
	if err != nil {
		return
	}
	targets := append(append([]uint64{}, members...), extra...)
	s.hub.Publish(ctx, targets, "", ws.Encode("message", map[string]any{"message": msg}))
	s.hub.Publish(ctx, members, "", ws.Encode("conversation_updated", map[string]any{"conversation_id": cid, "reason": p.Op}))
}

// requireGroup 校验是群成员且会话是群聊。
func (s *Server) requireGroup(w http.ResponseWriter, r *http.Request, uid uint64) (uint64, *store.Member, bool) {
	cid, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad_request", "会话 ID 无效")
		return 0, nil, false
	}
	m, ok := s.memberInfo(w, r.Context(), cid, uid)
	if !ok {
		return 0, nil, false
	}
	if m.ConvType != store.ConvGroup {
		writeErr(w, 400, "not_group", "不是群聊")
		return 0, nil, false
	}
	return cid, m, true
}

// validUsers 去重、去掉自己、校验用户存在。
func (s *Server) validUsers(ctx context.Context, ids []uint64, self uint64) ([]uint64, error) {
	seen := map[uint64]bool{self: true}
	var out []uint64
	for _, id := range ids {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	users, err := s.store.GetUsers(ctx, out)
	if err != nil {
		return nil, err
	}
	for _, id := range out {
		if users[id] == nil {
			return nil, fmt.Errorf("用户 %d 不存在", id)
		}
	}
	return out, nil
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request, uid uint64) {
	var req struct {
		Name      string   `json:"name"`
		MemberIDs []uint64 `json:"member_ids"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "bad_request", "参数错误")
		return
	}
	ctx := r.Context()
	members, err := s.validUsers(ctx, req.MemberIDs, uid)
	if err != nil {
		writeErr(w, 400, "bad_member", err.Error())
		return
	}
	if len(members) < 1 {
		writeErr(w, 400, "bad_member", "至少选择 1 位成员")
		return
	}
	if len(members)+1 > s.cfg.Group.MaxMembers {
		writeErr(w, 400, "group_full", fmt.Sprintf("群成员不能超过 %d 人", s.cfg.Group.MaxMembers))
		return
	}
	name := strings.TrimSpace(req.Name)
	if utf8.RuneCountInString(name) > maxGroupNameRunes {
		writeErr(w, 400, "bad_name", fmt.Sprintf("群名不超过 %d 个字符", maxGroupNameRunes))
		return
	}
	targets := s.sysUsers(ctx, members)
	actor := s.sysUsers(ctx, []uint64{uid})[0]
	if name == "" {
		all := append([]store.SysUser{actor}, targets...)
		if len(all) > 3 {
			all = all[:3]
		}
		name = joinNames(all)
		if r := []rune(name); len(r) > maxGroupNameRunes {
			name = string(r[:maxGroupNameRunes-1]) + "…"
		}
	}
	cid, err := s.store.CreateGroup(ctx, uid, name, members)
	if err != nil {
		slog.Error("create group", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	s.postSystem(ctx, cid, uid, store.SystemPayload{
		Op: "create", Actor: actor, Targets: targets, Name: name,
		Text: fmt.Sprintf("%s 创建了群聊“%s”，并邀请了 %s", actor.Nickname, name, joinNames(targets)),
	}, nil)
	c, err := s.store.GetConversation(ctx, uid, cid)
	if err != nil {
		writeJSON(w, 201, map[string]any{"conversation_id": cid})
		return
	}
	writeJSON(w, 201, map[string]any{"conversation_id": cid, "conversation": c})
}

func (s *Server) updateGroup(w http.ResponseWriter, r *http.Request, uid uint64) {
	cid, mem, ok := s.requireGroup(w, r, uid)
	if !ok {
		return
	}
	var req struct {
		Name         *string `json:"name"`
		Announcement *string `json:"announcement"`
	}
	if err := decode(r, &req); err != nil || (req.Name == nil && req.Announcement == nil) {
		writeErr(w, 400, "bad_request", "参数错误")
		return
	}
	ctx := r.Context()
	if req.Announcement != nil && mem.Role < store.RoleAdmin {
		writeErr(w, 403, "forbidden", "只有群主或管理员可以修改群公告")
		return
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || utf8.RuneCountInString(name) > maxGroupNameRunes {
			writeErr(w, 400, "bad_name", fmt.Sprintf("群名为 1~%d 个字符", maxGroupNameRunes))
			return
		}
		if err := s.store.RenameGroup(ctx, cid, name); err != nil {
			writeErr(w, 500, "internal", "服务器错误")
			return
		}
		actor := s.sysUsers(ctx, []uint64{uid})[0]
		s.postSystem(ctx, cid, uid, store.SystemPayload{Op: "rename", Actor: actor, Name: name,
			Text: fmt.Sprintf("%s 将群名修改为“%s”", actor.Nickname, name)}, nil)
	}
	if req.Announcement != nil {
		text := strings.TrimSpace(*req.Announcement)
		if utf8.RuneCountInString(text) > maxAnnouncementRunes {
			writeErr(w, 400, "bad_announcement", fmt.Sprintf("群公告不超过 %d 个字符", maxAnnouncementRunes))
			return
		}
		if err := s.store.SetAnnouncement(ctx, cid, uid, text); err != nil {
			writeErr(w, 500, "internal", "服务器错误")
			return
		}
		actor := s.sysUsers(ctx, []uint64{uid})[0]
		t := fmt.Sprintf("%s 更新了群公告：%s", actor.Nickname, store.PreviewText(store.MsgText, text, 60))
		if text == "" {
			t = actor.Nickname + " 清空了群公告"
		}
		s.postSystem(ctx, cid, uid, store.SystemPayload{Op: "announce", Actor: actor, Value: text, Text: t}, nil)
	}
	c, err := s.store.GetConversation(ctx, uid, cid)
	if err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request, uid uint64) {
	cid, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad_request", "会话 ID 无效")
		return
	}
	if !s.member(w, r.Context(), cid, uid) {
		return
	}
	list, err := s.store.ListMembers(r.Context(), cid)
	if err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) addMembers(w http.ResponseWriter, r *http.Request, uid uint64) {
	cid, _, ok := s.requireGroup(w, r, uid)
	if !ok {
		return
	}
	var req struct {
		UserIDs []uint64 `json:"user_ids"`
	}
	if err := decode(r, &req); err != nil || len(req.UserIDs) == 0 || len(req.UserIDs) > 100 {
		writeErr(w, 400, "bad_request", "user_ids 为 1~100 个")
		return
	}
	ctx := r.Context()
	ids, err := s.validUsers(ctx, req.UserIDs, uid)
	if err != nil {
		writeErr(w, 400, "bad_member", err.Error())
		return
	}
	added, err := s.store.AddMembers(ctx, cid, ids, s.cfg.Group.MaxMembers)
	if errors.Is(err, store.ErrFull) {
		writeErr(w, 400, "group_full", fmt.Sprintf("群成员不能超过 %d 人", s.cfg.Group.MaxMembers))
		return
	}
	if err != nil {
		slog.Error("add members", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	if len(added) > 0 {
		actor := s.sysUsers(ctx, []uint64{uid})[0]
		targets := s.sysUsers(ctx, added)
		s.postSystem(ctx, cid, uid, store.SystemPayload{Op: "join", Actor: actor, Targets: targets,
			Text: fmt.Sprintf("%s 邀请 %s 加入了群聊", actor.Nickname, joinNames(targets))}, nil)
	}
	if added == nil {
		added = []uint64{}
	}
	writeJSON(w, 200, map[string]any{"added": added})
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request, uid uint64) {
	cid, mem, ok := s.requireGroup(w, r, uid)
	if !ok {
		return
	}
	target, ok := pathUID(r)
	if !ok {
		writeErr(w, 400, "bad_request", "用户 ID 无效")
		return
	}
	if target == uid {
		writeErr(w, 400, "bad_request", "退出群聊请用 /leave")
		return
	}
	ctx := r.Context()
	tm, err := s.store.GetMember(ctx, cid, target)
	if err != nil {
		writeErr(w, 404, "not_found", "该用户不在群里")
		return
	}
	// 群主可移除任何人；管理员只能移除普通成员
	if mem.Role < store.RoleAdmin || tm.Role >= mem.Role {
		writeErr(w, 403, "forbidden", "没有权限移除该成员")
		return
	}
	if _, err := s.store.RemoveMember(ctx, cid, target); err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	actor := s.sysUsers(ctx, []uint64{uid})[0]
	targets := s.sysUsers(ctx, []uint64{target})
	s.postSystem(ctx, cid, uid, store.SystemPayload{Op: "remove", Actor: actor, Targets: targets,
		Text: fmt.Sprintf("%s 将 %s 移出了群聊", actor.Nickname, joinNames(targets))}, []uint64{target})
	s.hub.Publish(ctx, []uint64{target}, "", ws.Encode("conversation_removed", map[string]any{"conversation_id": cid, "reason": "removed"}))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) setRole(w http.ResponseWriter, r *http.Request, uid uint64) {
	cid, mem, ok := s.requireGroup(w, r, uid)
	if !ok {
		return
	}
	target, ok := pathUID(r)
	if !ok || target == uid {
		writeErr(w, 400, "bad_request", "用户 ID 无效")
		return
	}
	if mem.Role != store.RoleOwner {
		writeErr(w, 403, "forbidden", "只有群主可以设置管理员")
		return
	}
	var req struct {
		Role string `json:"role"` // admin | member
	}
	if err := decode(r, &req); err != nil || (req.Role != "admin" && req.Role != "member") {
		writeErr(w, 400, "bad_request", "role 只能是 admin 或 member")
		return
	}
	role := store.RoleMember
	if req.Role == "admin" {
		role = store.RoleAdmin
	}
	ctx := r.Context()
	if err := s.store.SetRole(ctx, cid, target, role); err != nil {
		writeErr(w, 404, "not_found", "该用户不在群里")
		return
	}
	actor := s.sysUsers(ctx, []uint64{uid})[0]
	targets := s.sysUsers(ctx, []uint64{target})
	t := fmt.Sprintf("%s 将 %s 设为管理员", actor.Nickname, targets[0].Nickname)
	if role == store.RoleMember {
		t = fmt.Sprintf("%s 取消了 %s 的管理员", actor.Nickname, targets[0].Nickname)
	}
	s.postSystem(ctx, cid, uid, store.SystemPayload{Op: "role", Actor: actor, Targets: targets, Value: req.Role, Text: t}, nil)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) leaveGroup(w http.ResponseWriter, r *http.Request, uid uint64) {
	cid, _, ok := s.requireGroup(w, r, uid)
	if !ok {
		return
	}
	ctx := r.Context()
	actor := s.sysUsers(ctx, []uint64{uid})[0]
	newOwner, err := s.store.RemoveMember(ctx, cid, uid)
	if err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	text := actor.Nickname + " 退出了群聊"
	p := store.SystemPayload{Op: "leave", Actor: actor, Text: text}
	if newOwner > 0 {
		no := s.sysUsers(ctx, []uint64{newOwner})
		p.Targets = no
		p.Text = text + "，" + no[0].Nickname + " 成为新群主"
	}
	s.postSystem(ctx, cid, uid, p, []uint64{uid})
	s.hub.Publish(ctx, []uint64{uid}, "", ws.Encode("conversation_removed", map[string]any{"conversation_id": cid, "reason": "left"}))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) hiddenSeqs(w http.ResponseWriter, r *http.Request, uid uint64) {
	cid, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad_request", "会话 ID 无效")
		return
	}
	if !s.member(w, r.Context(), cid, uid) {
		return
	}
	seqs, err := s.store.HiddenSeqs(r.Context(), uid, cid, queryUint(r, "min_seq"))
	if err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	writeJSON(w, 200, seqs)
}

// ---------- 上传 / 媒体 ----------

func (s *Server) presignUpload(w http.ResponseWriter, r *http.Request, uid uint64) {
	if s.storage == nil {
		writeErr(w, 503, "storage_disabled", "服务器未启用文件存储")
		return
	}
	var req struct {
		Kind       string `json:"kind"` // voice | image
		Mime       string `json:"mime"`
		Size       int64  `json:"size"`
		DurationMs int    `json:"duration_ms"` // voice
		Width      int    `json:"width"`       // image（可选，仅校验非负）
		Height     int    `json:"height"`      // image
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "bad_request", "参数错误")
		return
	}
	cfg := s.storage.Config()
	switch req.Kind {
	case "voice":
		mime, ext, ok := storage.NormalizeVoiceMime(req.Mime)
		if !ok {
			writeErr(w, 400, "bad_mime", "不支持的音频格式")
			return
		}
		if req.Size <= 0 || req.Size > cfg.VoiceMaxBytes {
			writeErr(w, 400, "too_large", fmt.Sprintf("语音文件不能超过 %d KB", cfg.VoiceMaxBytes/1024))
			return
		}
		if req.DurationMs <= 0 || req.DurationMs > cfg.VoiceMaxSeconds*1000+999 {
			writeErr(w, 400, "bad_duration", fmt.Sprintf("语音时长不能超过 %d 秒", cfg.VoiceMaxSeconds))
			return
		}
		key := storage.NewKey("voice", uid, ext)
		u, exp, err := s.storage.PresignPut(r.Context(), key)
		if err != nil {
			slog.Error("presign put", "err", err)
			writeErr(w, 500, "internal", "生成上传链接失败")
			return
		}
		writeJSON(w, 200, map[string]any{
			"key": key, "url": u, "method": "PUT", "headers": map[string]string{"Content-Type": mime},
			"expires_at": exp.UTC().Format(time.RFC3339), "max_bytes": cfg.VoiceMaxBytes,
		})
	case "image":
		mime, ext, ok := storage.NormalizeImageMime(req.Mime)
		if !ok {
			writeErr(w, 400, "bad_mime", "只支持 JPEG / PNG / WebP / GIF 图片")
			return
		}
		if req.Size <= 0 || req.Size > cfg.ImageMaxBytes {
			writeErr(w, 400, "too_large", fmt.Sprintf("图片不能超过 %d MB", cfg.ImageMaxBytes>>20))
			return
		}
		if req.Width < 0 || req.Height < 0 {
			writeErr(w, 400, "bad_request", "图片宽高无效")
			return
		}
		key := storage.NewKey("image", uid, ext)
		u, exp, err := s.storage.PresignPut(r.Context(), key)
		if err != nil {
			slog.Error("presign put", "err", err)
			writeErr(w, 500, "internal", "生成上传链接失败")
			return
		}
		writeJSON(w, 200, map[string]any{
			"key": key, "url": u, "method": "PUT", "headers": map[string]string{"Content-Type": mime},
			"expires_at": exp.UTC().Format(time.RFC3339), "max_bytes": cfg.ImageMaxBytes,
		})
	default:
		writeErr(w, 400, "bad_kind", "kind 只支持 voice 或 image")
	}
}

func (s *Server) mediaURL(w http.ResponseWriter, r *http.Request, uid uint64) {
	cid, ok := pathID(r)
	seq, err := strconv.ParseUint(r.PathValue("seq"), 10, 64)
	if !ok || err != nil || seq == 0 {
		writeErr(w, 400, "bad_request", "参数错误")
		return
	}
	ctx := r.Context()
	mem, ok := s.memberInfo(w, ctx, cid, uid)
	if !ok {
		return
	}
	if seq <= mem.JoinSeq {
		writeErr(w, 404, "not_found", "消息不存在")
		return
	}
	m, err := s.store.GetMessage(ctx, cid, seq)
	if err != nil {
		writeErr(w, 404, "not_found", "消息不存在")
		return
	}
	if m.Recalled {
		writeErr(w, 410, "recalled", "消息已撤回")
		return
	}
	if m.MsgType != store.MsgVoice && m.MsgType != store.MsgImage {
		writeErr(w, 400, "not_media", "不是媒体消息")
		return
	}
	if s.storage == nil {
		writeErr(w, 503, "storage_disabled", "服务器未启用文件存储")
		return
	}
	key, mime, size := ws.MediaKey(m.Content)
	if key == "" {
		writeErr(w, 404, "not_found", "文件不存在")
		return
	}
	// 不访问对象存储（跨地域部署时省一次往返）：按消息时间判断是否已被生命周期删除。
	// 生命周期按天执行，边界附近对象可能已删也可能还在，客户端下载 404 时同样显示“已过期”。
	cfg := s.storage.Config()
	if m.MsgType == store.MsgVoice {
		if time.Since(m.CreatedAt) > time.Duration(cfg.VoiceTTLDays)*24*time.Hour {
			writeErr(w, 410, "expired", "语音已过期")
			return
		}
	} else if cfg.ImageTTLDays > 0 && time.Since(m.CreatedAt) > time.Duration(cfg.ImageTTLDays)*24*time.Hour {
		writeErr(w, 410, "expired", "图片已过期")
		return
	}
	u, exp, err := s.storage.PresignGet(ctx, key, mime)
	if err != nil {
		writeErr(w, 500, "internal", "生成下载链接失败")
		return
	}
	writeJSON(w, 200, map[string]any{"url": u, "mime": mime, "size": size, "expires_at": exp.UTC().Format(time.RFC3339)})
}
