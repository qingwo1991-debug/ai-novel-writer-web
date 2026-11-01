# 部署验收记录

## 当前部署

- 应用：AI-Novel-Writer / 书童 Pagekin 1.4.0。
- 镜像：`ai-novel-writer:1.4.0`，ID `sha256:9a6be22fc866f129a8cb949b83a583b09c4d716d45cbf3251c00aaf892966eff`。
- 容器：`ai-novel-writer`，`--init`、`--restart unless-stopped`、共享内存 1 GB。
- 发布端口：Docker 后端仅 `127.0.0.1:6080`；5900 不发布，容器内 loopback；最终容器未开启 NOVEL_DEBUG。
- 对外入口：nginx `http://192.168.1.15:8093/` 或 `https://192.168.1.15:8445/`，复用 DSH Basic Auth 和证书。
- 导航：`http://192.168.1.15:3008/` → AI & 自动化 → 书童 AI 小说写作。
- 持久化：`/root/AI/ai-novel-writer-data` -> `/data`。

## 已执行并通过

1. `pnpm build`（TypeScript + Vite）成功，最终镜像构建成功。上游存在 Vite 弃用、chunk 大小等警告，未阻止构建。
2. Electron 44.9.3 内原生依赖：better-sqlite3 内存 SELECT 成功；LanceDB native import 成功。
3. 浏览器实际渲染中文书房；使用 Playwright 连接容器内部 CDP，对原应用新建作品操作，原生 GTK 选择 `/data/workspaces`，创建 `部署验收测试`。
4. 首次实测发现上游 `SECURE_FS_UNSUPPORTED_PLATFORM`；增加 Linux Python descriptor-bound helper 后，通过原应用 IPC 成功 list-dir、写中文正文、读回完全一致，UI 打开作品无该错误。
5. 保存测试大纲到真实项目数据库；重启容器后重新读取一致；进一步删除重建容器、更换最终镜像后仍一致。测试正文文件也在同一宿主挂载目录中。
6. noVNC 实际 WebSocket 连接成功；浏览器门户输入中文、点击发送后，从 X clipboard 读取内容完全一致。
7. 上传中文文件名和中文正文成功，下载字节一致；重复上传 409；路径穿越下载 400；跨 Origin 写请求 403。
8. Electron **正常主进程**（不只是 RUN_AS_NODE）检查：Node 24.22.0、`HTTPS_PROXY=http://192.168.1.15:7890`、global dispatcher 为 EnvHttpProxyAgent，正常主进程 fetch `https://registry.npmmirror.com` 返回 200。代理设置修改后必须重启。
9. `docker compose -f docker/compose.yaml config --quiet` 通过；`git diff --check` 通过。

## 针对性自动测试

- Linux helper Python 安全与协议测试：14/14 通过。
- Linux TS 实际 helper 集成测试：4/4 通过。
- 原安全适配层测试：4 通过、23 个仅 Win/mac 平台测试跳过。
- Docker 配置初始化、文件名与中文剪贴板单元测试：6/6 通过。

复跑示例（依赖已安装的容器内执行 TS 测试；不要在只读主机安装依赖）：

```bash
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s electron/security/__tests__ -p test_linux_safe_file_system.py -v
# 容器中项目目录 /app
pnpm exec vitest run electron/security/__tests__/linux-safe-file-system.test.ts electron/security/__tests__/windows-safe-file-system.test.ts
```

`docker/smoke.mjs` 是可选验收工具。仅使用容器内部 loopback 调试端口，不得发布调试端口。正式容器不启用调试，不需要运行该脚本。

## 后续模型配置与验收

按用户要求，使用其提供的凭据连接 `http://192.168.1.15:8317/v1`（OpenAI 兼容协议）。凭据只保存在服务器应用私有模型配置中，文件权限为 0600；不在本说明中记录。

| 用途 | 模型 ID |
| --- | --- |
| 默认生成、正文写作、定稿资料整理 | `gemini-3.8-flash` |
| 设定/架构/蓝图规划、审稿、修稿 | `gpt-6.1-sol` |

- 通过鉴权 `/v1/models` 查询确认上述模型名称存在。
- 两个模型均通过书童原生 `llm:test-connection`（实际生成请求）。
- 两个模型均通过书童 `llm:generate-stream`，收到中文「模型连接成功」，正常 `finishReason: stop`。
- 两个模型均通过 `response_format: json_object` 非流式测试，返回可解析 JSON，正常 `finishReason: stop`。
- 默认输出预算设为 16384 tokens、温度 0.8；这属于应用操作参数，并非对服务商上下文/最大输出能力的已验证声明。
- 重启后两个模型及阶段分工均保留；作品界面的「还没有连接 AI 模型」提示消失。
- 未配置 embedding 模型；本次只使用用户指定的两个生成模型，不将它们冒充向量模型。

## 3008 导航 / nginx 鉴权接入

- 新增 `/etc/nginx/conf.d/ai-novel-writer.conf`，副本 `docker/nginx-ai-novel-writer.conf`；只新增书童 server，不更改 DSH 原入口、共享密码或证书。
- nginx 配置校验通过，热重载成功；HTTP 8093 和 HTTPS 8445 未认证访问均为 401，要求 Basic Auth。
- `/`、`/vnc.html`、`/portal.js`、`/api/files` 和 `/websockify` 未认证请求均为 401；未对静态资源或桌面连接开认证例外。
- Docker 仅发布 `127.0.0.1:6080`；本机后端 HTTP 200、健康检查通过，`192.168.1.15:6080` TCP 连接被拒绝。
- 3008 首页服务端 HTML 已包含「书童 AI 小说写作」与 `192.168.1.15:8093` 链接。
- 用户选择自行登录验收；没有提供 DSH 登录凭据，因此本次**未声称已验证登录后的 nginx WebSocket、剪贴板、上传下载完整链路**。这些后端功能此前直连验收已通过。
- 最终服务由独立 Compose 项目 `ai-novel-writer` 管理；复用原数据挂载，不清除其他服务。

## 尚未验证 / 限制

- 模型连通、中文流式及 JSON 输出已验证，但完整长篇生成、审稿/修稿流程、嵌入/知识库、云备份等仍未做全链路验收。不能声称所有上游功能已经 100% 测试。
- 并非多账号原生 Web 应用；所有已通过 nginx 认证的访问者共享同一桌面。HTTP Basic Auth 不加密凭据，应使用可信局域网/VPN 或 8445 HTTPS。
- 原生文件选择框属于服务器；网页上传下载只操作 imports/exports 顶层。
- Linux helper 依赖 Python 3 与 `renameat2` 的 NOREPLACE/EXCHANGE，目标文件系统不支持时会拒绝写入。安全边界不保证抵御同一目录下持续主动改名攻击的恶意宿主进程；Linux 无 inode-conditional rename。
- Windows/mac 安装包自动更新不适用，应用外链不保证能打开访问者本机浏览器。
- 为验证保留了 `部署验收测试` 作品、`部署验收正文.txt` 和 `imports/部署验收上传.txt`；均为本次人工测试内容，可在确认后自行删除。
- 本次只移除了明确识别的旧书童镜像及其构建缓存，没有清理其他服务。当前服务器磁盘仍较紧张（验收结束约剩 1.9 GB），建议后续迁移到容量充足的磁盘。
