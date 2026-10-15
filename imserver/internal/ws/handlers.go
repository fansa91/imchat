package ws

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"imserver/internal/storage"
	"imserver/internal/store"
)

func (c *Client) fail(id, code, msg string) { c.reply("error", id, errorData{code, msg}) }

// member 校验当前用户是会话成员，失败时已回复 error。
func (c *Client) member(ctx context.Context, id string, cid uint64) (*store.Member, bool) {
	if cid == 0 {
		c.fail(id, "bad_request", "conversation_id 必填")
		return nil, false
	}
	m, err := c.store.GetMember(ctx, cid, c.userID)
	if errors.Is(err, store.ErrNotMember) {
		c.fail(id, "forbidden", "不是该会话成员")
		return nil, false
	}
	if err != nil {
		c.fail(id, "internal", "服务器错误")
		return nil, false
	}
	return m, true
}

func (c *Client) handleSend(ctx context.Context, env Envelope) {
	var req sendReq
	if err := json.Unmarshal(env.Data, &req); err != nil {
		c.fail(env.ID, "bad_request", "参数错误")
		return
	}
	req.ClientMsgID = strings.TrimSpace(req.ClientMsgID)
	if req.ClientMsgID == "" || len(req.ClientMsgID) > 64 {
		c.fail(env.ID, "bad_request", "client_msg_id 必填且不超过 64 字符")
		return
	}
	if req.MsgType == 0 {
		req.MsgType = store.MsgText
	}
	if req.MsgType < 1 || req.MsgType > 5 {
		c.fail(env.ID, "bad_request", "msg_type 无效")
		return
	}
	if strings.TrimSpace(req.Content) == "" || utf8.RuneCountInString(req.Content) > maxTextRunes {
		c.fail(env.ID, "bad_request", "内容为空或过长")
		return
	}
	mem, ok := c.member(ctx, env.ID, req.ConversationID)
	if !ok {
		return
	}
	// 幂等：同一 client_msg_id 已落库则直接回执，不再重复校验（例如语音文件之后被撤回删除）
	if existing, err := c.store.MessageByClientID(ctx, c.userID, req.ClientMsgID); err == nil {
		c.reply("send_ack", env.ID, map[string]any{"message": existing, "duplicate": true})
		return
	}
	if req.ReplyToSeq > 0 {
		orig, err := c.store.GetMessage(ctx, req.ConversationID, req.ReplyToSeq)
		if err != nil || req.ReplyToSeq <= mem.JoinSeq || orig.MsgType == store.MsgSystem {
			c.fail(env.ID, "bad_request", "引用的消息不存在")
			return
		}
	}
	if req.MsgType == store.MsgVoice {
		content, code, msg := c.validateVoice(ctx, req.Content)
		if code != "" {
			c.fail(env.ID, code, msg)
			return
		}
		req.Content = content
	}
	if req.MsgType == store.MsgImage {
		content, code, msg := c.validateImage(ctx, req.Content)
		if code != "" {
			c.fail(env.ID, code, msg)
			return
		}
		req.Content = content
	}
	members, err := c.store.Members(ctx, req.ConversationID)
	if err != nil {
		c.fail(env.ID, "internal", "服务器错误")
		return
	}
	msg := &store.Message{
		ConversationID: req.ConversationID, SenderID: c.userID,
		ClientMsgID: req.ClientMsgID, MsgType: req.MsgType, Content: req.Content, ReplyToSeq: req.ReplyToSeq,
	}
	dup, err := c.store.InsertMessage(ctx, msg)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.fail(env.ID, "not_found", "会话不存在")
			return
		}
		slog.Error("insert message", "err", err)
		c.fail(env.ID, "internal", "服务器错误")
		return
	}
	if !dup {
		_ = c.store.Enrich(ctx, msg)
	}
	c.reply("send_ack", env.ID, map[string]any{"message": msg, "duplicate": dup})
	if dup {
		return // 重发的消息之前已经投递过
	}
	c.hub.Publish(ctx, members, c.connID, encode("message", "", map[string]any{"message": msg}))

	// 离线成员走推送
	body := store.PreviewText(msg.MsgType, msg.Content, 60)
	for _, uid := range members {
		if uid == c.userID || c.hub.IsOnline(ctx, uid) {
			continue
		}
		go func(uid uint64) {
			pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := c.push.Notify(pctx, uid, "新消息", body, map[string]string{
				"conversation_id": jsonNum(msg.ConversationID),
			}); err != nil {
				slog.Warn("push failed", "user", uid, "err", err)
			}
		}(uid)
	}
}

// validateVoice 校验语音消息：key 属于自己、对象已上传且大小/格式合格、时长合法。
// 返回规范化后的 content。
func (c *Client) validateVoice(ctx context.Context, raw string) (content, code, msg string) {
	st := c.deps.Storage
	if st == nil {
		return "", "unsupported", "服务器未启用语音消息"
	}
	var v VoiceContent
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return "", "bad_request", "语音消息格式错误"
	}
	kind, owner, ok := storage.ParseKey(v.Key)
	if !ok || kind != "voice" || owner != c.userID {
		return "", "bad_request", "无效的语音文件"
	}
	maxMs := st.Config().VoiceMaxSeconds*1000 + 999
	if v.DurationMs <= 0 || v.DurationMs > maxMs {
		return "", "bad_request", "语音时长无效"
	}
	if _, _, ok := storage.NormalizeVoiceMime(v.Mime); !ok {
		return "", "bad_request", "不支持的音频格式"
	}
	if len(v.Waveform) > 64 {
		v.Waveform = v.Waveform[:64]
	}
	for i, x := range v.Waveform {
		v.Waveform[i] = min(max(x, 0), 100)
	}
	if !st.Config().VerifyUpload {
		// 不访问对象存储：只信任客户端声明（key 归属、类型、时长已校验）
		mime, _, _ := storage.NormalizeVoiceMime(v.Mime)
		if v.Size <= 0 || v.Size > st.Config().VoiceMaxBytes {
			return "", "bad_request", "语音文件大小无效"
		}
		out, _ := json.Marshal(VoiceContent{Key: v.Key, Mime: mime, DurationMs: v.DurationMs, Size: v.Size, Waveform: v.Waveform})
		return string(out), "", ""
	}
	vctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	info, err := st.VerifyVoice(vctx, v.Key, v.Mime)
	if errors.Is(err, storage.ErrNotFound) {
		return "", "bad_request", "语音文件未上传或已过期"
	}
	if err != nil {
		if strings.Contains(err.Error(), "音频") || strings.Contains(err.Error(), "大小") {
			return "", "bad_request", err.Error()
		}
		slog.Error("verify voice", "err", err)
		return "", "internal", "对象存储错误"
	}
	out, _ := json.Marshal(VoiceContent{Key: v.Key, Mime: info.Mime, DurationMs: v.DurationMs, Size: info.Size, Waveform: v.Waveform})
	return string(out), "", ""
}

// maxImageSide 宽高上限（防止明显错误的声明；客户端压缩后长边 ≤ 2048，GIF 原样）。
const maxImageSide = 30000

// validateImage 校验图片消息：key 属于自己的 image/ 前缀、宽高合法、对象已上传且大小/格式合格。
// 返回规范化后的 content。
func (c *Client) validateImage(ctx context.Context, raw string) (content, code, msg string) {
	st := c.deps.Storage
	if st == nil {
		return "", "unsupported", "服务器未启用图片消息"
	}
	var v ImageContent
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return "", "bad_request", "图片消息格式错误"
	}
	kind, owner, ok := storage.ParseKey(v.Key)
	if !ok || kind != "image" || owner != c.userID {
		return "", "bad_request", "无效的图片文件"
	}
	mime, _, ok := storage.NormalizeImageMime(v.Mime)
	if !ok {
		return "", "bad_request", "不支持的图片格式"
	}
	if v.Width <= 0 || v.Height <= 0 || v.Width > maxImageSide || v.Height > maxImageSide {
		return "", "bad_request", "图片宽高无效"
	}
	if !st.Config().VerifyUpload {
		if v.Size <= 0 || v.Size > st.Config().ImageMaxBytes {
			return "", "bad_request", "图片大小无效"
		}
		out, _ := json.Marshal(ImageContent{Key: v.Key, Mime: mime, Size: v.Size, Width: v.Width, Height: v.Height})
		return string(out), "", ""
	}
	vctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	info, err := st.VerifyImage(vctx, v.Key, mime)
	if errors.Is(err, storage.ErrNotFound) {
		return "", "bad_request", "图片未上传或已过期"
	}
	if err != nil {
		if strings.Contains(err.Error(), "图片") {
			return "", "bad_request", err.Error()
		}
		slog.Error("verify image", "err", err)
		return "", "internal", "对象存储错误"
	}
	out, _ := json.Marshal(ImageContent{Key: v.Key, Mime: info.Mime, Size: info.Size, Width: v.Width, Height: v.Height})
	return string(out), "", ""
}

func (c *Client) handleRecall(ctx context.Context, env Envelope) {
	var req recallReq
	if err := json.Unmarshal(env.Data, &req); err != nil || req.Seq == 0 {
		c.fail(env.ID, "bad_request", "参数错误")
		return
	}
	if _, ok := c.member(ctx, env.ID, req.ConversationID); !ok {
		return
	}
	before, after, err := c.store.Recall(ctx, req.ConversationID, req.Seq, c.userID, c.deps.RecallWindow)
	switch {
	case errors.Is(err, store.ErrNotFound):
		c.fail(env.ID, "not_found", "消息不存在")
		return
	case errors.Is(err, store.ErrNotSender):
		c.fail(env.ID, "forbidden", "只能撤回自己发送的消息")
		return
	case errors.Is(err, store.ErrRecallExpired):
		c.fail(env.ID, "recall_expired", "超过可撤回时间")
		return
	case errors.Is(err, store.ErrAlreadyRecalled):
		c.fail(env.ID, "already_recalled", "消息已撤回")
		return
	case err != nil:
		slog.Error("recall", "err", err)
		c.fail(env.ID, "internal", "服务器错误")
		return
	}
	// 撤回的语音/图片同时删除服务器上的文件；语音的转文字结果一并删除
	if (before.MsgType == store.MsgVoice || before.MsgType == store.MsgImage) && c.deps.Storage != nil {
		if key, _, _ := MediaKey(before.Content); key != "" {
			go func() {
				rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := c.deps.Storage.Remove(rctx, key); err != nil {
					slog.Warn("remove recalled media", "key", key, "err", err)
				}
			}()
		}
	}
	if before.MsgType == store.MsgVoice {
		if err := c.store.DeleteTranscript(ctx, req.ConversationID, req.Seq); err != nil {
			slog.Warn("delete transcript", "conversation", req.ConversationID, "seq", req.Seq, "err", err)
		}
	}
	data := map[string]any{
		"conversation_id": req.ConversationID, "seq": req.Seq, "user_id": c.userID,
		"recalled_at": after.RecalledAt, "mod_seq": after.ModSeq, "message": after,
	}
	c.reply("ack", env.ID, map[string]any{"op": "recall", "conversation_id": req.ConversationID, "seq": req.Seq})
	if members, err := c.store.Members(ctx, req.ConversationID); err == nil {
		c.hub.Publish(ctx, members, "", encode("recall", "", data))
	}
}

// validEmoji 粗略校验：1~32 字节、不含空白/控制字符/ASCII 字母，且不能全是 ASCII。
func validEmoji(s string) bool {
	if s == "" || len(s) > 32 || !utf8.ValidString(s) || utf8.RuneCountInString(s) > 10 {
		return false
	}
	nonASCII := false
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) || (r < 128 && unicode.IsLetter(r)) {
			return false
		}
		if r >= 128 {
			nonASCII = true
		}
	}
	return nonASCII
}

func (c *Client) handleReact(ctx context.Context, env Envelope) {
	var req reactReq
	if err := json.Unmarshal(env.Data, &req); err != nil || req.Seq == 0 {
		c.fail(env.ID, "bad_request", "参数错误")
		return
	}
	req.Emoji = strings.TrimSpace(req.Emoji)
	if !validEmoji(req.Emoji) {
		c.fail(env.ID, "bad_request", "无效的表情")
		return
	}
	mem, ok := c.member(ctx, env.ID, req.ConversationID)
	if !ok {
		return
	}
	if req.Seq <= mem.JoinSeq {
		c.fail(env.ID, "not_found", "消息不存在")
		return
	}
	on, mod, reactions, err := c.store.ToggleReaction(ctx, req.ConversationID, req.Seq, c.userID, req.Emoji, req.On)
	switch {
	case errors.Is(err, store.ErrNotFound):
		c.fail(env.ID, "not_found", "消息不存在或已撤回")
		return
	case errors.Is(err, store.ErrTooManyReactions):
		c.fail(env.ID, "too_many", "这条消息的表情太多了")
		return
	case err != nil:
		slog.Error("react", "err", err)
		c.fail(env.ID, "internal", "服务器错误")
		return
	}
	c.reply("ack", env.ID, map[string]any{"op": "react", "conversation_id": req.ConversationID, "seq": req.Seq, "emoji": req.Emoji, "on": on})
	if members, err := c.store.Members(ctx, req.ConversationID); err == nil {
		c.hub.Publish(ctx, members, "", encode("reaction", "", map[string]any{
			"conversation_id": req.ConversationID, "seq": req.Seq, "user_id": c.userID,
			"emoji": req.Emoji, "on": on, "reactions": reactions, "mod_seq": mod,
		}))
	}
}

func (c *Client) handleDelete(ctx context.Context, env Envelope) {
	var req deleteReq
	if err := json.Unmarshal(env.Data, &req); err != nil || len(req.Seqs) == 0 || len(req.Seqs) > 100 {
		c.fail(env.ID, "bad_request", "参数错误（seqs 1~100 条）")
		return
	}
	mem, ok := c.member(ctx, env.ID, req.ConversationID)
	if !ok {
		return
	}
	seqs, err := c.store.HideMessages(ctx, c.userID, req.ConversationID, mem.JoinSeq, req.Seqs)
	if err != nil {
		slog.Error("hide messages", "err", err)
		c.fail(env.ID, "internal", "服务器错误")
		return
	}
	if seqs == nil {
		seqs = []uint64{}
	}
	c.reply("ack", env.ID, map[string]any{"op": "delete", "conversation_id": req.ConversationID, "seqs": seqs})
	// 只同步给自己的所有设备
	c.hub.Publish(ctx, []uint64{c.userID}, "", encode("deleted", "", map[string]any{
		"conversation_id": req.ConversationID, "seqs": seqs,
	}))
}
