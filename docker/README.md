# 书童 Pagekin — 局域网 Docker 部署

## 访问

**从导航页 http://192.168.1.15:3008/ → AI & 自动化 →「书童 AI 小说写作」进入。**

- 带密码入口：**http://192.168.1.15:8093/**
- HTTPS 入口：**https://192.168.1.15:8445/**（复用 DSH 的本机证书，浏览器可能提示自签名证书）
- 登录账号和密码：**与 DSH 8091 入口相同**；复用 `/etc/nginx/.dsh_htpasswd`，不新建或重置密码。8317 模型 API Key 不是网页登录密码。
- 后端只绑定 **127.0.0.1:6080**；旧的 `http://192.168.1.15:6080/` 已不能从局域网直连，不能绕过 nginx 登录。

浏览器显示服务器上运行的完整 Electron 桌面，不是把桌面应用伪装成不带后端的静态网页。底层使用 Xvfb + x11vnc + noVNC，保留原应用的数据层和写作界面。首次启动默认中文。

所有页面、文件接口和 WebSocket 均由 nginx Basic Auth 保护。**HTTP Basic Auth 不加密传输，建议使用 HTTPS 或可信局域网/VPN。** 登录成功的访问者仍然共同操作同一个桌面、读写作品和模型设置，不是应用内多用户权限系统。3008 导航页本身原有的访问策略不变。VNC 5900 仅容器内回环监听，不发布到宿主机。

## 首次使用

1. 打开上述地址，桌面自动连接。若连接失败，可打开 `/vnc.html` 手动连接。
2. 当前已配置 `http://192.168.1.15:8317/v1` 的 `gemini-3.8-flash` 与 `gpt-6.1-sol`，默认 Gemini 写正文，GPT 用于规划、审稿与修稿。可在设置中调整分工；已有凭据不记录在本文中。两模型的连接、中文流式与 JSON 输出均已通过测试。
3. 新建作品时点击选择保存位置，默认打开 **`/data/workspaces`**。请保持作品在 `/data` 内；选 `/app`、`/tmp` 等其他路径会在容器重建时丢失。
4. 代理默认初始化为 **HTTP `192.168.1.15:7890`**。首次初始化只补缺失设置，不覆盖你已经保存的配置。修改代理后执行 `docker restart ai-novel-writer` 使 Node 请求代理重新生效。本地 `192.168.1.15` / localhost 服务直连。当前部署只适配 HTTP 代理；不要直接改为 SOCKS 后假定全部请求可用。

## 中文输入、文件导入导出

- **中文输入**：网页顶部点「中文剪贴板」，用本机输入法输入或粘贴中文，点「发送到服务器剪贴板」。然后点击桌面内的目标输入框，按 **Ctrl+V**。不要依赖旧 noVNC 自带剪贴板传递中文，可能乱码。
- **上传**：顶部「导入 / 下载」选择本机文件上传，文件落到 `/data/imports/文件名`；随后在书童导入对话框中选择该文件。单文件最大 64 MB，同名文件不会覆盖，请先改名。大文件可用 SSH/服务器共享目录传入宿主机 `ai-novel-writer-data/imports/`。
- **下载**：在书童里导出时选择 `/data/exports`，再点顶部「刷新文件列表」下载。下载入口只列出 imports/exports 顶层普通文件，不公开配置和 API Key。
- 桌面的文件选择框看到的是**服务器容器路径**，不是访问设备的磁盘。刷新或关闭浏览器不会关闭服务器上的写作桌面。
- noVNC 默认按浏览器窗口缩放，服务器虚拟屏幕为 1600×1000。右侧黑边/留白不代表数据丢失。

## 数据位置与备份

宿主机 **`/root/AI/ai-novel-writer-data/`** 挂载为容器 **`/data/`**：

| 内容 | 容器路径（对应宿主机目录下同名子目录） |
| --- | --- |
| 小说项目、正文、每部作品的 SQLite 数据库 | `/data/workspaces/<作品名>/`（包含隐藏的 `.ai-novel/`） |
| 全局配置、模型及 API Key | `/data/config/ai-novel-writer/`（应用会管理内部 generations 目录） |
| Electron 浏览器会话 | `/data/user-data/` |
| 上传 / 导出 | `/data/imports/`、`/data/exports/` |

删除或重建容器时保持同一挂载即可保留数据；**持久化不是备份**，误删作品也会同步删除服务器文件。数据目录含 API Key，应限制服务器访问权限。

SQLite 使用 WAL，不建议应用写入时直接打包。完整备份示例（先确认备份磁盘容量，最好放到另一块磁盘）：

```bash
docker stop -t 30 ai-novel-writer
tar -C /root/AI -czf /你的备份磁盘/ai-novel-writer-$(date +%F-%H%M).tgz ai-novel-writer-data
docker start ai-novel-writer
```

备份失败也要记得启动容器。恢复时停容器、还原整个目录后再启动；也可以使用应用自带项目归档功能逐作品备份。

## 管理与重建

```bash
docker ps --filter name=ai-novel-writer
docker logs --tail 100 ai-novel-writer
docker restart ai-novel-writer
docker stop -t 30 ai-novel-writer
docker start ai-novel-writer
```

本机 Docker Buildx 配置路径需要放在可写的工作区：

```bash
cd /root/AI/AI-Novel-Writer
export BUILDX_CONFIG=/root/AI/.docker-buildx DOCKER_CONFIG=/root/AI/.docker
docker build -t ai-novel-writer:1.4.0 .
# 构建成功后才移除旧容器；/data 对应的宿主机目录不动
docker rm -f ai-novel-writer
docker run -d --name ai-novel-writer --init \
  -p 127.0.0.1:6080:6080 \
  -v /root/AI/ai-novel-writer-data:/data \
  --shm-size=1g --stop-timeout=30 --restart unless-stopped \
  ai-novel-writer:1.4.0
```

提供 `docker/compose.yaml` 作为同等配置（独立 Compose 项目名 `ai-novel-writer`，不要加 `--remove-orphans` 操作其他项目）。已有同名容器首次切换 Compose 时，应先停止并移除旧容器，保持数据挂载一致，再执行：

```bash
export BUILDX_CONFIG=/root/AI/.docker-buildx DOCKER_CONFIG=/root/AI/.docker
docker compose -f docker/compose.yaml up -d --build
```

### nginx / 导航集成

- 生效配置：`/etc/nginx/conf.d/ai-novel-writer.conf`。
- 同步副本：`docker/nginx-ai-novel-writer.conf`；新主机需先配置 DSH 的 `.dsh_htpasswd` 和证书，再安装此配置并执行 `nginx -t && nginx -s reload`。
- nginx 保留外部 Host 及端口供门户 Origin 校验使用，支持 noVNC WebSocket、64 MB 上传，且不把网页登录凭据转发给后端。
- 导航配置：`/root/docker/homepage/config/services.yaml`，在「AI & 自动化」加入书童卡片。原导航备份在 `docker/backups/homepage-services.before-novel.yaml`；备份不进入镜像。
- 仅安装 Compose 不会安装宿主机 nginx 配置；后端保持 loopback 后，需通过 nginx 的 8093/8445 访问。不要为图方便把 6080 再发布到 LAN。

拉取源码更新需自行确认兼容性、先备份再升级。例如使用临时代理，不修改主机全局 Git 配置：

```bash
git -c http.proxy=http://192.168.1.15:7890 -c https.proxy=http://192.168.1.15:7890 pull --ff-only
```

构建依赖使用 npm/Electron 镜像源；Git 代理与应用运行代理是两件事。镜像 tag 与应用当前源码版本对应，更新源码时请相应修改标签。

## 能力边界

- 这是**浏览器远程操作完整桌面应用**，不是重新开发的原生 Web 版。可访问原有写作、设定、章节、拆文、模型设置等界面，但不能据此保证上游每个功能都无缺陷。本部署补充了上游缺失的 Linux 安全文件读写 helper（Python 3 + Linux `renameat2`）；不能直接省略此改动使用原始上游源码构建，否则作品文件操作会报 `SECURE_FS_UNSUPPORTED_PLATFORM`。
- AI 生成功能、向量检索/知识库等依赖生成模型、嵌入模型及服务网络；当前两模型已通过基础生成测试，尚未配置 embedding，不能宣称已端到端测试所有写作和知识库流程。调用第三方模型时相关提示词/正文会发送给所选服务商，并非完全离线处理。
- 软件更新通过重建 Docker 镜像完成，不使用 Windows/macOS 安装包升级流程。
- 应用内「打开官网/服务商」等外部链接不保证能打开本机浏览器；请在自己的浏览器直接打开相应网站。
- 不支持各设备独立桌面/独立账号；多设备看到的是同一会话，避免同时编辑。
- 容器内 Electron 以 root + `--no-sandbox` 运行以兼容当前方案，不要导入不可信可执行程序或把此服务暴露给不可信用户。

## 故障排查

| 现象 | 检查 |
| --- | --- |
| 页面无法打开 | 从 3008 导航进入 8093，或使用 8445 HTTPS；检查 nginx 和容器状态。6080 是本机后端，局域网直连失败是预期行为 |
| 提示用户名密码 / 返回 401 | 使用 DSH 8091 同一账号密码，不是模型 API Key；不要重置共享密码文件 |
| 登录后返回 502 | 检查 `curl http://127.0.0.1:6080/`、`docker logs ai-novel-writer` 及 nginx upstream 错误 |
| 页面能开但桌面未连接 | 查看 `docker logs ai-novel-writer`；稍后刷新，或访问 `/vnc.html` 手动连接 |
| 模型请求失败 | 检查 API Key/地址/模型名、代理是否允许来自 Docker 的连接；改代理后重启容器 |
| 文件找不到 | 本机文件须先上传；应用对话框 Ctrl+L 输入 `/data/imports` 或 `/data/exports` |
| 中文输入乱码 | 使用页面顶部专用中文剪贴板，不用 noVNC 原生剪贴板 |
| 服务器空间不足 | `df -h /root/AI`；勿删除数据目录或盲目清理其他服务。迁到大容量磁盘后更新 bind mount |

启动脚本会在 Electron、VNC 或 Web 服务退出时关闭容器，Docker 的 `unless-stopped` 策略可在非手动停止后重启；关闭窗口可能触发重启，退出访问只需关闭浏览器标签页。
