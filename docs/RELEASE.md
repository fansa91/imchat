# 发布新版本

发布完全由 GitHub Actions（`.github/workflows/release.yml`）完成：**推送一个以 `v` 开头的标签**即可。

## 发布 v0.2.0 的步骤

```bash
# 1. 确认 main 分支已推送、CI 通过
git checkout main
git pull

# 2.（可选）同步版本号
#    - imserver/Makefile 的 VERSION ?= v0.2.0（只影响本地 make；CI 会用标签名覆盖）
#    - imweb/package.json 的 "version": "0.2.0"
#    - imserver/config.example.yaml 第一行注释

# 3. 打带注释的标签并推送
git tag -a v0.2.0 -m "imchat v0.2.0"
git push origin v0.2.0
```

推送后到仓库的 **Actions → Release** 查看进度，完成后在 **Releases** 页面能看到：

| 文件 | 内容 |
| --- | --- |
| `imserver-v0.2.0-linux-amd64.tar.gz` | `imserver-v0.2.0-linux-amd64/` 目录：`imserver`（静态二进制）、`config.example.yaml`、`deploy/imserver.service` |
| `imserver-v0.2.0-linux-arm64.tar.gz` | 同上，arm64（OCI Ampere A1） |
| `imweb-v0.2.0-dist.tar.gz` | `imweb-v0.2.0-dist/` 目录：网页静态文件（`index.html`、`assets/`） |
| `SHA256SUMS` | 以上文件的 SHA-256 校验值 |

二进制编译参数与 `imserver/Makefile` 一致：`CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=<标签名>"`，
可以用 `imserver -v` 查看版本。

`deploy/install.sh` 正是按这些文件名下载的，**改文件名时两边要一起改**。

## 预发布版本

标签名中带 `-` 的（如 `v0.3.0-rc1`）会被标记为 **Pre-release**。注意 `install.sh` 不带 `--version` 时取的是
GitHub 的 “latest release”，预发布版本不会被当作 latest；要安装预发布版请显式指定：

```bash
bash install.sh --version v0.3.0-rc1
```

## 打错了标签怎么办

```bash
# 删除远端和本地标签，再在 GitHub 页面删除对应的 Release
git push --delete origin v0.2.0
git tag -d v0.2.0
# 修正后重新打标签、推送
```

## 首次推送仓库

本仓库是在本地准备好的（`git init -b main` 并已提交），首次推送：

```bash
# 在 GitHub 新建私有仓库 fansa91/imchat（不要勾选初始化 README）
git remote add origin git@github.com:fansa91/imchat.git
git push -u origin main
```

仓库设置里确认 **Settings → Actions → General → Workflow permissions** 允许工作流写入
（release.yml 已声明 `permissions: contents: write`，一般无需改动）。
