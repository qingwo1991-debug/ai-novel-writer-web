# 浏览器版 Go 后端：收尾计划（第二版）

> 第一版（`GO-REMAINING-PLAN.md`）按"281 个全齐"排期。这一版按**真实产品形态**重排：
> **后端 + 浏览器，没有桌面端**。凡是"只能在桌面上做"的操作，不是待办，是不存在。

数字口径不变：已服务数量是构建断言（`production_served_count_linux_test.go`），
当前 **239 / 281**，`ee88acbb` 全绿。

### 本文件三个阶段的实测结果（回填）

| 阶段 | 状态 | 证据 |
| --- | --- | --- |
| P0 定稿解锁（`kb:import-text`、`kb:import-finalized-text`） | **已完成** | 两个名字在 235 里，定稿链路的名字全部已服务 |
| P1 40 章导入 | **已用真实容器跑通** | `scripts/go-cutover-rehearsal.py --import-chapters 40`：40 章合成正文一次事务落库，40 章全部 `finalized`，第 1 / 40 章按字节读回，`db:draft-get-max-finalized-chapter` = 40 |
| P2 备份与回滚演练 | **已用真实容器跑通** | `--restore`：导出 904 KB 归档 → 两个 picker grant → `project:archive-restore` → 新项目打开后第 1 章正文与词数与原件完全一致 |
| P2 compose 切换 | **未执行，等作者决定** | 生产容器仍在跑旧镜像；回滚 digest 已记录 |

P1 的一个硬事实（写进脚本注释，避免下次再踩）：`db:draft-import-finalized-batch`
要求每章带上**渲染进程算出的可见字数**（`internal/draftalg/draftunits.go:222`），
与章节正文不符就整批拒绝（`internal/store/finalized_draft_import.go:358`）。
不是服务端算，是客户端算完随请求带上——脚本因此按同一算法算了一遍。

## 零、为什么重排

上一版把 `dialog:*`（文件选择框）、`fs:grant-*`（桌面文件授权）当缺口排进计划。
在浏览器里这些**没有对应物**：选文件是 `<input type=file>` + 上传接口，导出是下载接口，
两者都已经实现（`POST /api/uploads`、`GET /api/downloads/<path>`）。

所以真正要问的是：**点下按钮时，浏览器会用到哪些通道？** 我按这个口径重算了所有未服务操作，
结论是：**能让"写完整一章"跑通的缺口只有 2 个**。

## 一、当前唯一的硬阻塞

定稿命令（`src/services/workflows/commands/finalize-chapter.command.ts`）实际调用的通道：

| 通道 | 状态 |
| --- | --- |
| `db:continuity-read-source`、`db:continuity-save-finalized` | 已服务 |
| `db:blueprint-get`、`db:blueprint-update-notes` | 已服务 |
| `finalization-generation:read` | 已服务 |
| `finalized-character:read-context` | 已服务 |
| `db:finalization-link-knowledge-document` | 已服务 |
| `kb:import-text`、`kb:import-finalized-text` | **未服务** |

后两个是硬依赖：`requireIpcSuccess(result, '导入知识库')` 失败即抛，整个定稿步骤失败。

**P0 = 这两个。**

## 二、阶段划分

### P0 · 定稿解锁（2 个操作）

- `kb:import-text`
- `kb:import-finalized-text`

设计决定（写进代码注释，不许含糊）：

1. **不碰 LanceDB 向量库。** 桌面端用 LanceDB（`{projectPath}/.vela/lancedb/`），Go 侧不实现。
   只写"规范分块副本"（`knowledge-copy:` 文档），这是 `kb:search` 已经读取的那份数据，
   所以写完立刻可搜。
2. **向量标记为待补**，而不是假装成功。日后 `kb:backfill-vectors` 上线再补。
3. 分块算法必须与桌面端一致到字节，否则同一章的检索结果会与旧客户端不同。

### P1 · 导入 40 章（作者本人要用的那条路）

- 上传 → 分块 → 批量落库 → 定稿态导入，**单批 40 章**，失败整批回滚、可重试。
- 不需要逐文件分批：作者的稿子是 40 多章，不是 8G。
- 相关未服务操作：`import-generation:read`、`import-generation:execute`（2 个）。
  若浏览器导入路径只需要已有的 `db:draft-import-finalized-batch`，这两个可以不进 P1。

### P1 的实测：导入流程比预想重，走"成品导入"而不是"完整导入工作流"

按通道逐个查证（不是估）：

| 文件 | 用到的操作 | 未服务 |
| --- | --- | --- |
| `import-novel.command.ts` | 5 | `db:import-global-facts-commit`、`kb:search-writing-context` |
| `import-workflow.ts` | 17 | `kb:import-reference-text` |

其中 `db:import-global-facts-commit` 是**硬依赖**（`if (!commitResult.success) throw`），而它需要
`CharacterProposalService` + `probeExistingProject` + `planImportFill` 三样都不存在的端口。

**决定**：作者手里的 40 多章是纯文本正文章节，不需要"参考语料 + 全局事实推断"那套完整导入工作流。
P1 走**成品导入**：浏览器选文件 → `POST /api/uploads` → 已有的
`db:draft-import-finalized-batch`（已服务）落库。这条路已经具备全部后端能力，
不需要等 `import-global-facts-commit`。

完整导入工作流（含参考语料、全局事实）留在 P4，因为它是长尾：作者本人不会用，
而它需要的三个端口一个都不存在。

### P0.5 · 真正的生成缺口：车道里没有安装 provider

`internal/generation/provider.go:25` 的 `NativeProvider` 注释写得很明确：
**"This function is not installed into any public handler or the main binary."**
也就是说 `generation:execute` 走到派发时没有真实模型通道，浏览器里点"生成本章"到不了模型。

所以"写一章"的闭环缺的不是知识库（那个已完成），而是 **LLM 通道**：

| 操作 | 用途 | 状态 |
| --- | --- | --- |
| `llm:generate` | 非流式补全，技能/架构/审稿都在用 | 未服务 |
| `llm:generate-stream` | 章节正文的流式输出 | 未服务 |
| `llm:cancel` | 停止生成 | 未服务 |
| `llm:begin-execution-lease` / `llm:close-execution-lease` | 一次生成期间的凭据租约 | 未服务 |

这五个必须先做，否则前面所有绑定都只是"能给数据库写字"，而不是"能写小说"。
凭据只在租约内、只在 egress 允许的来源上、只出现在服务端。

### P2 · 切到浏览器验收

- `docker/compose.go.yaml` 已就位（容器名/端口/挂载/权限与旧文件一致，切换=换文件）。
- 顺序：停旧容器 → 起新容器 → 健康探针 → 浏览器真写一章 → 备份导出演练 → 回滚演练一次。
- 门槛：**打开项目 → 生成 → 采纳 → 终稿 → 导出** 全绿。

### P3 · 写作时会点到的功能（按使用频率）

| 优先 | 族 | 未服务 | 说明 |
| --- | --- | --- | --- |
| 1 | `review-revision` | 3 | 审稿/修订闭环；`prepare` 已通，差写入侧 |
| 2 | `character-proposal` | 6 | 定稿后的角色状态提案面板 |
| 3 | `editor-inline` | 4 | 行内 AI 改写 |
| 4 | `kb` 其余 | 12 | 搜索重建、删除、向量补建 |
| 5 | `graph-generation` | 2 | 剧情树 execute/confirm（现在半通） |
| 6 | `llm` 流式 | 5 | 流式生成、租约、取消（"停止生成"按钮） |

### P4 · 长尾（缺了不影响写作，点了给 501）

| 族 | 未服务 |
| --- | --- |
| `cloud-backup` | 8 |
| `agent-generation` | 8 |
| `legacy-roster` | 7 |
| `db` 维护 | 6 |
| `character-avatar` | 4 |
| `skills`（GitHub 安装） | 2 |
| `generation` 恢复 / `import-generation` | 4 |
| `project`（存档恢复、legacy 复制） | 3 |
| `fs:write-json` 等 | 1 |

### P5 · 把桌面操作"翻译"成浏览器操作（13 个，不是不做）

**修正**：这 13 个不是"不适用"，而是**必须换一种形态继续存在**。渲染进程在 web 模式下照样会调用它们
（`src/services/ipc-client.ts:40-45` 把 `fs:grant-*`、`dialog:select-*`、`skills:*` 单独归为
"能力通道"），所以不绑定就是按钮点下去报 501。分母仍然是 281。

浏览器里没有原生对话框，但**语义可以保留**：服务端给一个受控的"选择"结果，前端用文件选择器或
目录树来产生它。逐个定死语义，不许含糊：

| 操作 | 浏览器语义 |
| --- | --- |
| `dialog:select-knowledge-files` / `select-knowledge-folder` | 前端 `<input type=file multiple>`（或目录上传）→ `POST /api/uploads` → 返回上传后签发的授权标识 |
| `dialog:select-legacy-project` | 服务端在**书库根目录内**列目录，前端渲染目录树，选择后返回书库相对路径（越界拒绝） |
| `dialog:select-project-archive` / `select-project-restore-target` / `select-project-archive-export` / `select-export-directory` / `select-folder` | 同上：服务端受限目录选择器，返回书库相对路径；导出目标限定在允许的导出根 |
| `fs:grant-read-file` / `fs:grant-write-file` / `fs:grant-mkdir` | 服务端对**已校验的库内路径**签发能力令牌（不是任意绝对路径），后续读写凭令牌进行，令牌绑定 caller 与有效期 |
| `skills:pick-local` / `install-local` | skill 包以 zip 上传到服务端 → 从上传内容安装，安装前做与 GitHub 安装相同的校验 |

这些的前提是 P5.0：**一个受限的目录/授权层**（列目录、校验根、签发令牌、吊销）。先做它，
其余 13 个才有共同基础；否则每个操作各写一套路径校验，边界就会不一致。

**P5.0 已完成**（`internal/librarypath` + `production_fs_grants_linux.go`，`525e3ddb` 绿）：
令牌是 32 字节随机，记录带独立 MAC，绑定 caller 与租约 epoch，10 分钟上限，可吊销；
请求只能给"符号根 + 相对路径"，任何符号链接（哪怕指回根内）都算越界；
写与 mkdir 直接拒绝作者数据。3 个 grant 已服务，剩下 8 个 `dialog:*` 与 2 个 `skills:*` 复用这一层。

**状态**：168 / 281；写一章的闭环（开项目 → 生成 → 采纳 → 定稿 → 导出）所需通道只剩前端选择器，
后端已无缺口。

## 三、执行纪律（不因为要快就松）

这些是前几轮用真实故障换来的，继续有效：

1. **绑定 ≠ 跑通**：每个操作必须有 `mode=ro` 独立连接的回读证据；失败的写入必须让
   `db`、`-wal`、`-journal` 三个文件字节不变。
2. **零绑定批次作废**：只能拒绝的绑定比不绑定更糟。宁可交"这 6 个都缺能力"的报告。
3. **一次映射**：store 拒绝原样返回，边界只映射一次。重复映射会把所有拒绝压成同一个码。
4. **失败信息不用 `%q`**：CI 注解在第一个引号处截断。
5. **单例表用 upsert + 回读**：`UPDATE` 空表和 `INSERT OR IGNORE` 都是静默的。
6. **共享文件归父进程**：agent 只建新文件并报告绑定行，不碰 `production_open_linux.go`、
   `synthetic_*.go`、计数常量。
7. **编译与测试只在 GitHub Actions 上跑**（本地无工具链）。

## 四、进度看板

| 阶段 | 状态 | 证据 |
| --- | --- | --- |
| P0 kb 导入 | 进行中 | — |
| P1 导入 40 章 | 待开始 | — |
| P2 切换验收 | 待开始 | `docker/compose.go.yaml` 已写 |
| P3 写作功能 | 进行中 | `review-revision:prepare` 已服务 |
| P4 长尾 | 未开始 | — |
| P5 桌面操作浏览器化 | 🔄 3/13 已服务 | grant 3 个已服务，8 个 dialog + 2 个 skills 待做 |

每完成一批：CI 全绿 → 更新本表 → 更新 `GO-REMAINING-PLAN.md` 的计数。
