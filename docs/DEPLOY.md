# imchat 部署指南

本文档说明如何在 **Oracle Linux 8/9** 或 **Ubuntu 22.04/24.04**（OCI Ampere A1 arm64 或 x86_64 均可）上部署 imchat。

```
               ┌──────────────── 主机 A（imserver + 网页）────────────────┐
浏览器 ─HTTPS─> │ Caddy :443  im.fansa.ccwu.cc                             │
               │   ├─ /api/* /ws /healthz ─> imserver 127.0.0.1:8092 ─> MySQL / Redis
               │   └─ 其余 ─> /var/www/imweb                               │
               └──────────────────────────────────────────────────────────┘
   │ 语音预签名 PUT/GET（浏览器直传）              ▲ imserver 少量 S3 API 调用
   ▼                                              │
               ┌──────────────── 主机 S（Garage 存储）────────────────────┐
               │ Caddy :8443  s3.fansa.ccwu.cc ─> Garage S3 127.0.0.1:3900 │
               └──────────────────────────────────────────────────────────┘
```

- **主机 A**：imserver、MySQL、Redis、Caddy、网页静态文件。
- **主机 S**：Garage（S3 兼容对象存储，只存语音，默认保留 7 天）。也可以和主机 A 是同一台，见 [6.4](#64-garage-和-imserver-在同一台机器)。
- 不需要语音功能时可以完全跳过主机 S，把 `config.yaml` 的 `storage` 段删掉即可。

目录：

1. [前提条件](#1-前提条件)
2. [安装 MySQL 和 Redis](#2-安装-mysql-和-redis主机-a)
3. [运行 install.sh 安装 imserver + imweb](#3-运行-installsh-安装-imserver--imweb主机-a)
4. [config.yaml 配置说明](#4-configyaml-配置说明)
5. [Caddy 配置（主机 A）](#5-caddy-配置主机-a)
6. [Garage 存储（主机 S）](#6-garage-存储主机-s)
7. [防火墙与 OCI 安全列表](#7-防火墙与-oci-安全列表)
8. [升级](#8-升级)
9. [卸载](#9-卸载)
10. [排错](#10-排错)

---

## 1. 前提条件

| 项目 | 要求 |
| --- | --- |
| 系统 | Oracle Linux 8/9（含 SELinux Enforcing）、Ubuntu 22.04/24.04，使用 systemd |
| 架构 | arm64（aarch64）或 amd64（x86_64），脚本自动识别 |
| 权限 | root（`sudo -i`）。程序装在 `/root/imserver`、`/root/garage`，服务以 root 运行 |
| 域名 | 网页域名（如 `im.fansa.ccwu.cc`）指向主机 A；存储域名（如 `s3.fansa.ccwu.cc`）指向主机 S |
| GitHub Token | 仓库是私有的：需要一个对 `fansa91/imchat` 有 **Contents: Read-only** 权限的 fine-grained token（或 classic token 的 `repo` 权限），通过环境变量 `GH_TOKEN` 传给脚本 |
| 工具 | `curl`、`tar`、`openssl`、`python3` 或 `jq`——缺少时脚本会自动用 apt/dnf 安装 |

下载安装脚本（私有仓库通过 API 读取文件）：

```bash
export GH_TOKEN=<你的 token>
curl -fsSL -H "Authorization: Bearer $GH_TOKEN" -H "Accept: application/vnd.github.raw" \
  https://api.github.com/repos/fansa91/imchat/contents/deploy/install.sh -o install.sh
curl -fsSL -H "Authorization: Bearer $GH_TOKEN" -H "Accept: application/vnd.github.raw" \
  https://api.github.com/repos/fansa91/imchat/contents/deploy/install-garage.sh -o install-garage.sh
```

> 仓库改成公开后，`GH_TOKEN` 可以不设，脚本直接从 `https://github.com/fansa91/imchat/releases` 下载。

---

## 2. 安装 MySQL 和 Redis（主机 A）

### Ubuntu

```bash
apt update
apt install -y mysql-server redis-server
systemctl enable --now mysql redis-server
```

### Oracle Linux 8/9

```bash
dnf install -y mysql-server redis
systemctl enable --now mysqld redis
```

> 也可以用 MariaDB 10.6+（`apt install mariadb-server` / `dnf install mariadb-server`），用法相同。

### 建库建用户

```bash
DB_PASS=$(openssl rand -hex 16); echo "MySQL 密码：$DB_PASS"   # 记下来，填进 config.yaml
mysql -uroot <<SQL
CREATE DATABASE IF NOT EXISTS im CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER IF NOT EXISTS 'im'@'127.0.0.1' IDENTIFIED BY '${DB_PASS}';
GRANT ALL PRIVILEGES ON im.* TO 'im'@'127.0.0.1';
FLUSH PRIVILEGES;
SQL
```

表结构由 imserver 启动时自动创建/升级（`mysql.auto_migrate: true`），不需要手动导入 SQL。

Redis 默认只监听 `127.0.0.1:6379`、无密码，保持默认即可（不要对公网开放 6379）。

---

## 3. 运行 install.sh 安装 imserver + imweb（主机 A）

```bash
export GH_TOKEN=<你的 token>
bash install.sh                       # 最新版本
# 或：bash install.sh --version v0.2.0
```

参数：

| 参数 | 说明 | 默认 |
| --- | --- | --- |
| `--version vX.Y.Z` | 安装指定版本（`0.2.0` 也可以，会自动补 `v`） | 最新 Release |
| `--upgrade` | 升级：替换二进制和网页，**保留 config.yaml**，重启服务 | — |
| `--port N` | imserver 监听端口（只在**首次生成** config.yaml 时写入） | `8092` |
| `--web-dir DIR` | 网页静态文件目录 | `/var/www/imweb` |
| `--uninstall` | 卸载（保留 `/root/imserver/config.yaml`）；加 `--purge` 一并删除 | — |

脚本做了什么：

1. 识别架构，从 GitHub Release 下载 `imserver-<版本>-linux-<arch>.tar.gz` 和 `imweb-<版本>-dist.tar.gz`，并用 `SHA256SUMS` 校验。
2. 安装二进制到 `/root/imserver/imserver`（旧版本备份为 `imserver.bak`）。SELinux 为 Enforcing 时执行 `chcon -t bin_t`。
3. **只有 `/root/imserver/config.yaml` 不存在时**才从 `config.example.yaml` 生成，并自动：
   - `server.addr` 改为 `127.0.0.1:8092`（只监听本机，由 Caddy 反代）；
   - `jwt.secret` 填入随机值。
   已存在的 config.yaml **永远不会被覆盖或修改**。
4. 写入 `/etc/systemd/system/imserver.service`（root 运行），`systemctl enable`。
5. 网页解压到 `/var/www/imweb`（上一版保留在 `/var/www/imweb.bak`），目录权限 755、文件 644；有 `caddy` 用户时 `chown caddy:caddy`；SELinux 下恢复 `httpd_sys_content_t` 标签。
6. 首次安装**不会自动启动**（配置还没填）；配置已存在或 `--upgrade` 时会（重）启动并检查 `/healthz`。

脚本可以重复运行（幂等）。首次安装后：

```bash
vi /root/imserver/config.yaml      # 见第 4 节
systemctl start imserver
journalctl -u imserver -f          # 看到 "imserver listening" 即成功
curl -s http://127.0.0.1:8092/healthz
```

---

## 4. config.yaml 配置说明

文件位置：`/root/imserver/config.yaml`（权限 600）。完整示例见 [`imserver/config.example.yaml`](../imserver/config.example.yaml)。

### server

| 字段 | 说明 |
| --- | --- |
| `addr` | 监听地址。默认端口 **8092**；推荐 `"127.0.0.1:8092"`，只让本机 Caddy 访问 |
| `node_id` | 节点名，多实例部署时每台不同 |
| `allow_origins` | WebSocket 允许的 Origin，如 `["https://im.fansa.ccwu.cc"]`；留空不校验 |

### mysql / redis / jwt

| 字段 | 说明 |
| --- | --- |
| `mysql.host/port/user/password/database` | 第 2 节创建的库和用户 |
| `mysql.auto_migrate` | `true`：启动时自动建表/升级（可重复执行） |
| `redis.addr/password/db` | 默认 `127.0.0.1:6379` |
| `jwt.secret` | 登录令牌签名密钥，至少 32 位随机串（install.sh 已自动生成）。**修改后所有用户需重新登录** |
| `jwt.ttl_hours` | 登录有效期，默认 720（30 天） |

### storage（语音存储，S3 兼容）

不需要语音时整段删除（或写 `enabled: false`），其他功能不受影响。

| 字段 | 说明 | 示例 |
| --- | --- | --- |
| `endpoint` | **imserver 访问存储**的地址，`host:port`，**不带** `http://` | 跨机器：`s3.fansa.ccwu.cc:8443`；同 VCN 内网：`10.0.0.25:3900`；同机：`127.0.0.1:3900` |
| `use_ssl` | imserver 访问 `endpoint` 是否用 HTTPS | 走 Caddy 域名：`true`；直连 3900：`false` |
| `public_endpoint` | **浏览器访问存储**的地址，写进预签名 URL。必须是独立（子）域名，可带端口，**不能带路径** | `https://s3.fansa.ccwu.cc:8443` |
| `access_key` | S3 Key ID（`garage key create` 输出的 `GK...`） | `GK8e96ec85...` |
| `secret_key` | S3 Secret key（`garage key create` 输出的 `Secret key`）。**不是 rpc_secret！** | 64 位十六进制 |
| `bucket` | bucket 名 | `imchat` |
| `region` | 必须与存储端一致：Garage 为 `garage`，MinIO 为 `us-east-1` | `garage` |
| `cors_origins` | 启动时写入 bucket CORS 的网页来源；留空用 `server.allow_origins`，再空则 `*` | `["https://im.fansa.ccwu.cc"]` |
| `voice_ttl_days` | 服务器保留语音的天数（启动时写 bucket 生命周期规则）。客户端收到后本地永久保存 | `7` |
| `voice_max_seconds` | 单条语音最长秒数 | `60` |
| `voice_max_bytes` | 单条语音最大字节数 | `2097152`（2MB） |
| `set_lifecycle` | 启动时自动设置生命周期 + CORS（密钥需要 **owner** 权限） | `true` |
| `verify_upload` | 发送语音时服务端校验对象大小和文件头（每条 2 次 S3 请求）；跨地域嫌慢可改 `false` | `true` |
| `presign_put_seconds` / `presign_get_seconds` | 上传/下载链接有效期 | `300` |

> `endpoint` 和 `public_endpoint` 可以不同：例如 imserver 走内网 `10.0.0.25:3900`（`use_ssl: false`），浏览器走 `https://s3.fansa.ccwu.cc:8443`。
> 但 S3 签名包含 Host，`public_endpoint` 必须和浏览器实际访问的 `域名:端口` **完全一致**。

### group / message / log

| 字段 | 说明 |
| --- | --- |
| `group.max_members` | 群成员上限（含群主），默认 500 |
| `message.recall_seconds` | 可撤回时限，默认 120 秒 |
| `log.level` | `debug` / `info` / `warn` / `error` |

修改配置后：`systemctl restart imserver`。

---

## 5. Caddy 配置（主机 A）

### 安装 Caddy

```bash
# Ubuntu
apt install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' > /etc/apt/sources.list.d/caddy-stable.list
apt update && apt install -y caddy

# Oracle Linux 8/9
dnf install -y 'dnf-command(copr)'
dnf copr enable -y @caddy/caddy
dnf install -y caddy
```

### /etc/caddy/Caddyfile

```caddyfile
im.fansa.ccwu.cc {
    encode zstd gzip

    # 接口、WebSocket、健康检查 → imserver
    @backend path /api/* /ws /healthz
    handle @backend {
        reverse_proxy 127.0.0.1:8092 {
            stream_close_delay 5m   # reload Caddy 时不立刻断开 WebSocket
        }
    }

    # 其余 → 网页（单页应用，找不到的路径回落到 index.html）
    handle {
        root * /var/www/imweb
        try_files {path} /index.html
        file_server

        @assets path /assets/*
        header @assets Cache-Control "public, max-age=31536000, immutable"
        @html path / /index.html
        header @html Cache-Control "no-cache"
    }
}
```

端口不是 8092 时同步修改 `reverse_proxy`。应用：

```bash
caddy validate --config /etc/caddy/Caddyfile && systemctl enable --now caddy && systemctl reload caddy
```

Caddy 会自动申请 HTTPS 证书，需要 80/443 对外开放（第 7 节）。

---

## 6. Garage 存储（主机 S）

### 6.1 运行 install-garage.sh

```bash
bash install-garage.sh \
  --capacity 50G \
  --bucket imchat \
  --key-name imserver-key \
  --domain s3.fansa.ccwu.cc:8443 \
  --cors-origin https://im.fansa.ccwu.cc
```

| 参数 | 说明 | 默认 |
| --- | --- | --- |
| `--version` | Garage 版本（官方静态二进制：`https://garagehq.deuxfleurs.fr/_releases/<版本>/<target>/garage`） | `v2.1.0` |
| `--capacity` | 本节点容量（布局用，单节点时不是硬限制） | `20G` |
| `--bucket` | bucket 名 | `imchat` |
| `--key-name` | 访问密钥名 | `imserver-key` |
| `--domain` | 浏览器访问存储的 `域名[:端口]`，只用于打印配置片段 | `s3.fansa.ccwu.cc:8443` |
| `--cors-origin` | 允许跨域的网页来源 | `https://im.fansa.ccwu.cc` |

> 截至 2026-10，Garage 最新稳定版是 v2.4.1，可用 `--version v2.4.1` 安装；脚本使用的 CLI 命令在 v2.x 中通用。

脚本做了什么：

1. 下载对应架构的官方静态二进制（`aarch64-unknown-linux-musl` / `x86_64-unknown-linux-musl`）到 `/root/garage/garage`，SELinux 下 `chcon -t bin_t`。
2. **仅在不存在时**生成 `/root/garage/garage.toml`：`metadata_dir=/root/garage/meta`、`data_dir=/root/garage/data`、`db_engine="lmdb"`、`replication_factor=1`、`rpc_bind_addr=127.0.0.1:3901`、随机 `rpc_secret`、S3 API `127.0.0.1:3900`（region `garage`）、管理 API `127.0.0.1:3903` 和随机 `admin_token`。并建立软链接 `/etc/garage.toml`，之后直接运行 `/root/garage/garage status` 即可。
3. 写入 `/etc/systemd/system/garage.service`（root 运行，**不启用 ProtectHome**），启动服务。
4. 初始化：`layout assign -z dc1 -c <容量> <节点ID>` + `layout apply --version 1`（已有布局则跳过）、`bucket create imchat`、`key create imserver-key`（已存在则跳过）、`bucket allow --read --write --owner`。
5. 如果装了 `aws` 命令行，设置 bucket CORS；否则由 imserver 启动时自动设置（`storage.set_lifecycle: true`）。
6. 打印 **Key ID / Secret key** 和可直接粘贴的 imserver `storage:` 配置片段，并保存到 `/root/garage/imserver-credentials.txt`（600）。

以后再看密钥：

```bash
/root/garage/garage key info --show-secret imserver-key
```

### 6.2 rpc_secret 与 S3 Secret key：千万不要混用

| | `rpc_secret` | S3 `Secret key` |
| --- | --- | --- |
| 在哪里 | `/root/garage/garage.toml` | `garage key create` / `garage key info --show-secret` 的输出 |
| 谁生成 | `openssl rand -hex 32`（安装脚本） | Garage |
| 用途 | Garage **节点之间内部 RPC** 通信的共享密钥（多节点集群时所有节点相同） | **S3 客户端签名**（imserver 用它签发预签名 URL、设置 CORS/生命周期） |
| 填到哪里 | 只留在 garage.toml，**不填到任何客户端** | imserver `config.yaml` 的 `storage.secret_key`（配套的 `GK...` 填 `storage.access_key`） |

两者都是 64 位十六进制串，长得很像，但**绝不能互换**：把 `rpc_secret` 填进 `storage.secret_key` 会导致 `SignatureDoesNotMatch` / `InvalidAccessKeyId`，语音无法上传。

### 6.3 存储机上的 Caddy（非 443 端口，如 8443）

存储机的 443 若已被其他服务占用，可以用 8443：

```caddyfile
# /etc/caddy/Caddyfile（主机 S）
s3.fansa.ccwu.cc:8443 {
    reverse_proxy 127.0.0.1:3900 {
        flush_interval -1        # 不缓冲，上传/下载流式转发
    }
    request_body {
        max_size 10MB            # 语音最大 2MB，留余量
    }
}
```

要点：

- **保留原始 Host**（Caddy 默认就会透传），不要加路径前缀、不要改写 URL，否则 S3 签名校验失败。
- **不要在 Caddy 里加 CORS 头**：CORS 由 Garage 根据 bucket 的 CORS 配置返回（imserver 启动时写入），Caddy 再加会出现重复的 `Access-Control-Allow-Origin` 导致浏览器拒绝。
- 对应的 imserver 配置：`endpoint: "s3.fansa.ccwu.cc:8443"`、`use_ssl: true`、`public_endpoint: "https://s3.fansa.ccwu.cc:8443"`。
- 证书：Caddy 默认用 HTTP-01（80 端口）或 TLS-ALPN（443 端口）申请证书。站点在 8443 时，**主机 S 的 80 端口必须对外开放**；若 80 也不可用，需要改用 DNS-01（例如带 Cloudflare 插件的 Caddy + `tls { dns cloudflare <token> }`）。
- 用 443 时把站点地址写成 `s3.fansa.ccwu.cc` 即可（见 `imserver/deploy/Caddyfile.s3`），对应 `endpoint: "s3.fansa.ccwu.cc:443"`、`public_endpoint: "https://s3.fansa.ccwu.cc"`。

验证（返回 S3 的 XML 错误如 `AccessDenied` 即说明链路通了）：

```bash
curl -i https://s3.fansa.ccwu.cc:8443/
```

#### Cloudflare：存储域名建议用“仅 DNS”（灰色云朵）

- Cloudflare 代理（橙色云朵）**确实支持 8443**（HTTPS 代理端口：443、2053、2083、2087、2096、8443，见 <https://developers.cloudflare.com/fundamentals/reference/network-ports/>），所以开代理并不会因为端口而失败。
- 但仍**推荐存储域名设为 DNS only（灰色云朵）**：
  - 预签名 URL 的签名包含 Host 和完整路径，经过代理时任何改写（规则、Transform、缓存键等）都可能导致 `SignatureDoesNotMatch`；
  - 8443 等非标准端口在代理下不缓存，开代理对语音没有加速收益；
  - 证书由 Caddy 直接签发，避免 SSL 模式（Flexible/Full）配置错误造成重定向循环。
- 如果一定要开橙色云朵：SSL/TLS 模式选 **Full (strict)**，不要启用对该域名的缓存/改写规则。
- 网页域名 `im.fansa.ccwu.cc` 开不开代理都可以；开代理时同样选 Full (strict)，并确认 WebSocket 已启用（默认开启）。

### 6.4 Garage 和 imserver 在同一台机器

直接在主机 A 运行 `install-garage.sh`，然后：

```yaml
storage:
  endpoint: "127.0.0.1:3900"      # imserver 直连本机
  use_ssl: false
  public_endpoint: "https://s3.fansa.ccwu.cc:8443"   # 浏览器仍需走 HTTPS 独立域名
```

Caddyfile 中追加 6.3 的 `s3.fansa.ccwu.cc:8443 { ... }` 站点块。

### 6.5 同 VCN 内网直连（可选）

主机 A、S 在同一 VCN 时，可以让 imserver 走内网省流量：把 garage.toml 的 `[s3_api] api_bind_addr` 改为 `"0.0.0.0:3900"`，重启 Garage，在安全列表中**只允许主机 A 的内网 IP** 访问 3900，然后 imserver 配置 `endpoint: "10.0.0.x:3900"`、`use_ssl: false`。`public_endpoint` 仍为浏览器用的 HTTPS 域名。

---

## 7. 防火墙与 OCI 安全列表

需要对公网开放的端口：

| 主机 | 端口 | 用途 |
| --- | --- | --- |
| A | 80/tcp、443/tcp | Caddy（HTTPS + 证书申请） |
| S | 80/tcp、8443/tcp（或 443） | Caddy（证书申请 + S3 HTTPS） |

**不要**对公网开放：8092（imserver）、3306（MySQL）、6379（Redis）、3900/3901/3903（Garage）。

### 7.1 OCI 安全列表 / 网络安全组

控制台 → **Networking → Virtual Cloud Networks → 你的 VCN → Security Lists → Default Security List → Add Ingress Rules**：

- Source CIDR `0.0.0.0/0`，IP Protocol `TCP`，Destination Port Range `80`
- 同上，端口 `443`（主机 A）/ `8443`（主机 S）

（实例若使用了网络安全组 NSG，需在 NSG 中添加同样的规则。）

### 7.2 Oracle Linux（firewalld）

```bash
# 主机 A
firewall-cmd --permanent --add-service=http --add-service=https
# 主机 S（8443）
firewall-cmd --permanent --add-service=http --add-port=8443/tcp
firewall-cmd --reload
firewall-cmd --list-all
```

### 7.3 Ubuntu（OCI 官方镜像使用 iptables 规则）

OCI 的 Ubuntu 镜像在 `/etc/iptables/rules.v4` 中有一条 `REJECT` 规则，新规则必须插在它**前面**：

```bash
# 查看规则编号，找到 "REJECT ... reject-with icmp-host-prohibited" 那一行
iptables -L INPUT --line-numbers

# 插到 REJECT 之前（假设 REJECT 在第 6 行）
iptables -I INPUT 6 -m state --state NEW -p tcp --dport 80 -j ACCEPT
iptables -I INPUT 6 -m state --state NEW -p tcp --dport 443 -j ACCEPT    # 主机 A
iptables -I INPUT 6 -m state --state NEW -p tcp --dport 8443 -j ACCEPT   # 主机 S

# 持久化
apt install -y iptables-persistent   # 已安装可跳过
netfilter-persistent save
```

若启用了 ufw：`ufw allow 80/tcp && ufw allow 443/tcp`（主机 S 改为 `8443/tcp`）。

---

## 8. 升级

### imserver + imweb（主机 A）

```bash
export GH_TOKEN=<你的 token>
bash install.sh --upgrade                    # 升到最新
bash install.sh --upgrade --version v0.3.0   # 升到指定版本
```

升级会替换二进制和网页、**保留 config.yaml**、重启服务。数据库结构由 imserver 启动时自动升级（只加表/加列）。
建议升级前备份数据库：

```bash
mysqldump --single-transaction im > ~/im-backup-$(date +%F).sql
```

回滚：

```bash
mv /root/imserver/imserver.bak /root/imserver/imserver && systemctl restart imserver
rm -rf /var/www/imweb && mv /var/www/imweb.bak /var/www/imweb
```

新版本若新增配置项，对比 `/root/imserver/config.example.yaml`（每次安装都会更新）手动补到 `config.yaml`。

### Garage（主机 S）

```bash
systemctl stop garage
bash install-garage.sh --version v2.4.1      # 只替换二进制，配置/布局/bucket/key 保持不变
```

跨大版本升级前请阅读 Garage 的发布说明：<https://git.deuxfleurs.fr/Deuxfleurs/garage/releases>。

---

## 9. 卸载

```bash
bash install.sh --uninstall           # 停止并删除服务、二进制、/var/www/imweb；保留 /root/imserver/config.yaml
bash install.sh --uninstall --purge   # 连同 /root/imserver 整个目录一起删除
```

MySQL/Redis 数据和 Caddy 配置不会被动。彻底清理数据库：`mysql -uroot -e "DROP DATABASE im; DROP USER 'im'@'127.0.0.1';"`。

卸载 Garage（**会删除全部语音数据**）：

```bash
systemctl disable --now garage
rm -f /etc/systemd/system/garage.service /etc/garage.toml
systemctl daemon-reload
rm -rf /root/garage
```

---

## 10. 排错

| 现象 | 原因与解决 |
| --- | --- |
| `systemctl status imserver` 显示 **`status=203/EXEC`** | ① SELinux（Oracle Linux 默认 Enforcing）：`/root` 下的文件标签是 `admin_home_t`，systemd 不允许执行。解决：`chcon -t bin_t /root/imserver/imserver`（Garage 同理：`chcon -t bin_t /root/garage/garage`），确认：`ls -Z /root/imserver/imserver`。② 服务单元开启了 `ProtectHome=true`，程序在 `/root` 下就看不到——安装脚本生成的单元已去掉。③ 二进制架构不对（arm64 机器装了 amd64）：`file /root/imserver/imserver`。 |
| 启动后立即退出 | `journalctl -u imserver -n 50 --no-pager`。常见：MySQL 密码错误、Redis 未启动、`config.yaml` 缩进错误、`storage` 段缺 `access_key/secret_key`。 |
| 网页 **403 / 404** | ① `/var/www/imweb` 及上级目录需要 `755`、文件 `644`：`chmod 755 /var/www /var/www/imweb && find /var/www/imweb -type f -exec chmod 644 {} +`；② SELinux 标签：`restorecon -Rv /var/www/imweb`（或 `chcon -R -t httpd_sys_content_t /var/www/imweb`）；③ 不要把网页放在 `/root` 下，caddy 用户进不去。 |
| 网页正常但登录/发消息 **502** | imserver 没运行或端口不一致：`curl -s http://127.0.0.1:8092/healthz`，检查 Caddyfile 的 `reverse_proxy` 端口与 `server.addr` 一致。 |
| WebSocket 频繁断开 | 确认 Caddyfile 中 `/ws` 在 `@backend` 里；`server.allow_origins` 包含网页域名（含 `https://`）。 |
| 语音上传 **CORS 错误** | ① 存储机的 Caddy 不要加 CORS 头；② `storage.cors_origins` 包含网页来源，`set_lifecycle: true` 且密钥有 owner 权限（`garage bucket allow --owner ...`），重启 imserver 让它重新写入 CORS；③ 查看 imserver 启动日志里设置 CORS 是否报错。 |
| **`SignatureDoesNotMatch`** | ① `storage.secret_key` 填成了 `rpc_secret`（见 6.2）；② `region` 不是 `garage`；③ `public_endpoint` 与浏览器实际访问的 `域名:端口` 不一致（8443 端口必须写上）；④ Caddy 或 Cloudflare 改写了 Host/路径——存储域名改为灰色云朵。 |
| **`InvalidAccessKeyId`** | `access_key` 填错（应为 `GK` 开头），或 key 被删除：`/root/garage/garage key list`。 |
| `garage status` 报错连接不上 | `systemctl status garage`、`journalctl -u garage -n 50`；确认 `/etc/garage.toml` 软链接存在，或用 `/root/garage/garage -c /root/garage/garage.toml status`。 |
| Garage 节点显示 `NO ROLE ASSIGNED` | 布局没应用：`garage layout assign -z dc1 -c 20G <节点ID>` 然后 `garage layout apply --version <当前版本+1>`（再次运行 install-garage.sh 也会自动完成）。 |
| Caddy 申请证书失败 | 80 端口未开放（安全列表 + 防火墙），或域名在 Cloudflare 橙色云朵下 SSL 模式不对；查看 `journalctl -u caddy -n 100`。 |
| install.sh 报“读取 Release 失败” | 私有仓库未设置 `GH_TOKEN`，或 token 没有该仓库的 Contents 读权限，或版本号不存在。 |
