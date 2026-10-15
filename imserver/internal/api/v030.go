package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"imserver/internal/storage"
	"imserver/internal/store"
	"imserver/internal/ws"
)

// ---------- 在线状态（v0.3.0） ----------

const maxPresenceIDs = 200

// presence GET /api/presence?ids=1,2,3（最多 200 个）→ [{"user_id":1,"online":true,"last_seen":null}, ...]
// 只返回与请求者共享会话的用户，其他忽略。顺序与请求一致（去重）。
func (s *Server) presence(w http.ResponseWriter, r *http.Request, uid uint64) {
	raw := strings.TrimSpace(r.URL.Query().Get("ids"))
	ids := []uint64{}
	seen := map[uint64]bool{}
	if raw != "" {
		for _, p := range strings.Split(raw, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			id, err := strconv.ParseUint(p, 10, 64)
			if err != nil || id == 0 {
				writeErr(w, 400, "bad_request", "ids 格式错误")
				return
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if len(ids) > maxPresenceIDs {
		writeErr(w, 400, "too_many", fmt.Sprintf("ids 最多 %d 个", maxPresenceIDs))
		return
	}
	out := []ws.PresenceData{}
	if len(ids) == 0 {
		writeJSON(w, 200, out)
		return
	}
	ctx := r.Context()
	lastSeen, err := s.store.SharedLastSeen(ctx, uid, ids)
	if err != nil {
		slog.Error("presence last_seen", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	allowed := make([]uint64, 0, len(lastSeen))
	for _, id := range ids {
		if _, ok := lastSeen[id]; ok {
			allowed = append(allowed, id)
		}
	}
	online, err := s.hub.OnlineMany(ctx, allowed)
	if err != nil {
		slog.Error("presence online", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	for _, id := range allowed {
		p := ws.PresenceData{UserID: id, Online: online[id]}
		if !p.Online {
			p.LastSeen = ws.FormatLastSeen(lastSeen[id])
		}
		out = append(out, p)
	}
	writeJSON(w, 200, out)
}

// ---------- 语音转文字（v0.3.0） ----------

// maxSTTAudioBytes 送去转写的音频上限（Groq 25MB；语音本身受 voice_max_bytes 限制）。
const maxSTTAudioBytes = 25 << 20

// shanghai 日界（中国无夏令时，用固定时区避免依赖 tzdata）。
var shanghai = time.FixedZone("Asia/Shanghai", 8*3600)

// sttCounter 返回今天的计数 key 和次日 0 点（Asia/Shanghai）。
func sttCounter(uid uint64, now time.Time) (string, time.Time) {
	t := now.In(shanghai)
	next := time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, shanghai)
	return fmt.Sprintf("im:stt:%d:%s", uid, t.Format("20060102")), next
}

// transcribe POST /api/conversations/{id}/messages/{seq}/transcribe → {"text":"...","cached":false}
func (s *Server) transcribe(w http.ResponseWriter, r *http.Request, uid uint64) {
	if s.stt == nil || s.storage == nil {
		writeErr(w, 501, "stt_disabled", "服务器未启用语音转文字")
		return
	}
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
	if m.MsgType != store.MsgVoice {
		writeErr(w, 404, "not_voice", "不是语音消息")
		return
	}
	if text, err := s.store.GetTranscript(ctx, cid, seq); err == nil {
		writeJSON(w, 200, map[string]any{"text": text, "cached": true})
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		slog.Error("get transcript", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	key, mime, _ := ws.MediaKey(m.Content)
	if key == "" {
		writeErr(w, 404, "not_found", "文件不存在")
		return
	}
	if time.Since(m.CreatedAt) > time.Duration(s.storage.Config().VoiceTTLDays)*24*time.Hour {
		writeErr(w, 410, "expired", "语音已过期")
		return
	}

	// 每日限额：先占一次，失败退回
	limit := s.stt.Config().DailyLimitPerUser
	var counterKey string
	if limit > 0 {
		var next time.Time
		counterKey, next = sttCounter(uid, time.Now())
		n, err := s.rdb.Incr(ctx, counterKey).Result()
		if err != nil {
			slog.Error("stt counter", "err", err)
			writeErr(w, 500, "internal", "服务器错误")
			return
		}
		if n == 1 {
			s.rdb.ExpireAt(ctx, counterKey, next)
		}
		if n > int64(limit) {
			s.rdb.Decr(context.Background(), counterKey)
			writeErr(w, 429, "stt_limit", "今日转文字次数已用完")
			return
		}
	}
	refund := func() {
		if counterKey != "" {
			s.rdb.Decr(context.Background(), counterKey)
		}
	}

	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	audio, err := s.storage.Read(rctx, key, maxSTTAudioBytes)
	cancel()
	if err != nil {
		refund()
		if errors.Is(err, storage.ErrNotFound) {
			writeErr(w, 410, "expired", "语音已过期")
			return
		}
		slog.Error("stt read audio", "key", key, "err", err)
		writeErr(w, 502, "stt_failed", "读取语音失败")
		return
	}
	text, provider, err := s.stt.Transcribe(ctx, audio, mime)
	if err != nil {
		refund()
		slog.Warn("stt failed", "conversation", cid, "seq", seq, "err", err)
		writeErr(w, 502, "stt_failed", "语音转文字失败，请稍后重试")
		return
	}
	if err := s.store.SaveTranscript(ctx, cid, seq, text, provider); err != nil {
		slog.Error("save transcript", "err", err)
	} else if m2, err := s.store.GetMessage(ctx, cid, seq); err == nil && m2.Recalled {
		_ = s.store.DeleteTranscript(ctx, cid, seq) // 转写期间被撤回
	}
	writeJSON(w, 200, map[string]any{"text": text, "cached": false})
}
