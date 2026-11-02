# GitHub Actions 构建原生 Web 镜像

适用于 `Dockerfile.web`，不是旧 `Dockerfile` 的 Electron/VNC 版本。只构建 **linux/amd64**；ARM64 未验收，不发布多架构镜像。不会自动部署、切换生产、启用 Watchtower 或读取作者数据。

## 账号与仓库

- 需要 GitHub 账号和已启用 Actions 的仓库；私有仓库构建会消耗账户的 Actions/Packages 配额，先查看账单限制。
- 工作流 `Native Web image` 在 PR、`main` push 和手动运行时执行。没有路径过滤，所有运行源码/配置改动都会触发。手动选择非 main 分支只验证，不发布。
- PR 只有 `contents: read`，不登录 GHCR、不发布、不读取/写入共享构建缓存。仅 main 发布 job 有 `contents: read` 和 `packages: write`；用自动提供的 `GITHUB_TOKEN`，**无需添加 PAT、模型 key 或部署密钥**。组织策略仍须允许 Actions 发布 Packages。
- 首次发布后，在仓库 **Packages** 检查包的可见性及 Actions 访问关联。私有仓库不等于可以忽略包的独立可见性；若同名旧包存在，需要管理员授予该仓库 Actions 写入权限。

## 构建与验收范围

干净 runner 从官方 `node:22-bookworm-slim` 安装固定 `pnpm@11.11.0`，执行 `pnpm install --ignore-scripts --frozen-lockfile`。显式跳过根目录 Electron postinstall 和所有依赖生命周期脚本，同时禁止 Electron/Playwright 二进制下载；保留可选包以获得 esbuild/LanceDB 的目标平台二进制。better-sqlite3 13 的 npm 包自带 prebuild，最终镜像会实际执行内存 SQLite 查询，不能加载则构建/冒烟失败。

构建保持 TypeScript → Web Vite → Node bundle → 已审核生产依赖闭包流程，最终只含 Node/Python/flock/CA 和目标平台依赖，无 Electron/浏览器/桌面工具链。可选本地复用：`--build-arg BUILDER_IMAGE=ai-novel-writer:1.4.0`，须自行确保 `/app/node_modules` 与锁文件和 Node 22 一致；默认 CI 不依赖该本地镜像。

先跑 Node 22 的安全、响应清洗、生命周期和浏览器 transport 单元测试，再构建并加载最终镜像；用无网络、只读根、drop capabilities、空 `/data` tmpfs 冒烟健康端点、未认证拒绝、原生依赖及无 Electron，**成功后才登录并 push 同一个镜像**。不使用真实模型凭据、不挂作者卷、不上传镜像/构建记录附件；仅可信 main push 使用 GHA 构建缓存。缓存仍按私有仓库权限管理。

标签（owner/repo 自动小写）：

- `ghcr.io/<owner>/<repo>:sha-<完整提交 SHA>`：对应源码提交，便于追踪；重跑可能更新基础镜像，**严格固定版本仍应使用 digest**。
- `ghcr.io/<owner>/<repo>:latest`：最近一次 main 成功发布；不要无审核自动更新生产。
- Actions 运行摘要记录 `ghcr.io/...@sha256:...`。镜像含 OCI source/revision/GPL-3.0-only 标签；为保证本地冒烟后原样发布，当前没有附带 registry provenance/SBOM attestations，不作供应链证明声明。

本地磁盘紧张时不要再做完整依赖安装/干净镜像构建；把首次 clean build 交给 Actions。单元测试/健康冒烟不代表付费 provider、浏览器全部业务或生产切换验收通过。

## 拉取私有镜像

构建发布只用 `GITHUB_TOKEN`。**服务器拉取私有 GHCR 包**另需有包读取权限的 GitHub 账号及最小 `read:packages` classic PAT（组织启用 SSO 时另行授权）；不要把 token 写进仓库、Compose 或命令参数。公共包通常可匿名拉取。

```sh
# 先检查是否已有可用凭据；没有时按提示输入用户名和只读token。
# Docker原生config.json的base64不是加密：设0600，优先配置credential helper。
docker login ghcr.io
# 从可信 Actions 摘要复制真实 digest，替换占位值。
IMAGE='ghcr.io/<owner>/<repo>@sha256:<digest>'
docker pull --platform linux/amd64 "$IMAGE"
# 先隔离验收；生产使用已审核的 image digest，而不是盲跟 latest。
```

仅拉取不启动服务。Compose是pull-only配置，要求显式 `NOVEL_IMAGE`，无build段；仓库根目录的本机 `.env` 固定摘要（0600、已忽略、不含token），命令必须带 `docker compose --env-file .env -f docker/compose.web.yaml ...`，先用 `config --images` 验证，缺配置应拒绝而不是回退旧本地镜像。维护窗口内完成全卷离线备份、数据格式/UID权限检查后，再按 [原生 Web 运维说明](README.web.md) 启动。禁止桌面和原生服务同时写同一个数据卷；保留可回滚镜像与完整私有备份。

## 外网部署必须有认证反向代理

- 后端仅发布 `127.0.0.1:6081:6081`，不可直接暴露 WAN/LAN。外部入口使用 HTTPS 和认证代理；这是共享单作者工作区，不是多租户系统。
- 代理必须覆盖 `X-Novel-User` 为认证结果，清空上游 `Authorization`；绝不能信任客户端自填身份头。保留 Cookie/CSRF/Origin，SSE 关闭缓冲并延长超时。参考 `docker/nginx-ai-novel-writer-web.conf`，不要套旧 VNC upstream。
- `NOVEL_ALLOWED_ORIGINS` 设置真实 HTTPS 浏览器入口；`NOVEL_ALLOWED_ENDPOINTS` 只列经审核的模型/embedding/WebDAV 精确 origin。代理地址、NO_PROXY、管理员列表及密钥由部署环境单独管理，本说明不包含真实凭据或内网地址。
- 作者数据、模型 key、服务器密钥、`.env` 和完整备份不进入源码、Docker 构建上下文或 Actions 附件。日志分享前脱敏。
