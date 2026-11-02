# Go 基础层：GitHub Actions 验证记录

## 实际执行

源码提交：`a8ae8711cd52c5ce30ce7f830b937294f151428b`。

GitHub Actions：[Go backend migration / 41373886340](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41373886340)，2026-11-02，**success**，job 耗时 3m20s。

全部编译和测试在 GitHub 执行；本机只修改源码、轻量静态审查和读取远端结果，没有运行 Go 编译、单元测试、竞态检测或 Docker 构建。只下载了小型测试/格式化证据，未拉取二进制。

## 已通过

| 项目 | 结果 |
|---|---|
| Go 1.26.0 工具链与固定 SQLite 模块校验 | PASS |
| Runner 上格式化，输出修正 patch | PASS |
| `go vet ./...` | PASS |
| 合成数据单元/HTTP 契约测试 | **45 顶层测试；含子用例 108 项；0 fail** |
| `go test -race` | PASS |
| Go 服务二进制构建 + SHA256 | PASS |
| `PATH=/nonexistent` 下实际二进制启动，无 Node fallback | PASS |
| 健康 runtime=go、electron=false、migrationComplete=false | PASS |
| 未认证 bootstrap 401，合成可信身份 bootstrap 200 | PASS |
| 生产模式/作者数据挂载及非环回监听拒绝 | PASS |
| 小型测试证据和非生产二进制 GitHub artifact | 已上传，保留 7 天 |

通过的 5 个测试 package：`internal/policy`、`internal/runtime`、`internal/security`、`internal/transport`、`internal/store`。SQLite 测试仅创建临时合成数据库，不访问作品、服务器配置、模型凭据或正式挂载。

GitHub 的缓存恢复/保存服务产生非致命警告；未影响测试、竞态、编译和 smoke 通过。固定 SHA 的 Actions 存在旧 Node20 声明的弃用警告，runner 强制 Node24 执行；这是 CI action 的构建依赖，不是 Go 二进制的运行依赖声明。

## 不代表什么

- 281 项公开业务操作仍未绑定为 Go HTTP 业务实现，详见 [GO-MIGRATION-MATRIX.md](GO-MIGRATION-MATRIX.md)。安全/租约/传输与草稿仓储模块通过，不等于用户业务端到端可用。
- 草稿正文 CAS/读取已有 Go 源码和合成测试，但 schema 准入、项目打开、迁移与 HTTP 领域绑定尚未完成，不能对原作者数据库启用。
- AI 生成/审稿修稿定稿、后处理、LanceDB、归档、WebDAV、MCP 等仍待迁移。原 Node 通过的验收不能转成 Go 通过。
- 构建物明确标注非生产 scaffold；没有发布 Go 镜像或部署。正式容器保持原固定 Node 镜像；旧 Node 自动发布已改为仅手动 dispatch。

下一阶段必须先实现数据准入/物理路径安全，再接入作品与草稿纵向业务；所有新增编译与测试继续交由 GitHub Actions。
