package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var (
	v040Base      string // 开启 calls + smtp 的实例地址（calls.base_url → -calls-stub，smtp → -smtp-stub，tls_mode: none）
	smtpStubAddr  string // 本进程启动的假 SMTP 监听地址
	callsStubAddr string // 本进程启动的假 Cloudflare 监听地址
)

// ---------- 假 SMTP 服务器（只实现服务端发信需要的最小子集） ----------

type smtpStub struct {
	mu   sync.Mutex
	msgs []string
}

func (s *smtpStub) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	write := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }
	write("220 stub ESMTP")
	var data strings.Builder
	inData := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if inData {
			if trimmed == "." {
				s.mu.Lock()
				s.msgs = append(s.msgs, data.String())
				s.mu.Unlock()
				data.Reset()
				inData = false
				write("250 OK")
			} else {
				data.WriteString(line)
			}
			continue
		}
		up := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
			write("250 stub greets")
		case strings.HasPrefix(up, "MAIL"), strings.HasPrefix(up, "RCPT"), up == "RSET", up == "NOOP":
			write("250 OK")
		case up == "DATA":
			inData = true
			write("354 End data with <CR><LF>.<CR><LF>")
		case up == "QUIT":
			write("221 Bye")
			return
		default:
			write("250 OK")
		}
	}
}

func (s *smtpStub) findResetToken() string {
	re := regexp.MustCompile(`token=([A-Za-z0-9_-]{20,})`)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		msgs := append([]string{}, s.msgs...)
		s.mu.Unlock()
		for _, m := range msgs {
			if mm := re.FindStringSubmatch(m); mm != nil {
				return mm[1]
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return ""
}

// ---------- 假 Cloudflare（generate-ice-servers） ----------

type callsStub struct {
	mu       sync.Mutex
	hits     int
	lastAuth string
	lastTTL  int
}

func (s *callsStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || !strings.Contains(r.URL.Path, "/credentials/generate-ice-servers") {
		http.NotFound(w, r)
		return
	}
	var in struct {
		TTL int `json:"ttl"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	s.mu.Lock()
	s.hits++
	s.lastAuth = r.Header.Get("Authorization")
	s.lastTTL = in.TTL
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	_, _ = w.Write([]byte(`{"iceServers":[{"urls":["stun:stun.cloudflare.com:3478"]},{"urls":["turn:turn.cloudflare.com:3478?transport=udp"],"username":"stubuser","credential":"stubcred"}]}`))
}

// fakeMP4 只带 ftyp 文件头（服务端只校验头部与大小）。
func fakeMP4(n int) []byte {
	b := make([]byte, n)
	copy(b[4:], "ftypisom")
	return b
}

func historyAt(baseURL, token string, cid uint64, q string) []msgOut {
	var out []msgOut
	callAt(baseURL, "GET", fmt.Sprintf("/api/conversations/%d/messages%s", cid, q), token, nil, &out)
	return out
}

func directAt(baseURL, token string, peer uint64) uint64 {
	var dc struct {
		ConversationID uint64 `json:"conversation_id"`
	}
	callAt(baseURL, "POST", "/api/conversations/direct", token, map[string]uint64{"peer_id": peer}, &dc)
	return dc.ConversationID
}

func v040(suffix string, alice, bob, eve authResp) {
	fmt.Println("\n--- v0.4.0 ---")

	// ---------- /api/config 新字段 ----------
	var cfg map[string]any
	call("GET", "/api/config", alice.Token, nil, &cfg)
	_, ok1 := cfg["call_enabled"]
	_, ok2 := cfg["call_ring_timeout_seconds"]
	_, ok3 := cfg["file_enabled"]
	_, ok4 := cfg["file_max_bytes"]
	_, ok5 := cfg["video_enabled"]
	_, ok6 := cfg["video_max_bytes"]
	_, ok7 := cfg["password_reset_enabled"]
	check(ok1 && ok2 && ok3 && ok4 && ok5 && ok6 && ok7,
		"GET /api/config has call_enabled/call_ring_timeout_seconds/file_*/video_*/password_reset_enabled")

	// 主实例未启用 calls/smtp 时的兜底错误码
	check(call("GET", "/api/calls/ice-servers", alice.Token, nil, nil) == 501 || cfg["call_enabled"] == true,
		"ice-servers → 501 call_disabled when calls disabled")
	var er map[string]string
	st := call("POST", "/api/password/forgot", "", map[string]string{"email": "nobody" + suffix + "@test.com"}, &er)
	check(st == 501 || cfg["password_reset_enabled"] == true, "forgot → 501 reset_disabled when smtp disabled")

	// ---------- msg_type=6 客户端不可发 ----------
	cid := directAt(base, alice.Token, bob.User.ID)
	a1 := dial(alice.Token)
	time.Sleep(200 * time.Millisecond)
	_, e6, ok6 := sendMsg(a1, cid, "call-forge", map[string]any{"msg_type": 6, "content": `{"call_id":"x","kind":"audio","outcome":"completed","caller_id":1}`})
	check(!ok6 && strings.Contains(string(e6.Data), "bad_request"), "client send msg_type=6 rejected (bad_request)")
	a1.Close()

	if s3Endpoint != "" {
		v040Files(alice, bob, eve)
		v040Avatar(alice, bob)
	} else {
		fmt.Println("SKIP file/video/avatar checks (no -s3-endpoint)")
	}
	if v040Base != "" {
		v040Password(suffix)
		v040Calls(suffix)
	} else {
		fmt.Println("SKIP password/call checks (no -v040-base)")
	}
}

// ---------- 文件 / 视频消息 ----------

// presignOut / put 复用 v030.go 的定义。
func presign040(token string, body map[string]any) (presignOut, int) {
	var pre presignOut
	st := call("POST", "/api/uploads/presign", token, body, &pre)
	return pre, st
}

func v040Files(alice, bob, eve authResp) {
	cid := directAt(base, alice.Token, bob.User.ID)
	a, b := dial(alice.Token), dial(bob.Token)
	defer a.Close()
	defer b.Close()
	time.Sleep(200 * time.Millisecond)

	// presign 负面用例
	e1, st := presign040(alice.Token, map[string]any{"kind": "video", "mime": "video/avi", "size": 1000, "duration_ms": 1000, "width": 10, "height": 10})
	check(st == 400 && e1.Code == "bad_mime", "presign rejects non-whitelisted video mime (bad_mime)")
	e1, st = presign040(alice.Token, map[string]any{"kind": "video", "mime": "video/mp4", "size": 104857601, "duration_ms": 1000, "width": 10, "height": 10})
	check(st == 400 && e1.Code == "too_large", "presign rejects video > video_max_bytes (too_large)")
	e1, st = presign040(alice.Token, map[string]any{"kind": "file", "mime": "application/pdf", "size": 1000, "name": ""})
	check(st == 400 && e1.Code == "bad_name", "presign rejects file without name (bad_name)")
	e1, st = presign040(alice.Token, map[string]any{"kind": "file", "mime": "application/pdf", "size": 104857601, "name": "big.bin"})
	check(st == 400 && e1.Code == "too_large", "presign rejects file > file_max_bytes (too_large)")

	// 文件正流程
	payload := []byte(strings.Repeat("hello file ", 500))
	pre, st := presign040(alice.Token, map[string]any{"kind": "file", "mime": "application/pdf", "size": len(payload), "name": "报告.pdf"})
	check(st == 200 && strings.HasPrefix(pre.Key, fmt.Sprintf("file/%d/", alice.User.ID)) && strings.HasSuffix(pre.Key, ".pdf"),
		"presign file: key file/{uid}/….pdf")
	check(put(pre, payload) == 200, "PUT file to storage")
	fc, _ := json.Marshal(map[string]any{"key": pre.Key, "mime": "application/pdf", "size": len(payload), "name": "报告.pdf"})
	fm, _, ok := sendMsg(a, cid, "file-1", map[string]any{"msg_type": 5, "content": string(fc)})
	var fcc struct {
		Key  string `json:"key"`
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	_ = json.Unmarshal([]byte(fm.Content), &fcc)
	check(ok && fm.MsgType == 5 && fcc.Key == pre.Key && fcc.Name == "报告.pdf" && fcc.Size == int64(len(payload)),
		"file message accepted (content key/name/size)")
	_, ok = expectWhere(b, "message", 3*time.Second, has(`"msg_type":5`))
	check(ok, "file message delivered")
	var media struct {
		URL  string `json:"url"`
		Size int64  `json:"size"`
	}
	check(call("GET", fmt.Sprintf("/api/conversations/%d/messages/%d/media", cid, fm.Seq), bob.Token, nil, &media) == 200 && media.Size == int64(len(payload)),
		"file media url")
	resp, err := http.Get(media.URL)
	must(err, "GET file")
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	check(resp.StatusCode == 200 && bytes.Equal(got, payload), "presigned GET downloads identical file bytes")
	check(call("GET", fmt.Sprintf("/api/conversations/%d/messages/%d/media", cid, fm.Seq), eve.Token, nil, nil) == 403, "non-member cannot get file url")

	// 引用通话记录/文件的预览等在通话部分覆盖；这里验证文件不能冒用他人 key
	_, er2, ok2 := sendMsg(b, cid, "file-steal", map[string]any{"msg_type": 5, "content": string(fc)})
	check(!ok2 && strings.Contains(string(er2.Data), "无效的文件"), "cannot send someone else's file upload")

	// 视频正流程（带封面）
	poster := fakePNG(20_000)
	ppre, _ := presignImage(base, alice.Token, "image/png", len(poster), 320, 180)
	check(put(ppre, poster) == 200, "PUT video poster")
	vid := fakeMP4(60_000)
	vpre, st := presign040(alice.Token, map[string]any{"kind": "video", "mime": "video/mp4", "size": len(vid), "duration_ms": 5000, "width": 1280, "height": 720})
	check(st == 200 && strings.HasPrefix(vpre.Key, fmt.Sprintf("video/%d/", alice.User.ID)) && strings.HasSuffix(vpre.Key, ".mp4"),
		"presign video: key video/{uid}/….mp4")
	check(put(vpre, vid) == 200, "PUT video to storage")
	vc, _ := json.Marshal(map[string]any{"key": vpre.Key, "mime": "video/mp4", "size": len(vid), "duration_ms": 5000, "width": 1280, "height": 720, "poster_key": ppre.Key})
	vm, _, ok := sendMsg(a, cid, "video-1", map[string]any{"msg_type": 4, "content": string(vc)})
	check(ok && vm.MsgType == 4 && strings.Contains(vm.Content, `"poster_key"`), "video message with poster accepted")
	_, ok = expectWhere(b, "message", 3*time.Second, has(`"msg_type":4`))
	check(ok, "video message delivered")
	badvc, _ := json.Marshal(map[string]any{"key": vpre.Key, "mime": "video/mp4", "size": len(vid), "duration_ms": 0, "width": 1280, "height": 720})
	_, _, ok = sendMsg(a, cid, "video-baddur", map[string]any{"msg_type": 4, "content": string(badvc)})
	check(!ok, "video without valid duration rejected")

	// 撤回文件：对象删除 + media 410
	_, ok = request(a, "recall", "rc-file", map[string]any{"conversation_id": cid, "seq": fm.Seq})
	time.Sleep(500 * time.Millisecond)
	check(ok && call("GET", fmt.Sprintf("/api/conversations/%d/messages/%d/media", cid, fm.Seq), bob.Token, nil, nil) == 410,
		"recalled file: media 410")
	mc, merr := minio.New(s3Endpoint, &minio.Options{Creds: credentials.NewStaticV4(s3Key, s3Secret, ""), Secure: s3SSL, Region: s3Region})
	must(merr, "minio client")
	_, err = mc.StatObject(context.Background(), s3Bucket, pre.Key, minio.StatObjectOptions{})
	check(err != nil, "recalled file object deleted from storage")
}

// ---------- 头像 / 昵称 ----------

func v040Avatar(alice, bob authResp) {
	b := dial(bob.Token)
	defer b.Close()
	time.Sleep(200 * time.Millisecond)

	img := fakePNG(30_000)
	pre, st := presign040(alice.Token, map[string]any{"kind": "avatar", "mime": "image/png", "size": len(img)})
	check(st == 200 && strings.HasPrefix(pre.Key, fmt.Sprintf("avatar/%d/", alice.User.ID)), "presign avatar: key avatar/{uid}/…")
	check(put(pre, img) == 200, "PUT avatar to storage")

	var u struct {
		ID        uint64 `json:"id"`
		Nickname  string `json:"nickname"`
		AvatarURL string `json:"avatar_url"`
	}
	st = call("PATCH", "/api/me", alice.Token, map[string]any{"avatar_key": pre.Key}, &u)
	check(st == 200 && strings.HasPrefix(u.AvatarURL, fmt.Sprintf("/api/users/%d/avatar?v=", alice.User.ID)),
		"PATCH /api/me avatar_key → avatar_url composed as /api/users/{id}/avatar?v=…")

	// 头像下发：登录态 + 302 到预签名地址
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", base+u.AvatarURL, nil)
	req.Header.Set("Authorization", "Bearer "+bob.Token)
	resp, err := noRedirect.Do(req)
	must(err, "GET avatar")
	resp.Body.Close()
	check(resp.StatusCode == 302 && strings.Contains(resp.Header.Get("Location"), pre.Key), "GET /api/users/{id}/avatar → 302 presigned")
	check(call("GET", fmt.Sprintf("/api/users/%d/avatar", bob.User.ID), alice.Token, nil, nil) == 404, "avatar of user without avatar → 404 no_avatar")

	// 别人的头像 key 不能冒用
	var er map[string]string
	st = call("PATCH", "/api/me", bob.Token, map[string]any{"avatar_key": pre.Key}, &er)
	check(st == 400 && er["code"] == "bad_avatar", "cannot set someone else's avatar key (bad_avatar)")

	// 改昵称 → user_updated 推给共享会话成员
	st = call("PATCH", "/api/me", alice.Token, map[string]any{"nickname": "Alice v040"}, &u)
	check(st == 200 && u.Nickname == "Alice v040", "PATCH /api/me nickname")
	_, ok := expectWhere(b, "user_updated", 3*time.Second, has(`"nickname":"Alice v040"`))
	check(ok, "user_updated event pushed to co-member")
	st = call("PATCH", "/api/me", alice.Token, map[string]any{"nickname": "  "}, &er)
	check(st == 400 && er["code"] == "bad_nickname", "empty nickname rejected (bad_nickname)")
	// 还原昵称，避免影响其他检查
	call("PATCH", "/api/me", alice.Token, map[string]any{"nickname": "Alice 🍎"}, nil)
}

// ---------- 重置密码 / 修改密码（需要 -v040-base：smtp 指向假 SMTP） ----------

func v040Password(suffix string) {
	fmt.Println("--- v0.4.0 password (smtp stub) ---")
	smtp := &smtpStub{}
	ln, err := net.Listen("tcp", smtpStubAddr)
	must(err, "listen smtp stub")
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go smtp.handle(c)
		}
	}()
	defer ln.Close()

	// 注册并登录（v040 实例）
	pw1 := registerAt(v040Base, "pw1"+suffix, "PwOld-12345")
	var login struct {
		Token string `json:"token"`
	}
	check(callAt(v040Base, "POST", "/api/login", "", map[string]string{"email": "pw1" + suffix + "@test.com", "password": "PwOld-12345"}, &login) == 200,
		"v040 instance login")

	// forgot：不存在的邮箱也回 200（防枚举），且不发信
	var er map[string]string
	check(callAt(v040Base, "POST", "/api/password/forgot", "", map[string]string{"email": "ghost" + suffix + "@test.com"}, &er) == 200,
		"forgot unknown email → 200 (anti-enumeration)")
	time.Sleep(300 * time.Millisecond)
	smtp.mu.Lock()
	n0 := len(smtp.msgs)
	smtp.mu.Unlock()
	check(n0 == 0, "no mail sent for unknown email")

	// forgot：真实邮箱 → 200 + 假 SMTP 收到带 token 的重置链接
	check(callAt(v040Base, "POST", "/api/password/forgot", "", map[string]string{"email": "pw1" + suffix + "@test.com"}, &er) == 200,
		"forgot known email → 200")
	token := smtp.findResetToken()
	check(token != "", "reset mail received by smtp stub with token link")

	// reset：错 token / 弱密码 / 正确流程
	st := callAt(v040Base, "POST", "/api/password/reset", "", map[string]string{"token": "bogus-token-value", "new_password": "NewPass-678"}, &er)
	check(st == 400 && er["code"] == "bad_token", "reset with bogus token → bad_token")
	st = callAt(v040Base, "POST", "/api/password/reset", "", map[string]string{"token": token, "new_password": "short"}, &er)
	check(st == 400 && er["code"] == "bad_password", "reset with short password → bad_password")
	st = callAt(v040Base, "POST", "/api/password/reset", "", map[string]string{"token": token, "new_password": "NewPass-678"}, &er)
	check(st == 200, "reset succeeds")
	st = callAt(v040Base, "POST", "/api/password/reset", "", map[string]string{"token": token, "new_password": "Another-999"}, &er)
	check(st == 400 && er["code"] == "bad_token", "reset token is single-use")

	// 旧登录态失效：旧 token 401、旧密码登录 401、新密码登录 200
	check(callAt(v040Base, "GET", "/api/me", login.Token, nil, nil) == 401, "old token revoked after reset (401)")
	check(callAt(v040Base, "POST", "/api/login", "", map[string]string{"email": "pw1" + suffix + "@test.com", "password": "PwOld-12345"}, nil) == 401,
		"old password no longer logs in")
	var login2 struct {
		Token string `json:"token"`
	}
	check(callAt(v040Base, "POST", "/api/login", "", map[string]string{"email": "pw1" + suffix + "@test.com", "password": "NewPass-678"}, &login2) == 200 && login2.Token != "",
		"new password logs in")
	_ = pw1

	// 修改密码：错旧密码 403 / 新旧相同 400 / 成功后其他连接收到 session_revoked、旧 token 401、新 token 可用
	pw2 := registerAt(v040Base, "pw2"+suffix, "Start-12345")
	var l3 struct {
		Token string `json:"token"`
	}
	callAt(v040Base, "POST", "/api/login", "", map[string]string{"email": "pw2" + suffix + "@test.com", "password": "Start-12345"}, &l3)
	w2 := dialAt(v040Base, l3.Token)
	defer w2.Close()
	time.Sleep(200 * time.Millisecond)
	st = callAt(v040Base, "POST", "/api/me/password", l3.Token, map[string]string{"old_password": "wrong-old", "new_password": "Changed-777"}, &er)
	check(st == 403 && er["code"] == "wrong_password", "change password wrong old → wrong_password")
	st = callAt(v040Base, "POST", "/api/me/password", l3.Token, map[string]string{"old_password": "Start-12345", "new_password": "Start-12345"}, &er)
	check(st == 400 && er["code"] == "same_password", "change password same → same_password")
	var changed struct {
		Token string `json:"token"`
	}
	st = callAt(v040Base, "POST", "/api/me/password", l3.Token, map[string]string{"old_password": "Start-12345", "new_password": "Changed-777"}, &changed)
	check(st == 200 && changed.Token != "" && changed.Token != l3.Token, "change password returns fresh token")
	_, ok := expectWhere(w2, "session_revoked", 3*time.Second, has(`"reason":"password_changed"`))
	check(ok, "other connections get session_revoked on password change")
	check(callAt(v040Base, "GET", "/api/me", l3.Token, nil, nil) == 401, "old token dead after password change")
	check(callAt(v040Base, "GET", "/api/me", changed.Token, nil, nil) == 200, "new token works after password change")
	_ = pw2
}

func registerAt(baseURL, name, password string) authResp {
	var u authResp
	callAt(baseURL, "POST", "/api/register", "", map[string]string{
		"email": name + "@test.com", "password": password, "nickname": name,
	}, &u)
	return u
}

// ---------- 通话信令（需要 -v040-base：calls.base_url 指向假 Cloudflare） ----------

func v040Calls(suffix string) {
	fmt.Println("--- v0.4.0 calls (cloudflare stub) ---")
	stub := &callsStub{}
	srv := &http.Server{Addr: callsStubAddr, Handler: stub}
	go srv.ListenAndServe()
	defer srv.Close()

	c1 := registerAt(v040Base, "call1"+suffix, "Callpass-1")
	c2 := registerAt(v040Base, "call2"+suffix, "Callpass-2")
	c3 := registerAt(v040Base, "call3"+suffix, "Callpass-3") // 保持离线
	c4 := registerAt(v040Base, "call4"+suffix, "Callpass-4")

	// ice-servers：走假 Cloudflare，且第二次命中缓存不再打上游
	var ice struct {
		IceServers []map[string]any `json:"iceServers"`
		ExpiresAt  string           `json:"expires_at"`
	}
	st := callAt(v040Base, "GET", "/api/calls/ice-servers", c1.Token, nil, &ice)
	check(st == 200 && len(ice.IceServers) == 2 && ice.ExpiresAt != "", "ice-servers returns stub iceServers + expires_at")
	check(callAt(v040Base, "GET", "/api/calls/ice-servers", c1.Token, nil, &ice) == 200, "ice-servers second call ok")
	stub.mu.Lock()
	hits, authz, ttl := stub.hits, stub.lastAuth, stub.lastTTL
	stub.mu.Unlock()
	check(hits == 1 && strings.HasPrefix(authz, "Bearer ") && ttl > 0, "ice-servers cached per user (1 upstream hit, bearer auth, ttl sent)")

	cid12 := directAt(v040Base, c1.Token, c2.User.ID)
	w1, w2 := dialAt(v040Base, c1.Token), dialAt(v040Base, c2.Token)
	defer w1.Close()
	defer w2.Close()
	time.Sleep(300 * time.Millisecond)

	callID := "call-" + suffix + "-1"
	send040(w1, "call_invite", "ci1", map[string]any{"conversation_id": cid12, "call_id": callID, "kind": "audio"})
	ev, ok := expectWhere(w2, "call_invite", 3*time.Second, has(fmt.Sprintf(`"call_id":%q`, callID)))
	check(ok && strings.Contains(string(ev.Data), `"kind":"audio"`), "callee receives call_invite")
	send040(w2, "call_accept", "ca1", map[string]any{"conversation_id": cid12, "call_id": callID})
	_, ok = expectWhere(w1, "call_accept", 3*time.Second, has(fmt.Sprintf(`"call_id":%q`, callID)))
	check(ok, "caller receives call_accept")
	send040(w1, "call_signal", "cs1", map[string]any{"conversation_id": cid12, "call_id": callID, "kind": "offer", "sdp": "fake-offer-sdp"})
	ev, ok = expectWhere(w2, "call_signal", 3*time.Second, has("fake-offer-sdp"))
	check(ok, "call_signal forwarded verbatim")
	send040(w2, "call_hangup", "ch1", map[string]any{"conversation_id": cid12, "call_id": callID})
	_, ok = expectWhere(w1, "call_hangup", 3*time.Second, has(fmt.Sprintf(`"call_id":%q`, callID)))
	check(ok, "caller receives call_hangup")
	_, ok = expectWhere(w1, "message", 3*time.Second, has(`"msg_type":6`))
	check(ok, "call record message (msg_type=6) delivered")
	found := false
	var recSeq uint64
	for _, m := range historyAt(v040Base, c1.Token, cid12, "") {
		if m.MsgType == 6 && strings.Contains(m.Content, `"outcome":"completed"`) && strings.Contains(m.Content, `"call_id":"`+callID+`"`) {
			found = true
			recSeq = m.Seq
		}
	}
	check(found, "call record in history: outcome=completed")
	er6, ok6 := request(w1, "recall", "rc-call", map[string]any{"conversation_id": cid12, "seq": recSeq})
	check(!ok6 && strings.Contains(string(er6.Data), "forbidden"), "call record cannot be recalled (forbidden)")

	// 离线被叫：call_unavailable offline + 未接听记录
	cid13 := directAt(v040Base, c1.Token, c3.User.ID)
	callID2 := "call-" + suffix + "-2"
	send040(w1, "call_invite", "ci2", map[string]any{"conversation_id": cid13, "call_id": callID2, "kind": "video"})
	ev, ok = expectWhere(w1, "call_unavailable", 3*time.Second, has(`"reason":"offline"`))
	check(ok, "offline callee → call_unavailable offline")
	found = false
	for i := 0; i < 20 && !found; i++ {
		for _, m := range historyAt(v040Base, c1.Token, cid13, "") {
			if m.MsgType == 6 && strings.Contains(m.Content, `"outcome":"missed"`) {
				found = true
			}
		}
		if !found {
			time.Sleep(200 * time.Millisecond)
		}
	}
	check(found, "offline call leaves missed record")

	// 忙线：c2 正响铃时 c4 再呼 c2 → busy；随后 c2 拒接第一通
	w4 := dialAt(v040Base, c4.Token)
	defer w4.Close()
	time.Sleep(200 * time.Millisecond)
	cid42 := directAt(v040Base, c4.Token, c2.User.ID)
	callID3 := "call-" + suffix + "-3"
	send040(w1, "call_invite", "ci3", map[string]any{"conversation_id": cid12, "call_id": callID3, "kind": "audio"})
	_, ok = expectWhere(w2, "call_invite", 3*time.Second, has(fmt.Sprintf(`"call_id":%q`, callID3)))
	check(ok, "second call rings callee")
	send040(w4, "call_invite", "ci4", map[string]any{"conversation_id": cid42, "call_id": "call-" + suffix + "-4", "kind": "audio"})
	_, ok = expectWhere(w4, "call_unavailable", 3*time.Second, has(`"reason":"busy"`))
	check(ok, "busy callee → call_unavailable busy")
	send040(w2, "call_reject", "cr1", map[string]any{"conversation_id": cid12, "call_id": callID3})
	_, ok = expectWhere(w1, "call_reject", 3*time.Second, has(fmt.Sprintf(`"call_id":%q`, callID3)))
	check(ok, "caller receives call_reject")

	// 响铃超时：实例需把 ring_timeout_seconds 配小（如 3）；等双方 call_hangup timeout + missed 记录
	callID5 := "call-" + suffix + "-5"
	send040(w1, "call_invite", "ci5", map[string]any{"conversation_id": cid12, "call_id": callID5, "kind": "audio"})
	expectWhere(w2, "call_invite", 3*time.Second, has(fmt.Sprintf(`"call_id":%q`, callID5)))
	_, ok1 := expectWhere(w2, "call_hangup", 25*time.Second, has(`"reason":"timeout"`))
	_, ok2 := expectWhere(w1, "call_hangup", 5*time.Second, has(`"reason":"timeout"`))
	check(ok1 && ok2, "ring timeout: both sides get call_hangup timeout")
	found = false
	for i := 0; i < 20 && !found; i++ {
		for _, m := range historyAt(v040Base, c1.Token, cid12, "") {
			if m.MsgType == 6 && strings.Contains(m.Content, `"call_id":"`+callID5+`"`) && strings.Contains(m.Content, `"outcome":"missed"`) {
				found = true
			}
		}
		if !found {
			time.Sleep(200 * time.Millisecond)
		}
	}
	check(found, "ring timeout leaves missed record")
}

func send040(c *websocket.Conn, typ, id string, data map[string]any) {
	frame, _ := json.Marshal(map[string]any{"type": typ, "id": id, "data": data})
	if err := c.WriteMessage(websocket.TextMessage, frame); err != nil {
		must(err, "ws write "+typ)
	}
}
