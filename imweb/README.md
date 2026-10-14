# imweb v0.2.0

imserver 的 Web 客户端：React 19 + TypeScript + Vite + Material Web（Material You 3），桌面双栏、手机单栏自适应。

**v0.2.0 新增**

- **群聊**：新建群聊（右下角“新聊天” → 群聊，按邮箱添加成员）、群头像（成员首字拼图）、群名、成员数、发送者名字、系统消息、群公告横幅；群信息对话框：改群名、编辑公告（群主/管理员）、成员列表、邀请成员、移出成员、设/取消管理员、退出群聊；自己消息下显示“N人已读 / 全部已读”。
- **消息菜单**：桌面悬停气泡出现 😊 / ⋮ 按钮（或右键），手机长按气泡弹出底部菜单：表情回应（❤️👍👎😂‼️❓ + 任意表情）、回复、复制、撤回（2 分钟内）、删除（仅自己）。
- **撤回**：所有人看到“XX撤回了一条消息”，自己撤回的文字可“重新编辑”。
- **引用回复**：气泡内显示引用预览，点击跳转并高亮原消息（自动向前加载历史），原消息撤回后显示“原消息已撤回”。
- **表情回应**：气泡下方聚合显示，点一下切换，实时同步。
- **语音消息**：输入框为空时显示麦克风。点一下开始录音（再点 ➤ 发送 / 🗑 取消），或按住说话、松开发送、左滑取消；录音时显示时长、实时波形和 60 秒进度条（md-linear-progress）。优先录 `audio/webm;codecs=opus`，Safari 用 `audio/mp4`。气泡为 md-icon-button 播放/暂停 + 用 M3 颜色变量绘制的波形进度（跟随主题色），显示已播放/总时长，点击或拖动波形跳转。
- **语音本地永久保存**：服务器只保留 7 天。收到语音后立即通过预签名 URL 下载，以消息 ID（`会话ID:seq`）为键存进 IndexedDB（库 `imweb`，表 `voice`），播放优先用本地副本，并调用 `navigator.storage.persist()` 申请持久化存储。服务器已过期且本地没有时显示“语音已过期”。撤回或删除的语音会同时从本地删除。
- **离线同步**：重连后补拉新消息、撤回/回应变更（`changed_since`）和其他设备上的删除。

## 部署（直接用打包好的 dist）

> **推荐**：使用仓库根目录的一键安装脚本 `deploy/install.sh`（同时安装 imserver + imweb，装到 `/root/imserver`、`/var/www/imweb`），完整步骤见 [docs/DEPLOY.md](../docs/DEPLOY.md)。下面是手动部署方式，供参考。

```bash
sudo mkdir -p /var/www/imweb
sudo cp -r imweb-dist/* /var/www/imweb/          # dist 包里的 imweb-dist/ 目录
sudo chown -R caddy:caddy /var/www/imweb
sudo restorecon -Rv /var/www/imweb 2>/dev/null || true   # SELinux
sudo cp deploy/Caddyfile /etc/caddy/Caddyfile             # 域名不同请修改第一行；反代到 127.0.0.1:8092
sudo caddy validate --config /etc/caddy/Caddyfile && sudo systemctl reload caddy
```

更新网页时只需覆盖 `/var/www/imweb`。语音文件走独立的存储域名（如 `s3.fansa.ccwu.cc`，见 imserver 的升级文档），本站点的 Caddy 不需要额外配置；存储端需要允许本站点的 CORS（imserver 启动时会自动设置）。

## 开发

需要 Node.js 20+：

```bash
npm install
IM_SERVER=http://127.0.0.1:8092 npm run dev     # 本地开发，接口自动代理到 imserver
IM_SERVER=https://im.fansa.ccwu.cc npm run dev  # 也可以直接连线上服务端
npx tsc -p . && npx vite build                  # 类型检查 + 输出到 dist/
IM_SERVER=http://127.0.0.1:8092 npx vite preview --port 4173
```

本地测试语音时，imserver 的 `storage.cors_origins` 要包含开发页面的地址（如 `http://localhost:5173`）。

## 结构

```
src/api.ts                HTTP 接口
src/socket.ts             WebSocket：自动重连、心跳
src/store.ts              状态：会话、消息、发送队列、已读、离线补拉、群成员、撤回/回应/删除、语音上传下载
src/voice.ts              录音（MediaRecorder + 波形）、IndexedDB 语音缓存
src/theme.ts              Material You 动态配色（种子色 → --md-sys-color-*）
src/components/
  ChatView.tsx            聊天页（群聊头部、公告、已读人数、回复/跳转）
  MessageItem.tsx         单条消息：气泡、引用、表情回应、系统消息、撤回墓碑、长按
  MessageMenu.tsx         消息菜单（桌面浮层 / 手机底部弹出）
  VoiceBubble.tsx         语音气泡（播放、波形进度、拖动跳转）
  Composer.tsx            输入栏（文字 / 点按或按住录音）
  GroupInfo.tsx           群信息对话框
  ConversationList.tsx    会话列表（群头像、发送者前缀、撤回/系统消息预览）
  Dialogs.tsx             新建单聊/群聊、设置
deploy/Caddyfile          Caddy 配置示例
```

## 说明

- 不依赖 Google Fonts / 图标 CDN（图标为内联 SVG），国内可正常加载。
- 主题色、深浅色保存在浏览器本地。
- 消息先显示“发送中”，10 秒未确认标为失败，点感叹号重发（同一 client_msg_id，服务端去重）；语音会先重新上传（如果上次没传完）。
