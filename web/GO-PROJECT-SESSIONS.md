# Go 私有副本作品会话与草稿纵向链路

本模块是迁移中的**隔离验证链路**，不是正式作品打开/持久保存功能。所有编译、合成测试、race 和构建仅在 GitHub 执行；正式服务及作者数据不变。

## 已实现源码

- `backend-go/internal/projectsession/session.go`：`OpenScratchCopy` 把已获准 project capability 的 DB/WAL 全集复制到私有目录，v7 exact-schema 检查后才构造 repository。不会把原项目 SQLite 路径交给 SQLite，不把私有编辑复制回作者目录。
- 会话所有者由 cookie session ID + browser tab ID 共同确定，不使用用户名或路径单独授权。冻结上下文为 `{projectId,leaseId,projectPath}`。
- 同所有者 refresh 保留同一个 private DB 和 lease，不从源重新复制覆盖尚存的私有编辑；其他标签/会话不能偷用 context。同 path/UUID 的物理目录替换也拒绝刷新，不把旧 scratch 会话借给新 inode。Release 和 Close 在存在完整 repository 调用 hold 时失败；释放后旧 lease 永久失效。私有文件清理失败会保留重试记录并返回错误，`Cleanup`/再次 `Close` 可继续清理，不默默丢弃记录。
- `backend-go/internal/transport/drafts.go`：实际八个读取/查询与一个正文 CAS handler，依赖注入 admitted repository provider。所有者 hold 包围整个读/写操作。
- wire 保持现有 `ipc-client.ts`：原 typed arguments 后再追加冻结 context。保存仍是 `[id,content,wordCount,expectedProjectPath,expectedContent,context]`，没有移位 baseline，也没有 path-only fallback。
- `expectedContent` 缺失或正文/status 已变，返回原 `DRAFT_CONTENT_CONFLICT` 业务 envelope；不读取新的 server baseline 替用户重试。

## 主台账、审稿和修稿扩展

同一Manager新增`BeginProjectCore`及`BeginReviewRevisions`，各返回受完整leasehold保护的领域repository，不暴露rawDB。三项主台账及十四项审稿/修稿handler已经隔离HTTP+原TypeScript差分验证，合并/重放/跨tab/stale拒绝通过；仍不在main绑定、不发布回作者项目。当前合计26项隔离业务仅IN PROGRESS。普通draft CAS和修稿合并还拒绝跨draft/review/revision共享content_id改写，事务中检查独占性；旧schema可以存在共享引用，Go不会自动修复。

源码`a5e38a20`，[Actions 41391829983](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41391829983) 全套227顶层/1996含子用例PASS、0fail、1个非root writerlock权限skip。见`GO-REVIEW-REVISION.md`、`GO-PROJECT-CORE.md`；历史下面草稿证据仍保留原统计。

## 后处理账本/调用历史扩展

`BeginPostHistory`以同样owner+lease hold提供六项postprocess账本和三项LLM history/stats操作，cookie、tab、CSRF、stalelease和inflightrelease隔离验证。总计35项隔离业务仍未绑定main；仅账本状态不证明LanceDB/人物/定稿外部投影已执行，LLM调用记录也不是可信generation/billing receipt。见`GO-POST-HISTORY.md`。

## 叙事线索/作者豁免扩展

`BeginNarrative`同样以owner+lease hold开放六项作者线索与三项豁免RPC；证据必须匹配冻结outbox且draft现finalized，confirmation与来源/plan检查同事务。布尔身份不可被Go driver当1写入；不是授权AI直接确认的接口。总计44项隔离业务，main仍只两个导航意图；完整一致性预检/剧情树/finalization owner未迁移，见`GO-NARRATIVE.md`。

## 准入与生命周期前置条件

源写者/检查点进程必须由上层持续排除到 stage + schema 检查 + source proof 完成。scratch parent 来自可信管理员配置，不是 RPC 参数；私有目录不允许恶意并发替换。单个 Manager 的锁与 lease 不是跨进程单写者锁，不可替代部署时的全局 flock。

## 尚未接入正式二进制的业务

`project:open`/create/save、library 枚举与隐藏状态、release HTTP route、持久作者库发布、global 配置/LanceDB 协调、恢复 journal、旧版本全迁移尚未完成。因此 main **没有绑定这九项草稿操作**，也不能将 scratch editor 宣称为作者保存成功；未绑定仍返回 501。

矩阵这九项仅可标记 **IN PROGRESS — isolated HTTP/repository tests**，不能计入用户端到端完成数。当前两个固定导航操作是唯一已在 binary 绑定的业务。

## 远端证据

源码 `3a2bc966` 已通过 [Actions 41381280660](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41381280660)：全套 134 顶层 / 392 含子用例、0 fail、0 skip、10 个测试 package；vet/race/build/无 Node smoke 通过，详见 `GO-CI-ACCEPTANCE.md`。集成测试从原 TypeScript schema 的临时合成库开始，经过 authenticated HTTP、冻结 context、session lease 到真实 SQLite CAS，并证明两次相同 baseline 并发保存只有一次成功、另一次冲突，原项目字节未变。
