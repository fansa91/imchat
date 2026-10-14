# imserver v0.2.0

类 iMessage 聊天服务端：Go + MySQL + Redis + WebSocket，语音文件存放在 S3 兼容存储（推荐 Garage）。

**v0.1.0**：邮箱密码注册登录（JWT）、一对一聊天、多设备同步、消息重发幂等、已读回执、正在输入、离线消息补拉、历史分页、Redis Pub/Sub 多实例、离线推送接口（占位）。

**v0.2.0 新增**：

- **群聊**：建群、拉人、退群、改群名、群公告、群主/管理员移除成员、设置管理员、系统消息（入群/退群/改名/公告）、群已读人数、成员列表；每次发送/读取/拉历史都校验成员身份；新成员只能看到入群之后的消息；群成员上限可配置（默认 500）。
- **撤回 / 删除**：发送者 2 分钟内（可配置）可撤回，所有成员所有设备变成“XX撤回了一条消息”，离线设备通过 `changed_since` 补拉；“删除”为仅自己隐藏，同步到自己的所有设备。
- **引用回复 / 表情回应**：`reply_to_seq` 引用（预览实时计算，原消息撤回后显示已撤回）；iMessage 风格表情回应（❤️👍👎😂‼️❓ 或任意 emoji），可切换，聚合计数，实时同步。
- **语音消息**：客户端通过服务端签发的预签名 PUT 直传到 S3 存储，发送时服务端校验大小/类型/文件头；服务器只保留 7 天（启动时自动设置 bucket 生命周期规则 + CORS），客户端收到后永久保存在本地。

升级步骤见 [UPGRADE-v0.2.0.md](../docs/UPGRADE-v0.2.0.md)。

## 目录

```
cmd/server        主程序
cmd/smoketest     端到端冒烟测试（v0.1 + v0.2 共 79 项）
internal/config   YAML 配置加载
internal/store    MySQL 数据访问 + 建表/升级脚本（migrations/ + upgradeColumns）
internal/auth     bcrypt 密码 + JWT
internal/ws       WebSocket 连接管理、消息协议、Redis 跨节点分发、在线状态
internal/api      HTTP 接口（会话、群聊、上传、媒体）
internal/storage  S3 兼容存储：预签名、校验、生命周期、CORS
internal/push     离线推送接口
deploy/           systemd 服务（imserver、garage）、Garage 配置示例、存储机 Caddyfile
```

## 全新部署（OCI Ampere A1 / Ubuntu）

> **推荐**：使用仓库根目录的一键安装脚本 `deploy/install.sh`（同时安装 imserver + imweb，装到 `/root/imserver`、`/var/www/imweb`），完整步骤见 [docs/DEPLOY.md](../docs/DEPLOY.md)。下面是手动部署方式，供参考。

```bash
# 1. MySQL 8（或 MariaDB）与 Redis
sudo apt install -y mysql-server redis-server
sudo mysql -e "CREATE DATABASE im CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER 'im'@'127.0.0.1' IDENTIFIED BY '你的密码';
GRANT ALL ON im.* TO 'im'@'127.0.0.1';"

# 2. 安装程序（A1 用 arm64，x86 用 amd64）
sudo useradd -r -s /usr/sbin/nologin imserver
sudo mkdir -p /opt/imserver
sudo cp bin/imserver-linux-arm64 /opt/imserver/imserver
sudo cp config.example.yaml /opt/imserver/config.yaml
sudo nano /opt/imserver/config.yaml   # mysql 密码、jwt.secret（openssl rand -hex 32）、storage 段
sudo chown -R imserver:imserver /opt/imserver && sudo chmod 600 /opt/imserver/config.yaml

# 3. 开机自启
sudo cp deploy/imserver.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now imserver
journalctl -u imserver -f
```

首次启动自动建表（`mysql.auto_migrate: true`）。生产环境前面用 Caddy 做 HTTPS（见 imweb/deploy/Caddyfile，反代到 `127.0.0.1:8092`）。

语音需要 S3 兼容存储：推荐在另一台 A1 上用静态二进制部署 **Garage**，完整步骤见 [UPGRADE-v0.2.0.md](../docs/UPGRADE-v0.2.0.md) 第 1 节。不配置 `storage` 段时语音不可用，其他功能正常。

## 从源码编译

需要 Go 1.26+：

```bash
make all          # bin/imserver-linux-arm64 和 bin/imserver-linux-amd64
# 没有 make 时：
for a in arm64 amd64; do CGO_ENABLED=0 GOOS=linux GOARCH=$a go build -trimpath -ldflags "-s -w -X main.version=v0.2.0" -o bin/imserver-linux-$a ./cmd/server; done

# 冒烟测试（服务运行在 8092；带 -s3-* 参数时包含语音/生命周期/CORS 检查）
go run ./cmd/smoketest -base http://127.0.0.1:8092 \
  -s3-endpoint 127.0.0.1:3900 -s3-key GK... -s3-secret ... -bucket imchat -s3-region garage \
  -origin https://im.fansa.ccwu.cc
```

## 配置

见 `config.example.yaml`。v0.2.0 新增：

| 配置 | 默认 | 说明 |
|---|---|---|
| `group.max_members` | 500 | 群成员上限（含群主） |
| `message.recall_seconds` | 120 | 可撤回时间窗口（秒） |
| `storage.endpoint` | — | 服务端访问存储的 `host:port`（不带协议）；填了即启用语音 |
| `storage.use_ssl` | false | 服务端访问 endpoint 是否 https |
| `storage.public_endpoint` | 同 endpoint | 浏览器访问的地址（写进预签名 URL），须为独立域名、不能带路径 |
| `storage.access_key` / `secret_key` | — | S3 密钥（Garage：`garage key create`） |
| `storage.bucket` | imchat | bucket 名 |
| `storage.region` | garage | 必须与存储端一致（Garage 默认 `garage`，MinIO 默认 `us-east-1`）；显式设置后预签名不需要网络请求 |
| `storage.cors_origins` | `server.allow_origins` | 启动时写入 bucket CORS 的网页来源 |
| `storage.voice_ttl_days` | 7 | 服务器保留语音天数（生命周期规则，前缀 `voice/`） |
| `storage.voice_max_seconds` | 60 | 单条语音最长秒数 |
| `storage.voice_max_bytes` | 2097152 | 单条语音最大字节数 |
| `storage.set_lifecycle` | true | 启动时自动设置生命周期 + CORS（key 需 owner 权限，失败只打警告） |
| `storage.verify_upload` | true | 发送语音时检查对象（Stat + 读文件头 16 字节，共 2 次请求）；关闭则只做 key 归属/类型/时长/声明大小校验 |
| `storage.presign_put_seconds` / `presign_get_seconds` | 300 / 300 | 预签名有效期 |
| `storage.enabled` | 自动 | 可省略；显式 `false` 关闭语音 |

服务端访问存储的请求很少：启动时 1 次 HeadBucket + 生命周期/CORS 设置；每条语音发送时 2 次（可关）；撤回语音时 1 次删除。生成预签名 URL 和下载接口都不访问存储（下载接口按消息时间判断是否超过 `voice_ttl_days`）。

## HTTP 接口

除注册/登录外都需要请求头 `Authorization: Bearer <token>`。错误统一返回 `{code, message}`。

### v0.1.0（保持兼容）

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | /api/register | `{email, password, nickname}` → `{token, expires_at, user}` |
| POST | /api/login | `{email, password}` → `{token, expires_at, user}` |
| GET | /api/me | 当前用户 |
| GET | /api/users/search?email= | 按邮箱查找用户 |
| POST | /api/conversations/direct | `{peer_id}` → `{conversation_id}`，两人之间唯一 |
| GET | /api/conversations | 会话列表（见下方“会话对象”） |
| GET | /api/conversations/{id}/messages | `?after_seq=N` 补拉；`?before_seq=N` 翻历史；`?changed_since=M` 补拉变更（v0.2）；都不传返回最新一页；`limit` 最大 200。自动排除自己删除的消息和入群前的消息 |
| GET | /api/conversations/{id}/reads | 各成员已读位置 `{"用户ID": read_seq}`；群聊用它算“N人已读” |
| GET | /healthz | 健康检查 |

### v0.2.0 新增

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | /api/config | 客户端配置：`{recall_seconds, group_max_members, reactions[], voice_enabled, voice_max_seconds, voice_max_bytes, voice_ttl_days}` |
| GET | /api/conversations/{id} | 单个会话对象（必须是成员） |
| POST | /api/conversations/group | 建群 `{name, member_ids[]}`（name 可空，自动取前 3 人昵称）→ 201 `{conversation_id, conversation}` |
| PATCH | /api/conversations/{id} | `{name?}` 改群名（任何成员）；`{announcement?}` 改群公告（群主/管理员，空字符串为清空）→ 会话对象 |
| GET | /api/conversations/{id}/members | 成员列表 `[{user, role, read_seq, joined_at}]`，群主、管理员在前；role：0 成员 1 管理员 2 群主 |
| POST | /api/conversations/{id}/members | 拉人 `{user_ids[]}`（任何成员可拉，最多 100/次，已在群里的忽略）→ `{added[]}` |
| DELETE | /api/conversations/{id}/members/{uid} | 移除成员：群主可移除任何人，管理员只能移除普通成员 |
| PUT | /api/conversations/{id}/members/{uid}/role | `{role: "admin"\|"member"}`，仅群主 |
| POST | /api/conversations/{id}/leave | 退群；群主退群时自动转让给最早入群的管理员（没有则最早入群的成员） |
| GET | /api/conversations/{id}/hidden?min_seq= | 自己删除（隐藏）的 seq 列表，供其他设备同步 |
| POST | /api/uploads/presign | 申请上传 `{kind:"voice", mime, size, duration_ms}` → `{key, url, method:"PUT", headers, expires_at, max_bytes}`；客户端用返回的 `headers` 直接 PUT 到 `url` |
| GET | /api/conversations/{id}/messages/{seq}/media | 媒体下载地址 → `{url, mime, size, expires_at}`（预签名 GET，默认 5 分钟）；已撤回 410 `recalled`；超过保留期 410 `expired` |

**会话对象**（`/api/conversations` 列表项）：

```
id, type(1 单聊 2 群聊), last_seq, mod_seq, read_seq, join_seq, unread, updated_at,
last_message, last_sender_name(群聊),
peer(单聊对方),
name, owner_id, my_role, member_count, members_preview[最多4人], announcement, announcement_by, announcement_at  (群聊)
```

**消息对象**：

```
conversation_id, seq, sender_id, client_msg_id, msg_type, content, created_at,
reply_to_seq?, reply_to?{seq, sender_id, msg_type, preview, recalled, missing?},
recalled?, recalled_at?, mod_seq?, reactions?[{emoji, count, user_ids[]}]
```

`msg_type`：1 文本，2 图片（预留），3 语音，4 视频（预留），5 文件（预留），10 系统消息。

- 语音 `content`（JSON 字符串）：`{"key":"voice/{uid}/{yyyymm}/{hex}.webm","mime":"audio/webm","duration_ms":3200,"size":12345,"waveform":[0~100 最多 64 个]}`
- 系统消息 `content`（JSON 字符串）：`{"op":"create|join|leave|remove|rename|announce|role","actor":{id,nickname},"targets":[{id,nickname}],"name":"新群名","value":"公告内容/admin|member","text":"可直接显示的中文"}`
- 撤回后 `recalled=true`、`content` 清空、表情回应清空；语音文件同时从存储删除。

## WebSocket 协议

连接：`wss://host/ws?token=<token>`（或 Authorization 头）。帧格式：

```json
{"type": "send", "id": "客户端请求ID(可选，回复原样带回)", "data": {...}}
```

客户端发送：

| type | data | 说明 |
|---|---|---|
| send | `{client_msg_id, conversation_id, msg_type, content, reply_to_seq?}` | 发消息；`client_msg_id` 用于重发去重。语音：先 presign + PUT，再 `msg_type:3` 发送 |
| read | `{conversation_id, seq}` | 已读到第几条 |
| typing | `{conversation_id}` | 正在输入 |
| recall | `{conversation_id, seq}` | **v0.2** 撤回自己的消息（时间窗口内） |
| react | `{conversation_id, seq, emoji, on?}` | **v0.2** 表情回应；`on` 省略为切换，`true` 添加，`false` 取消；每条消息最多 20 种表情 |
| delete | `{conversation_id, seqs[]}` | **v0.2** 仅自己删除，1~100 条 |
| ping | `{}` | 心跳 |

服务端推送：

| type | data | 说明 |
|---|---|---|
| send_ack | `{message, duplicate}` | 发送成功（含 seq） |
| message | `{message}` | 新消息（含自己其他设备发的、系统消息） |
| read | `{conversation_id, user_id, read_seq}` | 已读位置变化 |
| typing | `{conversation_id, user_id}` | 正在输入 |
| recall | `{conversation_id, seq, user_id, recalled_at, mod_seq, message}` | **v0.2** 消息被撤回，`message` 为墓碑 |
| reaction | `{conversation_id, seq, user_id, emoji, on, reactions[], mod_seq}` | **v0.2** 回应变化，`reactions` 为该消息完整聚合结果 |
| deleted | `{conversation_id, seqs[]}` | **v0.2** 自己（任一设备）删除了这些消息 |
| conversation_updated | `{conversation_id, reason}` | **v0.2** 群信息/成员变化，客户端调 `GET /api/conversations/{id}` 刷新 |
| conversation_removed | `{conversation_id, reason: left\|removed}` | **v0.2** 自己已不在该会话 |
| ack | `{op: recall\|react\|delete, ...}` | **v0.2** 对带 `id` 的 recall/react/delete 请求的回执 |
| pong / error | `{}` / `{code, message}` | |

常见错误码：`forbidden`（不是成员/无权限）、`recall_expired`、`already_recalled`、`not_found`、`too_many`、`unsupported`（未启用存储）、`rate_limited`。

## 客户端同步要点

1. 发消息先本地显示“发送中”，收到 `send_ack` 后用 `seq` 落库；超时用**同一个** `client_msg_id` 重发。
2. 每次连上 WebSocket 后调 `/api/conversations`：
   - `last_seq` 大于本地 → `after_seq` 补拉新消息；
   - `mod_seq` 大于本地 → `changed_since=本地mod_seq` 补拉撤回/回应变化，用返回的消息整条替换本地；
   - 调 `/hidden?min_seq=本地最早seq` 同步其他设备上的删除。
3. 会话内按 `seq` 排序。自己删除的消息不会出现在结果里，seq 可能不连续，属于正常现象。
4. 收到 `message` 且 `msg_type=10` 时刷新会话信息（群名、成员数、公告）。
5. 语音：收到即通过 `/media` 下载并永久保存在本地（服务器只保留 `voice_ttl_days` 天）；下载返回 410 或存储返回 403/404 时显示“语音已过期”。

## 多实例

每台设置不同的 `server.node_id`，连同一个 MySQL、Redis 和存储，前面挂负载均衡即可；消息、撤回、回应、群事件都通过 Redis `im:events` 频道跨节点投递。

## 下一步

- 图片消息（上传链路已按 `kind` 设计，增加 `image` 类型即可）
- 接入 APNs / FCM（实现 `internal/push.Notifier`）
- 端到端加密
