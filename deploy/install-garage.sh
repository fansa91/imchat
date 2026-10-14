#!/usr/bin/env bash
# =============================================================================
# Garage（S3 兼容对象存储）单节点安装脚本 —— 给 imserver 存语音
# 在“存储机”上运行（通常是另一台 OCI A1），与 imserver 主机分开。
#
# 用法（root 运行）：
#   bash install-garage.sh
#   bash install-garage.sh --capacity 50G --bucket imchat --key-name imserver-key \
#        --domain s3.fansa.ccwu.cc:8443 --cors-origin https://im.fansa.ccwu.cc
#   bash install-garage.sh --version v2.4.1      # 指定 Garage 版本
#
# 参数：
#   --version      Garage 版本（默认 v2.1.0）
#   --capacity     本节点容量（默认 20G，单节点时只用于布局，不是硬限制）
#   --bucket       bucket 名（默认 imchat）
#   --key-name     访问密钥名（默认 imserver-key）
#   --domain       浏览器访问存储的公网域名[:端口]（默认 s3.fansa.ccwu.cc:8443），
#                  只用于打印 imserver 的 storage 配置片段
#   --cors-origin  允许跨域的网页来源（默认 https://im.fansa.ccwu.cc）
#
# 安装结果：
#   /root/garage/garage           官方静态二进制
#   /root/garage/garage.toml      配置（仅不存在时生成：rpc_secret / admin_token 随机）
#   /etc/garage.toml -> /root/garage/garage.toml（方便直接运行 garage 命令）
#   /root/garage/meta  /root/garage/data
#   /etc/systemd/system/garage.service（以 root 运行）
#   /root/garage/imserver-credentials.txt（S3 Key ID / Secret key，权限 600）
#
# 可重复执行：已有配置、已应用的布局、已存在的 bucket / key 都会跳过。
# =============================================================================
set -euo pipefail

GARAGE_VERSION="v2.1.0"
CAPACITY="20G"
BUCKET="imchat"
KEY_NAME="imserver-key"
DOMAIN="s3.fansa.ccwu.cc:8443"
CORS_ORIGIN="https://im.fansa.ccwu.cc"

GARAGE_DIR="/root/garage"
BIN="${GARAGE_DIR}/garage"
CONF="${GARAGE_DIR}/garage.toml"
UNIT_FILE="/etc/systemd/system/garage.service"
CRED_FILE="${GARAGE_DIR}/imserver-credentials.txt"

log()  { printf '\033[1;32m[garage]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[警告]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[错误]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  sed -n '2,32p' "$0" | sed 's/^# \{0,1\}//'
  exit "${1:-0}"
}

need_arg() { [[ $# -ge 2 && -n "$2" ]] || die "$1 需要参数"; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version)     need_arg "$@"; GARAGE_VERSION="$2"; shift 2 ;;
    --capacity)    need_arg "$@"; CAPACITY="$2"; shift 2 ;;
    --bucket)      need_arg "$@"; BUCKET="$2"; shift 2 ;;
    --key-name)    need_arg "$@"; KEY_NAME="$2"; shift 2 ;;
    --domain)      need_arg "$@"; DOMAIN="$2"; shift 2 ;;
    --cors-origin) need_arg "$@"; CORS_ORIGIN="$2"; shift 2 ;;
    -h|--help)     usage 0 ;;
    *) warn "未知参数：$1"; usage 1 ;;
  esac
done
[[ "$GARAGE_VERSION" == v* ]] || GARAGE_VERSION="v${GARAGE_VERSION}"
DOMAIN="${DOMAIN#https://}"; DOMAIN="${DOMAIN#http://}"; DOMAIN="${DOMAIN%/}"

[[ $EUID -eq 0 ]] || die "请用 root 运行"
command -v systemctl >/dev/null 2>&1 || die "需要 systemd"

selinux_enforcing() {
  command -v getenforce >/dev/null 2>&1 && [[ "$(getenforce 2>/dev/null)" == "Enforcing" ]]
}

pkg_install() {
  if command -v apt-get >/dev/null 2>&1; then
    DEBIAN_FRONTEND=noninteractive apt-get update -y >/dev/null
    DEBIAN_FRONTEND=noninteractive apt-get install -y "$@" >/dev/null
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y "$@" >/dev/null
  elif command -v yum >/dev/null 2>&1; then
    yum install -y "$@" >/dev/null
  else
    die "无法自动安装 $*，请手动安装"
  fi
}

missing=()
command -v curl >/dev/null 2>&1    || missing+=(curl)
command -v openssl >/dev/null 2>&1 || missing+=(openssl)
if [[ ${#missing[@]} -gt 0 ]]; then log "安装依赖：${missing[*]}"; pkg_install "${missing[@]}"; fi

case "$(uname -m)" in
  aarch64|arm64) TARGET="aarch64-unknown-linux-musl" ;;
  x86_64|amd64)  TARGET="x86_64-unknown-linux-musl" ;;
  *) die "不支持的架构：$(uname -m)" ;;
esac

garage() { "$BIN" -c "$CONF" "$@"; }

# ---------------------------------------------------------------- 1. 二进制
mkdir -p "$GARAGE_DIR" "${GARAGE_DIR}/meta" "${GARAGE_DIR}/data"
chmod 700 "$GARAGE_DIR"

URL="https://garagehq.deuxfleurs.fr/_releases/${GARAGE_VERSION}/${TARGET}/garage"
NEED_DOWNLOAD=1
if [[ -x "$BIN" ]] && "$BIN" --version 2>/dev/null | grep -q "${GARAGE_VERSION#v}"; then
  NEED_DOWNLOAD=0
  log "已安装 Garage ${GARAGE_VERSION}，跳过下载"
fi
BINARY_CHANGED=0
if [[ $NEED_DOWNLOAD -eq 1 ]]; then
  log "下载 ${URL}"
  curl -fSL --retry 3 -o "${BIN}.new" "$URL" || die "下载失败：${URL}（检查 --version）"
  # 简单校验：必须是 ELF 文件
  [[ "$(head -c 4 "${BIN}.new" | od -An -c | tr -d ' ')" == '177ELF' ]] || die "下载的不是 ELF 可执行文件"
  chmod 755 "${BIN}.new"
  mv -f "${BIN}.new" "$BIN"
  BINARY_CHANGED=1
  log "Garage 已安装到 ${BIN}"
fi
if selinux_enforcing; then
  chcon -t bin_t "$BIN" && log "SELinux：已设置 bin_t 标签"
fi

# ---------------------------------------------------------------- 2. 配置
if [[ ! -f "$CONF" ]]; then
  RPC_SECRET="$(openssl rand -hex 32)"
  ADMIN_TOKEN="$(openssl rand -base64 32 | tr -d '\n')"
  METRICS_TOKEN="$(openssl rand -base64 32 | tr -d '\n')"
  umask 077
  cat > "$CONF" <<EOF
# Garage 单节点配置（由 imchat deploy/install-garage.sh 生成）
# rpc_secret：Garage 节点之间内部 RPC 通信用的共享密钥，只在这里使用。
#             它不是 S3 的 Secret key！不要填进 imserver 的 storage.secret_key。
metadata_dir = "${GARAGE_DIR}/meta"
data_dir = "${GARAGE_DIR}/data"
db_engine = "lmdb"
metadata_auto_snapshot_interval = "6h"

replication_factor = 1

rpc_bind_addr = "127.0.0.1:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = "${RPC_SECRET}"

[s3_api]
s3_region = "garage"               # imserver 的 storage.region 必须与此一致
api_bind_addr = "127.0.0.1:3900"   # 只给本机 Caddy 反代

[admin]
api_bind_addr = "127.0.0.1:3903"
admin_token = "${ADMIN_TOKEN}"
metrics_token = "${METRICS_TOKEN}"
EOF
  umask 022
  chmod 600 "$CONF"
  log "已生成 ${CONF}（rpc_secret / admin_token 已随机生成）"
else
  log "保留已有配置 ${CONF}"
fi
ln -sfn "$CONF" /etc/garage.toml

# ---------------------------------------------------------------- 3. systemd
UNIT_TMP="$(mktemp)"
cat > "$UNIT_TMP" <<EOF
# 由 imchat deploy/install-garage.sh 生成。程序和数据在 /root/garage，以 root 运行，
# 因此不能开启 ProtectHome（否则 203/EXEC 或无法读写数据目录）。
[Unit]
Description=Garage S3-compatible object storage
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
Environment=RUST_LOG=garage=info
ExecStart=${BIN} -c ${CONF} server
Restart=always
RestartSec=3
LimitNOFILE=65536
NoNewPrivileges=true
ProtectSystem=full

[Install]
WantedBy=multi-user.target
EOF
UNIT_CHANGED=0
if [[ ! -f "$UNIT_FILE" ]] || ! cmp -s "$UNIT_TMP" "$UNIT_FILE"; then
  install -m 644 "$UNIT_TMP" "$UNIT_FILE"
  UNIT_CHANGED=1
  log "已写入 ${UNIT_FILE}"
fi
rm -f "$UNIT_TMP"
systemctl daemon-reload
systemctl enable garage >/dev/null 2>&1
if [[ $BINARY_CHANGED -eq 1 || $UNIT_CHANGED -eq 1 ]]; then
  systemctl restart garage
else
  systemctl start garage
fi

log "等待 Garage 启动..."
for _ in $(seq 1 30); do
  if garage status >/dev/null 2>&1; then break; fi
  sleep 1
done
garage status >/dev/null 2>&1 || die "Garage 未能启动：journalctl -u garage -n 50 --no-pager"

# ---------------------------------------------------------------- 4. 布局
STATUS_OUT="$(garage status 2>&1)"
if grep -q "NO ROLE ASSIGNED" <<<"$STATUS_OUT"; then
  NODE_ID="$(garage node id -q 2>/dev/null | cut -d@ -f1)"
  [[ -n "$NODE_ID" ]] || die "无法获取节点 ID"
  log "分配布局：zone=dc1 capacity=${CAPACITY} node=${NODE_ID:0:16}"
  garage layout assign -z dc1 -c "$CAPACITY" "$NODE_ID"
  CUR_LAYOUT="$(garage layout show 2>/dev/null | grep -oE 'Current cluster layout version: [0-9]+' | grep -oE '[0-9]+$' || true)"
  NEXT=$(( ${CUR_LAYOUT:-0} + 1 ))
  garage layout apply --version "$NEXT"
  log "布局已应用（version ${NEXT}）"
else
  log "布局已存在，跳过"
fi

# ---------------------------------------------------------------- 5. bucket + key
if garage bucket info "$BUCKET" >/dev/null 2>&1; then
  log "bucket ${BUCKET} 已存在，跳过"
else
  garage bucket create "$BUCKET"
  log "已创建 bucket ${BUCKET}"
fi

if garage key info "$KEY_NAME" >/dev/null 2>&1; then
  log "访问密钥 ${KEY_NAME} 已存在，跳过创建"
  KEY_OUT="$(garage key info --show-secret "$KEY_NAME" 2>/dev/null || garage key info "$KEY_NAME")"
else
  KEY_OUT="$(garage key create "$KEY_NAME")"
  log "已创建访问密钥 ${KEY_NAME}"
fi
KEY_ID="$(awk -F': *' '/^[[:space:]]*Key ID/ {print $2; exit}' <<<"$KEY_OUT" | tr -d '[:space:]')"
SECRET_KEY="$(awk -F': *' '/^[[:space:]]*Secret key/ {print $2; exit}' <<<"$KEY_OUT" | tr -d '[:space:]')"
if [[ -z "$KEY_ID" || -z "$SECRET_KEY" || "$SECRET_KEY" == *redacted* ]]; then
  warn "未能从输出中解析 Key ID / Secret key，请手动查看：garage key info --show-secret ${KEY_NAME}"
  printf '%s\n' "$KEY_OUT"
fi

# owner 权限：imserver 启动时要写 bucket 的生命周期规则和 CORS
garage bucket allow --read --write --owner "$BUCKET" --key "$KEY_NAME" >/dev/null
log "已授权 ${KEY_NAME} 对 ${BUCKET} 的 read/write/owner 权限"

if [[ -n "$KEY_ID" && -n "$SECRET_KEY" ]]; then
  umask 077
  cat > "$CRED_FILE" <<EOF
# Garage S3 访问密钥（给 imserver 的 storage.access_key / storage.secret_key）
bucket=${BUCKET}
key_name=${KEY_NAME}
access_key=${KEY_ID}
secret_key=${SECRET_KEY}
EOF
  umask 022
  chmod 600 "$CRED_FILE"
fi

# ---------------------------------------------------------------- 6. CORS
CORS_JSON="{\"CORSRules\":[{\"AllowedOrigins\":[\"${CORS_ORIGIN}\"],\"AllowedMethods\":[\"GET\",\"PUT\",\"HEAD\"],\"AllowedHeaders\":[\"*\"],\"ExposeHeaders\":[\"ETag\"],\"MaxAgeSeconds\":3600}]}"
CORS_DONE=0
if command -v aws >/dev/null 2>&1 && [[ -n "$KEY_ID" && -n "$SECRET_KEY" ]]; then
  if AWS_ACCESS_KEY_ID="$KEY_ID" AWS_SECRET_ACCESS_KEY="$SECRET_KEY" AWS_DEFAULT_REGION=garage \
     aws --endpoint-url http://127.0.0.1:3900 s3api put-bucket-cors \
       --bucket "$BUCKET" --cors-configuration "$CORS_JSON" >/dev/null 2>&1; then
    CORS_DONE=1
    log "已设置 bucket CORS（允许 ${CORS_ORIGIN}）"
  else
    warn "aws cli 设置 CORS 失败（imserver 启动时仍会自动设置）"
  fi
fi

# ---------------------------------------------------------------- 7. 输出
if [[ "$DOMAIN" == *:* ]]; then
  ENDPOINT="$DOMAIN"
  PUBLIC_ENDPOINT="https://${DOMAIN}"
else
  ENDPOINT="${DOMAIN}:443"
  PUBLIC_ENDPOINT="https://${DOMAIN}"
fi

cat <<EOF

================================================================
 Garage ${GARAGE_VERSION} 安装完成（服务：systemctl status garage）
================================================================
 S3 访问密钥（填到 imserver 的 storage 段；同时保存在 ${CRED_FILE}）：
   Key ID     : ${KEY_ID:-<见上方输出>}
   Secret key : ${SECRET_KEY:-<见上方输出>}

 注意：garage.toml 里的 rpc_secret 是 Garage 节点内部 RPC 密钥，
       和这里的 Secret key 完全不同，两者绝不能互换。

 ---- 复制到 imserver 主机 /root/imserver/config.yaml ----
storage:
  endpoint: "${ENDPOINT}"
  use_ssl: true
  public_endpoint: "${PUBLIC_ENDPOINT}"
  access_key: "${KEY_ID:-GK...}"
  secret_key: "${SECRET_KEY:-...}"
  bucket: "${BUCKET}"
  region: "garage"
  cors_origins: ["${CORS_ORIGIN}"]
  voice_ttl_days: 7
  voice_max_seconds: 60
  voice_max_bytes: 2097152
  set_lifecycle: true
  verify_upload: true
  presign_put_seconds: 300
  presign_get_seconds: 300
 ---------------------------------------------------------

下一步（存储机）：
  1. Caddy 反代 ${DOMAIN} → 127.0.0.1:3900（见 docs/DEPLOY.md 第 6 节），
     并在 OCI 安全列表 + 防火墙放行该端口。
  2. 验证：curl -I ${PUBLIC_ENDPOINT}/   （返回 403 AccessDenied 之类的 XML 即为正常）
EOF
if [[ $CORS_DONE -eq 0 ]]; then
  cat <<EOF
  3. CORS：imserver 启动时（storage.set_lifecycle: true）会用上面的密钥自动写入 bucket CORS。
     如需手动设置（需安装 aws cli）：
     AWS_ACCESS_KEY_ID=${KEY_ID:-GK...} AWS_SECRET_ACCESS_KEY=<Secret key> AWS_DEFAULT_REGION=garage \\
       aws --endpoint-url http://127.0.0.1:3900 s3api put-bucket-cors --bucket ${BUCKET} \\
       --cors-configuration '${CORS_JSON}'
EOF
fi
