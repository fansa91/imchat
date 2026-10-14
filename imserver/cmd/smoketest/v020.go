package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var (
	s3Endpoint, s3Key, s3Secret, s3Bucket string
	s3Region, webOrigin                   string
	s3SSL                                 bool
)

// expectWhere 读取帧直到出现满足条件的指定类型帧。
func expectWhere(c *websocket.Conn, typ string, timeout time.Duration, pred func(json.RawMessage) bool) (env, bool) {
	deadline := time.Now().Add(timeout)
	for {
		_ = c.SetReadDeadline(deadline)
		var e env
		if err := c.ReadJSON(&e); err != nil {
			return env{}, false
		}
		if e.Type == typ && (pred == nil || pred(e.Data)) {
			return e, true
		}
	}
}

func has(sub string) func(json.RawMessage) bool {
	return func(d json.RawMessage) bool { return strings.Contains(string(d), sub) }
}

type msgOut struct {
	ConversationID uint64 `json:"conversation_id"`
	Seq            uint64 `json:"seq"`
	SenderID       uint64 `json:"sender_id"`
	MsgType        int    `json:"msg_type"`
	Content        string `json:"content"`
	Recalled       bool   `json:"recalled"`
	ReplyTo        *struct {
		Seq      uint64 `json:"seq"`
		Preview  string `json:"preview"`
		Recalled bool   `json:"recalled"`
	} `json:"reply_to"`
	Reactions []struct {
		Emoji   string   `json:"emoji"`
		Count   int      `json:"count"`
		UserIDs []uint64 `json:"user_ids"`
	} `json:"reactions"`
}

func msgOf(e env) msgOut {
	var w struct {
		Message msgOut `json:"message"`
	}
	_ = json.Unmarshal(e.Data, &w)
	return w.Message
}

func history(token string, cid uint64, q string) []msgOut {
	var out []msgOut
	call("GET", fmt.Sprintf("/api/conversations/%d/messages%s", cid, q), token, nil, &out)
	return out
}

func findSeq(ms []msgOut, seq uint64) *msgOut {
	for i := range ms {
		if ms[i].Seq == seq {
			return &ms[i]
		}
	}
	return nil
}

// sendMsg 发送并等待 send_ack，返回消息。
func sendMsg(c *websocket.Conn, cid uint64, cmid string, data map[string]any) (msgOut, env, bool) {
	data["client_msg_id"] = cmid
	data["conversation_id"] = cid
	send(c, "send", cmid, data)
	deadline := time.Now().Add(4 * time.Second)
	for {
		_ = c.SetReadDeadline(deadline)
		var e env
		if err := c.ReadJSON(&e); err != nil {
			return msgOut{}, env{}, false
		}
		if e.ID == cmid && (e.Type == "send_ack" || e.Type == "error") {
			return msgOf(e), e, e.Type == "send_ack"
		}
	}
}

// request 发送 recall/react/delete，等待带同一 id 的 ack 或 error。
func request(c *websocket.Conn, typ, id string, data any) (env, bool) {
	send(c, typ, id, data)
	deadline := time.Now().Add(4 * time.Second)
	for {
		_ = c.SetReadDeadline(deadline)
		var e env
		if err := c.ReadJSON(&e); err != nil {
			return env{}, false
		}
		if e.ID == id && (e.Type == "ack" || e.Type == "error") {
			return e, e.Type == "ack"
		}
	}
}

func register(name, suffix string) authResp {
	var a authResp
	call("POST", "/api/register", "", map[string]string{"email": name + suffix + "@test.com", "password": "password123", "nickname": name}, &a)
	return a
}

// fakeWebm 构造一个带 EBML 头的假 webm（只用于校验文件头，不是能播放的音频）。
func fakeWebm(n int) []byte {
	b := make([]byte, n)
	copy(b, []byte{0x1a, 0x45, 0xdf, 0xa3, 0x9f, 0x42, 0x86, 0x81, 0x01})
	return b
}

func uploadVoice(token string, body []byte, mime string, durMs int) (key string, status int) {
	var pre struct {
		Key     string            `json:"key"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	st := call("POST", "/api/uploads/presign", token, map[string]any{"kind": "voice", "mime": mime, "size": len(body), "duration_ms": durMs}, &pre)
	if st != 200 {
		return "", st
	}
	req, _ := http.NewRequest("PUT", pre.URL, bytes.NewReader(body))
	for k, v := range pre.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	must(err, "PUT presigned")
	resp.Body.Close()
	return pre.Key, resp.StatusCode
}

func v020(suffix string, alice, bob, eve authResp) {
	fmt.Println("\n--- v0.2.0 ---")
	carol := register("carol", suffix)
	dave := register("dave", suffix)

	var cfg map[string]any
	check(call("GET", "/api/config", alice.Token, nil, &cfg) == 200 && cfg["recall_seconds"] != nil, "GET /api/config")

	a, b, b2, c := dial(alice.Token), dial(bob.Token), dial(bob.Token), dial(carol.Token)
	time.Sleep(200 * time.Millisecond)

	// ---------- 群聊 ----------
	var created struct {
		ConversationID uint64 `json:"conversation_id"`
		Conversation   struct {
			Type        int    `json:"type"`
			Name        string `json:"name"`
			MemberCount int    `json:"member_count"`
			MyRole      int    `json:"my_role"`
		} `json:"conversation"`
	}
	st := call("POST", "/api/conversations/group", alice.Token, map[string]any{"name": "周末爬山 ⛰️", "member_ids": []uint64{bob.User.ID, carol.User.ID}}, &created)
	g := created.ConversationID
	check(st == 201 && g > 0 && created.Conversation.Type == 2 && created.Conversation.MemberCount == 3 && created.Conversation.MyRole == 2, "create group (3 members, owner role)")
	e, ok := expectWhere(c, "message", 3*time.Second, has(`"msg_type":10`))
	check(ok && strings.Contains(string(e.Data), "创建了群聊"), "member receives system message for group creation")

	var convs []struct {
		ID             uint64 `json:"id"`
		Type           int    `json:"type"`
		Name           string `json:"name"`
		MembersPreview []any  `json:"members_preview"`
	}
	call("GET", "/api/conversations", carol.Token, nil, &convs)
	check(len(convs) == 1 && convs[0].Type == 2 && convs[0].Name == "周末爬山 ⛰️" && len(convs[0].MembersPreview) == 3, "conversation list shows group name + member preview")

	check(call("GET", fmt.Sprintf("/api/conversations/%d/messages", g), eve.Token, nil, nil) == 403, "non-member cannot read group history")
	ev := dial(eve.Token)
	_, er, _ := sendMsg(ev, g, "eve-x", map[string]any{"content": "hi"})
	check(strings.Contains(string(er.Data), "forbidden"), "non-member cannot send to group")

	m1, _, ok := sendMsg(a, g, "g-1", map[string]any{"content": "大家好"})
	check(ok && m1.Seq == 2, "owner sends group message")
	_, okb := expectWhere(b, "message", 3*time.Second, has("大家好"))
	_, okc := expectWhere(c, "message", 3*time.Second, has("大家好"))
	check(okb && okc, "group message fans out to all members")

	send(b, "read", "", map[string]any{"conversation_id": g, "seq": m1.Seq})
	send(c, "read", "", map[string]any{"conversation_id": g, "seq": m1.Seq})
	_, ok = expectWhere(a, "read", 3*time.Second, has(fmt.Sprintf(`"user_id":%d`, carol.User.ID)))
	var reads map[string]uint64
	call("GET", fmt.Sprintf("/api/conversations/%d/reads", g), alice.Token, nil, &reads)
	n := 0
	for uid, rs := range reads {
		if uid != fmt.Sprint(alice.User.ID) && rs >= m1.Seq {
			n++
		}
	}
	check(ok && n == 2, "group read count: 2人已读")

	check(call("PATCH", fmt.Sprintf("/api/conversations/%d", g), bob.Token, map[string]any{"name": "爬山小分队"}, nil) == 200, "member renames group")
	_, ok = expectWhere(c, "message", 3*time.Second, has(`\"op\":\"rename\"`))
	check(ok, "rename system message delivered")
	check(call("PATCH", fmt.Sprintf("/api/conversations/%d", g), bob.Token, map[string]any{"announcement": "周六 8 点集合"}, nil) == 403, "member cannot set announcement")
	var conv struct {
		Name         string `json:"name"`
		Announcement string `json:"announcement"`
	}
	check(call("PATCH", fmt.Sprintf("/api/conversations/%d", g), alice.Token, map[string]any{"announcement": "周六 8 点集合"}, &conv) == 200 &&
		conv.Announcement == "周六 8 点集合" && conv.Name == "爬山小分队", "owner sets announcement")

	var added struct {
		Added []uint64 `json:"added"`
	}
	check(call("POST", fmt.Sprintf("/api/conversations/%d/members", g), bob.Token, map[string]any{"user_ids": []uint64{dave.User.ID, alice.User.ID}}, &added) == 200 &&
		len(added.Added) == 1, "member adds dave (existing member ignored)")
	dh := history(dave.Token, g, "")
	check(len(dh) == 1 && dh[0].MsgType == 10 && strings.Contains(dh[0].Content, "加入了群聊"), "new member only sees messages after joining")

	check(call("DELETE", fmt.Sprintf("/api/conversations/%d/members/%d", g, carol.User.ID), bob.Token, nil, nil) == 403, "plain member cannot remove others")
	check(call("PUT", fmt.Sprintf("/api/conversations/%d/members/%d/role", g, bob.User.ID), alice.Token, map[string]string{"role": "admin"}, nil) == 200, "owner makes bob admin")
	d := dial(dave.Token)
	time.Sleep(150 * time.Millisecond)
	check(call("DELETE", fmt.Sprintf("/api/conversations/%d/members/%d", g, dave.User.ID), bob.Token, nil, nil) == 200, "admin removes dave")
	_, ok = expectWhere(d, "conversation_removed", 3*time.Second, nil)
	check(ok && call("GET", fmt.Sprintf("/api/conversations/%d/messages", g), dave.Token, nil, nil) == 403, "removed member notified and loses access")
	check(call("DELETE", fmt.Sprintf("/api/conversations/%d/members/%d", g, alice.User.ID), bob.Token, nil, nil) == 403, "admin cannot remove owner")

	var members []struct {
		User struct {
			ID uint64 `json:"id"`
		} `json:"user"`
		Role int `json:"role"`
	}
	call("GET", fmt.Sprintf("/api/conversations/%d/members", g), alice.Token, nil, &members)
	check(len(members) == 3 && members[0].Role == 2 && members[1].Role == 1, "member list with roles (owner, admin first)")

	// ---------- 引用回复 ----------
	r1, _, ok := sendMsg(b, g, "g-reply-1", map[string]any{"content": "收到 👌", "reply_to_seq": m1.Seq})
	check(ok && r1.ReplyTo != nil && r1.ReplyTo.Seq == m1.Seq && r1.ReplyTo.Preview == "大家好", "reply carries quoted preview")
	e, ok = expectWhere(a, "message", 3*time.Second, has("收到 👌"))
	check(ok && msgOf(e).ReplyTo != nil, "reply delivered with reply_to")
	_, er, ok = sendMsg(b, g, "g-reply-bad", map[string]any{"content": "x", "reply_to_seq": 99999})
	check(!ok && strings.Contains(string(er.Data), "引用"), "reply to nonexistent message rejected")

	// ---------- 表情回应 ----------
	_, ok = request(b, "react", "re-1", map[string]any{"conversation_id": g, "seq": m1.Seq, "emoji": "❤️"})
	e, ok2 := expectWhere(a, "reaction", 3*time.Second, has(`"on":true`))
	check(ok && ok2 && strings.Contains(string(e.Data), `"count":1`), "reaction added and live-synced")
	request(c, "react", "re-2", map[string]any{"conversation_id": g, "seq": m1.Seq, "emoji": "❤️"})
	request(c, "react", "re-3", map[string]any{"conversation_id": g, "seq": m1.Seq, "emoji": "😂"})
	hm := findSeq(history(alice.Token, g, ""), m1.Seq)
	check(hm != nil && len(hm.Reactions) == 2 && hm.Reactions[0].Emoji == "❤️" && hm.Reactions[0].Count == 2, "reactions aggregated in history")
	request(b, "react", "re-4", map[string]any{"conversation_id": g, "seq": m1.Seq, "emoji": "❤️"}) // 切换 = 取消
	e, ok = expectWhere(a, "reaction", 3*time.Second, has(`"on":false`))
	check(ok && strings.Contains(string(e.Data), `"count":1`), "reaction toggled off")
	_, ok = request(b, "react", "re-5", map[string]any{"conversation_id": g, "seq": m1.Seq, "emoji": "abc"})
	check(!ok, "invalid emoji rejected")

	// ---------- 撤回 ----------
	m2, _, _ := sendMsg(a, g, "g-recall", map[string]any{"content": "发错了"})
	expectWhere(b, "message", 3*time.Second, has("发错了"))
	_, ok = request(b, "recall", "rc-0", map[string]any{"conversation_id": g, "seq": m2.Seq})
	check(!ok, "cannot recall someone else's message")
	var modBefore struct {
		ModSeq uint64 `json:"mod_seq"`
	}
	call("GET", fmt.Sprintf("/api/conversations/%d", g), bob.Token, nil, &modBefore)
	_, ok = request(a, "recall", "rc-1", map[string]any{"conversation_id": g, "seq": m2.Seq})
	e, ok2 = expectWhere(b, "recall", 3*time.Second, has(fmt.Sprintf(`"seq":%d`, m2.Seq)))
	check(ok && ok2, "sender recalls message; members get recall event")
	_, ok = expectWhere(b2, "recall", 3*time.Second, nil)
	check(ok, "recall synced to member's other device")
	hm = findSeq(history(carol.Token, g, ""), m2.Seq)
	check(hm != nil && hm.Recalled && hm.Content == "", "recalled message is a tombstone in history")
	ch := history(bob.Token, g, fmt.Sprintf("?changed_since=%d", modBefore.ModSeq))
	check(findSeq(ch, m2.Seq) != nil && findSeq(ch, m2.Seq).Recalled, "offline sync of recall via changed_since")
	_, ok = request(a, "recall", "rc-2", map[string]any{"conversation_id": g, "seq": m2.Seq})
	check(!ok, "double recall rejected")
	r2, _, _ := sendMsg(b, g, "g-reply-2", map[string]any{"content": "引用已撤回", "reply_to_seq": m2.Seq})
	check(r2.ReplyTo != nil && r2.ReplyTo.Recalled && r2.ReplyTo.Preview == "", "reply to recalled message shows recalled quote")

	// ---------- 仅自己删除 ----------
	_, ok = request(b, "delete", "del-1", map[string]any{"conversation_id": g, "seqs": []uint64{m1.Seq}})
	e, ok2 = expectWhere(b2, "deleted", 3*time.Second, nil)
	check(ok && ok2 && strings.Contains(string(e.Data), fmt.Sprint(m1.Seq)), "delete-for-me synced to own devices")
	check(findSeq(history(bob.Token, g, ""), m1.Seq) == nil, "deleted message hidden from own history")
	check(findSeq(history(alice.Token, g, ""), m1.Seq) != nil, "deleted message still visible to others")
	var hidden []uint64
	call("GET", fmt.Sprintf("/api/conversations/%d/hidden", g), bob.Token, nil, &hidden)
	check(len(hidden) == 1 && hidden[0] == m1.Seq, "hidden seqs endpoint")

	// ---------- 退群 ----------
	check(call("POST", fmt.Sprintf("/api/conversations/%d/leave", g), carol.Token, nil, nil) == 200, "member leaves group")
	_, ok = expectWhere(a, "message", 3*time.Second, has(`\"op\":\"leave\"`))
	check(ok && call("GET", fmt.Sprintf("/api/conversations/%d/messages", g), carol.Token, nil, nil) == 403, "leave system message; left member loses access")

	// ---------- 语音 ----------
	if s3Endpoint == "" {
		fmt.Println("SKIP voice checks (no -s3-endpoint)")
		return
	}
	body := fakeWebm(4096)
	key, st := uploadVoice(alice.Token, body, "audio/webm;codecs=opus", 3200)
	check(st == 200 && strings.HasPrefix(key, fmt.Sprintf("voice/%d/", alice.User.ID)), "presign + PUT voice to S3 storage")
	content, _ := json.Marshal(map[string]any{"key": key, "mime": "audio/webm", "duration_ms": 3200, "size": len(body)})
	vm, _, ok := sendMsg(a, g, "g-voice-1", map[string]any{"msg_type": 3, "content": string(content)})
	check(ok && vm.MsgType == 3 && strings.Contains(vm.Content, `"duration_ms":3200`), "voice message accepted after server verification")
	_, ok = expectWhere(b, "message", 3*time.Second, has(`"msg_type":3`))
	check(ok, "voice message delivered")
	var media struct {
		URL  string `json:"url"`
		Mime string `json:"mime"`
	}
	check(call("GET", fmt.Sprintf("/api/conversations/%d/messages/%d/media", g, vm.Seq), bob.Token, nil, &media) == 200 && media.URL != "", "member gets presigned GET url")
	resp, err := http.Get(media.URL)
	must(err, "GET presigned")
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	check(resp.StatusCode == 200 && bytes.Equal(got, body), "presigned GET downloads identical bytes")
	check(call("GET", fmt.Sprintf("/api/conversations/%d/messages/%d/media", g, vm.Seq), eve.Token, nil, nil) == 403, "non-member cannot get media url")

	_, st = uploadVoice(alice.Token, make([]byte, 3<<20), "audio/webm", 1000)
	check(st == 400, "presign rejects oversize voice")
	_, st = uploadVoice(alice.Token, body, "image/png", 1000)
	check(st == 400, "presign rejects non-audio mime")
	// 用 bob 发 alice 上传的 key
	_, er, ok = sendMsg(b, g, "g-voice-steal", map[string]any{"msg_type": 3, "content": string(content)})
	check(!ok && strings.Contains(string(er.Data), "无效的语音文件"), "cannot send someone else's upload")
	// 声明 webm 实际不是音频
	junkKey, _ := uploadVoice(alice.Token, []byte(strings.Repeat("not audio", 100)), "audio/webm", 1000)
	junk, _ := json.Marshal(map[string]any{"key": junkKey, "mime": "audio/webm", "duration_ms": 1000, "size": 900})
	_, er, ok = sendMsg(a, g, "g-voice-junk", map[string]any{"msg_type": 3, "content": string(junk)})
	check(!ok && strings.Contains(string(er.Data), "音频"), "server rejects upload whose bytes are not audio")
	_, er, ok = sendMsg(a, g, "g-voice-long", map[string]any{"msg_type": 3, "content": strings.Replace(string(content), "3200", "600000", 1)})
	check(!ok, "voice longer than limit rejected")

	_, ok = request(a, "recall", "rc-v", map[string]any{"conversation_id": g, "seq": vm.Seq})
	time.Sleep(500 * time.Millisecond)
	check(ok && call("GET", fmt.Sprintf("/api/conversations/%d/messages/%d/media", g, vm.Seq), bob.Token, nil, nil) == 410, "recalled voice: media endpoint returns 410")

	mc, err := minio.New(s3Endpoint, &minio.Options{Creds: credentials.NewStaticV4(s3Key, s3Secret, ""), Secure: s3SSL, Region: s3Region})
	must(err, "minio client")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = mc.StatObject(ctx, s3Bucket, key, minio.StatObjectOptions{})
	check(err != nil, "recalled voice object deleted from storage")
	lc, err := mc.GetBucketLifecycle(ctx, s3Bucket)
	okLC := false
	if err == nil {
		for _, r := range lc.Rules {
			if r.Status == "Enabled" && r.RuleFilter.Prefix == "voice/" && int(r.Expiration.Days) == 7 {
				okLC = true
			}
		}
	}
	check(okLC, "bucket lifecycle rule: voice/ expires after 7 days")
	cc, err := mc.GetBucketCors(ctx, s3Bucket)
	check(err == nil && cc != nil && len(cc.CORSRules) > 0, "bucket CORS configured")
	// 浏览器预检：跨域 PUT
	scheme := "http"
	if s3SSL {
		scheme = "https"
	}
	pre, _ := http.NewRequest("OPTIONS", fmt.Sprintf("%s://%s/%s/%s", scheme, s3Endpoint, s3Bucket, key), nil)
	pre.Header.Set("Origin", webOrigin)
	pre.Header.Set("Access-Control-Request-Method", "PUT")
	pre.Header.Set("Access-Control-Request-Headers", "content-type")
	presp, err := http.DefaultClient.Do(pre)
	must(err, "CORS preflight")
	presp.Body.Close()
	ao := presp.Header.Get("Access-Control-Allow-Origin")
	check(presp.StatusCode < 300 && (ao == webOrigin || ao == "*"), "CORS preflight for PUT from web origin allowed")
}
