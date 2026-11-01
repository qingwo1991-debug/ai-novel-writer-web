# 原生 Web 部署、切换与回滚

本文件对应 **`Dockerfile.web` + `docker/compose.web.yaml`**：React 浏览器界面、纯 Node 22 网关/领域 worker、SQLite/LanceDB，以及 Python Linux 安全文件 helper。**运行镜像没有 Electron、Xvfb、VNC 或远程桌面。**

`docker/README.md`、`Dockerfile`、`docker/compose.yaml` 和当前仓库中的 `docker/nginx-ai-novel-writer.conf` 描述的是旧桌面/VNC 部署，不应原样当作原生 Web 安装步骤。尤其旧 nginx 副本指向 **6080**，没有提供原生网关所需的已认证身份头。

本文是运维步骤和已验证证据，**不是“全部功能已验收”声明**。逐项功能/验收边界见 `web/FEATURE-MATRIX.md`、`web/MIGRATION-NOTES.md`。本文编写不代表已修改生产 nginx、启动生产原生服务或完成切换。

## 1. 文件、端口与信任模型

| 项目 | 原生配置 |
| --- | --- |
| 镜像/容器 | `${NOVEL_IMAGE:-ai-novel-writer-web:1.4.0}` / `ai-novel-writer-web`；GHCR 部署设置经审核的 digest |
| 独立 Compose 项目 | `ai-novel-writer-native`；不要对其他 Compose 项目执行 `--remove-orphans` |
| 后端发布 | **`127.0.0.1:6081:6081`**；容器内 `HOST=0.0.0.0` 不代表可以把宿主端口发布到 LAN |
| 浏览器入口 | 由宿主 nginx 统一保护，例如 `http://192.168.1.15:8093`、`https://192.168.1.15:8445` |
| 数据挂载 | `/root/AI/ai-novel-writer-data:/data` |
| 只读范围 | 镜像根文件系统只读；仅 `/data` 和受限 `/tmp` tmpfs 可写 |
| 生命周期锁 | `/data/.native-web.gateway.lock`，由入口进程的内核 `flock` 持有；冲突退出 **75** |

这是 **共享单作者工作区**。所有通过 Basic Auth 的用户有意共享同一书库、全局设置和模型；不是租户隔离系统。不同浏览器会话的临时授权、项目/标签写占有权仍有各自边界。不要把共享登录用户视为可承载不可信租户的权限系统。

`X-Novel-User` **不是独立认证机制**。网关信任它的前提是：后端仅回环发布，nginx 已完成认证，并且**覆盖**客户端自带的同名头。能访问宿主回环或 Docker 网络的本机程序仍处于信任边界内。Basic Auth 在 HTTP 上不加密，优先 HTTPS 或受信任 VPN；证书与密码文件由管理员管理，不写入源码、镜像或本说明。

`NOVEL_ADMIN_USERS` 是已认证用户名的精确逗号列表（默认 `admin`），用于受限 MCP 操作。管理员仍必须在服务器侧预配置 MCP ID/命令/参数/环境；不要授予浏览器任意进程执行或把 root 容器当作不可信工具沙箱。普通共享作者不等于服务器管理员。

## 2. 构建与磁盘预算

优先使用 **GitHub Actions 构建 → GHCR 拉取**，见 [README.github.md](README.github.md)；服务器不必再安装依赖或编译。`Dockerfile.web` 的默认工具链可在干净的 GitHub runner 独立安装锁定依赖，最终 stage 使用 **`node:22-bookworm-slim`**。已有服务器也可显式指定本地 **`ai-novel-writer:1.4.0`** 作为一次性工具链，避免在磁盘紧张时重复安装；这是本地优化，不是云端必需条件。Node 22 必须支持 `NODE_USE_ENV_PROXY`；本次本地验收环境为 **22.23.4 / ABI 127 / Linux x64 glibc**。不要把 builder 的整个 `.pnpm` 复制到运行镜像。

`web/runtime-dependencies.mjs` 从 worker metafile 和实际 `createRequire` 调用收集已安装依赖，保留生产依赖/peer 的分版本链接，仅复制目标 Linux/glibc 原生二进制，并拒绝 Electron/sharp。四个根依赖是 `better-sqlite3`、`@lancedb/lancedb`、`apache-arrow`、`yauzl`。Python 用于既有能力安全文件 helper，`util-linux` 提供 `flock`，CA 用于 HTTPS。

已测候选镜像约 **520 MB（496 MiB，未压缩）**，依赖闭包 **35 包 / 139,176,748 字节（132.73 MiB）**。LanceDB 二进制和约 96 MB public 资源占主要空间；构建缓存/中间层还需额外空间。构建前检查磁盘；不要盲目清理其他服务镜像、生产数据或全局 Docker 缓存。

在仓库根目录运行（需管理员已授权的 Docker/文件权限）：

```sh
cd /root/AI/AI-Novel-Writer
df -h .
docker image inspect ai-novel-writer:1.4.0 node:22-bookworm-slim >/dev/null
docker compose -f docker/compose.web.yaml config --quiet
docker build --pull=false --progress=plain -f Dockerfile.web \
  -t ai-novel-writer-web:1.4.0 \
  --build-arg BUILDER_IMAGE=ai-novel-writer:1.4.0 \
  --build-arg HTTP_PROXY=http://192.168.1.15:7890 \
  --build-arg HTTPS_PROXY=http://192.168.1.15:7890 \
  --build-arg http_proxy=http://192.168.1.15:7890 \
  --build-arg https_proxy=http://192.168.1.15:7890 \
  --build-arg NO_PROXY=localhost,127.0.0.1,192.168.1.15 .
```

源码复制白名单与 `.dockerignore` 排除 `.native-test-data`、`node_modules`、构建产物和私有数据。不得把生产 `/data` 复制进镜像。构建运行 TypeScript、独立 Web Vite、纯 Node bundle，并实测加载 SQLite/LanceDB/Arrow/ZIP；构建通过并不证明业务全功能通过。若工具被明确沙箱拒绝，按当前授权处理，**不要切换 Docker 配置目录规避拒绝**。

## 3. 数据、权限与备份

| 内容 | 容器路径 |
| --- | --- |
| 作品与每项目 SQLite | `/data/workspaces/<作品>/`，包括隐藏 `.ai-novel/` |
| 既有全局配置与 generation | `/data/config/ai-novel-writer/` |
| 原生迁移控制 profile | `/data/config/ai-novel-writer-web-runtime/` |
| 上传/导出暂存 | `/data/web-transfers/`；不是旧门户的 `/data/imports`/`exports` 列表 |
| 服务器凭据保护 | 数据卷内服务管理的 `server-credentials.key` 等私有密钥；跟随完整卷备份 |

WebDAV 等使用 secret-store 的凭据采用 AES-256-GCM 服务器持钥加密，并检查密钥文件所有者/私有权限。这不是 OS keychain，也不防有权读取密钥的宿主管理员。**密钥丢失可能导致已有密文不可恢复**；不能只备份作品正文或某个 generation。模型配置沿用既有 `models.json`：API key 保存在服务器私有配置文件中（原子写入模式0600），目前不是全字段加密的模型凭据保险库；HTTP/浏览器只得到空 key 和 hasCredential。完整备份包含明文模型凭据，必须按秘密保护，不能公开或作为验收附件。旧桌面 OS keychain 密文不保证能直接在 Node 中解密，需要在 UI 中安全重新录入，不应打印旧值。

当前既有数据目录为 root 所有，因此 Compose 默认 UID/GID 为 `0:0`，但 drop 全部 capabilities、禁止提权、只读根目录。若使用 `NOVEL_UID=1000 NOVEL_GID=1000`，先在所有写入者停止后检查完整数据树、私有密钥、备份和恢复的所有权/权限；**不要直接改 UID 后假定原数据可写，不要启动时递归 chown 作者数据**。入口对不可写数据退出 73。

SQLite 有 WAL。备份必须停止所有会写这一卷的桌面/原生/测试服务，再复制**整个**数据目录（隐藏文件、WAL/SHM、迁移记录、密钥、附件都保留）。选择有空间且不在数据卷内部的私有备份磁盘，例如：

```sh
# 维护窗口：先暂停入口访问，再停止实际存在的写入者。
docker stop -t 30 ai-novel-writer
# 若原生容器已运行，也要停止；不存在的容器无需执行。
# docker stop -t 30 ai-novel-writer-web

# 把 /PRIVATE_BACKUP_DISK 替换为已准备好的私有备份盘；先核对容量。
umask 077
tar --numeric-owner -C /root/AI -czf \
  /PRIVATE_BACKUP_DISK/ai-novel-writer-pre-native-YYYYMMDD-HHMM.tgz \
  ai-novel-writer-data
# 记录备份文件校验值，并在隔离目录验证可读取/恢复；不要公开归档内容。
```

不在本文自动恢复、删除或重命名任何作者数据。备份失败时停止切换，按原服务状态恢复可用性。

## 4. 网络、代理与端点允许列表

Compose 使用自己的默认 bridge 网络，不配置 host 网络，也不要求附着多个已有网络。部署现场验证过独立应用 bridge 可访问本机 LAN 模型端点；其他测试服务的共享/双网络路由不代表本 Compose 有同样路由。不要为修复测试网络而更改生产路由、发布 LAN 后端或绕过端点策略。

默认环境：

- `HTTP_PROXY` / `HTTPS_PROXY`（大小写均有）：`http://192.168.1.15:7890`。
- `NODE_USE_ENV_PROXY=1`：Node fetch 的代理开关；由部署管理员控制。
- `NO_PROXY` / `no_proxy`：`localhost,127.0.0.1,::1,192.168.1.15`，因此本地 **8317 直连**，而非绕经 7890。
- `NOVEL_ALLOWED_ENDPOINTS`：精确 HTTP(S) **origin** 逗号列表，例如 `http://192.168.1.15:8317`，不是 `/v1` 路径、通配符或凭据 URL。为模型、embedding、WebDAV 分别审查后添加需要的 origin；不要以“连不上”为由允许任意地址。原生网络策略禁止重定向。
- `NOVEL_ALLOWED_ORIGINS`：浏览器真实入口 origin，**包括协议和端口**。修改域名/端口时同步修改此值，否则写请求会被拒绝。不要把任意 Origin 反射成允许值。

`docker/bootstrap.py` 属于旧桌面部署，原生入口不执行它、不 `eval` 作者配置来设置代理。更改部署环境后通过 Compose 重新创建服务，不是仅刷新页面或 `docker restart`：

```sh
# 在维护窗口、配置已审核后执行；不是“现在就切换”的指令。
docker compose -f docker/compose.web.yaml up -d --no-build
```

隔离环境先检查端点可达（不带 API key 的 `/v1/models` 返回 200 或 401 均能说明 HTTP 已抵达；不能据此认定模型授权/生成通过）：

```sh
docker exec ai-novel-writer-web node --input-type=module -e \
  'const r=await fetch("http://192.168.1.15:8317/v1/models",{signal:AbortSignal.timeout(5000)});console.log(r.status);await r.body?.cancel()'
```

若失败，先检查该容器实际网络/路由、宿主防火墙、模型服务监听及代理规则；不要把网络错误误判为模型凭据错误，更不要把凭据写进诊断命令。

## 5. nginx 原生入口要求

保持原有 Basic Auth 文件/证书，不重置共享密码；不要把网页登录密码当模型 API key。当前仓库旧 nginx 副本 **不能直接用于原生后端**。原生完整示例为 `docker/nginx-ai-novel-writer-web.conf`。管理员应备份实际生效的 `/etc/nginx/conf.d/ai-novel-writer.conf`，审核后修改对应 server 的 location。下方是需求示例，不是已安装配置；保留既有 TLS/认证配置：

```nginx
# server 级既有配置必须继续有效：
# auth_basic "AI Novel Writer";
# auth_basic_user_file /etc/nginx/.dsh_htpasswd;
# listen 8093; listen 8445 ssl; ssl_certificate/...;
client_max_body_size 64m;

location / {
    proxy_pass http://127.0.0.1:6081;
    proxy_http_version 1.1;
    proxy_set_header Host $http_host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Novel-User $remote_user; # 覆盖客户端输入，只用认证结果
    proxy_set_header Authorization "";       # 不把 Basic Auth 密码送给应用
    proxy_set_header Connection "";
    proxy_buffering off;                     # SSE 不缓存
    proxy_cache off;
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;
}
```

不得保留旧 noVNC 的 HTML `sub_filter` 或把 `/vnc.html`、WebSocket 远程桌面链路当原生入口。上游需原样保留 Cookie、CSRF、自定义客户端头和浏览器 Origin；不要注入伪造 Origin。上传限制、API 错误、SSE 及下载都必须在相同认证保护下。

配置只在管理员确认备份、数据和新服务健康后执行 `nginx -t` 并重载；验证失败不得重载。更改 location 不改变既有证书/共享密码或其他服务。

## 6. 安全切换顺序

1. **先构建、后停旧服务**。保留原镜像和实际 nginx 配置副本；为候选原生镜像记录 image ID，并打一个不会被后续重建覆盖的本地候选 tag。
2. 在**独立空卷或离线复制的数据卷**验证候选。不要把与生产同一 `/data` 的两个服务同时启动，也不要用生产凭据生成公开日志。
3. 进入维护窗口，停止旧 `ai-novel-writer` 及所有指向生产卷的测试写入者，完成第 3 节完整离线备份并核对恢复步骤。**旧桌面没有加入原生 flock**，不能依靠锁阻止它竞争。
4. 核对 Compose 挂载、UID/GID、origin、端点策略、管理员用户名和代理；运行 `docker compose -f docker/compose.web.yaml config --quiet`。
5. 仅启动原生服务：`docker compose -f docker/compose.web.yaml up -d --no-build`。确认原生 health、日志、库清单和单项目打开；不要删除旧容器/镜像以“省空间”而丢失回滚点。
6. 管理员审核并切换 nginx upstream **6080 → 6081** 与可信身份头，先 `nginx -t`，成功才重载。保持后端宿主绑定 `127.0.0.1`。
7. 用浏览器从真实 HTTPS/认证入口验证：登录、读书库、中文输入、真实上传/下载、保存后刷新/重启、SSE、取消、双标签互斥。真实模型/embedding/WebDAV/MCP 的测试独立记账；凭据缺失项不得冒称完成。
8. 只有通过本次明确验收范围，才解除维护并记录上线 image ID、数据备份点、nginx 版本和未验证项。本文不代替这次生产验收。

原生 `flock` 在进程退出（含 SIGKILL）后由内核释放，文件应一直保留。**不要删除 `.native-web.gateway.lock` 试图解锁**；删除会产生不同 inode，让两个持有者都以为自己独占。退出 75 表示有竞争进程，应查明写入者，而不是清锁。

## 7. 回滚

回滚必须停写，**不能原生和桌面同时启动**：

1. 恢复维护入口，停止 `ai-novel-writer-web`；确认所有原生 worker/同卷测试服务均停止。
2. 如果原生已修改数据，先保留一份切换后的完整私有快照以便人工合并。**不要假定旧桌面可以安全读取原生迁移后的所有状态或服务器密文。**
3. 最稳妥的桌面回滚是恢复已验证的**切换前完整数据快照**，连同原所有权/权限/密钥/隐藏文件；恢复前清楚说明新写入会回退，未经确认不能覆盖作者新增工作。不要只替换某个 SQLite 或 generation。
4. 恢复备份的旧 nginx upstream/配置及原镜像，先启动旧桌面服务，检查旧健康；`nginx -t` 成功后重载回原入口。
5. 验证原作品与设置，记录回滚点和需人工迁移的新工作。保留原生诊断与备份但限制权限，不发布含作者内容或凭据的文件。

若仅回滚到**另一个兼容的原生镜像**，也应先离线备份和确认数据格式兼容，使用已记录的不可变镜像 ID/tag，不要把正在被重建的 `1.4.0` 标签当固定版本。

## 8. 已完成验证与明确未完成项

已验证的**候选基础镜像**：

- ID：`sha256:5ab95634b969fd6e09b98f611e57f7d878223d3e21db1eed7ed00891a320ad4b`，520,190,633 字节。后续补丁集成会生成不同 ID，应重新记录，不能混用证据。
- 构建通过 TypeScript/Web/Node bundle；最终镜像 SQLite native ABI、LanceDB、Arrow、ZIP 导入成功。
- **隔离空卷**，只读根、drop ALL capabilities、no-new-privileges 下正常健康；`/healthz` 返回 `runtime: node`、`electron: false`。
- 无可信身份头 bootstrap 返回 401；隔离可信代理头测试 bootstrap 返回 200 与 CSRF。后者仅测试代理契约，不是允许外部伪造头。
- 第二容器争用同一隔离卷退出 **75**。
- 杀死 gateway 后同容器重启、SIGKILL 整个容器后新容器接管均成功；锁 inode 连续不变，没有删锁或陈旧 PID 阻塞。
- 实际运行打包的 Python 安全文件 helper，读取能力内的真实测试文件成功；Python/flock 存在，Electron/Xvfb/x11vnc/websockify 可执行文件不存在。
- 最小闭包 35 包，没有 Electron/sharp；测试容器和工作区测试卷均已清理，生产数据未复制/修改。

上述基础候选早于最后的响应清洗等补丁整合，**不是最终生产镜像或完整安全/功能验收**。本说明不宣称已切换线上、验证全部模型/知识库/导入归档/备份/MCP，亦不把第三方服务无凭据的项目标为完成。

云凭据/绑定元数据已采用**每文档同步读-改-写序列化**，不是伪装成单全局 owner，也不是跨两个 JSON 的原子事务。其未知/损坏遗留锁会保守拒绝；仅在所有写入者停止并审核后处理。不要将云元数据锁与可自动随进程释放的 gateway 内核 `flock` 混淆。

## 9. 常用检查与排错

```sh
docker compose -f docker/compose.web.yaml ps
docker logs --tail 100 ai-novel-writer-web
curl --noproxy '*' -fsS http://127.0.0.1:6081/healthz
# 以上 health 无需凭据；业务页面/API 必须从经过认证的 nginx 入口访问。
```

| 现象 | 处理 |
| --- | --- |
| 原生启动退出 75 | 同卷已有原生 gateway；确认并停止竞争者，不删除锁文件 |
| 退出 73 / `SERVER_SECRET_KEY_INVALID` | 核对挂载/UID/GID/私有密钥权限；停止服务后修复，不放宽成公开权限、不自动递归 chown |
| nginx 502 | 检查 6081 health、容器日志、upstream；不要临时放开 LAN 后端 |
| `AUTHENTICATION_REQUIRED` | 核对 nginx Basic Auth 与覆盖的 `X-Novel-User $remote_user`；不是模型 API key 问题 |
| `ORIGIN_DENIED` / CSRF | 浏览器真实协议/域名/端口与允许 origin 一致，代理不改 Origin，刷新过期会话；不要禁用校验 |
| 模型连接超时 | 从该容器网络检查 LAN8317/代理7890、NO_PROXY 和服务监听；不以其他测试容器路由代替实际检查 |
| `WEB_ENDPOINT_NOT_ALLOWED` | 管理员审核精确 origin 后改部署配置并重建容器；不允许通配私网/任意 URL |
| 写入显示另一标签占有 | 关闭/离开原写标签，按 UI 恢复/重开流程处理；不要手改 session lease 或数据库 |
| 页面仍是远程桌面 | 浏览器缓存/实际 nginx 仍指6080；审核切换，不以 `/vnc.html` 验收原生 Web |

日志也可能含作者文本或第三方错误；只在受控环境查看，分享前脱敏。不要在运维文档、命令历史或截图中记录模型/WebDAV 密钥、Basic Auth 密码、Cookie 或 CSRF token。
