# IMChat v0.4.0 接口约定（服务端 / 网页端 / Flutter 共用）

范围：1 对 1 音视频通话（P2P + Cloudflare TURN）、文件/视频消息、头像/昵称编辑、
重置密码/修改密码（含登录态吊销）。所有新增都必须向后兼容：
旧客户端遇到未知消息类型显示“[不支持的消息，请升级]”，新客户端遇到旧服务端
（`/api/config` 无新字段）时隐藏对应功能。

现有实现以代码为准：文件/视频照图片（v0.3.0）的预签名直传镜像；通话信令照现有
WS 事件信封（`internal/ws/protocol.go`）；新配置段的校验照 STT 的写法
（`enabled=true` 但凭据不全时启动报错，未启用时不校验）。

**明确不做（本版排除）**：

- **离线推送**：不接 APNs / FCM / 厂商通道，`internal/push` 占位实现保持不动。
  后果见 §1.6 的通话限制，客户端不得假装支持后台来电。
- 群通话、Cloudflare Realtime SFU（§1 预留切换点，但不实现）、RealtimeKit
  （其 Flutter SDK 已停更，禁用）。
- 消息搜索、置顶/免打扰、消息编辑/转发、端到端加密（后续版本）。

## 1. 1 对 1 音视频通话

媒体走 WebRTC P2P 直连，Cloudflare Realtime TURN 做 NAT 穿透兜底；
信令全部复用现有 WebSocket，服务端只做状态机、校验与转发，不解析 SDP 内容。

### 1.1 配置与 `/api/config`

```yaml
calls:
  enabled: false                 # 总开关
  ring_timeout_seconds: 60       # 响铃超时，未接听自动结束
  credential_ttl_seconds: 14400  # TURN 临时凭证有效期（发给 Cloudflare 的 ttl）
  turn_key_id: ""                # Cloudflare TURN Key ID
  turn_api_token: ""             # Cloudflare TURN API Token（只存服务端）
  # base_url: "https://rtc.live.cloudflare.com/v1"   # 默认值；测试/代理时可改
```

- `calls.enabled=true` 时 `turn_key_id`/`turn_api_token` 必填，缺一启动报错
  （照 `STTConfig` 的校验方式）。
- `/api/config` 新增：`"call_enabled": true/false`（enabled 且凭据齐全时为 true）、
  `"call_ring_timeout_seconds": 60`。

### 1.2 ICE 配置下发

- `GET /api/calls/ice-servers`（需登录）→ 服务端调 Cloudflare
  `POST {calls.base_url}/turn/keys/{turn_key_id}/credentials/generate-ice-servers`
  （`Authorization: Bearer <turn_api_token>`，body `{"ttl": credential_ttl_seconds}`），
  把 201 响应原样透传并加过期时间：

  ```json
  {"iceServers": [{"urls": ["stun:stun.cloudflare.com:3478"]},
                  {"urls": ["turn:turn.cloudflare.com:3478?transport=udp", "..."],
                   "username": "...", "credential": "..."}],
   "expires_at": "RFC3339"}
  ```

- 服务端按用户在 Redis 缓存凭证（key `im:turn:{uid}`，TTL 为
  `credential_ttl_seconds - 300` 秒），缓存命中不重复调 Cloudflare。
- 未启用通话时 501 `call_disabled`。Cloudflare 调用失败 502 `turn_failed`。
- 客户端在发起/接听通话前拉取一次，通话中不刷新；凭证不得写日志、不得落盘。

### 1.3 信令（WS 事件）

客户端发送（信封格式与现有事件一致，`type` 如下）：

| type | data | 说明 |
| --- | --- | --- |
| `call_invite` | `{call_id, conversation_id, kind}` | 发起通话；`kind: "audio"\|"video"`；`call_id` 由主叫客户端生成（UUID，与 `client_msg_id` 同理，用于重试去重） |
| `call_accept` | `{call_id}` | 被叫接听 |
| `call_reject` | `{call_id}` | 被叫拒绝 |
| `call_cancel` | `{call_id}` | 主叫在接通前取消 |
| `call_hangup` | `{call_id, reason?}` | 任一方挂断；`reason` 省略为正常挂断，`"failed"` 表示本地 ICE/媒体失败上报 |
| `call_signal` | `{call_id, signal}` | WebRTC 协商透传；`signal: {kind: "offer"\|"answer"\|"candidate", sdp?, candidate?}`，服务端不解析、只转发，单帧 ≤ 64KB |

服务端推送：

| type | data | 说明 |
| --- | --- | --- |
| `call_invite` | `{call_id, conversation_id, from_user_id, kind, created_at}` | 被叫的**所有在线设备**都收到并响铃 |
| `call_accept` / `call_reject` / `call_cancel` / `call_hangup` | `{call_id, conversation_id, user_id, reason?}` | 原样转发给对方所有在线设备；多设备接听时，其他设备另收一条 `call_hangup {reason: "answered_elsewhere"}` 并停止响铃 |
| `call_signal` | `{call_id, from_user_id, signal}` | 转发给对方所有在线设备，由发起方设备之外的设备忽略（带 `from_user_id` 与连接标识区分由客户端处理：仅正在通话的设备响应） |
| `call_unavailable` | `{call_id, conversation_id, reason}` | 服务端直接终结：`reason: "offline"`（被叫无在线连接）或 `"busy"`（任一方已在另一通通话中） |
| `error` | `{code: "call_busy"\|"forbidden"\|"not_found", ...}` | 对带 `id` 的非法 call 请求回执（非会话成员、call 不存在、状态不允许） |

规则：

- v0.4.0 只允许**单聊**（会话 `type=1`）通话；群会话发起返回 `forbidden`。
- 所有 call 事件服务端先校验：发送者是该会话成员、`call_id` 对应的通话存在
  且发送者是参与方，否则丢弃并回 `error`（带请求 `id` 时）。
- 角色固定：主叫是 offer 方。主叫发出 `call_invite` 后即可开始发
  `call_signal(offer/candidate)`；被叫点接听后才创建 PeerConnection、
  setRemoteDescription 并回 `answer`（响铃阶段不申请摄像头/麦克风权限，
  接听/发起时才申请）。
- 通话状态存 Redis（多实例一致）：`im:call:{call_id}` hash
  `{caller, callee, conversation_id, kind, state(ringing|connected), created_at, connected_at}`，
  TTL `ring_timeout_seconds + 7200`；`im:call:user:{uid}` → `call_id`
  保证每人同时只有一通通话（发起/被邀时已占用即 busy）。
- 状态流转只能由服务端推进：`ringing → connected`（accept）、
  任意状态 → 已终结（reject/cancel/hangup/unavailable/超时）。
  终结时删除上述两个 key，**只有成功删除状态的一方写通话记录**（§1.4），防多节点重复写。
- 响铃超时由服务端计时（状态 key 的过期回调不可靠，用独立的
  `im:call:ring:{call_id}` 带 TTL 的 key + Hub 巡检或 accept 时惰性检查均可，
  实现二选一，但行为必须一致）：超时后双方收 `call_hangup {reason: "timeout"}`，
  记录 outcome 为 `missed`。
- 被叫离线：服务端在 invite 时用现有 `Hub.IsOnline` 判断，无在线连接直接回
  `call_unavailable {reason: "offline"}` 并写 `missed` 记录，不排队、不补发。

### 1.4 通话记录消息

- 新增消息类型 `MsgCall = 6`（`internal/store` 常量；现有 4=视频、5=文件见 §2）。
  **只能由服务端生成**：客户端 `send` 用 `msg_type=6` 返回 `bad_request`；
  服务端消息校验的合法范围仍为 1–5（客户端可发）加系统消息 10。
- 通话终结时以主叫为 `sender_id` 插入一条消息，content（JSON 字符串）：

  ```json
  {"call_id": "...", "kind": "video", "outcome": "completed",
   "duration_ms": 312000, "caller_id": 123}
  ```

  `outcome`：`completed`（接通后挂断）/ `missed`（超时或对方离线）/
  `rejected` / `cancelled` / `busy` / `failed`（任一方上报 `reason:"failed"`
  或从未接通且信号阶段异常终结）。`duration_ms` 仅 `completed` 非 0，
  为 `connected_at` 到终结的时长。
- 会话列表预览：`store.PreviewText` 新增 `MsgCall` 分支，
  返回 `"[语音通话]"` / `"[视频通话]"`（按 `kind`）；客户端在消息流内
  按 content 渲染详情（如「视频通话 05:12」「未接听」「对方已拒绝」），
  渲染样式照 iMessage 的居中系统样式或普通气泡由各客户端统一选一种，三端一致即可。
- 通话记录可被“仅自己删除”（`delete`），不可撤回（`recall` 返回
  `forbidden`）、不可引用回复（`reply_to_seq` 指向它时按原消息不存在处理），
  可加表情回应（与普通消息一致）。
- 离线补拉、`changed_since` 等现有同步机制对 `msg_type=6` 自动生效，无需新接口。

### 1.5 客户端（Web / Flutter）

- Web：原生 `RTCPeerConnection`；Flutter：`flutter_webrtc` 插件。
  两端都从 `GET /api/calls/ice-servers` 取 `iceServers` 原样构造连接。
- 入口：单聊标题栏/加号菜单的「语音通话」「视频通话」，仅 `call_enabled` 时显示；
  视频通话中可切摄像头、关摄像头降级为纯语音（关闭 video track，不重协商）、
  静音、免提/听筒切换（Flutter）。
- 来电 UI：全屏来电页（头像、昵称、类型、接听/拒绝）；通话中页面显示
  计时、对方画面（视频）、本地小窗。Android 在通话中用前台服务保活
  （沿用现有媒体前台服务权限模式）；iOS 用 AVAudioSession 的 playback 类别。
- 权限：发起/接听时才申请麦克风（和摄像头）；被拒绝时 toast 提示并以
  `call_hangup {reason: "failed"}` 终结。

### 1.6 已知限制（无离线推送的代价，写进客户端提示与发版说明）

- 被叫**没有任何在线 WS 连接时打不通**（见 `offline` 规则）：App 被系统杀死、
  iOS 后台被冻结、桌面端已退出时都收不到来电，只会事后看到「未接听」记录。
- iOS 不接 VoIP Push 就没有系统级来电：App 在前台、或后台但进程未冻结且
  WS 未断时能响铃，接听体验用 App 内来电页实现，本版不接 CallKit。
- Cloudflare TURN 无中国大陆节点，国内用户中继时绕境外节点，延迟高于直连；
  直连成功时不受影响。

### 1.7 预留（不在本版实现）

切 SFU 时 `GET /api/calls/ice-servers` 与 §1.3 的信令信封不变，只需把
PeerConnection 的对端从对方客户端换成 SFU 会话；群通话在此基础上
把状态机的参与方从 2 人扩为多人。`call_id`、通话记录格式均不改。

## 2. 文件 / 视频消息

现状：`store` 已有常量 `MsgVideo = 4`、`MsgFile = 5`，
`PreviewText` 已返回 `[视频]` / `[文件]`，WS `send` 也接受 1–5，
但 presign 没有对应 kind、服务端不校验 content、客户端无入口。本节补齐。

### 2.1 上传

- `POST /api/uploads/presign` 新增 `kind: "video"` 与 `kind: "file"`，
  响应结构与 voice/image 完全相同，对象 key 前缀分别为 `video/`、`file/`。
  - `video`：请求字段 `mime`（仅 `video/mp4`、`video/webm`、`video/quicktime`）、
    `size`、`duration_ms`、`width`、`height`；
    `size <= storage.video_max_bytes`（默认 104857600）否则 400 `too_large`；
    mime 不在白名单 400 `bad_mime`；时长上限 `storage.video_max_seconds`
    （默认 600）否则 400 `bad_duration`。
  - `file`：请求字段 `mime`（任意，不设白名单）、`size`、`name`（原始文件名）；
    `size <= storage.file_max_bytes`（默认 104857600）否则 400 `too_large`；
    `name` 必填，去掉路径分隔符与控制字符后 1–128 个字符，否则 400 `bad_name`。
- 视频封面：客户端截首帧后按图片流程上传（`kind: "image"`），content 里以
  `poster_key` 引用，服务端校验其为该用户的 `image/` 前缀（存在时）。
- 客户端：视频发送前压缩到最长边 1280px / 720p 档（尽力而为，压缩失败发原文件）；
  文件原样上传。大文件上传必须显示进度（Web: XHR progress；
  Flutter: 带进度回调的 PUT），可取消（取消即不发送消息，已上传对象不清理，
  与现有 presign 后未发送的行为一致）。

### 2.2 发送与下载

- WS `send`，content（JSON 字符串）：
  - 视频 `msg_type: 4`：`{"key": "...", "mime": "video/mp4", "size": 12345, "duration_ms": 32000, "width": 1280, "height": 720, "poster_key": "image/..."}`（`poster_key` 可选，其余必填）
  - 文件 `msg_type: 5`：`{"key": "...", "mime": "application/pdf", "size": 12345, "name": "报告.pdf"}`（`name` 必填，与 presign 时一致）
- 服务端校验与图片相同（key 必须是该用户的对应前缀、`verify_upload` 开启时
  HEAD 校验对象存在与大小），另校验视频的 `duration_ms`/宽高为正、
  文件的 `name` 规则同 presign。校验不过回 `error` 且不落库。
- 下载复用 `GET /api/conversations/{id}/messages/{seq}/media`；
  保留期新配置 `storage.file_ttl_days`、`storage.video_ttl_days`（默认均为 `0`
  = 永久），超期 410 `expired`；撤回时删对象（视频同时删其封面对象）。
- 会话列表预览：服务端 `PreviewText` 维持 `[视频]` / `[文件]`；
  客户端渲染会话列表时，文件消息可自行拼为 `[文件] {name}`（截断），不改服务端。

### 2.3 配置与 `/api/config`

- 新 YAML：`storage.file_max_bytes`（默认 104857600）、`storage.video_max_bytes`
  （默认 104857600）、`storage.video_max_seconds`（默认 600）、
  `storage.file_ttl_days` / `storage.video_ttl_days`（默认 0）。
- `/api/config` 新增：`"file_enabled": true`、`"file_max_bytes": ...`、
  `"video_enabled": true`、`"video_max_bytes": ...`（均为存储已配置时 true/有值，
  与 `image_enabled` 同规则）。

### 2.4 客户端

- 入口：输入栏加号菜单「文件」「视频」（相册视频/拍摄/文件选择），
  仅对应 `*_enabled` 时显示。
- 气泡：视频按宽高预留比例占位 + 封面 + 时长角标，点击全屏播放
  （Web: `<video>`；Flutter: 既有播放器组件），播放走 `/media` 的短期 URL，
  不做本地永久缓存（文件太大）；文件气泡显示图标（按扩展名）、文件名、大小，
  点击下载并打开/保存，本地缓存目录镜像图片：
  `<documents>/IMChat/file/<host>/`、`<documents>/IMChat/video/<host>/`，
  缓存上限由客户端 LRU 清理（默认各 500MB，设置页可见）。
- Web 端文件下载用浏览器下载（`a[download]` 指向 `/media` URL 时需带鉴权：
  先调 `/media` 拿短期 URL 再触发下载，与图片保存一致）。

## 3. 头像 / 昵称编辑

现状：`users.avatar_url` 字段存在但没有写入路径。本节补上传与修改，
并把头像改为走鉴权下发，避免把 Garage 对象地址直接当永久 URL。

### 3.1 数据与下发

- `users` 新增列（走 `upgradeColumns` 幂等添加，与 `last_seen_at` 同方式）：
  `avatar_key VARCHAR(512) NOT NULL DEFAULT ''`。
- 用户对象（`User`）的 `avatar_url` 字段**格式不变**，但取值改为服务端拼装：
  `avatar_key` 非空时返回 `/api/users/{id}/avatar?v={avatar_key 的短哈希}`，
  为空时返回 `""`。旧数据里手工填过的 `avatar_url` 在 `avatar_key` 为空时
  继续原样返回（迁移不清空）。
- `GET /api/users/{id}/avatar`（需登录）：302 跳转到该对象短期预签名 GET
  （有效期用 `storage.presign_get_seconds`）；无头像 404 `no_avatar`；
  未配置存储 503 `storage_disabled`。客户端头像组件带 token 请求后跟随跳转，
  并按 URL（含 `v` 参数）做本地缓存，`v` 变即刷新。

### 3.2 上传与修改

- 头像上传走 presign 新增 `kind: "avatar"`：mime 与图片相同
  （JPEG/PNG/WebP/GIF），`size <= storage.avatar_max_bytes`
  （默认 2097152），key 前缀 `avatar/`。客户端上传前裁成正方形、
  最长边 512px、JPEG 质量 0.85。
- `PATCH /api/me`（需登录）：`{nickname?, avatar_key?}`，两字段至少传一个。
  - `nickname`：去首尾空白后 1–64 个字符，否则 400 `bad_nickname`。
  - `avatar_key`：必须是该用户的 `avatar/` 前缀，`verify_upload` 开启时
    HEAD 校验存在；传空字符串表示清除头像。设置成功后尽力删除旧头像对象
    （失败只记日志）。
  - 响应为更新后的完整用户对象（同 `GET /api/me`）。
- 变更通知：服务端向“与该用户共享至少一个会话的成员 + 本人所有设备”
  推 WS 事件 `user_updated {user}`（user 为完整用户对象，推送范围与
  `presence` 相同）。客户端收到后更新本地用户缓存、会话列表与成员列表中
  的昵称/头像；未收到的场景（离线）靠下次拉会话列表自然刷新。
  旧客户端忽略未知事件（现有信封机制已保证）。

### 3.3 客户端

- 设置/个人资料页：点头像更换（相册/拍照/文件），昵称可编辑保存；
  Web 与 Flutter 行为一致。入口不受 `/api/config` 新字段控制
  （`PATCH /api/me` 在旧服务端返回 404 时客户端提示“服务端不支持”即可，
  不预先隐藏；头像上传依赖存储，未配置存储时头像行置灰）。

## 4. 重置密码 / 修改密码

现状：注册/登录为邮箱+密码（bcrypt），服务端**没有任何发信能力**；
JWT 为无状态校验（claims 只有用户 ID 与过期时间），改密码后旧 token
无法吊销。本节同时补发信、重置流程与 token 吊销机制。

### 4.1 登录态吊销（token_version）

- `users` 新增列（`upgradeColumns` 幂等）：`token_version INT NOT NULL DEFAULT 0`。
- JWT claims 新增 `"tv"`（token_version）。签发时写入用户当前值；
  校验（HTTP `authed` 中间件与 WS 握手共用 `auth.Verify`）返回 `(uid, tv)` 后，
  再比对当前值：当前值读 Redis 缓存 `im:tv:{uid}`（未命中查 DB 回填，
  TTL 24 小时；改密/重置时直接覆写缓存，多实例一致）。
- **兼容**：旧 token 无 `tv` claim 时按 `0` 处理；用户从未改密/重置时
  `token_version=0`，旧 token 继续有效，升级不需要全员重登。
  第一次改密/重置后版本变 1，之前所有 token（含无 `tv` 的）全部失效。
- 版本变更时服务端向该用户所有在线连接推 `session_revoked {reason}`
  （`reason: "password_changed" | "password_reset"`）后断开连接
  （跨节点走现有 `im:events` 通道，Hub 增加按用户断开能力）；
  客户端收到后清本地 token 回登录页。进行重置/改密的当前设备按 §4.3 的
  返回值换新 token，不被踢。

### 4.2 配置（SMTP）

```yaml
smtp:
  enabled: false
  host: ""              # 如 smtp.example.com
  port: 587
  username: ""
  password: ""
  from: ""              # 发件邮箱，如 no-reply@example.com
  tls_mode: "starttls"  # starttls | tls | none
  reset_url_base: ""    # 网页端公开地址，如 https://im.fansa.ccwu.cc（拼重置链接用）
  token_ttl_minutes: 30
  rate_limit_per_hour: 5   # 同一邮箱每小时最多发几封重置邮件（0 = 不限）
```

- `enabled=true` 时 host/from/reset_url_base 必填，缺一启动报错（照 STT 校验）。
  发信实现只用 Go 标准库（`net/smtp` + `crypto/tls`），超时 10 秒，不加新依赖。
- `/api/config` 新增：`"password_reset_enabled": true/false`
  （`smtp.enabled` 且配置完整时 true）。

### 4.3 接口

| 方法 | 路径 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| POST | `/api/password/forgot` | 无 | `{email}` → 恒定 200 `{ok: true}` |
| POST | `/api/password/reset` | 无 | `{token, new_password}` → 200 `{ok: true}` |
| POST | `/api/me/password` | 有 | `{old_password, new_password}` → 200 `{token, expires_at, user}` |

**forgot**：

- 邮箱格式非法 400 `bad_email`（与注册同一 `normalizeEmail`）；
  格式合法时**无论账号是否存在都返回 200**（防账号枚举），不存在则不发信。
- 限流（Redis 计数、1 小时窗口）：同邮箱超 `rate_limit_per_hour` 时
  静默不发信、仍回 200；同 IP 每小时最多 20 次，超出回 429 `rate_limited`。
- 生成 token：`crypto/rand` 32 字节 base64url 明文只出现在邮件链接里；
  库里只存 SHA-256 哈希。新表 `password_reset_tokens`
  （迁移 `004_v040.sql`）：`token_hash CHAR(64) PK, user_id, expires_at, used_at NULL, created_at, request_ip`。
  同一用户新申请时删除其旧的未用 token（一人同时只有一个有效链接）。
- 发信在后台 goroutine 异步执行（响应时间不泄露账号是否存在），
  失败只记服务端日志。邮件：主题「重置你的 IMChat 密码」，纯文本+HTML 各一份，
  正文含链接 `{reset_url_base}/reset-password?token={明文}`、有效期分钟数、
  “不是你本人请忽略”提示。
- 服务端未启用 SMTP 时该接口返回 501 `reset_disabled`（客户端按
  `password_reset_enabled` 本就隐藏入口，此为兜底）。

**reset**：

- 按哈希查 token：不存在/已过期/已使用统一 400 `bad_token`
  （文案“链接无效或已过期”，不区分原因）。
- `new_password` 规则与注册一致（8–72 个字符），否则 400 `bad_password`。
- 成功在一个事务内：更新 `password_hash`（bcrypt）、标记 token 已用、
  删除该用户其他 token、`token_version + 1`（并覆写 Redis 缓存、
  按 §4.1 断开其全部连接）。响应不发新 token，客户端跳登录页用新密码登录。

**修改密码（已登录）**：

- 校验 `old_password`（bcrypt 比对），错 403 `wrong_password`；
  新密码规则同上且不得与旧密码相同（400 `same_password`）。
- 成功后的副作用与 reset 相同（版本+1、断开其他连接），但响应返回
  新签发的 `{token, expires_at, user}`，当前设备无感续用。

### 4.4 客户端

- 登录页「忘记密码」：仅 `password_reset_enabled` 时显示。点开填邮箱提交
  forgot 接口，提交后统一提示“如果该邮箱已注册，重置邮件已发送”
  （不按返回值区分）。Flutter 端提交后引导用户去邮箱点链接（链接落在
  imweb 的重置页，改完回 App 用新密码登录），App 内不做深链处理。
- imweb 新增公开路由 `/reset-password`：从 query 取 `token`，表单输新密码
  （两次确认）调 reset 接口；成功跳登录页并提示，失败显示统一文案。
  该路由不需要登录态，SPA 的 Caddy 回退规则已覆盖。
- 设置页「修改密码」：输旧密码+新密码调 `/api/me/password`，成功后
  用返回的新 token 替换本地 token，toast 提示“其他设备已退出登录”。

## 5. 版本、迁移与验收

- 服务端 / 网页端 `0.4.0`，Flutter `pubspec: 0.4.0+4`。
- 迁移 `004_v040.sql` 只建 `password_reset_tokens` 表；`users` 的
  `token_version`、`avatar_key` 两列走 `upgradeColumns`（现有幂等方式）。
  新 YAML 键全部有默认值：旧配置文件不改也能启动
  （通话默认关、SMTP 默认关、文件/视频随存储自动开）。
- `/api/config` v0.4.0 新增字段汇总：`call_enabled`、
  `call_ring_timeout_seconds`、`file_enabled`、`file_max_bytes`、
  `video_enabled`、`video_max_bytes`、`password_reset_enabled`。
- 冒烟测试新增 `cmd/smoketest/v040.go`：
  - 文件/视频 presign（超限、bad_mime、bad_name）+ 发送校验 + `/media` 下载 +
    撤回删对象（照 v030 的 S3 参数开关）；
  - 头像 presign → `PATCH /api/me` → `user_updated` 事件 →
    `GET /api/users/{id}/avatar` 302；
  - 密码：起假 SMTP 桩（照 STT 桩的模式，参数 `-smtp-stub`）验证 forgot
    发信与防枚举响应、reset 全流程、旧 token 失效、新 token 可用、
    已登录改密后其他连接收 `session_revoked`；
  - 通话：两个 WS 客户端跑 invite→accept→signal 透传→hangup 全流程并校验
    通话记录消息；被叫离线 `offline`、并发第二通 `busy`、响铃超时
    `missed`；ICE 接口用假 Cloudflare 桩验证（`calls.base_url` 指向桩，
    参数 `-calls-stub`），不真调 Cloudflare。
