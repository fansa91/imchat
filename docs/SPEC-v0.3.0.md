# IMChat v0.3.0 接口约定（服务端 / 网页端 / Flutter 共用）

范围：图片消息、在线状态、语音转文字、Flutter 主题色。所有新增都必须向后兼容：
旧客户端遇到未知消息类型显示“[不支持的消息，请升级]”，新客户端遇到旧服务端（`/api/config` 无新字段）时隐藏对应功能。

现有实现以代码为准：语音的预签名上传、消息 content 结构、`GET /api/conversations/{id}/messages/{seq}/media` 响应、WS 事件信封，图片都**照语音的写法镜像**。

## 1. 图片消息

### 上传
- `POST /api/uploads/presign`，`kind: "image"`。
  - 请求额外字段：`mime`（仅允许 `image/jpeg`、`image/png`、`image/webp`、`image/gif`）、`size`（字节）、`width`、`height`。
  - 限制：`size <= storage.image_max_bytes`（默认 10485760），否则 400 `too_large`；mime 不在白名单 400 `bad_mime`。
  - 对象 key：`storage.NewKey("image", uid, ext)`，即前缀 `image/`。
  - 响应结构与 voice 完全相同。
- 客户端上传前压缩：长边最大 2048px，JPEG 质量 0.85（PNG 有透明时可转 WebP 或保持 PNG；GIF 原样上传，不压缩）。同时读出压缩后的宽高。

### 发送
- WS `send`，`type: "image"`，content：`{"key": "...", "mime": "image/jpeg", "size": 12345, "width": 1024, "height": 768}`。
- 服务端校验与 voice 相同（key 必须是该用户的 image/ 前缀、`verify_upload` 开启时 HEAD 校验对象存在与大小）。
- 会话列表预览文字：`[图片]`。
- 撤回：与语音相同，删除 Garage 对象，双方清理本地缓存。

### 下载
- 复用 `GET /api/conversations/{id}/messages/{seq}/media` 拿短期 URL。
- 保留期：`storage.image_ttl_days`，默认 `0` = 永久保存（不加生命周期规则）。语音仍按 `voice_ttl_days`。
- 客户端：气泡按 width/height 预留比例占位（最大 260px 宽 / 320px 高），加载后显示；点击全屏查看（可缩放、可保存到本地/相册/下载）。本地永久缓存（Web: IndexedDB，与语音同库新 store；Flutter: `<documents>/IMChat/image/<host>/<cid>_<seq>.<ext>`）。

### 配置与 `/api/config`
- 新 YAML：`storage.image_max_bytes`（默认 10485760）、`storage.image_ttl_days`（默认 0）。
- `/api/config` 新增：`"image_enabled": true`（存储已配置时为 true）、`"image_max_bytes": 10485760`。

## 2. 在线状态

- 用户有任意一个 WS 连接即在线（多设备/多标签页计数，Redis 维护，跨实例正确）。
- 数据库 `users` 新增 `last_seen_at DATETIME(3) NULL`（迁移幂等），最后一个连接断开时写入。
- WS 下行事件：`{"type":"presence","data":{"user_id":123,"online":true,"last_seen":"RFC3339 或 null"}}`。
  - 只在状态翻转时推送（第一个连接建立 / 最后一个连接断开），推给与该用户同在至少一个会话中的成员。
- `GET /api/presence?ids=1,2,3`（最多 200 个）→ `[{"user_id":1,"online":true,"last_seen":null}, ...]`，只返回与请求者共享会话的用户，其他忽略。
- `/api/config` 新增：`"presence_enabled": true`。
- 客户端：
  - 单聊标题下显示“在线” / “最后在线 今天 14:03”（时间格式与现有 util 一致）；头像右下角绿点。
  - 群聊标题下显示“N 人在线”。
  - 登录/重连后对所有会话成员批量调 `/api/presence`，之后靠 presence 事件更新。

## 3. 语音转文字

### 配置
```yaml
stt:
  enabled: false
  provider: groq            # groq | cloudflare
  fallback: cloudflare      # 可留空；主提供方失败（5xx/超时/429）时尝试
  language: zh              # 可留空让模型自动识别
  daily_limit_per_user: 50  # 0 = 不限
  timeout_seconds: 30
  groq:
    api_key: ""
    model: whisper-large-v3-turbo
  cloudflare:
    account_id: ""
    api_token: ""
    model: "@cf/openai/whisper-large-v3-turbo"
```
- Groq：`POST https://api.groq.com/openai/v1/audio/transcriptions`，multipart（file, model, language, response_format=json）。
- Cloudflare：`POST https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/run/{model}`，JSON `{"audio": "<base64>", "language": "zh"}`，结果在 `result.text`。
- 音频由服务端从 Garage 读取（webm/opus 与 m4a 两种都直接送，两家都支持）。

### 接口
- `POST /api/conversations/{id}/messages/{seq}/transcribe` → `{"text":"...","cached":false}`。
  - 必须是会话成员、消息类型 voice、未撤回，否则 403/404/410 `recalled`。
  - 结果存表 `message_transcripts(conversation_id, seq, text, provider, created_at, PRIMARY KEY(conversation_id, seq))`，有缓存直接返回 `cached:true`，不计次数。
  - 每用户每天（Asia/Shanghai 日界）限额，超出 429 `stt_limit`（Redis 计数，次日过期）。
  - 未启用 501 `stt_disabled`；提供方全部失败 502 `stt_failed`。
  - 撤回消息时删除对应 transcript。
- `/api/config` 新增：`"stt_enabled": true/false`。

### 客户端
- 语音消息长按菜单（Flutter 长按/右键；Web 右键/长按）增加“转文字”（仅 `stt_enabled`）。
- 结果显示在语音气泡下方的小文字块，可复制；加载中显示“转写中…”；错误给 toast（限额用完：“今日转文字次数已用完”）。
- 本地缓存结果（Web: IndexedDB/localStorage；Flutter: shared_preferences 或文件），再次打开直接显示。

## 4. Flutter 主题色
- 与网页端 `imweb/src/theme.ts` 相同的 8 个种子色：`#6750A4 #0061A4 #006E1C #9C4146 #8B5000 #006A6A #7D5260 #4A6363`，配色用 Material 3 tonal spot（`ColorScheme.fromSeed`，`DynamicSchemeVariant.tonalSpot`）。
- 额外第一个选项“跟随系统”（dynamic_color；平台不支持时回退第一个种子色），默认“跟随系统”。
- 放在设置面板，与浅色/深色/跟随系统模式并列；持久化。

## 5. 版本
- 服务端 / 网页端 `0.3.0`，Flutter `pubspec: 0.3.0+3`。
- 服务端新增迁移必须幂等；新 YAML 键全部有默认值，旧配置文件不改也能启动（stt 默认关闭）。
