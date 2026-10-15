package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"

	"imserver/internal/auth"
	"imserver/internal/config"
	"imserver/internal/push"
	"imserver/internal/storage"
	"imserver/internal/store"
	"imserver/internal/stt"
	"imserver/internal/ws"
)

type Server struct {
	store    *store.Store
	auth     *auth.Manager
	hub      *ws.Hub
	push     push.Notifier
	storage  *storage.Storage // 可能为 nil（未启用对象存储）
	stt      *stt.Transcriber // 可能为 nil（未启用语音转文字）
	rdb      *redis.Client
	cfg      *config.Config
	deps     *ws.Deps
	upgrader websocket.Upgrader
}

func New(cfg *config.Config, st *store.Store, am *auth.Manager, hub *ws.Hub, p push.Notifier, stg *storage.Storage,
	tr *stt.Transcriber, rdb *redis.Client) *Server {
	allowOrigins := cfg.Server.AllowOrigins
	s := &Server{store: st, auth: am, hub: hub, push: p, storage: stg, stt: tr, rdb: rdb, cfg: cfg}
	s.deps = &ws.Deps{Store: st, Push: p, Storage: stg, RecallWindow: cfg.Message.RecallWindow()}
	s.upgrader = websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin: func(r *http.Request) bool {
			if len(allowOrigins) == 0 {
				return true // 移动端 App 通常不带 Origin
			}
			o := r.Header.Get("Origin")
			if o == "" {
				return true
			}
			for _, a := range allowOrigins {
				if a == o {
					return true
				}
			}
			return false
		},
	}
	return s
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("POST /api/register", s.register)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("GET /api/me", s.authed(s.me))
	mux.HandleFunc("GET /api/users/search", s.authed(s.searchUser))
	mux.HandleFunc("POST /api/conversations/direct", s.authed(s.openDirect))
	mux.HandleFunc("GET /api/conversations", s.authed(s.listConversations))
	mux.HandleFunc("GET /api/conversations/{id}/messages", s.authed(s.listMessages))
	mux.HandleFunc("GET /api/conversations/{id}/reads", s.authed(s.readStates))
	// v0.2.0
	mux.HandleFunc("GET /api/config", s.authed(s.clientConfig))
	mux.HandleFunc("GET /api/conversations/{id}", s.authed(s.getConversation))
	mux.HandleFunc("PATCH /api/conversations/{id}", s.authed(s.updateGroup))
	mux.HandleFunc("POST /api/conversations/group", s.authed(s.createGroup))
	mux.HandleFunc("GET /api/conversations/{id}/members", s.authed(s.listMembers))
	mux.HandleFunc("POST /api/conversations/{id}/members", s.authed(s.addMembers))
	mux.HandleFunc("DELETE /api/conversations/{id}/members/{uid}", s.authed(s.removeMember))
	mux.HandleFunc("PUT /api/conversations/{id}/members/{uid}/role", s.authed(s.setRole))
	mux.HandleFunc("POST /api/conversations/{id}/leave", s.authed(s.leaveGroup))
	mux.HandleFunc("GET /api/conversations/{id}/hidden", s.authed(s.hiddenSeqs))
	mux.HandleFunc("GET /api/conversations/{id}/messages/{seq}/media", s.authed(s.mediaURL))
	mux.HandleFunc("POST /api/uploads/presign", s.authed(s.presignUpload))
	// v0.3.0
	mux.HandleFunc("GET /api/presence", s.authed(s.presence))
	mux.HandleFunc("POST /api/conversations/{id}/messages/{seq}/transcribe", s.authed(s.transcribe))
	mux.HandleFunc("GET /ws", s.websocket)
	return logRequests(mux)
}

// ---------- helpers ----------

type ctxKey struct{}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, map[string]string{"code": errCode, "message": msg})
}

func decode(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	return json.NewDecoder(r.Body).Decode(v)
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

func (s *Server) authed(h func(http.ResponseWriter, *http.Request, uint64)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, err := s.auth.Verify(bearer(r))
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "未登录或登录已过期")
			return
		}
		h(w, r, uid)
	}
}

func pathID(r *http.Request) (uint64, bool) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func queryUint(r *http.Request, key string) uint64 {
	v, _ := strconv.ParseUint(r.URL.Query().Get(key), 10, 64)
	return v
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(c int) { s.code = c; s.ResponseWriter.WriteHeader(c) }

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" { // WebSocket 需要原始 ResponseWriter 来 Hijack
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		next.ServeHTTP(rec, r)
		slog.Debug("http", "method", r.Method, "path", r.URL.Path, "status", rec.code, "dur", time.Since(start))
	})
}

// ---------- handlers ----------

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true, "connections": s.hub.LocalConnCount()})
}

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Nickname string `json:"nickname"`
}

func normalizeEmail(e string) (string, bool) {
	e = strings.ToLower(strings.TrimSpace(e))
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || len(e) > 255 {
		return "", false
	}
	return e, true
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if err := decode(r, &c); err != nil {
		writeErr(w, 400, "bad_request", "参数错误")
		return
	}
	email, ok := normalizeEmail(c.Email)
	if !ok {
		writeErr(w, 400, "bad_email", "邮箱格式不正确")
		return
	}
	if l := utf8.RuneCountInString(c.Password); l < 8 || l > 72 {
		writeErr(w, 400, "bad_password", "密码长度需为 8 到 72 位")
		return
	}
	nick := strings.TrimSpace(c.Nickname)
	if nick == "" {
		nick = strings.Split(email, "@")[0]
	}
	if utf8.RuneCountInString(nick) > 32 {
		writeErr(w, 400, "bad_nickname", "昵称不超过 32 个字符")
		return
	}
	hash, err := auth.HashPassword(c.Password)
	if err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	u, err := s.store.CreateUser(r.Context(), email, hash, nick)
	if errors.Is(err, store.ErrDuplicate) {
		writeErr(w, 409, "email_taken", "该邮箱已注册")
		return
	}
	if err != nil {
		slog.Error("create user", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	s.issue(w, u, http.StatusCreated)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if err := decode(r, &c); err != nil {
		writeErr(w, 400, "bad_request", "参数错误")
		return
	}
	email, _ := normalizeEmail(c.Email)
	u, err := s.store.GetUserByEmail(r.Context(), email)
	if err != nil || !auth.CheckPassword(u.PasswordHash, c.Password) {
		writeErr(w, 401, "bad_credentials", "邮箱或密码错误")
		return
	}
	s.issue(w, u, 200)
}

func (s *Server) issue(w http.ResponseWriter, u *store.User, code int) {
	tok, exp, err := s.auth.Issue(u.ID)
	if err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	writeJSON(w, code, map[string]any{"token": tok, "expires_at": exp, "user": u})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, uid uint64) {
	u, err := s.store.GetUser(r.Context(), uid)
	if err != nil {
		writeErr(w, 404, "not_found", "用户不存在")
		return
	}
	writeJSON(w, 200, u)
}

func (s *Server) searchUser(w http.ResponseWriter, r *http.Request, uid uint64) {
	email, ok := normalizeEmail(r.URL.Query().Get("email"))
	if !ok {
		writeErr(w, 400, "bad_email", "邮箱格式不正确")
		return
	}
	u, err := s.store.GetUserByEmail(r.Context(), email)
	if err != nil {
		writeErr(w, 404, "not_found", "用户不存在")
		return
	}
	writeJSON(w, 200, u)
}

func (s *Server) openDirect(w http.ResponseWriter, r *http.Request, uid uint64) {
	var req struct {
		PeerID uint64 `json:"peer_id"`
	}
	if err := decode(r, &req); err != nil || req.PeerID == 0 {
		writeErr(w, 400, "bad_request", "peer_id 必填")
		return
	}
	if _, err := s.store.GetUser(r.Context(), req.PeerID); err != nil {
		writeErr(w, 404, "not_found", "对方用户不存在")
		return
	}
	cid, err := s.store.GetOrCreateDirect(r.Context(), uid, req.PeerID)
	if err != nil {
		slog.Error("open direct", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	writeJSON(w, 200, map[string]any{"conversation_id": cid})
}

func (s *Server) listConversations(w http.ResponseWriter, r *http.Request, uid uint64) {
	list, err := s.store.ListConversations(r.Context(), uid)
	if err != nil {
		slog.Error("list conversations", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	if list == nil {
		list = []store.ConversationSummary{}
	}
	writeJSON(w, 200, list)
}

// listMessages
//
//	?after_seq=N     补拉 seq>N 的消息（离线同步，正序）
//	?before_seq=N    拉取 seq<N 的历史；都不传则返回最新一页
//	?changed_since=M 拉取 mod_seq>M 的消息（撤回/表情回应变化的离线同步，按 mod_seq 正序）
//	?limit=50        最大 200
//
// 自动排除自己删除的消息和入群前的消息。
func (s *Server) listMessages(w http.ResponseWriter, r *http.Request, uid uint64) {
	cid, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad_request", "会话 ID 无效")
		return
	}
	mem, ok := s.memberInfo(w, r.Context(), cid, uid)
	if !ok {
		return
	}
	limit := int(queryUint(r, "limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := store.MsgQuery{Viewer: uid, ConversationID: cid, JoinSeq: mem.JoinSeq, Limit: limit,
		AfterSeq: queryUint(r, "after_seq"), BeforeSeq: queryUint(r, "before_seq"), ChangedSince: queryUint(r, "changed_since")}
	if q.AfterSeq == 0 && q.BeforeSeq == 0 && q.ChangedSince == 0 {
		q.BeforeSeq = math.MaxInt64
	}
	msgs, err := s.store.Messages(r.Context(), q)
	if err != nil {
		slog.Error("list messages", "err", err)
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	writeJSON(w, 200, msgs)
}

func (s *Server) readStates(w http.ResponseWriter, r *http.Request, uid uint64) {
	cid, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "bad_request", "会话 ID 无效")
		return
	}
	if !s.member(w, r.Context(), cid, uid) {
		return
	}
	m, err := s.store.ReadStates(r.Context(), cid)
	if err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return
	}
	out := make(map[string]uint64, len(m))
	for k, v := range m {
		out[strconv.FormatUint(k, 10)] = v
	}
	writeJSON(w, 200, out)
}

func (s *Server) member(w http.ResponseWriter, ctx context.Context, cid, uid uint64) bool {
	_, ok := s.memberInfo(w, ctx, cid, uid)
	return ok
}

// memberInfo 校验成员身份，失败时已写入错误响应。
func (s *Server) memberInfo(w http.ResponseWriter, ctx context.Context, cid, uid uint64) (*store.Member, bool) {
	m, err := s.store.GetMember(ctx, cid, uid)
	if errors.Is(err, store.ErrNotMember) {
		writeErr(w, 403, "forbidden", "不是该会话成员")
		return nil, false
	}
	if err != nil {
		writeErr(w, 500, "internal", "服务器错误")
		return nil, false
	}
	return m, true
}

// websocket 握手：token 放在 Authorization 头或 ?token= 查询参数。
func (s *Server) websocket(w http.ResponseWriter, r *http.Request) {
	tok := bearer(r)
	if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	uid, err := s.auth.Verify(tok)
	if err != nil {
		writeErr(w, 401, "unauthorized", "未登录或登录已过期")
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.NewClient(s.hub, s.deps, conn, uid).Serve()
}
