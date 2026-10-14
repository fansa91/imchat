#!/usr/bin/env bash
# =============================================================================
# imchat 一键安装脚本：同时安装 imserver（服务端）+ imweb（网页）
#
# 适用：Oracle Linux 8/9、RHEL 系、Ubuntu/Debian；arm64（OCI A1）/ amd64 自动识别
# 来源：https://github.com/fansa91/imchat/releases
#
# 用法（root 运行）：
#   bash install.sh                       # 安装最新版本
#   bash install.sh --version v0.2.0      # 安装指定版本
#   bash install.sh --upgrade             # 升级到最新版（保留 config.yaml，重启服务）
#   bash install.sh --port 8092 --web-dir /var/www/imweb
#   bash install.sh --uninstall           # 卸载（保留 /root/imserver/config.yaml）
#   bash install.sh --uninstall --purge   # 卸载并删除 /root/imserver 全部内容
#
# 私有仓库：先 export GH_TOKEN=<有 repo 读权限的 token>，脚本通过 GitHub API 下载附件。
#
# 安装结果：
#   /root/imserver/imserver         二进制
#   /root/imserver/config.yaml      配置（仅在不存在时由 config.example.yaml 生成，绝不覆盖）
#   /root/imserver/config.example.yaml
#   /etc/systemd/system/imserver.service（以 root 运行）
#   /var/www/imweb                  网页静态文件（Caddy 的 root）
#
# 可重复执行（幂等）：已存在的配置不会被修改。
# =============================================================================
set -euo pipefail

REPO="${IMCHAT_REPO:-fansa91/imchat}"
INSTALL_DIR="/root/imserver"
SERVICE_NAME="imserver"
UNIT_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

VERSION=""          # 空 = 最新版本
UPGRADE=0
UNINSTALL=0
PURGE=0
PORT=8092
PORT_SET=0
WEB_DIR="/var/www/imweb"

log()  { printf '\033[1;32m[imchat]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[警告]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[错误]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'
  exit "${1:-0}"
}

# ---------------------------------------------------------------- 参数解析
while [[ $# -gt 0 ]]; do
  case "$1" in
    --version)   [[ $# -ge 2 ]] || die "--version 需要参数"; VERSION="$2"; shift 2 ;;
    --version=*) VERSION="${1#*=}"; shift ;;
    --upgrade)   UPGRADE=1; shift ;;
    --port)      [[ $# -ge 2 ]] || die "--port 需要参数"; PORT="$2"; PORT_SET=1; shift 2 ;;
    --port=*)    PORT="${1#*=}"; PORT_SET=1; shift ;;
    --web-dir)   [[ $# -ge 2 ]] || die "--web-dir 需要参数"; WEB_DIR="$2"; shift 2 ;;
    --web-dir=*) WEB_DIR="${1#*=}"; shift ;;
    --uninstall) UNINSTALL=1; shift ;;
    --purge)     PURGE=1; shift ;;
    -h|--help)   usage 0 ;;
    *) warn "未知参数：$1"; usage 1 ;;
  esac
done

if ! [[ "$PORT" =~ ^[0-9]+$ ]] || (( PORT < 1 || PORT > 65535 )); then die "端口无效：$PORT"; fi
WEB_DIR="${WEB_DIR%/}"
[[ -n "$WEB_DIR" && "$WEB_DIR" != "/" ]] || die "--web-dir 不能是根目录"
if [[ -n "$VERSION" && "$VERSION" != v* ]]; then VERSION="v${VERSION}"; fi

[[ $EUID -eq 0 ]] || die "请用 root 运行（sudo -i 后再执行）"
command -v systemctl >/dev/null 2>&1 || die "需要 systemd"

# ---------------------------------------------------------------- 通用工具
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
    die "无法自动安装 $*，请手动安装后重试"
  fi
}

ensure_deps() {
  local missing=()
  command -v curl >/dev/null 2>&1      || missing+=(curl)
  command -v tar >/dev/null 2>&1       || missing+=(tar)
  command -v gzip >/dev/null 2>&1      || missing+=(gzip)
  command -v openssl >/dev/null 2>&1   || missing+=(openssl)
  command -v sha256sum >/dev/null 2>&1 || missing+=(coreutils)
  if ! command -v python3 >/dev/null 2>&1 && ! command -v jq >/dev/null 2>&1; then
    missing+=(jq)
  fi
  if [[ ${#missing[@]} -gt 0 ]]; then
    log "安装依赖：${missing[*]}"
    pkg_install "${missing[@]}"
  fi
}

detect_arch() {
  case "$(uname -m)" in
    aarch64|arm64) echo arm64 ;;
    x86_64|amd64)  echo amd64 ;;
    *) die "不支持的架构：$(uname -m)（仅支持 arm64 / amd64）" ;;
  esac
}

# GitHub API 请求（有 GH_TOKEN 时带认证）
gh_api() {
  local url="$1"
  local args=(-fsSL -H "Accept: application/vnd.github+json" -H "X-GitHub-Api-Version: 2022-11-28")
  if [[ -n "${GH_TOKEN:-}" ]]; then args+=(-H "Authorization: Bearer ${GH_TOKEN}"); fi
  curl "${args[@]}" "$url"
}

# 从 release JSON（stdin）中取字段：tag  或  asset-id <文件名>
json_get() {
  local what="$1" name="${2:-}"
  if command -v python3 >/dev/null 2>&1; then
    python3 -c '
import json, sys
d = json.load(sys.stdin)
what, name = sys.argv[1], sys.argv[2]
if what == "tag":
    print(d.get("tag_name", ""))
else:
    for a in d.get("assets", []):
        if a.get("name") == name:
            print(a.get("id")); break
' "$what" "$name"
  else
    if [[ "$what" == "tag" ]]; then
      jq -r '.tag_name // empty'
    else
      jq -r --arg n "$name" '.assets[] | select(.name == $n) | .id' | head -n1
    fi
  fi
}

RELEASE_JSON=""
resolve_release() {
  local url
  if [[ -n "$VERSION" ]]; then
    url="https://api.github.com/repos/${REPO}/releases/tags/${VERSION}"
  else
    url="https://api.github.com/repos/${REPO}/releases/latest"
  fi
  if ! RELEASE_JSON="$(gh_api "$url")"; then
    if [[ -z "${GH_TOKEN:-}" ]]; then
      die "读取 Release 失败：${url}
       仓库是私有的话，请先 export GH_TOKEN=<token>（需要 repo / Contents:read 权限）再运行。"
    fi
    die "读取 Release 失败：${url}（检查 GH_TOKEN 权限和版本号 ${VERSION:-latest}）"
  fi
  VERSION="$(printf '%s' "$RELEASE_JSON" | json_get tag)"
  [[ -n "$VERSION" ]] || die "无法解析 Release 版本号"
}

# 下载一个 Release 附件到指定路径
download_asset() {
  local name="$1" dest="$2"
  if [[ -n "${GH_TOKEN:-}" ]]; then
    local id
    id="$(printf '%s' "$RELEASE_JSON" | json_get asset "$name")"
    if [[ -z "$id" ]]; then
      warn "Release ${VERSION} 中没有附件 ${name}"
      return 1
    fi
    curl -fSL --retry 3 -o "$dest" \
      -H "Accept: application/octet-stream" \
      -H "Authorization: Bearer ${GH_TOKEN}" \
      -H "X-GitHub-Api-Version: 2022-11-28" \
      "https://api.github.com/repos/${REPO}/releases/assets/${id}"
  else
    curl -fSL --retry 3 -o "$dest" \
      "https://github.com/${REPO}/releases/download/${VERSION}/${name}"
  fi
}

write_unit() {
  local tmp
  tmp="$(mktemp)"
  cat > "$tmp" <<EOF
# 由 imchat deploy/install.sh 生成（${VERSION}）。重新运行安装脚本会覆盖本文件。
# 程序在 /root 下，因此不能开启 ProtectHome。
[Unit]
Description=imserver chat server
After=network-online.target mysqld.service mysql.service mariadb.service redis.service redis-server.service
Wants=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory=${INSTALL_DIR}
ExecStart=${INSTALL_DIR}/imserver -c ${INSTALL_DIR}/config.yaml
Restart=always
RestartSec=3
LimitNOFILE=1048576
NoNewPrivileges=true
ProtectSystem=full

[Install]
WantedBy=multi-user.target
EOF
  if [[ -f "$UNIT_FILE" ]] && cmp -s "$tmp" "$UNIT_FILE"; then
    rm -f "$tmp"
  else
    install -m 644 "$tmp" "$UNIT_FILE"
    rm -f "$tmp"
    log "已写入 ${UNIT_FILE}"
  fi
  systemctl daemon-reload
}

config_port() {
  # 读取 config.yaml 中 server.addr 的端口（找不到时返回空）
  # 只看 server: 段，避免误读 redis.addr
  sed -n '/^server:/,/^[^[:space:]#]/ s/^[[:space:]]*addr:[[:space:]]*"\{0,1\}[^":]*:\([0-9][0-9]*\)"\{0,1\}.*/\1/p' \
    "${INSTALL_DIR}/config.yaml" 2>/dev/null | head -n1
}

# ---------------------------------------------------------------- 卸载
do_uninstall() {
  log "卸载 imserver / imweb"
  if systemctl list-unit-files "${SERVICE_NAME}.service" >/dev/null 2>&1; then
    systemctl disable --now "${SERVICE_NAME}" 2>/dev/null || true
  fi
  rm -f "$UNIT_FILE"
  systemctl daemon-reload
  systemctl reset-failed "${SERVICE_NAME}" 2>/dev/null || true

  if [[ -d "$WEB_DIR" ]]; then
    rm -rf "$WEB_DIR" "${WEB_DIR}.bak"
    log "已删除网页目录 ${WEB_DIR}"
  fi

  if [[ $PURGE -eq 1 ]]; then
    rm -rf "$INSTALL_DIR"
    log "已删除 ${INSTALL_DIR}（含 config.yaml）"
  elif [[ -d "$INSTALL_DIR" ]]; then
    rm -f "${INSTALL_DIR}/imserver" "${INSTALL_DIR}/imserver.bak" "${INSTALL_DIR}/config.example.yaml"
    rm -rf "${INSTALL_DIR}/deploy"
    log "已删除程序文件；保留 ${INSTALL_DIR}/config.yaml（彻底删除用 --uninstall --purge）"
  fi
  log "卸载完成。MySQL / Redis 数据和 Caddy 配置未改动，如不再需要请手动清理。"
  exit 0
}

if [[ $UNINSTALL -eq 1 ]]; then do_uninstall; fi

# ---------------------------------------------------------------- 安装 / 升级
ensure_deps
ARCH="$(detect_arch)"

if [[ $UPGRADE -eq 1 && ! -x "${INSTALL_DIR}/imserver" ]]; then
  warn "未发现已安装的 ${INSTALL_DIR}/imserver，按全新安装处理"
  UPGRADE=0
fi

resolve_release
if [[ -x "${INSTALL_DIR}/imserver" ]]; then
  CURRENT="$("${INSTALL_DIR}/imserver" -v 2>/dev/null | awk '{print $2}' || true)"
  log "当前已安装：${CURRENT:-未知}，目标版本：${VERSION}"
else
  log "目标版本：${VERSION}（linux-${ARCH}）"
fi

WORK="$(mktemp -d /tmp/imchat-install.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

SRV_PKG="imserver-${VERSION}-linux-${ARCH}.tar.gz"
WEB_PKG="imweb-${VERSION}-dist.tar.gz"

log "下载 ${SRV_PKG}"
download_asset "$SRV_PKG" "${WORK}/${SRV_PKG}" || die "下载 ${SRV_PKG} 失败"
log "下载 ${WEB_PKG}"
download_asset "$WEB_PKG" "${WORK}/${WEB_PKG}" || die "下载 ${WEB_PKG} 失败"

# 校验 SHA256（Release 中有 SHA256SUMS 时）
if download_asset SHA256SUMS "${WORK}/SHA256SUMS" 2>/dev/null; then
  (cd "$WORK" && grep -E " \*?(${SRV_PKG}|${WEB_PKG})\$" SHA256SUMS | sha256sum -c -) \
    || die "SHA256 校验失败"
  log "SHA256 校验通过"
else
  warn "Release 中没有 SHA256SUMS，跳过校验"
fi

mkdir -p "${WORK}/srv" "${WORK}/web"
tar -xzf "${WORK}/${SRV_PKG}" -C "${WORK}/srv"
tar -xzf "${WORK}/${WEB_PKG}" -C "${WORK}/web"

NEW_BIN="$(find "${WORK}/srv" -type f -name imserver | head -n1)"
[[ -n "$NEW_BIN" ]] || die "压缩包中没有 imserver 二进制"
SRV_ROOT="$(dirname "$NEW_BIN")"
WEB_INDEX="$(find "${WORK}/web" -type f -name index.html | head -n1)"
[[ -n "$WEB_INDEX" ]] || die "网页压缩包中没有 index.html"
WEB_ROOT="$(dirname "$WEB_INDEX")"

# ---- 1. 服务端
mkdir -p "$INSTALL_DIR"
chmod 700 "$INSTALL_DIR"

WAS_ACTIVE=0
systemctl is-active --quiet "${SERVICE_NAME}" 2>/dev/null && WAS_ACTIVE=1

if [[ -f "${INSTALL_DIR}/imserver" ]]; then
  cp -f "${INSTALL_DIR}/imserver" "${INSTALL_DIR}/imserver.bak"
fi
install -m 755 "$NEW_BIN" "${INSTALL_DIR}/imserver.new"
mv -f "${INSTALL_DIR}/imserver.new" "${INSTALL_DIR}/imserver"   # 原子替换，运行中的进程不受影响
install -m 644 "${SRV_ROOT}/config.example.yaml" "${INSTALL_DIR}/config.example.yaml"
if [[ -d "${SRV_ROOT}/deploy" ]]; then
  mkdir -p "${INSTALL_DIR}/deploy"
  cp -f "${SRV_ROOT}/deploy/"* "${INSTALL_DIR}/deploy/" 2>/dev/null || true
fi
log "已安装 ${INSTALL_DIR}/imserver（$("${INSTALL_DIR}/imserver" -v 2>/dev/null || echo "${VERSION}")）"

if selinux_enforcing; then
  chcon -t bin_t "${INSTALL_DIR}/imserver" && log "SELinux：已设置 bin_t 标签"
fi

CONFIG_CREATED=0
if [[ ! -f "${INSTALL_DIR}/config.yaml" ]]; then
  install -m 600 "${INSTALL_DIR}/config.example.yaml" "${INSTALL_DIR}/config.yaml"
  # 监听端口（只改 server: 段的 addr；只监听本机，由 Caddy 反代）
  sed -i -E "/^server:/,/^[^[:space:]#]/ s/^([[:space:]]*addr:[[:space:]]*)\"[^\"]*:[0-9]+\"/\1\"127.0.0.1:${PORT}\"/" \
    "${INSTALL_DIR}/config.yaml"
  # 随机 JWT 密钥（只改 jwt: 段）
  JWT_SECRET="$(openssl rand -hex 32)"
  sed -i -E "/^jwt:/,/^[^[:space:]#]/ s/^([[:space:]]*secret:[[:space:]]*)\".*\"/\1\"${JWT_SECRET}\"/" \
    "${INSTALL_DIR}/config.yaml"
  CONFIG_CREATED=1
  log "已生成 ${INSTALL_DIR}/config.yaml（端口 ${PORT}，jwt.secret 已随机生成）——还需要填写 mysql / storage"
else
  chmod 600 "${INSTALL_DIR}/config.yaml"
  log "保留已有配置 ${INSTALL_DIR}/config.yaml（不做任何修改）"
  CUR_PORT="$(config_port || true)"
  if [[ $PORT_SET -eq 1 && -n "$CUR_PORT" && "$CUR_PORT" != "$PORT" ]]; then
    warn "--port ${PORT} 与现有配置中的端口 ${CUR_PORT} 不同；脚本不会修改已有配置，请手动编辑 server.addr"
  fi
  if [[ -n "$CUR_PORT" ]]; then PORT="$CUR_PORT"; fi
fi

write_unit
systemctl enable "${SERVICE_NAME}" >/dev/null 2>&1

# ---- 2. 网页
mkdir -p "$(dirname "$WEB_DIR")"
rm -rf "${WEB_DIR}.new"
mkdir -p "${WEB_DIR}.new"
cp -r "${WEB_ROOT}/." "${WEB_DIR}.new/"
find "${WEB_DIR}.new" -type d -exec chmod 755 {} +
find "${WEB_DIR}.new" -type f -exec chmod 644 {} +
WEB_BACKUP=0
if [[ -d "$WEB_DIR" ]]; then
  rm -rf "${WEB_DIR}.bak"
  mv "$WEB_DIR" "${WEB_DIR}.bak"
  WEB_BACKUP=1
fi
mv "${WEB_DIR}.new" "$WEB_DIR"
# Caddy（caddy 用户）需要能逐级进入目录
chmod 755 "$WEB_DIR"
if id caddy >/dev/null 2>&1; then chown -R caddy:caddy "$WEB_DIR"; fi
if selinux_enforcing; then
  if [[ "$WEB_DIR" == /var/www/* ]] && command -v restorecon >/dev/null 2>&1; then
    restorecon -R "$WEB_DIR" || true   # /var/www 默认就是 httpd_sys_content_t
  else
    chcon -R -t httpd_sys_content_t "$WEB_DIR" || true
  fi
fi
if [[ $WEB_BACKUP -eq 1 ]]; then
  log "网页已安装到 ${WEB_DIR}（上一版备份在 ${WEB_DIR}.bak）"
else
  log "网页已安装到 ${WEB_DIR}"
fi

# ---- 3. 启动 / 重启
STARTED=0
if [[ $UPGRADE -eq 1 || $WAS_ACTIVE -eq 1 ]]; then
  systemctl restart "${SERVICE_NAME}"
  STARTED=1
elif [[ $CONFIG_CREATED -eq 0 ]]; then
  systemctl start "${SERVICE_NAME}"
  STARTED=1
fi

if [[ $STARTED -eq 1 ]]; then
  sleep 2
  if systemctl is-active --quiet "${SERVICE_NAME}"; then
    if curl -fsS --max-time 5 "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
      log "imserver 运行正常：http://127.0.0.1:${PORT}/healthz"
    else
      warn "服务已启动但 /healthz 暂未响应，请查看：journalctl -u ${SERVICE_NAME} -n 50"
    fi
  else
    warn "imserver 启动失败，请查看：journalctl -u ${SERVICE_NAME} -n 50 --no-pager"
    if [[ -f "${INSTALL_DIR}/imserver.bak" && $UPGRADE -eq 1 ]]; then
      warn "如需回滚：mv ${INSTALL_DIR}/imserver.bak ${INSTALL_DIR}/imserver && systemctl restart ${SERVICE_NAME}"
    fi
  fi
fi

# ---------------------------------------------------------------- 后续步骤
cat <<EOF

================================================================
 imchat ${VERSION} 安装完成
   程序：${INSTALL_DIR}/imserver
   配置：${INSTALL_DIR}/config.yaml
   网页：${WEB_DIR}
   服务：systemctl status ${SERVICE_NAME}
================================================================
EOF

if [[ $CONFIG_CREATED -eq 1 ]]; then
  cat <<EOF
下一步：
  1. 准备 MySQL 和 Redis（见 docs/DEPLOY.md 第 2 节）。
  2. 编辑配置：  vi ${INSTALL_DIR}/config.yaml
       - mysql.password / mysql.user / mysql.database
       - storage.*（Garage 用 deploy/install-garage.sh 安装后打印的片段；不需要语音可删掉 storage 段）
       - server.allow_origins: ["https://你的网页域名"]
  3. 启动：      systemctl start ${SERVICE_NAME}
  4. 查看日志：  journalctl -u ${SERVICE_NAME} -f
  5. 配置 Caddy：/api/* /ws /healthz 反代到 127.0.0.1:${PORT}，其余 root ${WEB_DIR}（见 docs/DEPLOY.md）
EOF
else
  cat <<EOF
下一步：
  - 查看日志：journalctl -u ${SERVICE_NAME} -f
  - 确认 Caddy 的网页 root 是 ${WEB_DIR}，接口反代到 127.0.0.1:${PORT}
  - 以后升级：bash install.sh --upgrade
EOF
fi
