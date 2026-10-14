# imchat

类 iMessage 的即时通讯系统，单仓库（monorepo）包含服务端和网页客户端：

| 目录 | 说明 | 技术栈 |
| --- | --- | --- |
| [`imserver/`](imserver/) | 聊天服务端：注册登录、一对一/群聊、撤回、引用回复、表情回应、语音消息、多设备同步 | Go + MySQL + Redis + WebSocket，语音存 S3 兼容存储（推荐 Garage） |
| [`imweb/`](imweb/) | Web 客户端：桌面双栏 / 手机单栏自适应 | React 19 + TypeScript + Vite + Material Web（M3） |
| [`deploy/`](deploy/) | 一键安装脚本 | `install.sh`（imserver + imweb）、`install-garage.sh`（存储机） |
| [`docs/`](docs/) | 部署、发布、升级文档 | — |

## 架构

```
浏览器 ──HTTPS──> im.fansa.ccwu.cc（Caddy）
                    ├─ /api/* /ws /healthz ──> imserver 127.0.0.1:8092 ──> MySQL / Redis
                    └─ 其余 ──> /var/www/imweb（静态网页）
   │
   │  语音：预签名 PUT/GET 直传（不经过 imserver）
   └──HTTPS──> s3.fansa.ccwu.cc:8443（存储机上的 Caddy）──> Garage 127.0.0.1:3900
```

## 快速开始

**主机 A（imserver + 网页）**，root 执行：

```bash
# 私有仓库需要 token（Contents 只读权限即可）
export GH_TOKEN=<你的 GitHub token>
curl -fsSL -H "Authorization: Bearer $GH_TOKEN" -H "Accept: application/vnd.github.raw" \
  https://api.github.com/repos/fansa91/imchat/contents/deploy/install.sh -o install.sh
bash install.sh                 # 安装最新 Release；--version v0.2.0 指定版本
vi /root/imserver/config.yaml   # 填 MySQL 密码、storage 段
systemctl start imserver
```

**主机 S（Garage 存储）**：

```bash
bash install-garage.sh --capacity 50G --domain s3.fansa.ccwu.cc:8443
```

完整步骤（MySQL/Redis、Caddy、防火墙、OCI 安全列表、排错）见 **[docs/DEPLOY.md](docs/DEPLOY.md)**。

## 文档

- [docs/DEPLOY.md](docs/DEPLOY.md) —— 部署指南（全新安装、配置项说明、Garage、Caddy、防火墙、升级、卸载、排错）
- [docs/RELEASE.md](docs/RELEASE.md) —— 如何发布新版本（打 tag 自动构建 Release）
- [docs/UPGRADE-v0.2.0.md](docs/UPGRADE-v0.2.0.md) —— 从 v0.1.0 升级到 v0.2.0 的手动步骤（历史文档）
- [imserver/README.md](imserver/README.md) —— 服务端功能与目录结构
- [imweb/README.md](imweb/README.md) —— 网页端功能与开发说明

## 本地开发

```bash
# 服务端（需要 Go 1.26+、本机 MySQL 和 Redis）
cd imserver
cp config.example.yaml config.yaml   # 按需修改
go run ./cmd/server -c config.yaml

# 网页（需要 Node.js 20.19+ / 22+）
cd imweb
npm ci
IM_SERVER=http://127.0.0.1:8092 npm run dev
```

## CI / 发布

- `.github/workflows/ci.yml`：每次 push / PR 执行 `go vet` + `go build`（imserver）和 `npm ci` + `tsc` + `vite build`（imweb）。
- `.github/workflows/release.yml`：推送 `v*` 标签时编译 linux-amd64 / linux-arm64 静态二进制和网页，生成
  `imserver-<tag>-linux-amd64.tar.gz`、`imserver-<tag>-linux-arm64.tar.gz`、`imweb-<tag>-dist.tar.gz`、`SHA256SUMS` 并创建 GitHub Release。
