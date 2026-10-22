// smoketest 端到端冒烟测试。
// v0.1：注册、单聊收发、多设备同步、幂等、已读、离线补拉。
// v0.2：群聊、撤回、仅自己删除、引用回复、表情回应、语音上传（预签名）、生命周期规则。
// v0.3：图片消息、在线状态（含跨实例）、语音转文字（假提供方）。
// v0.4：文件/视频消息、头像昵称、重置/修改密码（假 SMTP）、1 对 1 通话信令（假 Cloudflare）。
// v0.5：好友系统（搜索/申请/备注/删除）、allow_find、退出其他设备、多人通话（假 SFU，借 -v040-base 实例，
// 需另开 calls.group_enabled 并把 group_max_participants 配 2、sfu_app_id/sfu_app_secret 任意非空）。
//
//	-v040-base 指向开启 calls+smtp 的实例（calls.base_url → -calls-stub、smtp → -smtp-stub、tls_mode: none，
//	ring_timeout_seconds 请配小如 3 以便测响铃超时），留空跳过通话/密码检查。
//
// 用法: go run ./cmd/smoketest -base http://127.0.0.1:8092 -s3-endpoint 127.0.0.1:3900 -s3-key GK... -s3-secret ... -bucket imchat
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var base string

func must(err error, what string) {
	if err != nil {
		fmt.Printf("FAIL %s: %v\n", what, err)
		os.Exit(1)
	}
}

func check(ok bool, what string) {
	if !ok {
		fmt.Println("FAIL", what)
		os.Exit(1)
	}
	passed++
	fmt.Println("PASS", what)
}

var passed int

func call(method, path, token string, body any, out any) int {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, base+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	must(err, method+" "+path)
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

type authResp struct {
	Token string `json:"token"`
	User  struct {
		ID uint64 `json:"id"`
	} `json:"user"`
}

type env struct {
	Type string          `json:"type"`
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data"`
}

func dial(token string) *websocket.Conn {
	u := strings.Replace(base, "http", "ws", 1) + "/ws?token=" + token
	c, _, err := websocket.DefaultDialer.Dial(u, nil)
	must(err, "dial ws")
	return c
}

func send(c *websocket.Conn, typ, id string, data any) {
	raw, _ := json.Marshal(data)
	must(c.WriteJSON(env{Type: typ, ID: id, Data: raw}), "ws write")
}

// expect 读取帧直到出现指定类型（跳过 typing/pong 等无关帧）
func expect(c *websocket.Conn, typ string, timeout time.Duration) (env, bool) {
	deadline := time.Now().Add(timeout)
	for {
		_ = c.SetReadDeadline(deadline)
		var e env
		if err := c.ReadJSON(&e); err != nil {
			return env{}, false
		}
		if e.Type == typ {
			return e, true
		}
	}
}

func main() {
	flag.StringVar(&base, "base", "http://127.0.0.1:8092", "服务地址")
	flag.StringVar(&s3Endpoint, "s3-endpoint", "", "S3 存储地址 host:port（检查生命周期/CORS 用，留空跳过语音相关检查）")
	flag.StringVar(&s3Key, "s3-key", "", "S3 access key")
	flag.StringVar(&s3Secret, "s3-secret", "", "S3 secret key")
	flag.StringVar(&s3Bucket, "bucket", "imchat", "bucket")
	flag.StringVar(&s3Region, "s3-region", "garage", "S3 region（Garage 默认 garage）")
	flag.StringVar(&webOrigin, "origin", "http://127.0.0.1:4173", "网页 Origin（检查 CORS 用）")
	flag.BoolVar(&s3SSL, "s3-ssl", false, "S3 使用 https")
	flag.StringVar(&sttBase, "stt-base", "", "v0.3.0：开启 stt 的第二个实例地址（其 stt.*.base_url 指向 -stt-stub），留空跳过转写成功路径")
	flag.StringVar(&sttStubAddr, "stt-stub", "127.0.0.1:18099", "v0.3.0：假 STT 提供方监听地址")
	flag.StringVar(&v040Base, "v040-base", "", "v0.4.0：开启 calls+smtp 的实例地址，留空跳过通话/密码检查")
	flag.StringVar(&callsStubAddr, "calls-stub", "127.0.0.1:18126", "v0.4.0：假 Cloudflare 监听地址")
	flag.StringVar(&smtpStubAddr, "smtp-stub", "127.0.0.1:18125", "v0.4.0：假 SMTP 监听地址")
	flag.Parse()
	suffix := fmt.Sprintf("%d%d", time.Now().Unix(), rand.Intn(1000))

	var alice, bob authResp
	check(call("POST", "/api/register", "", map[string]string{"email": "alice" + suffix + "@test.com", "password": "password123", "nickname": "Alice 🍎"}, &alice) == 201, "register alice")
	check(call("POST", "/api/register", "", map[string]string{"email": "bob" + suffix + "@test.com", "password": "password123"}, &bob) == 201, "register bob")
	check(call("POST", "/api/register", "", map[string]string{"email": "bob" + suffix + "@test.com", "password": "password123"}, nil) == 409, "duplicate email rejected")
	var login authResp
	check(call("POST", "/api/login", "", map[string]string{"email": "BOB" + suffix + "@test.com", "password": "password123"}, &login) == 200 && login.User.ID == bob.User.ID, "login (case-insensitive email)")
	check(call("POST", "/api/login", "", map[string]string{"email": "bob" + suffix + "@test.com", "password": "wrong-pass"}, nil) == 401, "wrong password rejected")

	var conv struct {
		ConversationID uint64 `json:"conversation_id"`
	}
	call("POST", "/api/conversations/direct", alice.Token, map[string]uint64{"peer_id": bob.User.ID}, &conv)
	var conv2 struct {
		ConversationID uint64 `json:"conversation_id"`
	}
	call("POST", "/api/conversations/direct", bob.Token, map[string]uint64{"peer_id": alice.User.ID}, &conv2)
	check(conv.ConversationID > 0 && conv.ConversationID == conv2.ConversationID, "direct conversation is unique per pair")
	cid := conv.ConversationID

	a1, a2, b1 := dial(alice.Token), dial(alice.Token), dial(bob.Token)
	time.Sleep(200 * time.Millisecond)

	send(a1, "send", "r1", map[string]any{"client_msg_id": "m-1", "conversation_id": cid, "content": "你好 Bob 👋"})
	ack, ok := expect(a1, "send_ack", 3*time.Second)
	check(ok && strings.Contains(string(ack.Data), `"seq":1`) && ack.ID == "r1", "sender gets send_ack with seq=1")
	m, ok := expect(b1, "message", 3*time.Second)
	check(ok && strings.Contains(string(m.Data), "你好 Bob 👋"), "receiver gets message (utf8mb4 emoji ok)")
	_, ok = expect(a2, "message", 3*time.Second)
	check(ok, "sender's other device gets message (multi-device sync)")

	send(a1, "send", "r2", map[string]any{"client_msg_id": "m-1", "conversation_id": cid, "content": "你好 Bob 👋"})
	ack, ok = expect(a1, "send_ack", 3*time.Second)
	check(ok && strings.Contains(string(ack.Data), `"duplicate":true`) && strings.Contains(string(ack.Data), `"seq":1`), "resend is idempotent")
	_, ok = expect(b1, "message", 500*time.Millisecond)
	check(!ok, "duplicate not delivered twice")

	send(b1, "typing", "", map[string]any{"conversation_id": cid})
	_, ok = expect(a1, "typing", 2*time.Second)
	check(ok, "typing indicator forwarded")

	send(b1, "read", "", map[string]any{"conversation_id": cid, "seq": 1})
	rd, ok := expect(a1, "read", 2*time.Second)
	check(ok && strings.Contains(string(rd.Data), `"read_seq":1`), "read receipt delivered to sender")

	// 离线消息
	b1.Close()
	time.Sleep(300 * time.Millisecond)
	for i := 2; i <= 4; i++ {
		send(a1, "send", "", map[string]any{"client_msg_id": fmt.Sprintf("m-%d", i), "conversation_id": cid, "content": fmt.Sprintf("离线消息 %d", i)})
		_, ok = expect(a1, "send_ack", 3*time.Second)
		check(ok, fmt.Sprintf("offline message %d acked", i))
	}
	var convs []struct {
		ID      uint64 `json:"id"`
		Unread  uint64 `json:"unread"`
		LastSeq uint64 `json:"last_seq"`
		Peer    *struct {
			Nickname string `json:"nickname"`
		} `json:"peer"`
	}
	call("GET", "/api/conversations", bob.Token, nil, &convs)
	check(len(convs) == 1 && convs[0].Unread == 3 && convs[0].LastSeq == 4 && convs[0].Peer != nil && convs[0].Peer.Nickname == "Alice 🍎", "bob sees 3 unread, peer info")

	var msgs []struct {
		Seq uint64 `json:"seq"`
	}
	call("GET", fmt.Sprintf("/api/conversations/%d/messages?after_seq=1", cid), bob.Token, nil, &msgs)
	check(len(msgs) == 3 && msgs[0].Seq == 2 && msgs[2].Seq == 4, "offline sync via after_seq")
	call("GET", fmt.Sprintf("/api/conversations/%d/messages?before_seq=4&limit=2", cid), bob.Token, nil, &msgs)
	check(len(msgs) == 2 && msgs[0].Seq == 2 && msgs[1].Seq == 3, "history paging via before_seq")

	var eve authResp
	call("POST", "/api/register", "", map[string]string{"email": "eve" + suffix + "@test.com", "password": "password123"}, &eve)
	check(call("GET", fmt.Sprintf("/api/conversations/%d/messages", cid), eve.Token, nil, nil) == 403, "non-member cannot read messages")
	e1 := dial(eve.Token)
	send(e1, "send", "", map[string]any{"client_msg_id": "x", "conversation_id": cid, "content": "hack"})
	er, ok := expect(e1, "error", 2*time.Second)
	check(ok && strings.Contains(string(er.Data), "forbidden"), "non-member cannot send")
	check(call("GET", "/api/me", "bad-token", nil, nil) == 401, "invalid token rejected")

	v020(suffix, alice, bob, eve)
	v030(suffix, alice, bob, eve)
	v040(suffix, alice, bob, eve)
	v050(suffix, alice, bob, eve)
	fmt.Printf("\nALL PASSED (%d checks)\n", passed)
}
