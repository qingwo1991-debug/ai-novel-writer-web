# 原生 Web 正式入口切换记录

## 当前状态

- 官方入口：`https://192.168.1.15:8445`（优先），原HTTP `http://192.168.1.15:8093` 仍需密码。
- 原nginx Basic Auth文件、TLS证书与私钥均与切换前私有备份逐字节相同；没有重置登录。
- 导航卡仍叫“书童 AI 小说写作”，链接改为HTTPS，容器状态关联 `ai-novel-writer-web`。
- **待用户本人登录验收**。代理契约/环回测试不是用户登录；没有获取或绕过其密码。

## 实际运行镜像

```text
ghcr.io/qingwo1991-debug/ai-novel-writer-web@sha256:84801a8a9016f115899c5be419cf2a29a1d112f82be3312fdc12609b0e95cf37
```

- GHCR对应源码：`d18f43f987c0a02e25db96f3b70cb76b1eae85b8`。
- 实际Docker image ID：`sha256:00010f8df6050c89d430eb35222bbc5610a4e8c45dbfb4bca9aa25273d06d977`；520,247,903字节。
- 原生容器：`ai-novel-writer-web`，独立 `ai-novel-writer-native_default` 网桥，数据仍挂原作者目录，不复制替换作品。
- 服务器只拉取运行，不在服务器构建。Compose已删除build段，并强制显式 `NOVEL_IMAGE`；根目录被忽略的 `.env` 保存固定摘要，不含凭据。所有命令必须显式 `--env-file .env`。
- 首次未显式指定env文件时Compose选中了本地验收镜像；在切公开upstream前已发现、重建为上述GHCR镜像并核实实际image ID，未把本地ID冒充GHCR。

## 备份与旧写入者

- 停止旧桌面后，完整私有归档包含隐藏文件、SQLite/WAL、全局generation、服务器密钥/凭据及附件。私有备份留在宿主专用 `ai-novel-writer-backups/pre-native-<UTC>` 目录，目录0700、归档0600，不发布为附件或推送仓库。
- 实际解包验证：**234个目录项、182个普通文件、1个停机后剩余符号链接**的类型/权限/UID/GID/内容或链接目标完全一致；恢复副本中5个SQLite库 `quick_check=ok`，并记录归档SHA256。
- 旧 `ai-novel-writer` 容器和 `ai-novel-writer:1.4.0` 镜像保留。旧容器已停止且 `restart=no`，防宿主重启后与原生同时写卷。
- 回滚先停止原生并保留切换后数据；如需要恢复旧快照，应先说明新增写入回退并取得确认。恢复数据兼容性、旧nginx/导航后才重启旧服务及恢复restart策略，详见部署指南。**不删除gateway锁文件。**

## 已核实的正式环境边界

1. 实际GHCR镜像健康：`runtime=node`、`electron=false`；只发布宿主 `127.0.0.1:6081`，只读根、cap-drop ALL、no-new-privileges与受限tmpfs保留。
2. 旧writer停止；生产库启动ready，原有单个项目能预览和打开，原项目UUID未改；两份8317模型配置保留，公开结果key为空、hasCredential为真。
3. 正式容器所在网桥访问LAN8317 `/v1/models` 返回200。未把无凭据列表HTTP200当成付费生成授权证明；此次切换检查无模型派发、无显式正文编辑。
4. 原生React HTML存在且不是VNC页面；真实nginx配置 `6080→6081`、认证后覆盖身份头并剥离Authorization，SSE不缓冲。`nginx -t`成功并重载。
5. 官方HTTP/HTTPS的首页和bootstrap在**未登录、伪造身份头**时全部401；不放开后端、不增加免密location。
6. 独立GitHub CLI Packages API仍缺read:packages而403，但现有Docker GHCR凭据成功读取清单并拉取固定镜像；匿名令牌请求401。未打印/新增/更换token或改变包可见性。

## 用户验收与明确保留项

登录HTTPS入口后，请核对书库/既有作品、中文编辑保存刷新、模型选择、一次小额生成、导出与恢复，遇到错误保留提示和操作步骤。局域网自签证书可能需在可信网络中核验证书后接受警告。

真实UI规划→蓝图→Gemini正文→GPT审改→合并→单次定稿，以及真实导入/下载/归档恢复/双作品隔离，已在独立副本中有界验证，见其他验收文档；没有在正式作品上重放付费步骤。

- 外部embedding端点此前404，仅FTS降级已验证，不宣称向量检索通过；WebDAV与外部MCP真实服务凭据仍未配置验收。
- 全部32功能族的细分失败/陈旧来源/恢复/批量等场景仍按 `FEATURE-MATRIX.md` 保留，不把部署成功称作全功能验收。
- 浏览器仍编排整条流程，运行时保持标签打开；不是多租户或任意MCP程序的OS沙箱。
