# 升级到 v0.2.0（imserver + imweb）

v0.2.0 新增群聊、撤回/删除、引用回复、表情回应、语音消息。数据库**自动原地升级**（只加表/加列，不删数据，可重复执行），WebSocket 协议向后兼容（旧网页还能收发文字）。

语音文件存放在 **S3 兼容存储（推荐 Garage）**，部署在**另一台 A1（另一个地域/租户）**上，通过公网 HTTPS 域名访问：

```
浏览器 ──HTTPS──> im.fansa.ccwu.cc（Caddy）──> imserver 127.0.0.1:8092 ──> MySQL / Redis
   │                                              │
   │  预签名 PUT/GET（直传，不经过 imserver）       │ 少量 S3 API 调用（启动设置 + 每条语音校验）
   └──────HTTPS──> s3.fansa.ccwu.cc（存储机上的 Caddy）──> Garage 127.0.0.1:3900
```

下文：**主机 A** = 现在跑 imserver + Caddy + 网页的机器；**主机 S** = 新的存储机（Garage）。

> 先在主机 A 备份数据库：`mysqldump --single-transaction im > ~/im-backup-$(date +%F).sql`

---

## 1. 主机 S：安装 Garage（静态二进制）

### 1.1 下载二进制并校验

Garage 官方提供静态编译的单文件二进制（musl），不需要 apt/deb/rpm/docker。下载页：<https://garagehq.deuxfleurs.fr/download/>

```bash
GARAGE_VERSION=v2.4.1
ARCH=aarch64-unknown-linux-musl          # A1 (arm64)；x86 机器用 x86_64-unknown-linux-musl
curl -fL -o /tmp/garage "https://garagehq.deuxfleurs.fr/_releases/${GARAGE_VERSION}/${ARCH}/garage"

sha256sum /tmp/garage
# v2.4.1 aarch64 实测值（2026-10-14 下载）：
# 004b6081c6e3cccbb80317c69e3c5749fbb3fc889e217cc9b89c215bf0dd7ded
# 官方下载页没有单独的校验文件；不一致时请重新下载或从源码 tag 自行编译后再继续。

chmod +x /tmp/garage
/tmp/garage --version                    # 应输出 garage v2.4.1 ...
sudo install -m 0755 /tmp/garage /usr/local/bin/garage
```

### 1.2 用户、目录、配置

```bash
sudo useradd -r -s /usr/sbin/nologin -d /var/lib/garage garage
sudo mkdir -p /var/lib/garage/meta /var/lib/garage/data
sudo chown -R garage:garage /var/lib/garage
sudo chmod 750 /var/lib/garage
```

如果数据要放在块存储卷上（推荐），先把卷挂到 `/var/lib/garage`（或把 `data_dir` 指到卷上，例如 `/data/garage`，并相应修改 systemd 里的 `ReadWritePaths`）。

`/etc/garage.toml`（模板见 `imserver/deploy/garage.toml.example`）：

```bash
RPC_SECRET=$(openssl rand -hex 32)
sudo tee /etc/garage.toml >/dev/null <<EOF
metadata_dir = "/var/lib/garage/meta"
data_dir = "/var/lib/garage/data"
db_engine = "lmdb"
metadata_auto_snapshot_interval = "6h"

replication_factor = 1

rpc_bind_addr = "127.0.0.1:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = "${RPC_SECRET}"

[s3_api]
s3_region = "garage"
api_bind_addr = "127.0.0.1:3900"
root_domain = ".s3.fansa.ccwu.cc"

[admin]
api_bind_addr = "127.0.0.1:3903"
EOF
sudo chown root:garage /etc/garage.toml && sudo chmod 640 /etc/garage.toml
```

S3 API 只监听 `127.0.0.1:3900`，对外统一走 Caddy 的 443；3900/3901/3903 都**不要**对公网开放。

### 1.3 systemd

```bash
sudo cp deploy/garage.service /etc/systemd/system/garage.service   # 在 imserver 包里
sudo systemctl daemon-reload
sudo systemctl enable --now garage
systemctl status garage --no-pager
journalctl -u garage -n 30 --no-pager
```

### 1.4 集群布局、bucket、密钥

`garage` 命令默认读 `/etc/garage.toml`，需要能读配置的用户执行（用 sudo）。

```bash
sudo garage status                                   # 记下 ID 列的节点 ID（16 位十六进制前缀即可）
sudo garage layout assign -z dc1 -c 50G <节点ID>     # 容量按磁盘大小填
sudo garage layout apply --version 1
sudo garage status                                   # 确认节点已有容量、HEALTHY

sudo garage bucket create imchat
sudo garage key create imserver-key                  # 输出 Key ID（GK 开头）和 Secret key，记下来
sudo garage bucket allow --read --write --owner imchat --key imserver-key
sudo garage bucket info imchat                       # 确认 imserver-key 权限为 RWO
# 以后再查看密钥：sudo garage key info imserver-key --show-secret
```

`--owner` 是必须的：imserver 启动时要通过 S3 API 设置 bucket 的 **7 天生命周期规则**和 **CORS**。

---

## 2. 主机 S：Caddy（HTTPS 域名 s3.fansa.ccwu.cc）

### 2.1 DNS / Cloudflare

在 Cloudflare 为 `s3.fansa.ccwu.cc` 添加 A 记录指向**主机 S 的公网 IP**。两种方式二选一：

- **推荐：仅 DNS（灰色云朵）**。Caddy 自动申请 Let's Encrypt 证书，存储流量不经过 Cloudflare，没有缓存问题。
- **代理（橙色云朵）**：SSL/TLS 模式设为 **完全（严格）**，并添加一条缓存规则：主机名等于 `s3.fansa.ccwu.cc` → **绕过缓存**（否则 `.webm/.mp3/.ogg` 会被 Cloudflare 按静态文件缓存，撤回后的语音仍可能被取到）。

> 免费版 Universal SSL 只覆盖一级子域名 `*.fansa.ccwu.cc`，所以用 `s3.fansa.ccwu.cc`，**不要**用 `s3.im.fansa.ccwu.cc` 这种两级子域名（橙色云朵下会证书错误）。
> 不能用 `im.fansa.ccwu.cc/s3/` 这样的路径前缀：S3 签名包含路径和 Host，反代改路径会导致签名失败。

### 2.2 安装 Caddy 并配置

Caddy 的安装方式不限（主机 A 怎么装的就照做）。`/etc/caddy/Caddyfile`（模板见 `imserver/deploy/Caddyfile.s3`）：

```
s3.fansa.ccwu.cc {
    reverse_proxy 127.0.0.1:3900 {
        flush_interval -1
    }
    request_body {
        max_size 10MB
    }
}
```

- Caddy 默认**透传原始 Host 头**，不要加 `header_up Host ...`，否则签名校验失败。
- **不要**在 Caddy 里加 CORS 头：CORS 由 imserver 启动时写入 bucket，Garage 自己处理预检请求，重复的头会让浏览器报错。

```bash
sudo caddy validate --config /etc/caddy/Caddyfile && sudo systemctl reload caddy
```

### 2.3 防火墙

1. **OCI 控制台**：主机 S 所在子网的安全列表（或 NSG）添加入站规则 TCP 80、443，来源 `0.0.0.0/0`（80 用于证书签发和跳转）。不要开放 3900/3901/3903。
2. **本机防火墙**：
   - Ubuntu（OCI 镜像默认 iptables 规则）：
     ```bash
     sudo iptables -I INPUT 6 -m state --state NEW -p tcp --dport 80 -j ACCEPT
     sudo iptables -I INPUT 6 -m state --state NEW -p tcp --dport 443 -j ACCEPT
     sudo netfilter-persistent save
     ```
   - Oracle Linux（firewalld）：
     ```bash
     sudo firewall-cmd --permanent --add-service=http --add-service=https && sudo firewall-cmd --reload
     ```
3. 两台机器都开启时间同步（`timedatectl` 显示 `System clock synchronized: yes`）：S3 签名要求时钟误差在 15 分钟内。

### 2.4 验证

```bash
curl -sI https://s3.fansa.ccwu.cc/imchat | head -1    # 返回 403（未签名请求被拒）说明链路通了
```

---

## 3. 主机 A：升级 imserver

生产目录为 `/root/imserver`（以 `systemctl cat imserver` 里 `ExecStart` 的实际路径为准）。

### 3.1 config.yaml 新增字段

编辑 `/root/imserver/config.yaml`，确认 `server.addr` 为 `":8092"`，并在末尾加入：

```yaml
group:
  max_members: 500
message:
  recall_seconds: 120

storage:
  endpoint: "s3.fansa.ccwu.cc:443"          # 主机 A 通过公网 HTTPS 访问存储（跨地域）
  use_ssl: true
  public_endpoint: "https://s3.fansa.ccwu.cc"  # 浏览器用；可以和 endpoint 相同
  access_key: "GK..."                        # 第 1.4 步的 Key ID
  secret_key: "..."                          # 第 1.4 步的 Secret key
  bucket: "imchat"
  region: "garage"                           # 与 garage.toml 的 s3_region 一致
  cors_origins: ["https://im.fansa.ccwu.cc"]
  voice_ttl_days: 7
  voice_max_seconds: 60
  voice_max_bytes: 2097152
  set_lifecycle: true
  verify_upload: true                        # 每条语音 2 次跨地域请求；嫌慢可改 false
```

完整说明见 `config.example.yaml`。建议同时设置 `server.allow_origins: ["https://im.fansa.ccwu.cc"]`。

### 3.2 替换二进制并重启

```bash
cd ~ && unzip imserver-v0.2.0-bin.zip            # 解压出 imserver-v0.2.0-bin/
sudo systemctl stop imserver
sudo cp /root/imserver/imserver /root/imserver/imserver.v0.1.0.bak
sudo cp imserver-v0.2.0-bin/bin/imserver-linux-arm64 /root/imserver/imserver
sudo chmod +x /root/imserver/imserver
/root/imserver/imserver -v                        # imserver v0.2.0
sudo systemctl start imserver
journalctl -u imserver -n 30 --no-pager
```

启动日志应包含：

```
msg="database migrated"
msg="storage lifecycle ensured" bucket=imchat prefix=voice/ expire_days=7
msg="storage CORS ensured" bucket=imchat origins=[https://im.fansa.ccwu.cc]
msg="imserver listening" addr=:8092 ... version=v0.2.0
```

出现 `⚠️ 无法自动设置 bucket 生命周期规则` / `CORS` 警告时，检查密钥是否有 `--owner` 权限、`region` 是否为 `garage`。连接存储失败（如 `bucket "imchat" 不存在`、签名错误）时 imserver 会拒绝启动并打印原因；可先把 `storage` 段注释掉启动，其他功能不受影响。

### 3.3 Caddy（主机 A）

`im.fansa.ccwu.cc` 站点只需确认反代到 `127.0.0.1:8092`（已经是的话不用动），见 `imweb-v0.2.0-dist.zip` 里的 `deploy/Caddyfile`。语音走独立域名，主机 A 的 Caddy 不需要任何 S3 相关配置。

```bash
sudo caddy validate --config /etc/caddy/Caddyfile && sudo systemctl reload caddy
```

## 4. 主机 A：升级网页

```bash
cd ~ && unzip imweb-v0.2.0-dist.zip               # 解压出 imweb-v0.2.0-dist/imweb-dist/
sudo rm -rf /var/www/imweb.bak && sudo cp -a /var/www/imweb /var/www/imweb.bak
sudo rm -rf /var/www/imweb/*
sudo cp -r imweb-v0.2.0-dist/imweb-dist/* /var/www/imweb/
sudo chown -R caddy:caddy /var/www/imweb
sudo restorecon -Rv /var/www/imweb 2>/dev/null || true   # SELinux
```

`index.html` 带 `Cache-Control: no-cache`，用户刷新即可拿到新版本。

## 5. 验证

1. 打开 <https://im.fansa.ccwu.cc>，新建群聊（右下角“新聊天” → 群聊），发消息、长按/悬停回复、表情回应、撤回、删除。
2. 输入框为空时点麦克风录一段语音并发送；另一个账号应能播放。浏览器开发者工具 Network 里应看到对 `s3.fansa.ccwu.cc` 的 `PUT`（上传）和 `GET`（下载）都是 200，无 CORS 报错。
3. 存储机上确认文件和规则：
   ```bash
   sudo garage bucket info imchat          # Objects 数量增加
   ```
   生命周期规则可用任意 S3 客户端查看（如 `aws s3api get-bucket-lifecycle-configuration --endpoint-url https://s3.fansa.ccwu.cc --region garage --bucket imchat`），应有 `imserver-voice-expire`：前缀 `voice/`、7 天过期。也可以在主机 A 上用冒烟测试一次性验证：
   ```bash
   # 需要 Go；用测试账号，不影响正式用户
   go run ./cmd/smoketest -base http://127.0.0.1:8092 -s3-endpoint s3.fansa.ccwu.cc:443 -s3-ssl \
     -s3-key GK... -s3-secret ... -bucket imchat -s3-region garage -origin https://im.fansa.ccwu.cc
   ```

## 6. 回滚

```bash
sudo systemctl stop imserver
sudo cp /root/imserver/imserver.v0.1.0.bak /root/imserver/imserver
sudo systemctl start imserver
sudo rm -rf /var/www/imweb/* && sudo cp -r /var/www/imweb.bak/* /var/www/imweb/
```

v0.1.0 可以直接运行在升级后的数据库上（新增的表和列会被忽略）；`config.yaml` 里多出的 `group/message/storage` 段 v0.1.0 也会忽略。群聊消息在 v0.1.0 网页上无法正常显示。

---

## 附录 A：其他部署方式

### A.1 存储机在同一地域、同一 VCN（走内网）

- `garage.toml` 的 `[s3_api] api_bind_addr = "0.0.0.0:3900"`（或主机 S 的内网 IP）。
- OCI 安全列表：入站 TCP 3900，来源**只填 VCN 网段**（如 `10.0.0.0/16`）；本机防火墙同样只放行该网段，例如
  `sudo iptables -I INPUT 6 -s 10.0.0.0/16 -p tcp --dport 3900 -j ACCEPT && sudo netfilter-persistent save`。
- 浏览器仍走 `https://s3.fansa.ccwu.cc`（Caddy 不变）。
- imserver：
  ```yaml
  storage:
    endpoint: "10.0.0.25:3900"     # 主机 S 内网 IP
    use_ssl: false
    public_endpoint: "https://s3.fansa.ccwu.cc"
    region: "garage"
  ```
  内网直连和公网域名访问的是同一个 Garage，签名各自按对应 Host 计算，互不影响。

### A.2 存储和 imserver 在同一台机器

在主机 A 上按第 1 节装 Garage，第 2 节的 `s3.fansa.ccwu.cc` 站点块直接加到主机 A 的 Caddyfile（DNS 指向主机 A），然后：

```yaml
storage:
  endpoint: "127.0.0.1:3900"
  use_ssl: false
  public_endpoint: "https://s3.fansa.ccwu.cc"
  region: "garage"
```

### A.3 已有 MinIO（不推荐）

imserver 只使用标准 S3 API（SigV4、path-style、预签名、生命周期、CORS），MinIO 仍可使用：`region` 改为 `us-east-1`（或你的 MinIO 区域），用 `mc mb` 建 bucket，密钥需要能设置生命周期和 CORS；也可以手动设置生命周期：`mc ilm rule add --expire-days 7 --prefix voice/ myminio/imchat`。

但 MinIO 社区版已停止维护、不再发布二进制，且存在未修复的安全漏洞（CVE-2026-40344，未认证写对象），新部署请使用 Garage。
