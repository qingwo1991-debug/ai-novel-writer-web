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

## 并发基础层第二轮

源码 `0350a13cff8eb86bab89dd5af1529d3423d7dea8`，[Actions 41374819804](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41374819804) **success**：52 顶层 / 115 含子用例、0 fail，vet、race、build、无 Node smoke 均通过。

新增回归覆盖：读取慢请求 body 前即固定标签身份并占用并发配额；SSE 每连接待写/在写 payload 有 8 MiB 字节预算且完成后释放；同标签并发打开成功采纳租约后，不被另一路失败回滚删除。编译/测试仍全部在 GitHub；本机仅下载小型 JSON/格式化证据。

## 数据准入第三轮

源码 `f24e145ea94f44f56dc5a477b6a8c4e7cc11a9d4`（数据实现 `b3d956fc`），[Actions 41376594444](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41376594444) **success**，job 2m24s：**81 顶层 / 237 含子用例，0 fail、0 skip，8 个测试 package**。vet、race、build、无 Node smoke 全部通过。

新增通过：`internal/projectfs`、`internal/admission`、`internal/projectdata`。原 TypeScript M00–M06 在 GitHub 生成两种确切 v7 schema 和一个 v6 合成库，Go 逐一核对；`TestTypeScriptRegistryContract` 与包含未 checkpoint WAL-only 行的 `TestInspectPhysicalWALSetFromOriginalSchema` 实际执行并通过，没有依靠 skip 绕过。原合成 DB/WAL/SHM 字节未变、私有暂存清理通过。详细边界见 [GO-DATA-ADMISSION.md](GO-DATA-ADMISSION.md)。

首个提交的 CI 因 workflow job-level `env` 引用了该位置不可用的 `runner` context，没有启动 runner，也未执行测试。随后改为步骤内使用 `RUNNER_TEMP` 并写入 `GITHUB_ENV`，上面记录的是修正后的实际成功结果，不隐瞒首轮失败。

## 流式快照、凭据与内容证明第四轮

源码 `e7457541ad637051e5252a15081ea2e06fa16112`（实现 `414cf30e`），[Actions 41378784128](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41378784128) **success**，job 3m17s：**113 顶层 / 345 含子用例，0 fail、0 skip，9 个测试 package**。vet、race、Go binary build、无 Node smoke 均通过。

新增实际通过：

- 128 KiB buffer 的 DB/WAL 全集流式快照；65 MiB 合成 sparse file 超过旧 ceiling 的复制、hash、proof 校验；预算、取消、漂移、冲突目标和只清理自建文件回归。
- `TestTypeScriptAuthorContentHash`：两种原 v7 schema 及特殊 SQLite 存储类型 fixture 与原 Node `authorContentHash` 对照一致。包含大整数、REAL 数值格式、BLOB、NULL、中文/emoji、UTF-16 排序及 HTML/U2028/U2029 转义。排序仍有明确 64 MiB 编码行预算，不代表任意大库行级证明完成。
- `TestNodeGoSecretContract`：原 TypeScript 加密可被 Go 解密，Go 加密又由原 TypeScript 解密；空串、Unicode/NUL、64 KiB 边界，只有公开合成 key。私人 key 权限、损坏不覆盖、链接、并发创建与 envelope 篡改回归通过。
- 两个固定导航意图已绑定：`official-homepage:open`、`model-provider-resource:open`；authenticated RPC → caller-private SSE，固定地址/ID、非法 URL 拒绝、跨标签隔离与 CSRF 回归通过。

首轮测试发现 preservation 测试错误地调用要求 single-link 的 SOURCE validator 检查故意构造的目录/hardlink，导致 8 个子用例失败。修正为比较目标身份/元数据而不要求安全文件类型；未放宽实际 StageDatabase 拒绝规则。以上是修正后的实际通过记录。所有执行继续仅在 GitHub，本机未编译/测试。

## 私有项目会话与 M06 第五轮

源码 `3a2bc9669d62b4c44ef9e8ec99dc47c6e7f6aa40`（实现 `aba22a03`），[Actions 41381280660](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41381280660) **success**，job 2m47s：**134 顶层 / 392 含子用例，0 fail、0 skip，10 个测试 package**。vet、race、build、双向 credential oracle 与无 Node smoke 全通过。

- `TestBrowserWireDraftCASOnAdmittedScratchSession`：原 TypeScript schema 合成库 → projectfs 流式快照/schema 准入 → 真实 cookie/CSRF RPC → 冻结 context/租约 → SQLite 正文 CAS。两个相同 baseline 并发写入恰好 1 成功/1 冲突；源项目字节不变。
- 四个草稿读与正文 CAS handler 保持当前前端原 typed args + trailing context wire；缺 baseline 不补读、不重试；跨标签/会话、旧 lease、只读状态与错误脱敏通过。
- scratch refresh 保留编辑；Release/Close 被在途 hold 围栏；同 UUID 物理目录替换被拒绝；清理失败返回错误并保留记录，后续 Cleanup/Close 能重试。
- `TestUpgradeV6MatchesOriginalNodeM06BothDonors`：相同 v6 源字节，原 Node 与 Go 分别完成**空审稿周期限定 lane**，baseline/donor schema、counts 和完整逻辑 proof 一致。最终指纹失败回滚 DDL/版本；非空周期、坏域、FK、unsafe file/sidecar 拒绝。

首轮 oracle 误用分别初始化的 v6/v7 fixture 做全文 proof 比较，metadata defaults 不同导致 hash 不同；改为从同一关闭 v6 字节独占复制给原 Node 升级，没有删除内容验证。另 missing-file 测试 parent 未设0700，被更早的 private-path 门拒绝；修正 fixture 权限后断言实际缺文件错误。源码审查发现的 refresh 物理身份与 cleanup 重试两项也已修复并补测。

五项草稿操作还**没有绑定正式 binary**：scratch 写入不是持久作者保存，`project:open` 与 LanceDB/global/发布恢复未完成。矩阵只标 IN PROGRESS，不计入 2 项已完成业务。详见 [GO-PROJECT-SESSIONS.md](GO-PROJECT-SESSIONS.md)。

## 外部内容证明与早期升级第六轮

源码 `30373e5152608708e39de6bee047e420fc2022cd`，[Actions 41383135731](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41383135731) **success**，job 3m34s：**151 顶层 / 465 含子用例、0 fail、0 skip、10 个测试 package**。vet、race、build、credential oracle 与无 Node smoke 通过；本轮首个提交即通过所有CI门。

- `TestTypeScriptDomainProofBeyondOld64MiBCeiling`：280 行合成大文本，总 encoded rows >64MiB，Go私有外排 proof 与独立 Node 原格式 hash 一致。多层8路归并、重复行、UTF16、取消/IO/visitor失败、部分及完整记录边界截断拒绝/权限/清理回归通过。新 SQLite upper-bound preflight 在 Go driver fetch 前拒绝过大行，避免先分配巨大正文才检查。不是4MiB进程RSS承诺，资源/清理边界见 [GO-CONTENT-PROOF.md](GO-CONTENT-PROOF.md)。
- `TestEarlyUpgradeMatchesOriginalTypeScriptM04M05BothDonors`：v4→v5/v5→v6 两种 source family 的原实现对照通过；坏域/指纹/FK、非空审稿/导入历史、unsafefile/sidecar、回滚均覆盖。只支持文档所列空历史窄 lane；M05仅schema不处理avatar文件。
- 新四项草稿查询保持原 metadata/scalar形状和version-only顺序；原例的NULL/zero/negative/default/stale依赖行为被覆盖。真实 authenticated HTTP 私有副本集成也执行了四项查询和跨tab拒绝。

当前 **2 项完成、9 项进行中、270 项未开始**。新增查询仍未绑定 binary；无持久作品业务完成或作者保存可用声明。

## 原生AI传输与跨进程写者第七轮

源码 `73244fa788dcbe878c2185bf9a8c047a1cd524bf`，[Actions 41385531191](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41385531191) **success**，job2m59s：**183 顶层PASS / 1512 含子用例PASS、0 fail、1 skipped子用例、13个测试package通过**。另cmd/novel-server没有unit test的package级skip不计为测试子用例。不能把本轮说成零跳过：`writerlock/TestRejectUnsafeLockEntriesWithoutMutation/foreign-owner` 需要chown，runner非root而明确跳过；其他ownership guard及进程竞争测试通过，foreign-owner实际文件场景仍待有权限的隔离runner补验。

- 原TypeScript OpenAI provider **72 case（46 stream/26 generate）**，原visible/reasoning scanner **922 case** 对照通过。原请求、ordered callbacks、partial content/usage、显式DONE与semantic finish区分、CR/LF/UTF8与think拆分保密均覆盖。mapping只是明确synthetic stub，不声称真实preset/parameterpolicy移植完成。
- 原生受控egress的精确origin、禁止所有redirect/closebody、无环境proxy、TLS默认验证、取消、request snapshot与symbolic error验证通过。允许管理员显式LAN/loopback；DNS和hostname保守语法限制见专文。
- nativeHTTP provider集成、最终read时取消不返回完成、response+error的body关闭、64MiB aggregate heartbeat预算通过；legacy输出改strings.Builder，避免tinychunks二次复制。
- Linux openat/NOFOLLOW+flock：sameprocess/多个self-Go-child恰一赢家、退出/kill释放、CLOEXEC、entry/ancestor替换和常见unsafe文件拒绝通过。不是现production/global writer集成；旧Node/desktop必须持续排除，不能假定新lockname互斥旧进程。
- vet/race/build、credential双向oracle和无Node smoke全部通过。源码bridge guard仍检查cmd/internal全部production `.go`；只排除 `_test.go` 允许Go自执行child锁测试，不放宽实际runtime为Node代理。

无RPC新增绑定，矩阵仍 **2完成、9进行中、270未开始**。本轮没有触碰作者数据或部署镜像；Gemini/AI工作流/LanceDB/持久项目/全局协调及外部真实服务验收仍未完成。详见 [GO-AI-TRANSPORT.md](GO-AI-TRANSPORT.md)、[GO-WRITER-LOCK.md](GO-WRITER-LOCK.md)。

## Gemini与主台账业务第八轮

源码 `6d5f2f7190b6240128f04cd5ed8dca9cc25b99ec`，[Actions 41388270772](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41388270772) **success**，job4m26s：**207 顶层PASS /1764含子用例PASS、0 fail、1 skipped子用例、13测试package通过**。该skip仍为非root runner不能chown的writerlock foreign-owner，未被隐藏；vet/race/build/credentials双向oracle/无Node smoke均通过。

- **154 Gemini原provider differential cases**（实际计数，不使用最初估算170）：请求、visible/legacy精确LF framing、rawusage/规范化/thoughts、finish、EOF、空与非法payload、HTTPcontext、null/omitted选项。Go仅将原JSON.parse engine-dependent SyntaxError安全固定为`Error: LLM_STREAM_INVALID`，测试对此prefix做显式局部转换，其余字段原样比较；该差异不伪装为完全字节一致。原OpenAI72/scanner922继续通过。
- **30 项原ProjectCoreRepository differential cases**：同原schema字节复制、完整返回及原始行变化、allowlist/readonly/threshold、11个源字段CASdrift。Go另覆盖独立连接池并发CAS和SQLabort原子性。
- 新authenticated HTTP私有副本test从原schema开始，通过冻结context/lease读取与更新主台账，两次相同source/target baseline提交恰1成功1冲突；cross-tab、stale、readonly、generated/recovery presence拒绝及inflight release阻止均通过。NULL/negative/REAL baseline和Node Buffer wire的额外transport回归通过。
- 初run41387677962失败在oracle driver的lazy native binding被过窄loader拦截，Go未执行；先在hook前初始化memory-only driver解决。run41387998418仅三项Gemini回调对照因显式空content和省略字段比较错误而失败，修正test adapter保留visible parser error空串后通过；未把实际provider错误删掉或放宽全局比较。

矩阵 **2完成、12进行中、267未开始**；三个core操作仅隔离业务实现（synopsis generated/recovery仍缺），无main绑定、持久作者保存或AI工作流完成声明。见 [GO-PROJECT-CORE.md](GO-PROJECT-CORE.md)、[GO-AI-TRANSPORT.md](GO-AI-TRANSPORT.md)。

## 不代表什么

- 281 项中只有 2 项固定导航意图实现并绑定，279 项仍未迁移，详见 [GO-MIGRATION-MATRIX.md](GO-MIGRATION-MATRIX.md)。安全/租约/传输与草稿仓储模块通过，不等于用户业务端到端可用。
- 草稿正文 CAS/读取以及只读 v7 schema/物理路径核验已有 Go 源码和远端合成测试，但项目打开、旧库升级、完整作者内容证明与 HTTP 领域绑定尚未完成，不能对原作者数据库启用。
- AI 生成/审稿修稿定稿、后处理、LanceDB、归档、WebDAV、MCP 等仍待迁移。原 Node 通过的验收不能转成 Go 通过。
- 构建物明确标注非生产 scaffold；没有发布 Go 镜像或部署。正式容器保持原固定 Node 镜像；旧 Node 自动发布已改为仅手动 dispatch。

下一阶段继续完善数据准入的剩余兼容门并接入作品与草稿纵向业务；所有新增编译与测试继续交由 GitHub Actions。
