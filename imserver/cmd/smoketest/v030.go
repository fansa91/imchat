package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var (
	sttBase     string // 开启 stt 的第二个实例（stt.groq.base_url / stt.cloudflare.base_url 指向 sttStubAddr）
	sttStubAddr string // 本进程启动的假 STT 提供方监听地址
)

// ---------- 假 STT 提供方 ----------

type sttStub struct {
	mu        sync.Mutex
	groqFail  int // 非 0 时 groq 返回该状态码
	groqHits  int
	cfHits    int
	lastAudio []byte
	lastForm  map[string]string
	lastAuth  string
	cfLang    string
	cfPath    string
}

func (st *sttStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st.mu.Lock()
	defer st.mu.Unlock()
	switch {
	case r.Method == "POST" && r.URL.Path == "/openai/v1/audio/transcriptions":
		st.groqHits++
		if st.groqFail != 0 {
			http.Error(w, `{"error":{"message":"stub failure"}}`, st.groqFail)
			return
		}
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "bad multipart", 400)
			return
		}
		f, fh, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "no file", 400)
			return
		}
		st.lastAudio, _ = io.ReadAll(f)
		st.lastForm = map[string]string{"filename": fh.Filename}
		for k, v := range r.MultipartForm.Value {
			st.lastForm[k] = v[0]
		}
		st.lastAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":" 你好世界 "}`))
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/client/v4/accounts/"):
		st.cfHits++
		st.cfPath = r.URL.Path
		var in struct {
			Audio    string `json:"audio"`
			Language string `json:"language"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			http.Error(w, "bad json", 400)
			return
		}
		st.lastAudio, _ = base64.StdEncoding.DecodeString(in.Audio)
		st.cfLang = in.Language
		st.lastAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"text":"来自 Cloudflare"},"success":true,"errors":[],"messages":[]}`))
	default:
		http.NotFound(w, r)
	}
}

// ---------- helpers ----------

type presenceOut struct {
	UserID   uint64  `json:"user_id"`
	Online   bool    `json:"online"`
	LastSeen *string `json:"last_seen"`
}

func dialAt(baseURL, token string) *websocket.Conn {
	u := strings.Replace(baseURL, "http", "ws", 1) + "/ws?token=" + token
	c, _, err := websocket.DefaultDialer.Dial(u, nil)
	must(err, "dial ws "+baseURL)
	return c
}

func callAt(baseURL, method, path, token string, body any, out any) int {
	saved := base
	base = baseURL
	defer func() { base = saved }()
	return call(method, path, token, body, out)
}

// seenBefore 等待 wait 后发 ping，读到对应 pong 为止，返回期间是否出现满足条件的帧。
// （gorilla 读超时后连接不可再用，所以“不应收到”的检查用 ping/pong 做哨兵而不是读超时。）
func seenBefore(c *websocket.Conn, typ string, wait time.Duration, pred func(json.RawMessage) bool) bool {
	time.Sleep(wait)
	id := fmt.Sprintf("sentinel-%d", time.Now().UnixNano())
	send(c, "ping", id, map[string]any{})
	deadline := time.Now().Add(4 * time.Second)
	seen := false
	for {
		_ = c.SetReadDeadline(deadline)
		var e env
		if err := c.ReadJSON(&e); err != nil {
			must(err, "sentinel pong")
		}
		if e.Type == typ && (pred == nil || pred(e.Data)) {
			seen = true
		}
		if e.Type == "pong" && e.ID == id {
			return seen
		}
	}
}

func presenceOf(uid uint64) func(json.RawMessage) bool {
	return has(fmt.Sprintf(`"user_id":%d,`, uid))
}

// fakePNG / fakeJPEG 只带文件头（服务端只校验头部与大小）。
func fakePNG(n int) []byte {
	b := make([]byte, n)
	copy(b, "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	return b
}

func fakeJPEG(n int) []byte {
	b := make([]byte, n)
	copy(b, "\xff\xd8\xff\xe0\x00\x10JFIF\x00")
	return b
}

type presignOut struct {
	Key       string            `json:"key"`
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt string            `json:"expires_at"`
	MaxBytes  int64             `json:"max_bytes"`
	Code      string            `json:"code"`
}

func presignImage(baseURL, token, mime string, size, w, h int) (presignOut, int) {
	var pre presignOut
	st := callAt(baseURL, "POST", "/api/uploads/presign", token, map[string]any{"kind": "image", "mime": mime, "size": size, "width": w, "height": h}, &pre)
	return pre, st
}

func put(pre presignOut, body []byte) int {
	req, _ := http.NewRequest("PUT", pre.URL, bytes.NewReader(body))
	for k, v := range pre.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	must(err, "PUT presigned")
	resp.Body.Close()
	return resp.StatusCode
}

func v030(suffix string, alice, bob, eve authResp) {
	fmt.Println("\n--- v0.3.0 ---")
	p1 := register("pres1", suffix)
	p2 := register("pres2", suffix)
	var dc struct {
		ConversationID uint64 `json:"conversation_id"`
	}
	call("POST", "/api/conversations/direct", p1.Token, map[string]uint64{"peer_id": p2.User.ID}, &dc)
	cid := dc.ConversationID

	// ---------- /api/config ----------
	var cfg map[string]any
	call("GET", "/api/config", p1.Token, nil, &cfg)
	check(cfg["presence_enabled"] == true && cfg["stt_enabled"] == false && cfg["image_enabled"] != nil && cfg["image_max_bytes"] != nil,
		"GET /api/config has presence_enabled/stt_enabled/image_enabled/image_max_bytes")
	if s3Endpoint != "" {
		check(cfg["image_enabled"] == true && cfg["image_max_bytes"] == float64(10485760), "config: image_enabled=true, image_max_bytes=10485760")
	}

	// ---------- 在线状态 ----------
	y := dial(p2.Token)
	outsider := dial(eve.Token)
	time.Sleep(200 * time.Millisecond)
	x1 := dial(p1.Token)
	e, ok := expectWhere(y, "presence", 3*time.Second, presenceOf(p1.User.ID))
	var pd presenceOut
	_ = json.Unmarshal(e.Data, &pd)
	check(ok && pd.Online && pd.LastSeen == nil && strings.Contains(string(e.Data), `"last_seen":null`), "presence online event to co-member (last_seen null)")
	x2 := dial(p1.Token)
	check(!seenBefore(y, "presence", 700*time.Millisecond, presenceOf(p1.User.ID)), "second device does not re-broadcast presence")
	check(!seenBefore(outsider, "presence", 0, presenceOf(p1.User.ID)), "non-co-member gets no presence events")
	outsider.Close()

	var plist []presenceOut
	st := call("GET", fmt.Sprintf("/api/presence?ids=%d,%d,%d,%d,999999999", p1.User.ID, p2.User.ID, eve.User.ID, p1.User.ID), p2.Token, nil, &plist)
	check(st == 200 && len(plist) == 2 && plist[0].UserID == p1.User.ID && plist[0].Online && plist[0].LastSeen == nil && plist[1].UserID == p2.User.ID,
		"GET /api/presence: only shared users, deduped, online")
	ids := make([]string, 201)
	for i := range ids {
		ids[i] = fmt.Sprint(i + 1)
	}
	check(call("GET", "/api/presence?ids="+strings.Join(ids, ","), p2.Token, nil, nil) == 400, "GET /api/presence rejects > 200 ids")
	check(call("GET", "/api/presence?ids=", p2.Token, nil, &plist) == 200 && len(plist) == 0, "GET /api/presence with no ids → []")

	x2.Close()
	check(!seenBefore(y, "presence", 700*time.Millisecond, presenceOf(p1.User.ID)), "closing one of two devices keeps user online (no event)")
	x1.Close()
	e, ok = expectWhere(y, "presence", 3*time.Second, presenceOf(p1.User.ID))
	pd = presenceOut{}
	_ = json.Unmarshal(e.Data, &pd)
	okTime := false
	if pd.LastSeen != nil {
		t, err := time.Parse(time.RFC3339, *pd.LastSeen)
		okTime = err == nil && time.Since(t) < time.Minute
	}
	check(ok && !pd.Online && okTime, "presence offline event with RFC3339 last_seen")
	call("GET", fmt.Sprintf("/api/presence?ids=%d", p1.User.ID), p2.Token, nil, &plist)
	check(len(plist) == 1 && !plist[0].Online && plist[0].LastSeen != nil, "GET /api/presence: offline with last_seen persisted")

	if sttBase != "" { // 跨实例：p1 连到第二个实例，p2 在第一个实例收到事件
		xr := dialAt(sttBase, p1.Token)
		_, ok = expectWhere(y, "presence", 3*time.Second, func(d json.RawMessage) bool { return presenceOf(p1.User.ID)(d) && has(`"online":true`)(d) })
		check(ok, "presence event crosses instances (via Redis)")
		var pl2 []presenceOut
		call("GET", fmt.Sprintf("/api/presence?ids=%d", p1.User.ID), p2.Token, nil, &pl2)
		check(len(pl2) == 1 && pl2[0].Online, "GET /api/presence sees connection on other instance")
		xr.Close()
		_, ok = expectWhere(y, "presence", 3*time.Second, func(d json.RawMessage) bool { return presenceOf(p1.User.ID)(d) && has(`"online":false`)(d) })
		check(ok, "offline event crosses instances")
	}

	// ---------- 语音转文字：未启用 ----------
	var er map[string]string
	check(call("POST", fmt.Sprintf("/api/conversations/%d/messages/1/transcribe", cid), p1.Token, nil, &er) == 501 && er["code"] == "stt_disabled",
		"transcribe returns 501 stt_disabled when disabled")

	if s3Endpoint == "" {
		fmt.Println("SKIP image / stt checks (no -s3-endpoint)")
		return
	}
	v030Image(alice, bob, eve)
	if sttBase == "" {
		fmt.Println("SKIP stt provider checks (no -stt-base)")
		return
	}
	v030STT(suffix, eve)
}

func v030Image(alice, bob, eve authResp) {
	var dc struct {
		ConversationID uint64 `json:"conversation_id"`
	}
	call("POST", "/api/conversations/direct", alice.Token, map[string]uint64{"peer_id": bob.User.ID}, &dc)
	cid := dc.ConversationID
	a, b := dial(alice.Token), dial(bob.Token)
	time.Sleep(200 * time.Millisecond)

	img := fakePNG(50_000)
	pre, st := presignImage(base, alice.Token, "image/png", len(img), 1024, 768)
	check(st == 200 && strings.HasPrefix(pre.Key, fmt.Sprintf("image/%d/", alice.User.ID)) && strings.HasSuffix(pre.Key, ".png") &&
		pre.Method == "PUT" && pre.Headers["Content-Type"] == "image/png" && pre.MaxBytes == 10485760 && pre.ExpiresAt != "",
		"presign image: key image/{uid}/…png, same shape as voice")
	check(put(pre, img) == 200, "PUT image to storage")

	e1, st := presignImage(base, alice.Token, "image/bmp", 1000, 10, 10)
	check(st == 400 && e1.Code == "bad_mime", "presign rejects non-whitelisted image mime (bad_mime)")
	e1, st = presignImage(base, alice.Token, "image/jpeg", 10485761, 1, 1)
	check(st == 400 && e1.Code == "too_large", "presign rejects image > image_max_bytes (too_large)")
	pre2, st := presignImage(base, alice.Token, "image/jpeg", 3000, 10, 10)
	check(st == 200 && strings.HasSuffix(pre2.Key, ".jpg"), "presign jpeg → .jpg key")

	content, _ := json.Marshal(map[string]any{"key": pre.Key, "mime": "image/png", "size": len(img), "width": 1024, "height": 768})
	im, _, ok := sendMsg(a, cid, "img-1", map[string]any{"msg_type": 2, "content": string(content)})
	var ic struct {
		Key    string `json:"key"`
		Mime   string `json:"mime"`
		Size   int64  `json:"size"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	}
	_ = json.Unmarshal([]byte(im.Content), &ic)
	check(ok && im.MsgType == 2 && ic.Key == pre.Key && ic.Mime == "image/png" && ic.Size == int64(len(img)) && ic.Width == 1024 && ic.Height == 768,
		"image message accepted after verification (content key/mime/size/width/height)")
	_, ok = expectWhere(b, "message", 3*time.Second, has(`"msg_type":2`))
	check(ok, "image message delivered")
	r1, _, ok := sendMsg(b, cid, "img-reply", map[string]any{"content": "好看", "reply_to_seq": im.Seq})
	check(ok && r1.ReplyTo != nil && r1.ReplyTo.Preview == "[图片]", "reply to image shows [图片] preview")

	var media struct {
		URL       string `json:"url"`
		Mime      string `json:"mime"`
		Size      int64  `json:"size"`
		ExpiresAt string `json:"expires_at"`
	}
	check(call("GET", fmt.Sprintf("/api/conversations/%d/messages/%d/media", cid, im.Seq), bob.Token, nil, &media) == 200 &&
		media.URL != "" && media.Mime == "image/png" && media.Size == int64(len(img)) && media.ExpiresAt != "", "image media url {url,mime,size,expires_at}")
	resp, err := http.Get(media.URL)
	must(err, "GET image")
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	check(resp.StatusCode == 200 && bytes.Equal(got, img), "presigned GET downloads identical image bytes")
	check(call("GET", fmt.Sprintf("/api/conversations/%d/messages/%d/media", cid, im.Seq), eve.Token, nil, nil) == 403, "non-member cannot get image url")

	// 负面用例
	_, er, ok := sendMsg(b, cid, "img-steal", map[string]any{"msg_type": 2, "content": string(content)})
	check(!ok && strings.Contains(string(er.Data), "无效的图片文件"), "cannot send someone else's image upload")
	vkey, _ := uploadVoice(alice.Token, fakeWebm(2048), "audio/webm", 1000)
	vc, _ := json.Marshal(map[string]any{"key": vkey, "mime": "image/png", "size": 2048, "width": 10, "height": 10})
	_, er, ok = sendMsg(a, cid, "img-voicekey", map[string]any{"msg_type": 2, "content": string(vc)})
	check(!ok && strings.Contains(string(er.Data), "无效的图片文件"), "voice/ key rejected for image message")
	jpg := fakeJPEG(3000)
	put(pre2, jpg)
	mism, _ := json.Marshal(map[string]any{"key": pre2.Key, "mime": "image/png", "size": 3000, "width": 10, "height": 10})
	_, er, ok = sendMsg(a, cid, "img-mismatch", map[string]any{"msg_type": 2, "content": string(mism)})
	check(!ok && strings.Contains(string(er.Data), "图片"), "image whose bytes don't match declared mime rejected")
	pre3, _ := presignImage(base, alice.Token, "image/gif", 100, 5, 5)
	put(pre3, []byte(strings.Repeat("not an image", 10)))
	junk, _ := json.Marshal(map[string]any{"key": pre3.Key, "mime": "image/gif", "size": 120, "width": 5, "height": 5})
	_, er, ok = sendMsg(a, cid, "img-junk", map[string]any{"msg_type": 2, "content": string(junk)})
	check(!ok && strings.Contains(string(er.Data), "不是有效的图片"), "non-image bytes rejected")
	pre4, _ := presignImage(base, alice.Token, "image/png", 1000, 0, 0)
	put(pre4, fakePNG(1000))
	nowh, _ := json.Marshal(map[string]any{"key": pre4.Key, "mime": "image/png", "size": 1000})
	_, _, ok = sendMsg(a, cid, "img-nowh", map[string]any{"msg_type": 2, "content": string(nowh)})
	check(!ok, "image without width/height rejected")

	// 撤回
	_, ok = request(a, "recall", "rc-img", map[string]any{"conversation_id": cid, "seq": im.Seq})
	_, ok2 := expectWhere(b, "recall", 3*time.Second, has(fmt.Sprintf(`"seq":%d`, im.Seq)))
	time.Sleep(500 * time.Millisecond)
	check(ok && ok2 && call("GET", fmt.Sprintf("/api/conversations/%d/messages/%d/media", cid, im.Seq), bob.Token, nil, nil) == 410, "recalled image: recall event + media 410")
	mc, err := minio.New(s3Endpoint, &minio.Options{Creds: credentials.NewStaticV4(s3Key, s3Secret, ""), Secure: s3SSL, Region: s3Region})
	must(err, "minio client")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = mc.StatObject(ctx, s3Bucket, pre.Key, minio.StatObjectOptions{})
	check(err != nil, "recalled image object deleted from storage")
	lc, err := mc.GetBucketLifecycle(ctx, s3Bucket)
	imgRule, voiceRule := false, false
	if err == nil {
		for _, r := range lc.Rules {
			if r.RuleFilter.Prefix == "image/" {
				imgRule = true
			}
			if r.RuleFilter.Prefix == "voice/" && int(r.Expiration.Days) == 7 {
				voiceRule = true
			}
		}
	}
	check(voiceRule && !imgRule, "image_ttl_days=0: no image/ lifecycle rule, voice rule kept")
	a.Close()
	b.Close()
}

func v030STT(suffix string, eve authResp) {
	stub := &sttStub{}
	ln, err := net.Listen("tcp", sttStubAddr)
	must(err, "listen stt stub")
	srv := &http.Server{Handler: stub}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	s1 := register("stt1", suffix)
	s2 := register("stt2", suffix)
	var dc struct {
		ConversationID uint64 `json:"conversation_id"`
	}
	callAt(sttBase, "POST", "/api/conversations/direct", s1.Token, map[string]uint64{"peer_id": s2.User.ID}, &dc)
	cid := dc.ConversationID
	var cfg map[string]any
	callAt(sttBase, "GET", "/api/config", s1.Token, nil, &cfg)
	check(cfg["stt_enabled"] == true, "config: stt_enabled=true on STT instance")

	c := dialAt(sttBase, s1.Token)
	time.Sleep(200 * time.Millisecond)
	sendVoice := func(id string, body []byte) msgOut {
		saved := base
		base = sttBase
		key, st := uploadVoice(s1.Token, body, "audio/webm", 2000)
		base = saved
		must(map[bool]error{true: nil, false: fmt.Errorf("upload status %d", st)}[st == 200], "upload voice")
		ct, _ := json.Marshal(map[string]any{"key": key, "mime": "audio/webm", "duration_ms": 2000, "size": len(body)})
		m, er, ok := sendMsg(c, cid, id, map[string]any{"msg_type": 3, "content": string(ct)})
		if !ok {
			fmt.Println("send voice error:", string(er.Data))
		}
		return m
	}
	tr := func(token string, seq uint64) (int, map[string]any) {
		var out map[string]any
		st := callAt(sttBase, "POST", fmt.Sprintf("/api/conversations/%d/messages/%d/transcribe", cid, seq), token, nil, &out)
		return st, out
	}

	audio1 := fakeWebm(3000)
	audio1[100] = 7
	v1 := sendVoice("stt-v1", audio1)
	st, out := tr(s1.Token, v1.Seq)
	stub.mu.Lock()
	form, gotAudio, auth := stub.lastForm, stub.lastAudio, stub.lastAuth
	stub.mu.Unlock()
	check(st == 200 && out["text"] == "你好世界" && out["cached"] == false, `transcribe → {"text":"你好世界","cached":false} (groq)`)
	check(bytes.Equal(gotAudio, audio1) && form["model"] == "whisper-large-v3-turbo" && form["language"] == "zh" &&
		form["response_format"] == "json" && form["filename"] == "audio.webm" && auth == "Bearer test-groq-key",
		"groq request: multipart file/model/language/response_format + bearer")
	st, out = tr(s2.Token, v1.Seq)
	check(st == 200 && out["text"] == "你好世界" && out["cached"] == true && stub.groqHits == 1, "second transcribe (other member) is cached (no provider call)")
	check(callAt(sttBase, "POST", fmt.Sprintf("/api/conversations/%d/messages/%d/transcribe", cid, v1.Seq), eve.Token, nil, nil) == 403, "non-member transcribe → 403")
	tm, _, _ := sendMsg(c, cid, "stt-text", map[string]any{"content": "文字"})
	st, _ = tr(s1.Token, tm.Seq)
	check(st == 404, "transcribe non-voice message → 404")
	st, _ = tr(s1.Token, 99999)
	check(st == 404, "transcribe missing message → 404")

	audio2 := fakeWebm(2500)
	audio2[200] = 9
	v2 := sendVoice("stt-v2", audio2)
	stub.mu.Lock()
	stub.groqFail = 400
	stub.mu.Unlock()
	st, out = tr(s1.Token, v2.Seq)
	check(st == 502 && out["code"] == "stt_failed" && stub.cfHits == 0, "non-retryable provider error (400) → 502 stt_failed, no fallback")
	stub.mu.Lock()
	stub.groqFail = 503
	stub.mu.Unlock()
	st, out = tr(s1.Token, v2.Seq)
	stub.mu.Lock()
	cfAudio, cfLang, cfPath, cfAuth := stub.lastAudio, stub.cfLang, stub.cfPath, stub.lastAuth
	stub.mu.Unlock()
	check(st == 200 && out["text"] == "来自 Cloudflare" && out["cached"] == false && stub.cfHits == 1, "groq 503 → falls back to cloudflare")
	check(bytes.Equal(cfAudio, audio2) && cfLang == "zh" && cfPath == "/client/v4/accounts/test-account/ai/run/@cf/openai/whisper-large-v3-turbo" && cfAuth == "Bearer test-cf-token",
		"cloudflare request: base64 audio + language + model path + bearer")

	// 限额（STT 实例配置 daily_limit_per_user: 2；失败的那次已退回）
	v3 := sendVoice("stt-v3", fakeWebm(1500))
	stub.mu.Lock()
	stub.groqFail = 0
	stub.mu.Unlock()
	st, out = tr(s1.Token, v3.Seq)
	check(st == 429 && out["code"] == "stt_limit", "daily limit exceeded → 429 stt_limit")
	st, out = tr(s1.Token, v1.Seq)
	check(st == 200 && out["cached"] == true, "cached result still served after limit reached")
	st, _ = tr(s2.Token, v3.Seq)
	check(st == 200, "limit is per user (other member can transcribe)")

	// 撤回后 410，transcript 删除
	_, ok := request(c, "recall", "stt-rc", map[string]any{"conversation_id": cid, "seq": v1.Seq})
	st, out = tr(s2.Token, v1.Seq)
	check(ok && st == 410 && out["code"] == "recalled", "transcribe recalled voice → 410 recalled")
	c.Close()
}
