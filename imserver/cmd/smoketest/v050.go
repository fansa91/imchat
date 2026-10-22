package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// v0.5.0 冒烟：好友系统（搜索/申请/接受/拒绝/撤回/备注/删除）、
// 「我的」（allow_find / revoke-other-sessions）、多人通话（假 SFU）。
// 群通话部分仍借 -v040-base 的实例，但该实例除 v0.4 的 calls+smtp 外还要开
// calls.group_enabled=true、sfu_app_id/sfu_app_secret 任意非空、calls.base_url 指向本桩，
// 且 group_max_participants 请配 2（验 room_full），ring_timeout_seconds 配小。

// ---------- 假 Cloudflare SFU（sessions / tracks / renegotiate / close） ----------

type sfuStub struct {
	mu             sync.Mutex
	seq            int
	lastAuth       string
	remoteSession  string // 最近一次 remote tracks/new 组装到的发布方 session
	remoteTrack    string
	renegotiations int
	closes         int
}

func (s *sfuStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	s.mu.Lock()
	s.lastAuth = r.Header.Get("Authorization")
	s.mu.Unlock()
	write := func(code int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
	switch {
	case strings.Contains(path, "/credentials/generate-ice-servers") && r.Method == "POST":
		write(201, `{"iceServers":[{"urls":["stun:stun.cloudflare.com:3478"]},{"urls":["turn:turn.cloudflare.com:3478?transport=udp"],"username":"stubuser","credential":"stubcred"}]}`)
	case strings.HasSuffix(path, "/sessions/new") && r.Method == "POST":
		s.mu.Lock()
		s.seq++
		n := s.seq
		s.mu.Unlock()
		write(201, fmt.Sprintf(`{"sessionId":"sess-%d"}`, n))
	case strings.Contains(path, "/tracks/new") && r.Method == "POST":
		var in struct {
			Tracks []struct {
				Location  string `json:"location"`
				TrackName string `json:"trackName"`
				SessionID string `json:"sessionId"`
			} `json:"tracks"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		var out []string
		for _, t := range in.Tracks {
			if t.Location == "remote" {
				s.mu.Lock()
				s.remoteSession, s.remoteTrack = t.SessionID, t.TrackName
				s.mu.Unlock()
			}
			out = append(out, fmt.Sprintf(`{"trackName":%q,"mid":"mid-%s"}`,
				t.TrackName, strings.ReplaceAll(t.TrackName, "/", "_")))
		}
		write(200, fmt.Sprintf(`{"sessionDescription":{"type":"answer","sdp":"stub-answer-sdp"},"tracks":[%s]}`,
			strings.Join(out, ",")))
	case strings.HasSuffix(path, "/renegotiate") && r.Method == "PUT":
		s.mu.Lock()
		s.renegotiations++
		s.mu.Unlock()
		write(200, `{}`)
	case strings.HasSuffix(path, "/tracks/close") && r.Method == "PUT":
		s.mu.Lock()
		s.closes++
		s.mu.Unlock()
		write(200, `{}`)
	default:
		http.NotFound(w, r)
	}
}

// rawAt 发请求并返回状态码与原始 body（校验字段不存在等场景用）。
func rawAt(baseURL, method, path, token string, body any) (int, []byte) {
	saved := base
	base = baseURL
	defer func() { base = saved }()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, baseURL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	must(err, method+" "+path)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

type pubUser struct {
	ID        uint64 `json:"id"`
	Nickname  string `json:"nickname"`
	AvatarURL string `json:"avatar_url"`
	Relation  string `json:"relation"`
}

type friendEntry struct {
	User      pubUser `json:"user"`
	Remark    string  `json:"remark"`
	Online    bool    `json:"online"`
	CreatedAt string  `json:"created_at"`
}

type friendReqLists struct {
	Incoming []struct {
		ID      uint64  `json:"id"`
		User    pubUser `json:"user"`
		Message string  `json:"message"`
	} `json:"incoming"`
	Outgoing []struct {
		ID uint64 `json:"id"`
	} `json:"outgoing"`
}

func friendsOf(token string) []friendEntry {
	var out []friendEntry
	call("GET", "/api/friends", token, nil, &out)
	return out
}

func v050(suffix string, alice, bob, eve authResp) {
	fmt.Println("\n--- v0.5.0 ---")
	v050Friends(suffix, alice, bob, eve)
	if v040Base != "" {
		v050GroupCalls(suffix)
	} else {
		fmt.Println("SKIP group call checks (no -v040-base)")
	}
}

func v050Friends(suffix string, alice, bob, eve authResp) {
	// ---------- /api/config 新字段 ----------
	var cfg map[string]any
	call("GET", "/api/config", alice.Token, nil, &cfg)
	_, okG1 := cfg["group_call_enabled"]
	_, okG2 := cfg["group_call_max_participants"]
	check(okG1 && okG2, "GET /api/config has group_call_enabled/group_call_max_participants")
	var er map[string]string
	st := call("POST", "/api/calls/group", alice.Token, map[string]any{"conversation_id": 1, "kind": "audio"}, &er)
	check(st == 501 || cfg["group_call_enabled"] == true, "group call → 501 group_call_disabled when disabled")

	var carol authResp
	check(call("POST", "/api/register", "", map[string]string{
		"email": "carol" + suffix + "@test.com", "password": "password123", "nickname": "Carol",
	}, &carol) == 201, "register carol")

	aConn := dial(alice.Token)
	bConn := dial(bob.Token)
	defer aConn.Close()
	defer bConn.Close()
	time.Sleep(200 * time.Millisecond)

	// ---------- 搜索：email / id 两入口、PublicUser 收紧、relation ----------
	st, raw := rawAt(base, "GET", "/api/users/search?email=bob"+suffix+"@test.com", alice.Token, nil)
	check(st == 200 && strings.Contains(string(raw), `"relation":"none"`) && !strings.Contains(string(raw), `"email"`),
		"search by email → PublicUser (no email field) + relation none")
	var su pubUser
	st = call("GET", fmt.Sprintf("/api/users/search?id=%d", bob.User.ID), alice.Token, nil, &su)
	check(st == 200 && su.ID == bob.User.ID && su.Relation == "none", "search by id → relation none")
	st = call("GET", fmt.Sprintf("/api/users/search?id=%d", alice.User.ID), alice.Token, nil, &er)
	check(st == 400 && er["code"] == "bad_request", "search self → 400 bad_request")
	check(call("GET", "/api/users/search?id=999999999", alice.Token, nil, &er) == 404 && er["code"] == "not_found",
		"search nonexistent id → 404 not_found")

	// ---------- 申请 → 事件 → 接受 ----------
	var created struct {
		ID     uint64 `json:"id"`
		Status string `json:"status"`
	}
	st = call("POST", "/api/friends/requests", alice.Token, map[string]any{"user_id": bob.User.ID, "message": "hi 我是 Alice"}, &created)
	check(st == 201 && created.ID > 0 && created.Status == "pending", "friend request created (201 pending)")
	_, ok := expectWhere(bConn, "friend_request", 3*time.Second, has(`"message":"hi 我是 Alice"`))
	check(ok, "target receives friend_request event")
	st = call("POST", "/api/friends/requests", alice.Token, map[string]any{"user_id": bob.User.ID}, &er)
	check(st == 409 && er["code"] == "request_pending", "duplicate pending request → 409 request_pending")
	st = call("POST", "/api/friends/requests", alice.Token, map[string]any{"user_id": alice.User.ID}, &er)
	check(st == 400 && er["code"] == "self", "request to self → 400 self")
	var lists friendReqLists
	call("GET", "/api/friends/requests", bob.Token, nil, &lists)
	check(len(lists.Incoming) == 1 && lists.Incoming[0].User.ID == alice.User.ID && len(lists.Outgoing) == 0,
		"incoming request listed for target")
	check(call("POST", fmt.Sprintf("/api/friends/requests/%d/accept", created.ID), bob.Token, nil, nil) == 200, "accept request")
	_, ok = expectWhere(aConn, "friend_request_accepted", 3*time.Second, has(fmt.Sprintf(`"id":%d`, bob.User.ID)))
	check(ok, "requester receives friend_request_accepted")
	st = call("POST", "/api/friends/requests", alice.Token, map[string]any{"user_id": bob.User.ID}, &er)
	check(st == 409 && er["code"] == "already_friends", "request to friend → 409 already_friends")
	fa := friendsOf(alice.Token)
	check(len(fa) == 1 && fa[0].User.ID == bob.User.ID, "alice friend list has bob")
	check(len(friendsOf(bob.Token)) == 1, "bob friend list has alice")

	// ---------- 备注 ----------
	var patched friendEntry
	st = call("PATCH", fmt.Sprintf("/api/friends/%d", bob.User.ID), alice.Token, map[string]any{"remark": "老鲍"}, &patched)
	check(st == 200 && patched.Remark == "老鲍", "PATCH friend remark")
	fa = friendsOf(alice.Token)
	check(len(fa) == 1 && fa[0].Remark == "老鲍", "remark persisted in friend list")
	st = call("PATCH", fmt.Sprintf("/api/friends/%d", bob.User.ID), alice.Token, map[string]any{"remark": strings.Repeat("备", 65)}, &er)
	check(st == 400 && er["code"] == "bad_remark", "remark > 64 runes → 400 bad_remark")
	st = call("PATCH", fmt.Sprintf("/api/friends/%d", eve.User.ID), alice.Token, map[string]any{"remark": "x"}, &er)
	check(st == 404 && er["code"] == "not_friends", "remark on non-friend → 404 not_friends")

	// ---------- 删好友：会话与记录仍在 ----------
	dcID := directAt(base, alice.Token, bob.User.ID)
	check(call("DELETE", fmt.Sprintf("/api/friends/%d", bob.User.ID), alice.Token, nil, nil) == 200, "delete friend")
	_, ok = expectWhere(bConn, "friend_removed", 3*time.Second, has(fmt.Sprintf(`"by":%d`, alice.User.ID)))
	check(ok, "removed side receives friend_removed")
	check(len(friendsOf(alice.Token)) == 0 && len(friendsOf(bob.Token)) == 0, "friend lists empty after delete")
	check(call("GET", fmt.Sprintf("/api/conversations/%d/messages", dcID), alice.Token, nil, nil) == 200,
		"direct conversation & history survive friend deletion")

	// ---------- 反向待处理 → 自动成友 ----------
	var c2 struct {
		ID uint64 `json:"id"`
	}
	call("POST", "/api/friends/requests", alice.Token, map[string]any{"user_id": carol.User.ID}, &c2)
	var acc struct {
		Status string `json:"status"`
	}
	st = call("POST", "/api/friends/requests", carol.Token, map[string]any{"user_id": alice.User.ID}, &acc)
	check(st == 200 && acc.Status == "accepted", "reverse pending request auto-accepts")
	_, ok = expectWhere(aConn, "friend_request_accepted", 3*time.Second, has(fmt.Sprintf(`"id":%d`, carol.User.ID)))
	check(ok, "original requester gets friend_request_accepted on auto-accept")
	check(len(friendsOf(carol.Token)) == 1, "carol friend list has alice after auto-accept")
	call("DELETE", fmt.Sprintf("/api/friends/%d", alice.User.ID), carol.Token, nil, nil)
	expectWhere(aConn, "friend_removed", 3*time.Second, nil) // 排掉事件，避免干扰后续

	// ---------- 拒绝 ----------
	var rj struct {
		ID uint64 `json:"id"`
	}
	call("POST", "/api/friends/requests", alice.Token, map[string]any{"user_id": eve.User.ID}, &rj)
	check(call("POST", fmt.Sprintf("/api/friends/requests/%d/reject", rj.ID), eve.Token, nil, nil) == 200, "reject request")
	_, ok = expectWhere(aConn, "friend_request_rejected", 3*time.Second, has(fmt.Sprintf(`"id":%d`, eve.User.ID)))
	check(ok, "requester receives friend_request_rejected")

	// ---------- 撤回 ----------
	var wd struct {
		ID uint64 `json:"id"`
	}
	call("POST", "/api/friends/requests", alice.Token, map[string]any{"user_id": eve.User.ID}, &wd)
	check(call("DELETE", fmt.Sprintf("/api/friends/requests/%d", wd.ID), alice.Token, nil, nil) == 200, "withdraw request")
	call("GET", "/api/friends/requests", eve.Token, nil, &lists)
	check(len(lists.Incoming) == 0, "withdrawn request disappears from incoming")

	// ---------- allow_find ----------
	var meResp struct {
		AllowFind *bool `json:"allow_find"`
	}
	st = call("PATCH", "/api/me", eve.Token, map[string]any{"allow_find": false}, &meResp)
	check(st == 200 && meResp.AllowFind != nil && !*meResp.AllowFind, "PATCH /api/me allow_find=false")
	call("GET", "/api/me", eve.Token, nil, &meResp)
	check(meResp.AllowFind != nil && !*meResp.AllowFind, "GET /api/me shows allow_find=false")
	check(call("GET", "/api/users/search?email=eve"+suffix+"@test.com", alice.Token, nil, &er) == 404,
		"search hidden user by email → 404")
	check(call("GET", fmt.Sprintf("/api/users/search?id=%d", eve.User.ID), alice.Token, nil, &er) == 404,
		"search hidden user by id → 404")
	call("PATCH", "/api/me", eve.Token, map[string]any{"allow_find": true}, nil)
	check(call("GET", "/api/users/search?email=eve"+suffix+"@test.com", alice.Token, nil, &su) == 200,
		"search works again after allow_find=true")

	// ---------- revoke-other-sessions ----------
	var dave authResp
	check(call("POST", "/api/register", "", map[string]string{
		"email": "dave" + suffix + "@test.com", "password": "password123",
	}, &dave) == 201, "register dave")
	dConn := dial(dave.Token)
	defer dConn.Close()
	time.Sleep(200 * time.Millisecond)
	var revoked struct {
		Token string `json:"token"`
	}
	st = call("POST", "/api/me/revoke-other-sessions", dave.Token, nil, &revoked)
	check(st == 200 && revoked.Token != "" && revoked.Token != dave.Token, "revoke-other-sessions returns fresh token")
	_, ok = expectWhere(dConn, "session_revoked", 5*time.Second, has(`"reason":"sessions_revoked"`))
	check(ok, "existing connections receive session_revoked")
	check(call("GET", "/api/me", dave.Token, nil, nil) == 401, "old token dead after revoke-other-sessions")
	check(call("GET", "/api/me", revoked.Token, nil, nil) == 200, "fresh token works after revoke-other-sessions")
}

// ---------- 多人通话（假 SFU） ----------

type gcallResp struct {
	CallID       string    `json:"call_id"`
	SessionID    string    `json:"session_id"`
	Participants []pubUser `json:"participants"`
}

func groupAt(baseURL, token, name string, members []uint64) uint64 {
	var out struct {
		ConversationID uint64 `json:"conversation_id"`
	}
	callAt(baseURL, "POST", "/api/conversations/group", token, map[string]any{"name": name, "member_ids": members}, &out)
	return out.ConversationID
}

func v050GroupCalls(suffix string) {
	fmt.Println("--- v0.5.0 group calls (sfu stub) ---")
	stub := &sfuStub{}
	srv := &http.Server{Addr: callsStubAddr, Handler: stub}
	go srv.ListenAndServe()
	defer srv.Close()

	g1 := registerAt(v040Base, "g1"+suffix, "Group-12345")
	g2 := registerAt(v040Base, "g2"+suffix, "Group-12345")
	g3 := registerAt(v040Base, "g3"+suffix, "Group-12345")

	gid := groupAt(v040Base, g1.Token, "通话测试群", []uint64{g2.User.ID, g3.User.ID})
	check(gid > 0, "group created for call tests")

	w1 := dialAt(v040Base, g1.Token)
	w2 := dialAt(v040Base, g2.Token)
	defer w1.Close()
	defer w2.Close()
	time.Sleep(300 * time.Millisecond)

	var room gcallResp
	st := callAt(v040Base, "POST", "/api/calls/group", g1.Token,
		map[string]any{"conversation_id": gid, "kind": "video"}, &room)
	check(st == 200 && room.CallID != "" && room.SessionID != "", "create group call room → call_id + session_id")
	check(len(room.Participants) == 1, "creator is first participant")
	_, ok := expectWhere(w2, "group_call_started", 3*time.Second, has(fmt.Sprintf(`"call_id":%q`, room.CallID)))
	check(ok, "group member receives group_call_started")
	callID := room.CallID

	// 发布：冒名轨道被拒，本人轨道成功
	pub := func(token string, tracks []map[string]any) (int, []byte) {
		return rawAt(v040Base, "POST", fmt.Sprintf("/api/calls/group/%s/tracks/publish", callID), token,
			map[string]any{"session_description": map[string]string{"type": "offer", "sdp": "fake-offer"}, "tracks": tracks})
	}
	var er map[string]string
	st, rawBad := pub(g1.Token, []map[string]any{{"track_name": "999999/audio", "kind": "audio"}})
	_ = json.Unmarshal(rawBad, &er)
	check(st == 400 && er["code"] == "bad_track", "publish with foreign track_name → 400 bad_track")
	st, raw := pub(g1.Token, []map[string]any{
		{"track_name": fmt.Sprintf("%d/audio", g1.User.ID), "kind": "audio"},
		{"track_name": fmt.Sprintf("%d/video", g1.User.ID), "kind": "video"},
	})
	check(st == 200 && strings.Contains(string(raw), "stub-answer-sdp") && strings.Contains(string(raw), `"mid"`),
		"publish own tracks → SFU answer passthrough")
	_, ok = expectWhere(w2, "group_call_tracks", 3*time.Second, has(fmt.Sprintf(`"user_id":%d`, g1.User.ID)))
	check(ok, "member receives group_call_tracks after publish")

	// 第二人加入；第三人超上限（实例需配 group_max_participants=2）
	var room2 gcallResp
	st = callAt(v040Base, "POST", fmt.Sprintf("/api/calls/group/%s/join", callID), g2.Token, nil, &room2)
	check(st == 200 && room2.SessionID != "" && room2.SessionID != room.SessionID && len(room2.Participants) == 2,
		"second member joins with own SFU session")
	st = callAt(v040Base, "POST", fmt.Sprintf("/api/calls/group/%s/join", callID), g3.Token, nil, &er)
	check(st == 403 && er["code"] == "room_full", "third member → 403 room_full")

	// g2 发布音频 + 订阅 g1 的音频（校验 remote 组装到发布方 session）
	st, _ = pub(g2.Token, []map[string]any{{"track_name": fmt.Sprintf("%d/audio", g2.User.ID), "kind": "audio"}})
	check(st == 200, "g2 publishes audio")
	st, raw = rawAt(v040Base, "POST", fmt.Sprintf("/api/calls/group/%s/tracks/subscribe", callID), g2.Token,
		map[string]any{"tracks": []map[string]any{{"owner_id": g1.User.ID, "kind": "audio"}}})
	check(st == 200 && strings.Contains(string(raw), "stub-answer-sdp"), "subscribe returns SFU answer")
	stub.mu.Lock()
	rs, rt := stub.remoteSession, stub.remoteTrack
	stub.mu.Unlock()
	check(rs == room.SessionID && rt == fmt.Sprintf("%d/audio", g1.User.ID),
		"subscribe assembled remote track against publisher session")
	st, _ = rawAt(v040Base, "POST", fmt.Sprintf("/api/calls/group/%s/tracks/subscribe", callID), g2.Token,
		map[string]any{"tracks": []map[string]any{{"owner_id": g3.User.ID, "kind": "audio"}}})
	check(st == 400, "subscribe nonexistent track → 400 bad_track")

	// busy 占用：g1 已在群通话中，另建房 409；1 对 1 呼叫也 busy
	gid2 := groupAt(v040Base, g1.Token, "另一个群", []uint64{g2.User.ID})
	st = callAt(v040Base, "POST", "/api/calls/group", g1.Token, map[string]any{"conversation_id": gid2, "kind": "audio"}, &er)
	check(st == 409 && er["code"] == "call_unavailable", "busy user creating another room → 409 call_unavailable")
	dcID := directAt(v040Base, g1.Token, g2.User.ID)
	send040(w1, "call_invite", "ci-busy", map[string]any{"conversation_id": dcID, "call_id": "call-busy-" + suffix, "kind": "audio"})
	_, ok = expectWhere(w1, "error", 3*time.Second, has("call_busy"))
	check(ok, "1:1 invite while in group call → call_busy")

	// renegotiate + close
	st = callAt(v040Base, "POST", fmt.Sprintf("/api/calls/group/%s/renegotiate", callID), g2.Token,
		map[string]any{"session_description": map[string]string{"type": "answer", "sdp": "fake-answer"}}, nil)
	check(st == 200, "renegotiate proxied")
	st = callAt(v040Base, "POST", fmt.Sprintf("/api/calls/group/%s/tracks/close", callID), g2.Token,
		map[string]any{"kinds": []string{"audio"}}, nil)
	check(st == 200, "close tracks proxied")
	_, ok = expectWhere(w1, "group_call_tracks_closed", 3*time.Second, has(fmt.Sprintf(`"user_id":%d`, g2.User.ID)))
	check(ok, "member receives group_call_tracks_closed")

	// 房间状态（重连恢复）
	var state struct {
		State        string    `json:"state"`
		Participants []pubUser `json:"participants"`
		Tracks       []struct {
			TrackName string `json:"track_name"`
		} `json:"tracks"`
	}
	st = callAt(v040Base, "GET", fmt.Sprintf("/api/calls/group/%s", callID), g1.Token, nil, &state)
	check(st == 200 && state.State == "open" && len(state.Participants) == 2 && len(state.Tracks) == 2,
		"GET room state: 2 participants, g1 tracks remain")

	// 离开与结束：g1 先走（房间不结束），g2 再走（房空结束 → 记录 + ended 事件）
	check(callAt(v040Base, "POST", fmt.Sprintf("/api/calls/group/%s/leave", callID), g1.Token, nil, nil) == 200, "g1 leaves")
	st = callAt(v040Base, "GET", fmt.Sprintf("/api/calls/group/%s", callID), g2.Token, nil, &state)
	check(st == 200, "room survives first leave")
	check(callAt(v040Base, "POST", fmt.Sprintf("/api/calls/group/%s/leave", callID), g2.Token, nil, nil) == 200, "g2 leaves")
	_, ok = expectWhere(w2, "group_call_ended", 3*time.Second, has(fmt.Sprintf(`"call_id":%q`, callID)))
	check(ok, "group_call_ended broadcast")
	check(callAt(v040Base, "GET", fmt.Sprintf("/api/calls/group/%s", callID), g1.Token, nil, &er) == 404 && er["code"] == "room_gone",
		"ended room → 404 room_gone")
	found := false
	for _, m := range historyAt(v040Base, g1.Token, gid, "") {
		if m.MsgType == 6 && strings.Contains(m.Content, `"mode":"group"`) &&
			strings.Contains(m.Content, `"joined_count":2`) && strings.Contains(m.Content, `"outcome":"completed"`) &&
			strings.Contains(m.Content, `"participant_count":3`) {
			found = true
		}
	}
	check(found, "group call record: mode=group joined_count=2 participant_count=3 outcome=completed")

	// 无人加入 → missed
	var room3 gcallResp
	st = callAt(v040Base, "POST", "/api/calls/group", g1.Token, map[string]any{"conversation_id": gid2, "kind": "audio"}, &room3)
	check(st == 200 && room3.CallID != "", "second room created")
	check(callAt(v040Base, "POST", fmt.Sprintf("/api/calls/group/%s/leave", room3.CallID), g1.Token, nil, nil) == 200,
		"creator leaves alone room")
	found = false
	for i := 0; i < 10 && !found; i++ {
		for _, m := range historyAt(v040Base, g1.Token, gid2, "") {
			if m.MsgType == 6 && strings.Contains(m.Content, `"mode":"group"`) &&
				strings.Contains(m.Content, `"joined_count":1`) && strings.Contains(m.Content, `"outcome":"missed"`) {
				found = true
			}
		}
		if !found {
			time.Sleep(200 * time.Millisecond)
		}
	}
	check(found, "solo room leaves missed record (joined_count=1)")

	// SFU 代理带了 App Secret（Bearer），且发布方 session 与订阅组装一致已在上面校验
	stub.mu.Lock()
	authz := stub.lastAuth
	stub.mu.Unlock()
	check(strings.HasPrefix(authz, "Bearer ") && len(authz) > len("Bearer "), "sfu requests carry Bearer app secret")
}
