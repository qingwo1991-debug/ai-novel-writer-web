# Go 服务端迁移（进行中）

目标：保留现有 React/CodeMirror 网页及 HTTP/SSE 契约，将全部服务端业务迁移成 Go，最终核心运行时不依赖 Node.js、Electron 或 VNC。不是 Go 反向代理包裹 Node。

**此目录当前不是可替换正式服务的完整实现。** 已知公开操作但没有 Go handler 时返回 `501 GO_OPERATION_NOT_MIGRATED`，绝不转发给 Node 或伪造成功。现阶段二进制拒绝生产模式、数据挂载和非环回监听；没有读取现有作者数据库。

## 已实现源码范围

- `internal/security`：可信代理身份、会话 cookie、CSRF、Origin、JSON 限额/参数结构校验及兼容错误封装。
- `internal/runtime`：按会话 + 标签绑定的独占项目租约、在途请求围栏、同标签重开保留租约、失败打开仅回滚新占用；有界 SSE 历史/重放/慢连接断开。
- `internal/policy`：读取现有显式 281 操作路由清单，拒绝暴露内部/桌面排除项。清单不是授权，也不代表业务已迁移。
- `internal/transport`：bootstrap、健康与 SSE、RPC envelope；域操作仅显式注册，未迁移路由/操作返回 501。
- `internal/store`：现有 SQLite 布局的草稿读取和事务化正文 CAS 原型；仅对已验证数据库开放，**九项草稿和三项主台账 handler 已通过隔离私有副本 HTTP/CAS 集成（主台账synopsis生成/恢复owner仍拒绝），但没有绑定正式 binary，不能持久保存作者项目**。
- `internal/store/reviewrevision.go`、`transport/reviewrevision.go`：14项审稿/修稿RPC隔离业务，完整来源快照、事务内index分配与pending替换、作者全文CAS合并和防重放覆盖；162项原TypeScript repository差分+合成并发HTTP通过。绑定cycle/generation/recovery拒绝，未绑定main、无持久保存。合并与普通draft CAS拒绝共享content_id误覆盖，原始BLOB不强转string，见 `web/GO-REVIEW-REVISION.md`。
- `internal/store/posthistory.go`、`transport/posthistory.go`：后处理run/step账本与LLM历史/nullable统计的9项隔离RPC；定稿source身份、原子汇总、随机UUID与诊断不泄露。不是实际后处理执行器或billing authority，main不绑定，见 `web/GO-POST-HISTORY.md`；GitHub资格记录见 `web/GO-CI-ACCEPTANCE.md`。
- `internal/store/narrative.go`、`transport/narrative.go`：作者叙事计划/冻结定稿证据确认/活跃线索与一致性豁免9项隔离RPC，JS trim/UTF16、原状态/超期/区间外匹配与豁免upsert；不是完整一致性预检或AI工作流，见 `web/GO-NARRATIVE.md`。main未绑定。
- `internal/store/plottree*.go`、`transport/plottree.go`：剧情树完整来源聚合、原JSON.stringify顺序SHA256、历史/当前来源双快照校验与事务Save原型；公开save仍拒绝`GENERATION_GRAPH_ADMISSION_REQUIRED`。read/clear仅隔离HTTP，生成权限图未迁移，旧异常UTF8/孤立surrogate与未枚举Date.parse格式待全量验收，见 `web/GO-PLOT-TREE.md`。
- `internal/store/blueprints*.go`、`transport/blueprints.go`：普通蓝图CRUD、范围幂等commit+双ledger、作者notes/time组保留与pending-sync读取；生成/恢复owner、角色完成证据、clear recovery未迁移。Unicode/punctuation canonical keys显式拒绝，序列化JSON字段只证明语义而非任意键插入顺序；见 `web/GO-BLUEPRINTS.md`。main未绑定。
- `internal/projectfs`、`internal/admission`、`internal/projectdata`：只读物理路径/项目身份与 SQLite v7 准入检查；连接私有完整 DB/WAL 副本而非原库，保留写者排除前置条件。新增固定 128 KiB buffer 流式快照，检查流程不再使用旧 64 MiB 内存快照；opt-in 行级内容证明已用私有外部排序并通过>64MiB总行量验证（单行限制/内存峰值/磁盘预算仍见专文），部分 manifest 日期格式有兼容差异，v4→v5、v5→v6、v6→v7 仅空历史限定私有副本 lane 已验证，其余旧版本升级待移植。详见 `web/GO-DATA-ADMISSION.md`。
- `internal/projectsession`：私有 scratch copy 的 schema 准入、cookie session+tab 所有权、冻结上下文、refresh 保留编辑、在途围栏与可重试 cleanup；非持久作者会话，见 `web/GO-PROJECT-SESSIONS.md`。
- `internal/store.Recovery`、`projectsession.BeginRecovery`、`transport.BindRecovery`：第二十一轮隔离私有副本的恢复候选记录/列表/编辑/处置，蓝图8字段有序JSON sourceHash、草稿id/version currentness、visible正文hash/think过滤、同连接事务/readback及冻结身份注入。不是AI执行receipt、全文CAS或durable保存；main不绑定，详见`web/GO-RECOVERY-CANDIDATES.md`。
- `internal/store.Summary`、`projectsession.BeginSummary`、`transport.BindSummary`：第二十二轮legacy摘要快照写/最新读取与定稿来源/连续性投影读取；immutable outbox全文/hash核对、generation失效、冻结角色身份及原JSON容错语义。正式连续性写仍`GENERATION_FINALIZATION_ADMISSION_REQUIRED`拒绝，不当完成；main未绑定，见`web/GO-SUMMARY-CONTINUITY.md`。
- `internal/secrets`：旧 AES-GCM envelope 和私人 key 文件兼容模块，不自动读取默认作者 key；全局配置/provider 尚未连接，见 `web/GO-CREDENTIAL-COMPATIBILITY.md`。
- `internal/llm`、`internal/egress`：Go原生OpenAI-compatible/Gemini非流式/流式、UTF16可见/推理隔离、finish/usage证据、取消围栏和精确origin无proxy/redirect HTTP；72个原OpenAI/154个原Gemini provider及922个原scanner合成对照已通过，**未绑定AI工作流或RPC**，见 `web/GO-AI-TRANSPORT.md`。
- `internal/embedding`：第二十轮原生OpenAI/Gemini embedding、有界响应/index重排/维数校验、50/100协议默认batch与每批authority检查、真实egress/cancel、ECMAScript UTF16文本分块；原TS306case差分及显式lone-surrogate输出拒绝边界详见`web/GO-EMBEDDING.md`。模块不接main或KB写/AI工作流，不提升操作状态。
- `internal/writerlock`：真实openat/NOFOLLOW+flock跨进程排他，安全persistent lock inode、多进程/exit/CLOEXEC验证。第十四轮新增固定`AcquireLegacy`复用现gateway `.native-web.gateway.lock`及`Lock.Verify`；原Acquire不同名不互代。未接main/全局状态/正式生命周期，见 `web/GO-WRITER-LOCK.md`。
- `internal/projectaccess`：锁内只读作者预检，拒绝portable历史authority/restore markers，私有完整副本SQLite准入+source未变证明与cleanup；第十八轮增加可信管理员`Knowledge.Native` opt-in，组合copied native typed领域验证与document/copy状态分类，同一legacy排他至所有proof和cleanup结束。非native编译提前拒绝；不返回源DB/repository/写能力、不计operation实现。当前仅有界合成资格，非任意作者库。见 `web/GO-AUTHOR-PREFLIGHT.md`、`web/GO-JOINT-KNOWLEDGE-PREFLIGHT.md`。
- `native-lance`、`internal/lancenative`、`cmd/lance-interop`：精确Rust LanceDB0.22.3/Arrow56.2静态库+Go/CGO进程内IPC stream导入导出隔离试验；6表/14行原Node合成fixture、原生编译/5项测试/完整跨语言schema与全文互操作/物理保全已通过独立GitHub任务41420077722（源码bc14231b），仅隔离合成资格，详见`native-lance/README.md`。未接main/KB业务；第十八轮仅在可信管理员opt-in的copied preflight内组合。不通过Node/Python桥接；不能将源码或普通GoCI通过算原生通过。
- `internal/knowledgedomain`：有界合成typed快照的schema/cell、document/chunk count/order/orphan、embedding空间/active完整性验证。原TS真实22case Oracle与139领域用例在GitHub通过；自身不含Go精确logicalHash或durable authority，document/copy引用分类由第十八轮`knowledgecopy.ValidateLinks`补充，native typed reader→Go领域验证组合亦已独立通过（8native tests、10项JS保全检查）；仍为有上限的合成域。见 `web/GO-KNOWLEDGE-DOMAIN.md`。
- `internal/projectfs/knowledge*.go`、`internal/knowledgecopy`与opt-in `projectaccess.Config.Knowledge`：固定知识资产完整物理stage、缺失/legacy门、源/副本字节与identity proof、replacement-safe清理、只读registry/copy状态和rawdigest。普通Go合成回归和Go stage→native scan→原JS全schema/rows组合均已通过；默认仍是metadata-only；第十八轮显式Native opt-in追加bounded domain与copy引用分类，missing非fatal且不current，dirty合法保留，绝不意味着索引可直接用于AI；不授予作者原库写能力。详见 `web/GO-KNOWLEDGE-COPY-PREFLIGHT.md`。
- `internal/knowledgequery`、`projectaccess.InspectKnowledge`、`projectsession.KnowledgeManager`、`transport.BindKnowledge`：第十九轮隔离有界capture的4项KB只读业务（文档列表/三字段统计/无向量计数/副本读取），samecapture finalization章号注释、精确cookie+tab+context whole-call lease、固定snapshot refresh与scratch清理后无FD依赖。原TS14组完整逻辑Oracle+独立真实native→capture→HTTP验收见 `web/GO-KNOWLEDGE-READS.md`；main不绑定，无durable作者会话/保存/reindex/search/embedding写业务。
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
