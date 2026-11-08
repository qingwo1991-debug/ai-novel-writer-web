# Go 后端：已完成 / 未完成 与后续计划

> 这份文件的数字来自构建断言（`production_served_count_linux_test.go`），不是手数。
> 谁加了一个操作却忘了改常量，CI 会红并把完整已服务列表打出来。

## 一、当前状态

| 项 | 值 |
| --- | --- |
| 清单里的操作总数 | 281 |
| 生产车道已服务 | **263** / 281（未服务 18，由 CI 断言计数；`9c054330` 全绿） |
| 仍未服务 | 50（每个都有具名端口缺口，见下） |
| 承载方式 | 单个 Go 进程 + 单个容器，无 Node 运行时 |
| 生产容器 | **未动**，仍跑旧镜像，回滚摘要已记录 |

### 未服务的 50 个：不是"没做"，是"缺端口"

每个未服务名字要么已经绑定、要么在文件头写明缺哪个符号（`file:line` 可复核）。
按家族：`db` 13、`agent-generation` 8、`legacy-roster` 6、`generation` 3、
`finalization-generation` 3、`import-generation` 2、`graph-generation` 2、
`character-proposal` 2、`editor-inline` 2、`review-revision` 2、`cloud-backup` 2、
其余 5。代表性缺口：导入/定稿生成需要一种本车道开不出的 run（`synthetic_generation_linux.go:386`
的准入表里没有 import 槽）；`generation:list-recovery` 三个桶里的两个没有 `planningContinuation`；
`db:*` 13 个要么读时自修复（`internal/store/blueprints.go:137` 的 `INSERT OR IGNORE`），
要么需要跨表同事务（没有导出的 `BEGIN IMMEDIATE` 句柄）。

**本轮学到的两条硬规则**（否则会反复踩）：

1. **CI 会在跑测试前格式化源码**：`gofmt -w ./cmd ./internal`（`go-backend.yml:56`、`:445`）。
   所以任何"读源码某一行当证据"的 seam 断言必须按 token 比较（`productionSeamContains`），
   逐字节比较会对完全正确的文本误报漂移。
2. **"已绑定"不等于"能回答"**：`graph-generation:begin` 曾长期在计数里，却因为
   `internal/generationowner/binding.go:35` 的 prompt key 正则拒绝 `graph-plan` 而拒绝每一次真实调用。
   计数只能证明装了 handler；成功路径必须有测试跑过一遍。

## 一之三、演练结果（用 CI 镜像 + 作者数据副本）

`scripts/go-cutover-rehearsal.py` 用 CI 产出的镜像跑真实容器，数据目录是**副本**，
按浏览器同样的协议（X-Novel-User + cookie 会话 + CSRF）驱动，结果：

- `/healthz` → `{ok:true, runtime:"go", electron:false}`；不带受信代理头时 `/api/bootstrap` 401；
  首页返回构建好的 React 外壳（`id="root"`）。
- `project:open` 成功，11 个读操作全部有答案（名册、草稿列表、模型配置等）。
- `dialog:select-project-archive-export` 发 grant → `project:archive-export` →
  SSE `browser:download-ready` → 下载 729KB 归档（manifest、freeze、project.db、authority）。
- 副本 **0 个文件被改动**，只有导出产生的那 1 个新文件。

演练中查出并修掉的两个真问题：

1. **归档导出对每一次真实调用都拒绝**：暂存写的是 `snapshot.db`，导出却打开 `project.db`，
   SQLite 顺手建了个空库，schema 校验读到 `user_version=0` → `PORTABLE_CONTEXT_INVALID`。
   已改用 `productionCloudBackupStagedDatabase`。
2. **空的 `.ai-novel/lancedb` 目录挡住了整本书的写入准入**：旧网关清空知识库后会留下空目录，
   准入把"目录存在"当成"有知识资产"，于是 `AUTHOR_WRITE_KNOWLEDGE_UNPROVED`。
   现在空目录不算资产（条目仍进 inventory，写入 diff 不受影响），目录里有文件仍然拒绝。

## 一之二、验收门槛（两段，别混）

| 门槛 | 内容 | 现在还差 |
| --- | --- | --- |
| **一：能真写一章** | 浏览器打开项目 → 生成 → 采纳 → 终稿 → 备份导出 | 导入链路；镜像发布 + compose 切换 + 作者数据备份演练 |
| **二：281 个全齐** | 知识库 19、云备份 8、MCP 8、Skills 6 等 | 绝大多数是长尾，不属于写作闭环 |

**决定**：门槛一作为验收范围。门槛二继续推进，但不阻塞交付。

### 镜像不是"没启动过"——CI 每次都在真实容器里跑一遍

`gh run` 的 `image` 作业做的是：构建 `Dockerfile.go` → 以 uid 65532、只读根文件系统、`--cap-drop ALL` 启动 → 轮询 `/healthz` → 用 `production-fixture` 播种一个真实 v7 项目 → 通过容器内的 HTTP 接口驱动 17 个操作（`project:open`、`db:draft-*`、`db:character-*`、`character-identity:read`、`db:project-core-get`、`db:blueprint-*`、`project:overview-current`、`generation:list/read`、`llm:*`）并把证据存档。

所以"镜像能起来、能读能写"是被验证过的。**没有**被验证的是：真实作者数据、compose 切换、备份与回滚演练。

## 二、已完成（不是"能编译"，是有证据）

### 2.1 车道骨架与安全边界

- 注册表门控：未绑定的操作答 `501 GO_OPERATION_NOT_MIGRATED`；未挂载的路由答 `GO_ROUTE_NOT_MIGRATED`；重复绑定在启动时致命失败。
- 单写者：flock 排他；每个写操作都经 `borrowAuthorDatabase` 走同一把锁。
- 准入：打开数据库时重算 `sqlite_master` 摘要，与钉死的契约指纹比对。
- 凭据：模型凭据不进响应；`llm:generate` 走租约与执行器，不在读路径拨号。

### 2.2 已落地的能力族

| 族 | 状态 |
| --- | --- |
| 数据读写（草稿、内容、蓝图、连续性、叙事线程、审稿、修订、恢复、后处理、LLM 账本） | 已服务 |
| 生成账本（begin / execute / read / list / cancel / pause / resume / restart / 素材决策 / 候选丢弃） | 已服务 |
| 导入运行（10 个读 + 7 个账本写） | 已服务 |
| 批次（begin-batch / read-batch / list-batches / confirm-batch-finalization / list-directory-progress） | 已服务 |
| 章节上下文（prepare-draft-context / read-context） | 已服务 |
| 可见组合（compose-visible / read-visible-composition） | 已服务 |
| 项目（open / save / create / delete / recent-list / overview / peek） | 已服务 |
| 配置、模型路由、提示词、技能（除本地安装类） | 已服务 |
| 文件系统（读写 JSON、读写文件、mkdir、exists） | 已服务 |
| 终稿槽位读、终稿取消、`finalization:commit` / `retry` | 已服务 |
| 草稿单事务提交（`generation:commit-draft`，一次 `BEGIN IMMEDIATE`） | 已服务 |
| 批次写入（`generation:begin-batch`） | 已服务 |
| 图谱 begin（`graph-generation:begin`，plan 类） | 已服务 |
| 导入运行生命周期（`finalize-parsing` / `restart` / `fail` / `effect-receipt-get`） | 已服务 |
| 终稿角色上下文读（`finalized-character:read-context`） | 已服务 |
| 角色状态候选（`list` / `read` / `decide`） | 已服务 |
| 终稿章节删除（`delete-finalized` / `retry-deletion` / `confirm-legacy-knowledge-absent` / `list-incomplete-deletions`） | 已服务 |
| 终稿草稿导入（`db:draft-import-finalized-batch`，一条 `BEGIN IMMEDIATE` 导入整批） | 已服务 |
| 终稿角色状态提交（`finalized-character:commit`） | 已服务 |
| LLM 连接测试与模型发现（`llm:test-connection` / `llm:discover-models`） | 已服务，走与生成执行器同一条出口策略 |
| `db:import-run-author-preview` | **已绑定但按设计拒绝**：参考实现要一个进程内 inspection id，本车道没有那个持有者，所以 id 形式答 `IMPORT_INSPECTION_OWNER_UNAVAILABLE`；直接传 chapters 才是真的预览 |

> 158 里有 1 个是"绑定但告知名牌缺口"，别把它读成 159 个都能用。

### 2.3 支撑性资产（这些让后面的活变可能）

- **v7 schema 由 Go 拥有**：`internal/admission/baseline_v7_ddl.go`（74 个对象），测试用准入自己的编码器重算摘要并对钉死指纹断言。漂了会点名是哪个对象。
- **三个纯算法移植**：`draft-units`、`draft-visible-text`、`visible-continuation`，含 UTF-16 码元窗口、JS `\s` 集合、sha256 规范形式。
- **两份移植互证**：`internal/composition/parity_test.go` 断言组合侧与提交侧算出同一个哈希——任何一边漂了构建就红。
- **服务数断言**：把"进展"从手算变成一个构建会检查的数。

### 2.5 最近三轮：瓶颈已从"接线"变成"缺端口"，而端口正在补上

这三轮的价值不在数字（142→149），在于**证明了正确的做法**：

| 操作 | 之前为什么绑不上 | 补了什么 |
| --- | --- | --- |
| `generation:commit-draft` | 没有单事务提交端口 | `internal/store/draft_commit_tx.go` |
| `graph-generation:begin` | 没有冻结 plan 上下文的端口 | `syntheticFreezeGraphContext` + 读路径的重推导 |
| 4 个导入生命周期操作 | `BeginParsing`/`FinalizeParsing`/`Restart`/`Fail` 不存在 | `internal/store/importrun_lifecycle.go`（1664 行） |
| `finalized-character:read-context` | `readFinalizedCharacterContext` 全仓无实现 | `internal/store/finalized_character_context.go`（603 行） |

**推论**：剩下的 132 个操作里，凡是没有端口的一律先补端口。零绑定批次已经作废两次（只有"永远拒绝"的绑定），不再做。

**两个反复出现的假象，都已付出 CI 轮次**：

1. **CI annotation 在第一个引号处截断**。用 `%q` 格式化错误信息时，日志里的"空错误码"多半是截断，不是真的空。测试里禁用 `%q`。
2. **在空表上 `UPDATE` 是无声空操作**。基线建的 `continuity_projection_meta` 是空表，夹具用 `UPDATE ... WHERE id='main'` 一行没写，而源读取是 `INNER JOIN`，于是每条读都在第一个守卫处返回 `SOURCE_CHANGED`。一个原因伪装成三个不同的失败。

### 2.5 审稿/修订闭环：prepare 已通，其余三个还差写入侧

`review-revision:prepare` 已服务（`e9dc6440` 全绿）。这一条不是"接线"——它需要三样东西同时存在：

1. **冻结生产者**（`internal/store/review_revision_context.go`，1725 行）：把章节、设定、历史、蓝图、目标、预检风险冻结成一段文本，`ContextHash` 与校验器逐字节一致。
2. **进程内手柄注册表**（`internal/reviewrevprep`）：桌面端靠 `webContents.id` 把令牌存在主进程内存里，浏览器没有这个，所以 id 必须能被服务端映射回冻结上下文。id 不可猜、绑定 caller/epoch/operation、有有效期、消费时 CAS。
3. **运行准入真的会冻结它**：`generation:begin` 接受 `reviewRevisionContextId`，从注册表取回上下文，**再对数据库重算一次**——不一致就拒绝，一行都不写。这一条是最容易被跳过、也最容易变成"手柄是死的"的地方。

`commit-review` / `commit-revision` / `read-recovery` 仍缺：`review_cycles` 写入者、`isNoopRevision` + `assertMateriallyCompleteRevision` 两个门槛、以及 effect 戳。账本已经允许 `reviewRevisionEffect`（三处），剩下的是写入侧。

### 2.6 六个 db 维护操作：全部 C 类（能力缺口），一个都没绑

派了 agent 逐条查证，结论是**零绑定**，而且这个结论是对的：这六条的参考实现都要么依赖进程内持有者，
要么依赖文件系统，要么依赖未移植的提案服务。

| 操作 | 缺什么 |
| --- | --- |
| `db:review-cycle-list-pending-rechecks` | M03 收据证明 + recheck 投影（`store/reviewcycle_read_tx.go:9-12`） |
| `db:blueprint-character-sync-complete` | 提案/名册收据证明未移植（`transport/blueprints.go:15-16`） |
| `db:blueprint-clear-all` | 冻结文档读取 + 恢复退役（`blueprint-repository.ts:202-203`） |
| `db:project-core-commit-generated` | 进程内 generation owner 的 effect 断言（`main-generation-owner.ts:1548`） |
| `db:project-clear-generated-data` | 回收站文件系统 + 身份退役（`project-clear-repository.ts:34-83,119-134`） |
| `db:import-global-facts-commit` | 提案服务 + 已有项目探测（`import-global-facts-repository.ts:193-204`） |

**破坏性顺序记在这里，免得下次重新推**：

- `blueprint-clear-all`：`blueprint_character_sync_operations` → `blueprint_commit_operations` → `blueprints`（`:205-207`），且在冻结读取与恢复退役之后。
- `project-clear-generated-data`：先 `finalized_draft_import_operations, post_process_steps, post_process_runs, reviews, revisions, drafts, contents, summary_snapshots`（`:102-109`），再 blueprint 三表（`:114`），最后身份退役 + `UPDATE project_core SET writing_style, reference_works, global_guidance, golden_finger, premise, worldbuilding, synopsis, character_states`（`:135-147`）。注意它**不清** `core_outline` / `world_setting` / `protagonist_profile`，和 `resetCreativeFields` 不是一回事。

### 2.7 部署面：健康探针与切换用 compose

- 镜像里没有 node、没有 curl、没有 shell 工具，所以**健康探针就是服务二进制自己**：`novel-server -healthcheck -listen 127.0.0.1:16082`，探 `/healthz` 并要求 `runtime=="go"`。这个字段不是装饰——被替换的 Node 镜像也答同一个路径，只检查 200 会把过期容器、错标签和挡在前面的代理都当成健康。
- 新增 `docker/compose.go.yaml`：与 `docker/compose.web.yaml` 保持**容器名、发布端口（127.0.0.1:6081）、数据挂载、cap_drop、只读根文件系统、重启策略**完全一致，只改镜像、启动参数和探针。这样切换与回滚都只是换一个 compose 文件，nginx 不用动。
- 不设 `NODE_ENV`、不传 proxy/origins：Go 车道从作者数据目录读出口策略与浏览器来源，留一个陈旧的环境变量只会制造虚假的安全感。
- 两个容器共用同一份 SQLite 与同一把排他写锁，**必须先停旧容器再起新容器**，否则第二个启动的直接拒绝。

### 2.8 已知的"半通"流程：graph-generation

`graph-generation:begin` / `read` / `cancel` 已服务，但 `execute` / `confirm` 未绑定。也就是说剧情树生成能开、不能跑。
这不是漏改常量，而是这条流程的第二段确实没有 Go 实现。我选择**保留 begin**（它在准入阶段干净失败）而不是为了好看去绑定一个只会拒绝的 execute。验收时若点到这个按钮，会看到明确的 `501 GO_OPERATION_NOT_MIGRATED`，不是静默卡死。

## 三、未完成

### 3.1 写作闭环还差的最后一块（最高优先）

| 操作 | 缺什么 |
| --- | --- |
| `generation:commit-draft` | 在**同一个立即事务**里写 `contents` + `drafts` + 账本回执；参考实现把"下一章检查"放在事务**内部**（`generation-draft-effects.ts:175-179`），这是防止两台设备提交同一章的唯一手段。`store.Drafts.Create` 自带事务，需要在事务内变体。 |
| `finalization-generation:begin` | 精简版的准入白名单不认终稿上下文键，账本只在 `finalized-character-state` 下允许该效果键。 |
| `finalization-generation:execute` | 唯一可复用的派发器要求浏览器冻结的 `task` + `invocationNonce`，而契约只发 `{handle, retryOf?, recoverFrom?}`。在这里自己造 task 等于写第二个派发器——被明确禁止。 |
| `finalization-generation:commit` | 依赖上面两个先产生已结算工件。 |
| `graph-generation:begin` / `execute` / `confirm` | 精简版的 begin 不冻结图谱上下文，所以图谱读也只能拒绝；这三项是它的前置。 |
| `generation:retry-draft-short-outline` | **拒绝**：它合成一个浏览器从未冻结的请求并重新派发。 |

### 3.2 导入既有小说

`import-generation:read` / `execute`（2），以及导入运行剩余的账本写：
这 8 个一个都绑不了，原因**不是接线，是缺 store**（`internal/store/importrun.go` 只有 11 个方法，
到 `Complete` 为止就没有了；表是有的，`baseline_v7_ddl.go:328`）。逐个对应：

| 操作 | 缺的东西 |
| --- | --- |
| `-author-preview` | `importInspectionStore.peek`（`import-inspection-store.ts:161`） |
| `-prepare-inspection` | 同一个 peek/consume + `ImportRunRepository.prepare`（`import-run-repository.ts:2005`）与 `beginParsing`（`:1570`） |
| `-finalize-parsing` | `finalizeParsing`（`:1819`） |
| `-restart` | `restart`（`:2575`，它会插入下一条 run） |
| `-fail` | `fail`（`:2721`）加本地化失败原因（`import-run-failure.ts`） |
| `-effect-receipt-get` | `getEffectReceipt`（`:2330`，是读，不能写） |
| `-effect-receipt-prepare` | `prepareEffectReceipt`（`:2345`，插入 `import_run_receipts`） |
| `-effect-receipt-commit` | `commitEffectReceipt`（`:2410`，牵动另外四个 store 族） |

所以这条路的下一步不是写适配器，是**先把这 5~6 个 store 方法补出来**，再谈绑定。
参考实现的拒绝码已经查到（`IMPORT_RUN_PARSING_CLOSED`、`IMPORT_RUN_NOT_RESTARTABLE`、
`IMPORT_RUN_RECEIPT_CONFLICT` 等 11 个），补 store 时照着用。

### 3.2b 一个必须说清楚的结论：瓶颈已经从"接线"变成"缺 store"

最近三个族（导入运行残余 8 个、角色提案 6 个、头像 4 个）派下去，绑成 **0 个**。
原因不是接线没做完，是**底层方法根本不存在**：

| 族 | 缺的东西 |
| --- | --- |
| 角色提案 6 | `newProductionGeneration` 没有提案面；`proveCharacterProposal`（`generation-character-proposal-proof.ts:43`）没有 Go 移植。**批准的那半写操作其实已经有了**（`internal/store/character_identity.go:161`）——正因如此，绑上去只会变成"能写不能证"，比拒绝更糟 |
| 头像 4 | `CharacterAssetRepository` 六个方法 + `projectfs` 的**写入根**（`projectfs` 只发布 `OpenFileRead`，`browser_files.go:73-90` 还有意排除 `.ai-novel`） |
| 导入运行 8 | 见 3.2 的表 |

所以接下来的顺序要改：**先补 store / 移植证明逻辑，再谈绑定**。
把适配器写在一个不存在的方法上，产物只能是"永远拒绝的绑定"，那等于零。

### 3.3 角色与设定

`character-proposal`（6）、`finalized-character`（5）、`legacy-roster`（7）、`character-avatar`（4）。
头像那 4 个是桌面端图像处理，浏览器版可能整类重设计，不是简单绑定。

### 3.4 编辑器内联与审稿恢复

`editor-inline`（4）、`review-revision`（4）。

### 3.5 桌面专属 → 需改产品形态

这 9 个不是一类，浏览器能做的比我说得多。浏览器有文件选择框（`<input type="file">`），
区别只在它交出来的是什么：**文件内容**，不是宿主上的绝对路径。

**A. 选"我电脑上的文件"导入（5 个：novel-files / knowledge-files / knowledge-folder /
project-archive / legacy-project）** — 浏览器完全能做，而且手机上更方便。但形状变了：
不再是一次 IPC 往返拿到路径，而是**上传字节**，由服务器落盘。这 5 个操作在浏览器版里
根本不需要存在，它们被"上传"这条路取代。

**B. 选"服务器上的目录"写出（4 个：select-folder / select-export-directory /
select-project-archive-export / select-project-restore-target）** — 这个浏览器确实做不到：
网页不能指定服务器路径。两条路：
- 服务端目录浏览（列目录 API + 校验根目录），
- 或者干脆不选：导出直接作为浏览器下载，归档固定落在服务端数据目录。

`skills:pick-local` / `skills:install-local` 属于 A：改为上传 zip。

### 3.6 MCP 与归档

`mcp`（8）：binder 存在，但没有管理器与凭据登记处。
`archive-export` / `archive-restore`：需要先定"归档落在服务器哪里"。

### 3.7 按决定跳过

- 知识库（19）：需要向量能力（Rust 静态库 + `-tags lance_native`），当前 FTS-only 是静默降级，不绑。
- 云备份（8）：需要凭据服务与七项前置。

### 3.8 有意的拒绝（保持 501）

| 操作 | 理由 |
| --- | --- |
| `db:blueprint-character-sync-get` / `-list-pending` | store 先 INSERT 再 SELECT，绑了就是"200 加一次写" |
| `db:blueprint-update-notes` | 无条件闸门，且 `store.Blueprints` 没有 `UpdateNotes` |
| `db:plot-tree-save` | 会重绑 `db:plot-tree-read`，且图生成准入未满足 |
| `fs:list-dir` | 泄露宿主路径 |
| `project:recent-remove` | 没有导航目录可改 |
| `generation:retry-draft-short-outline` | 见 3.1 |

## 四、之后的顺序

1. **`commit-draft`** — 没有它，生成出来的正文没有地方落盘，写作闭环是断的。
2. **终稿链**（begin / execute / commit）— 需要先决定终稿执行器是否复用现有派发路径；若不能，就明确保持 501 并把选择告诉前端。
3. **图谱链**（begin / execute / confirm）— 前置是"冻结图谱上下文"的 begin。
4. **导入链**（import-generation + 导入运行余额）— 让旧稿能进来。
5. **角色与设定**（proposal / finalized / legacy-roster）。
6. **编辑器内联 + 审稿恢复**。
7. **MCP、归档**。
8. **桌面专属的改造**（对话框 → 上传），这是一次产品决定，不是接线。

## 五、交付前还必须做的（不属于操作数）

- 全量验收：281 个操作逐个过一遍浏览器路径，不只是 200。
- 真实数据演练：用作者数据的**副本**验证打开、写入、终稿，备份先行。
- 镜像：按摘要拉取，不按 tag。
- 切换：compose 文件与 `--env-file` 就位，回滚摘要 `sha256:84801a8a…` 可达。
- 切换后观察：单写者锁、租约、错误码是否如测试所述。

## 六、这份文件怎么保持真实

`productionServedCount` 变了而这里没变，就是过期。改操作数必须同时改两处：
`production_served_count_linux_test.go` 的常量，和本文第一章的表。
