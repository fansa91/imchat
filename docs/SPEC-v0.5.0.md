# IMChat v0.5.0 接口约定（服务端 / 网页端 / Flutter 共用）

范围：独立「我的」界面、好友系统（含备注好友）、多人音视频通话
（群聊走 Cloudflare Realtime SFU）、Release 更新内容说明。
所有新增都必须向后兼容：新客户端遇到旧服务端（`/api/config` 无新字段）
时隐藏对应功能；旧客户端不受新字段影响。

现有实现以代码为准：信令照现有 WS 事件信封（`internal/ws/protocol.go`）；
新配置项的校验照 `calls` 段现有写法（`enabled=true` 但凭据不全时启动报错，
未启用时不校验）；迁移照 004 的模式（SQL 文件 + `upgradeColumns` 幂等加列）。

**明确不做（本版排除）**：

- **手机号整条线**（用户 2026-10-22 定）：不做绑定手机号、不做手机号
  密码登录、不接任何短信服务商。原因：阿里云短信认证无免费额度
  （按量 ¥0.042/次），个人项目不值得为此付费与维护。好友搜索只保留
  邮箱与用户 ID 两种入口。
- 离线推送（继续不接 APNs / FCM）；群通话只能通知在线成员（见 §3.6）。
- RealtimeKit（Flutter SDK 已停更，禁用）；1 对 1 通话不改，保留
  P2P + TURN。
- 拉黑、好友分组、消息搜索/置顶、E2EE（后续版本）。

## 1. 好友系统

### 1.1 数据模型（迁移 `005_v050.sql`）

```sql
CREATE TABLE friend_requests (
  id         BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  from_id    BIGINT UNSIGNED NOT NULL,
  to_id      BIGINT UNSIGNED NOT NULL,
  message    VARCHAR(128) NOT NULL DEFAULT '',   -- 申请附言
  status     TINYINT NOT NULL DEFAULT 0,          -- 0 待处理 1 已接受 2 已拒绝 3 已撤回
  created_at DATETIME(3) NOT NULL,
  handled_at DATETIME(3) NULL,
  KEY idx_pair (from_id, to_id),
  KEY idx_to_status (to_id, status)
);
CREATE TABLE friend_edges (
  user_id    BIGINT UNSIGNED NOT NULL,
  friend_id  BIGINT UNSIGNED NOT NULL,
  remark     VARCHAR(64) NOT NULL DEFAULT '',     -- 备注名：单向，只对 user_id 本人可见
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (user_id, friend_id)
);
```

- 好友关系用**有向边成对存储**：A、B 成友时同时插入 (A,B) 与 (B,A)；
  备注挂在各自那条边上，互不可见、互不影响。
- 同一对用户最多一条待处理申请：MySQL 无部分唯一索引，由服务端在
  事务内校验（查 status=0 的同向/反向申请后再插入），不靠表约束。
- 删好友删双边；**不删会话、不删消息记录**，原单聊会话保持可用。
- `users` 新增 `allow_find TINYINT(1) NOT NULL DEFAULT 1`，走
  `upgradeColumns` 幂等加列（不进 SQL 迁移的 ALTER，避免重复执行报错）。

### 1.2 公开用户 DTO 与隐私收紧

现状问题：`store.User` 的 JSON 含 `email`，用户搜索、群成员等响应
直接回了完整结构，等于对任意登录用户暴露邮箱。本版收紧：

```json
// PublicUser —— 一切「他人视角」的响应只用这个结构
{"id": 12, "nickname": "小明", "avatar_url": "/api/users/12/avatar?v=abcd1234"}
```

- 适用：`GET /api/users/search`、好友列表/申请、群成员、会话对端信息。
- `email` 只允许出现在本人视角：`GET /api/me`、登录/注册响应。
- `GET /api/me` 另返回 `"allow_find": true`（本人隐私开关，见 §2）。
- 旧客户端不依赖他人 email 字段（只用 id/nickname/avatar_url），
  收紧不构成破坏。

### 1.3 接口

搜索扩展（现有 `GET /api/users/search?email=` 保留，新增 id 入口）：

- `GET /api/users/search?id={uid}`：按用户 ID 精确查。
- 两种入口统一返回 PublicUser，并附关系状态（对搜索者而言）：
  `{"id":..,"nickname":..,"avatar_url":..,"relation":"none|friend|pending_out|pending_in"}`。
- 目标 `allow_find=0` 时一律 404 `not_found`（不区分「不存在」与
  「不可见」，防枚举）；搜自己返回 400 `bad_request`。
- 限流：复用现有接口限流中间件（如有）外，搜索按用户 30 次/分钟。

好友申请：

- `POST /api/friends/requests` `{user_id, message?}` → 201 `{id, status:"pending"}`。
  校验顺序与错误码：目标不存在 404 `not_found`；给自己发 400 `self`；
  已是好友 409 `already_friends`；已有待处理申请（同向）409 `request_pending`。
  **反向已有待处理申请时直接成友**：把对方那条申请置为已接受、建双边，
  返回 200 `{status:"accepted"}`，并给对方发 `friend_request_accepted` 事件。
- `GET /api/friends/requests` → `{"incoming":[...], "outgoing":[...]}`，
  每项 `{id, user: PublicUser, message, created_at}`，只含待处理。
- `POST /api/friends/requests/{id}/accept`（仅被申请方）→ 建双边、置状态，
  给申请方发 `friend_request_accepted`。非本人相关申请 404。
- `POST /api/friends/requests/{id}/reject`（仅被申请方）→ 置已拒绝，
  给申请方发 `friend_request_rejected`。
- `DELETE /api/friends/requests/{id}`（仅申请方）→ 撤回，删除该待处理行
  （或置状态 3=已撤回，二选一，实现时定死为置 3 并在列表过滤）。

好友列表与维护：

- `GET /api/friends` → `[{user: PublicUser, remark, online, created_at}]`，
  `online` 复用现有 presence（Redis）判定；排序：备注/昵称拼音升序由
  客户端排，服务端按 created_at 倒序返回即可。
- `PATCH /api/friends/{user_id}` `{remark}` → 改自己这条边的备注；
  remark ≤ 64 字符（超出 400 `bad_remark`），空串 = 清除备注。
  不是好友 404 `not_friends`。
- `DELETE /api/friends/{user_id}` → 删双边；给对方发 `friend_removed`
  事件（对方客户端据此刷新列表，**不弹提示**）。会话与记录不动。

WS 事件（信封照 protocol.go 现有格式）：

- `friend_request` `{request}` —— 新申请到达（被申请方在线时）。
- `friend_request_accepted` `{by: PublicUser}`。
- `friend_request_rejected` `{by: PublicUser}`。
- `friend_removed` `{by: user_id}`。

### 1.4 客户端（Web / Flutter 同构）

- 新增「好友」页/标签：好友列表（**备注优先显示**，无备注显示昵称；
  显示名规则 `remark != "" ? remark : nickname`，会话列表、群成员、
  通话界面等一切出现好友名字的地方统一走这个规则）、在线状态点、
  「新的朋友」入口（带待处理红点数）。
- 添加好友：搜索框（邮箱或用户 ID）→ 结果卡（关系状态决定按钮：
  添加 / 等待验证 / 接受 / 已是好友）→ 填写附言发送。
- 好友详情：设置备注（弹窗输入）、发消息、语音/视频通话（复用 1 对 1）、
  拉进群（在建群选人里默认置顶好友）、删除好友（二次确认，明示
  「聊天记录不会删除」）。

## 2. 「我的」界面

### 2.1 服务端补充

- `PATCH /api/me` 扩展可改字段：`allow_find`（bool）。与现有
  nickname/avatar_key 并存，部分更新语义不变。
- `POST /api/me/revoke-other-sessions`：`token_version` +1 并给**当前
  设备**换发新 token 返回 `{token}`；其余连接按 v0.4 既有流程收
  `session_revoked` 下线。当前设备客户端照 v0.4 的处理：先用新 token
  续连，不登出。
- `/api/config` 不新增字段（「我的」页功能不设开关）。

### 2.2 客户端信息架构

导航从「会话列表 + 设置弹层」改为三入口：**消息 / 好友 / 我的**
（Flutter 底部导航栏；Web 左侧导航栏）。原 SettingsSheet 的内容
整体迁入「我的」页，分区如下：

- 资料卡：头像、昵称、邮箱（只读）、用户 ID（可复制）；点进编辑资料
  （复用 v0.4 头像/昵称编辑）。
- 账号安全：修改密码（复用 v0.4）、退出其他设备。
- 隐私：开关「允许通过邮箱 / 用户 ID 找到我」（对应 `allow_find`，
  关后 §1.3 搜索对本人返回 404；已是好友、已加入的群不受影响）。
- 外观：主题 / 动态取色（现有设置原样迁移）。
- 服务器：接口地址（现有设置原样迁移）。
- 存储：缓存占用显示与清理（客户端本地实现）。
- 关于：版本号、更新日志入口（链到 GitHub Release 页）。
- 退出登录（红色，二次确认）。

## 3. 多人音视频通话（群通话）

### 3.1 配置与 `/api/config`

```yaml
calls:
  # ……v0.4 既有字段不变……
  group_enabled: false            # 多人通话总开关
  group_max_participants: 9       # 单房间上限，校验范围 2–16
  sfu_app_id: ""                  # Cloudflare Realtime(Calls) 应用 ID
  sfu_app_secret: ""              # Cloudflare Realtime(Calls) 应用密钥，只存服务端
```

- `group_enabled=true` 时 `sfu_app_id`/`sfu_app_secret` 必填，缺一
  启动报错（照现有校验风格）。
- 注意凭据是**两套**：TURN 用 Key ID + API Token（v0.4 已配），
  SFU 用 Calls 应用的 App ID + App Secret，需在 Cloudflare 控制台
  Realtime → Calls 另行创建应用获得，与 TURN Key 不通用。
- `/api/config` 新增：`"group_call_enabled": bool`（enabled 且凭据
  齐全时 true）、`"group_call_max_participants": 9`。
- SFU API 基地址复用 `calls.base_url`（默认
  `https://rtc.live.cloudflare.com/v1`），路径
  `/apps/{sfu_app_id}/sessions/...`，`Authorization: Bearer <sfu_app_secret>`。

### 3.2 房间与状态（Redis）

- 房间只存在于**群会话**（type=group）；发起人必须是群成员。
- Redis 结构：`im:gcall:{call_id}`（hash：conversation_id、kind、
  creator、created_at、state=open）、`im:gcall:{call_id}:members`
  （set of user_id）、`im:gcall:{call_id}:tracks`（hash：
  `{user_id}/{kind}` → `{session_id, track_name, mid}`）。
- **一人同时只能在一通通话里**（1 对 1 与群通话共用）：复用现有
  用户→通话占用映射；已在通话中再加入/发起 → 409/事件
  `call_unavailable {reason:"busy"}`。
- 每位参与者在 SFU 侧有**独立 session**（服务端创建后把 session_id
  下发本人），发布/订阅都走本人 session，服务端代理时校验
  「session 属于该用户且在该房间」。
- 房间结束条件：成员为空即结束；只剩 1 人且持续 120 秒由通话
  管理器清扫结束。结束写通话记录（§3.5）并广播 `group_call_ended`。

### 3.3 接口（服务端代理 SFU，App Secret 不下发客户端）

- `POST /api/calls/group` `{conversation_id, kind:"audio|video"}` →
  该群已有 open 房间时等同 join；否则建房。返回
  `{call_id, session_id, ice_servers, participants:[PublicUser...]}`。
  `ice_servers` 复用 v0.4 的 TURN 凭证下发逻辑（`GET /api/calls/ice-servers`
  背后的那套，SFU 媒体
  不需要 TURN，但客户端 RTCPeerConnection 统一带上无害，且订阅
  拉流在个别网络下仍可能受益）。非群成员 403 `not_member`；
  群会话不存在 404；未启用 501 `group_call_disabled`。
- `POST /api/calls/group/{call_id}/join` → 同上返回；房间已满
  403 `room_full`；房间不存在/已结束 404 `room_gone`。
- `POST /api/calls/group/{call_id}/tracks/publish`
  `{session_description:{type:"offer",sdp}, tracks:[{track_name, kind}]}` →
  服务端代理 Cloudflare `POST /apps/{app}/sessions/{sid}/tracks/new`
  （`location:"local"`），把 answer 透传回客户端，并把
  track_name→mid 登记进 Redis 后广播 `group_call_tracks`。
  **track_name 由服务端分配**：`{user_id}/audio`、`{user_id}/video`，
  客户端请求里的 track_name 必须与之相等，否则 400 `bad_track`
  （防冒名发布他人轨道）。
- `POST /api/calls/group/{call_id}/tracks/subscribe`
  `{tracks:[{owner_id, kind}], session_description?}` → 服务端按
  Redis 登记组装 `location:"remote"` 的 tracks/new 到**本人**
  session；返回 Cloudflare 的 sessionDescription 与
  `requires_immediate_renegotiation` 标记原样透传。
- `POST /api/calls/group/{call_id}/renegotiate`
  `{session_description:{type:"answer",sdp}}` → 代理 Cloudflare
  `PUT /apps/{app}/sessions/{sid}/renegotiate`。
- `POST /api/calls/group/{call_id}/tracks/close` `{kinds:["video"]}` →
  服务端按 Redis 的 mid 组装 Cloudflare `tracks/close`，
  成功后清登记并广播 `group_call_tracks_closed`。
- `POST /api/calls/group/{call_id}/leave` → 关本人全部轨道、退出
  成员集、广播 `group_call_left`；触发房间结束条件时走 §3.2。
- `GET /api/calls/group/{call_id}` → `{state, participants, tracks}`
  供断线重连后恢复 UI 与订阅关系。
- Cloudflare 调用失败统一 502 `sfu_failed`，服务端记日志
  （不得记 App Secret 与完整 SDP）。

### 3.4 WS 事件（房间维度，广播给群内在线成员 / 房间成员）

- `group_call_started` `{call_id, conversation_id, kind, creator}` ——
  即邀请：客户端在任意页面弹可加入横幅（60 秒后自动收起，
  照 `call_ring_timeout_seconds`）；**不做逐人响铃状态机**。
- `group_call_joined` `{call_id, user: PublicUser}`。
- `group_call_tracks` `{call_id, user_id, tracks:[{track_name, kind, mid}]}`。
- `group_call_tracks_closed` `{call_id, user_id, kinds}`。
- `group_call_left` `{call_id, user_id}`。
- `group_call_ended` `{call_id, conversation_id}`。

### 3.5 通话记录

复用 `msg_type=6` 与 `store.CallContent`，扩展三字段（旧记录无
mode 时按 single 渲染，旧客户端忽略新字段照常显示）：

```go
Mode             string `json:"mode,omitempty"`              // single | group
ParticipantCount int    `json:"participant_count,omitempty"` // 群成员总数
JoinedCount      int    `json:"joined_count,omitempty"`      // 实际加入人数
```

- 群通话 outcome：`completed`（≥2 人加入过）、`missed`（只有发起人、
  无人加入）、`failed`（SFU 侧异常结束）。
- 记录消息由服务端在房间结束时以发起人为 sender 写入群会话。

### 3.6 媒体策略与已知限制

- 发布：视频在客户端支持时启用 simulcast 三层（720p/360p/180p）；
  订阅默认最低层，聚焦/大窗时切高层（客户端通过 subscribe 时的
  期望层级参数表达，服务端透传给 Cloudflare 的轨道选择字段；
  若 Cloudflare 侧以 track 为粒度则订阅时协商对应层）。
- 音频轨道必订；自己不发布的轨道不回订（防回声由客户端
  RTCPeerConnection 常规处理）。
- 费用：SFU 与 TURN 共用 Cloudflare Realtime 每月前 1000 GB
  出流量免费额度，超出 $0.05/GB；Cloudflare 无中国大陆节点，
  大陆成员延迟偏高——写进发版说明，不另做优化。
- 无离线推送：`group_call_started` 只达在线成员；未接到的成员
  事后只能在群里看到通话记录。客户端不得假装支持后台来电。

### 3.7 客户端（Web / Flutter 同构）

- 群聊页顶栏加通话按钮（语音/视频，`group_call_enabled=false`
  时隐藏）；群里有 open 房间时，会话列表与群聊页显示
  「通话中 · n 人，点击加入」条。
- 通话界面：宫格布局（备注优先显示名规则同 §1.4）、静音/关摄像头/
  离开、点某人放大（切高层订阅）、仅语音模式只订音频。
- 断线重连：WS 重连后 `GET /api/calls/group/{call_id}` 恢复成员
  与轨道，重新 subscribe；房间已结束则退出通话界面。

### 3.8 实现前关口（原型验证，不过关先回报）

用**真实 Calls App 凭据**跑通最小原型：服务端代理四接口
（建 session / publish / subscribe / renegotiate）+ Web×2 与
Flutter×1 三方同房音视频互通。凭据需用户在 Cloudflare 控制台
创建 Calls 应用后提供（App Secret 只进服务器 config，不进仓库、
不进聊天明文）。原型不过关时停下回报，不硬编完整功能。

## 4. 发布要求：Release 更新内容说明

（用户 2026-10-22 定，机制已落地，本版起执行。）

- 打 `v0.5.0` tag 前，**两仓** `CHANGELOG.md` 必须先写好
  `## v0.5.0 - YYYY-MM-DD` 小节（按 新增/修复/变更 写给用户看的
  内容）；Release workflow 会自动摘录为 GitHub Release 说明正文。
- 检查小节存在再打 tag 是发版流程的一步（见 `docs/RELEASE.md`）。

## 5. 版本、迁移与验收

- 服务端 / 网页端 `0.5.0`，Flutter `pubspec: 0.5.0+5`。
- 迁移 `005_v050.sql` 只建 `friend_requests`、`friend_edges` 两表；
  `users.allow_find` 走 `upgradeColumns`。新 YAML 键全部有默认值：
  旧配置文件不改也能启动（群通话默认关）。
- `/api/config` v0.5.0 新增字段汇总：`group_call_enabled`、
  `group_call_max_participants`。
- 冒烟测试新增 `cmd/smoketest/v050.go`：
  - 好友全流程：搜索（email/id 两入口）→ 申请 → 接受 → 备注 →
    列表显示名 → 删除 → 会话与记录仍在；反向待处理自动成友；
    `allow_find=0` 时搜索 404；各错误码（self / already_friends /
    request_pending / not_friends）逐一校验；WS 事件四种各验一次。
  - 「我的」：`PATCH /api/me` 改 allow_find 生效；
    `revoke-other-sessions` 后旧 token 失效、当前设备新 token 可用。
  - 群通话：假 SFU 桩（`calls.base_url` 指向桩，参数 `-calls-stub`
    复用 v040 桩并扩展 sessions/tracks 路径）验证代理形状与状态机：
    建房 → 第二人 join → publish 登记 → subscribe 组装 →
    leave → 房空写记录消息（mode=group、joined_count 正确）；
    room_full、busy 占用、bad_track 冒名各验一次。
    真 SFU 三方互通只在 §3.8 原型关口人工验，不进冒烟。
