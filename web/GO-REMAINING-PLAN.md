# Go 后端：已完成 / 未完成 与后续计划

> 这份文件的数字来自构建断言（`production_served_count_linux_test.go`），不是手数。
> 谁加了一个操作却忘了改常量，CI 会红并把完整已服务列表打出来。

## 一、当前状态

| 项 | 值 |
| --- | --- |
| 清单里的操作总数 | 281 |
| 生产车道已服务 | **142**（`54d58ba6` 起） |
| 仍未服务 | 139 |
| 承载方式 | 单个 Go 进程 + 单个容器，无 Node 运行时 |
| 生产容器 | **未动**，仍跑旧镜像，回滚摘要已记录 |

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

### 2.3 支撑性资产（这些让后面的活变可能）

- **v7 schema 由 Go 拥有**：`internal/admission/baseline_v7_ddl.go`（74 个对象），测试用准入自己的编码器重算摘要并对钉死指纹断言。漂了会点名是哪个对象。
- **三个纯算法移植**：`draft-units`、`draft-visible-text`、`visible-continuation`，含 UTF-16 码元窗口、JS `\s` 集合、sha256 规范形式。
- **两份移植互证**：`internal/composition/parity_test.go` 断言组合侧与提交侧算出同一个哈希——任何一边漂了构建就红。
- **服务数断言**：把"进展"从手算变成一个构建会检查的数。

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
