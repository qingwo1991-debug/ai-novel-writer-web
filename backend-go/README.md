# Go 服务端迁移（进行中）

目标：保留现有 React/CodeMirror 网页及 HTTP/SSE 契约，将全部服务端业务迁移成 Go，最终核心运行时不依赖 Node.js、Electron 或 VNC。不是 Go 反向代理包裹 Node。

**此目录当前不是可替换正式服务的完整实现。** 已知公开操作但没有 Go handler 时返回 `501 GO_OPERATION_NOT_MIGRATED`，绝不转发给 Node 或伪造成功。现阶段二进制拒绝生产模式、数据挂载和非环回监听；没有读取现有作者数据库。

## 已实现源码范围

- `internal/security`：可信代理身份、会话 cookie、CSRF、Origin、JSON 限额/参数结构校验及兼容错误封装。
- `internal/runtime`：按会话 + 标签绑定的独占项目租约、在途请求围栏、同标签重开保留租约、失败打开仅回滚新占用；有界 SSE 历史/重放/慢连接断开。
- `internal/policy`：读取现有显式 281 操作路由清单，拒绝暴露内部/桌面排除项。清单不是授权，也不代表业务已迁移。
- `internal/transport`：bootstrap、健康与 SSE、RPC envelope；域操作仅显式注册，未迁移路由/操作返回 501。
- `internal/store`：现有 SQLite 布局的草稿读取和事务化正文 CAS 原型；仅对已验证数据库开放，**五项草稿 handler 已通过隔离私有副本 HTTP/CAS 集成，但没有绑定正式 binary，不能持久保存作者项目**。
- `internal/projectfs`、`internal/admission`、`internal/projectdata`：只读物理路径/项目身份与 SQLite v7 准入检查；连接私有完整 DB/WAL 副本而非原库，保留写者排除前置条件。新增固定 128 KiB buffer 流式快照，检查流程不再使用旧 64 MiB 内存快照；opt-in 行级内容证明暂有限量内存排序，部分 manifest 日期格式有兼容差异，v6→v7 仅空审稿周期私有副本 lane 已验证，其余旧版本升级待移植。详见 `web/GO-DATA-ADMISSION.md`。
- `internal/projectsession`：私有 scratch copy 的 schema 准入、cookie session+tab 所有权、冻结上下文、refresh 保留编辑、在途围栏与可重试 cleanup；非持久作者会话，见 `web/GO-PROJECT-SESSIONS.md`。
- `internal/secrets`：旧 AES-GCM envelope 和私人 key 文件兼容模块，不自动读取默认作者 key；全局配置/provider 尚未连接，见 `web/GO-CREDENTIAL-COMPATIBILITY.md`。
- `cmd/novel-server`：仅供合成测试的 Go 基础二进制，不打开数据库、不启动 Node 子进程。已绑定两个固定浏览器导航意图；其余业务仍不支持。

完整操作清单和迁移门见 `web/GO-MIGRATION-MATRIX.md`、`web/GO-MIGRATION-PLAN.md`。AI 生成、定稿及后处理、知识库、归档和其他全部业务仍需要逐项迁移并验证。

## 只在 GitHub 编译和测试

按部署者要求，**不要在本机运行 `go test` / `go build` / 竞态检测、Docker 构建或大型依赖安装**。本机仅源码修改、轻量静态检查及查看 GitHub Actions 结果。

工作流：`.github/workflows/go-backend.yml`，随 Go 源码或契约变化触发，也可手动 dispatch。

远端执行：

1. 固定模块校验；
2. Go 格式化（产生可下载的小型修正 patch）；
3. `go vet`；
4. 合成临时 SQLite / httptest 单元与 HTTP 契约测试；
5. `go test -race`；
6. 构建 Go 二进制并进行无作者数据的环回 smoke；
7. 上传测试证据及非生产二进制，有效期 7 天。

没有使用生产卷、provider key、作者作品或部署密钥。此工作流没有 packages 写权限、不发布镜像、不修改正式容器。现有 Node 镜像发布改为手动 dispatch；当前正式服务继续固定原镜像，未切换到本目录实现。

## 数据与兼容硬要求

- 原 SQLite 数据不静默重建；版本号还必须结合 schema 指纹、完整性/外键检查与受控迁移。
- 保存 CAS 比较上次确认持久化的完整正文，基线缺失拒绝，空字符串合法；IMMEDIATE 事务内检查可编辑状态后一起更新正文/字数。
- 定稿提交、修稿合并、幂等 receipt、来源证明不能为了“接口能返回”而简化掉。
- LanceDB 不只是可重建的向量索引，既有文档和文本必须保留并真实验证迁移。
- 服务器持钥加密格式、grant 的调用者/用途/时效以及项目租约都需兼容；不得生成新秘密覆盖旧数据。
- 管理员可选外部 MCP 工具自身的 Node/Python 依赖必须单独披露，不与核心 Go 服务的无 Node 运行声明混淆。
