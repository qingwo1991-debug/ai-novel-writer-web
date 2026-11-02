# Go 基础层：GitHub Actions 验证记录

## 第二十三轮：五agent角色卡/名册/公开身份读取

最终资格源码`058f98059c943f688bb77bf267dcc0d19bd66044`，[41464730633](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41464730633) **success：495顶层/4576含子用例PASS、0fail、3既有foreign-owner测试skip、18packages**（另cmd/novel-server无testfiles package skip）。本轮新增26顶层/179含子用例全通过，无fail/skip；140实际原版case=25getAll+29Roster+9内部rawIdentity+12公开IdentityView+65CurrentDerivedFields，另2独立Go-only缺meta边界，共142契约子用例。31实际递归原模块，原完整SQL后状态、原有时间字符串与输入DB bytes不变均比较。vet/race/原凭据decrypt/build/noNode smoke通过。

四个实施agent分别负责base/derived、M02名册有序哈希、HTTP及Oracle，第五只读审查agent核对安全和来源，父整合实际OpenScratchCopy→三公开HTTP→generation/epoch/fullbody失效→commit拒绝→foreigncookie/tab/CSRF→hold/revoke→原合成库bytes不变。公开identity实际采用proposal-service DTO、args仅[context]，不是内部含proposals的SQL快照；名册/derived一次同连接snapshot；ID/name hash及provenance嵌套顺序、UTF16/locale历史投影忠实原版。审查提前修复非法UTF8 Buffer转String替换分组、MAİN/MİNOR Go/JS lowercase差异；不支持的输入域显式拒绝，未做泛化RSS保证。只有三row→IN PROGRESS，**2/69/210**；roster commit拒绝未迁移author/ID owner不算完成。main/production不变，native Lance未变不重验。

首次`c383420b`，[41463551803](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41463551803)140实际差分+2边界全过，但整体失败于3合成测试断言：父bootstrap空args误编码null，及rosterFixture未插入baseline中不存在的project_core.main导致两投影UPDATE no-op。只修复夹具后第二次全资格通过，失败run不计整体通过。见[GO-CHARACTER-READS.md](GO-CHARACTER-READS.md)和[evidence/go-character-reads-round23.json](evidence/go-character-reads-round23.json)。

## 第二十二轮：多agent摘要快照与定稿连续性读取

最终资格源码`02c04cb6fa2e4202cf8f80eeb95aea6a2699bbb5`，[41457578715](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41457578715) **success：469顶层/4397含子用例PASS、0fail、3foreign-owner测试skip、18packages**（另cmd/novel-server无testfiles package skip）。本轮新增19顶层/160含子用例全通过、0fail/skip，含实际SummaryRepository141个差分case；另有一个明确分开的Go-only RAISE(FAIL)事务强保证fixture，不冒充原非事务行为。vet/race/原凭据decrypt/build/PATH无Node smoke通过。

三个agent分别实现仓储、认证HTTP和真实递归原TS Oracle，父代理整合实际OpenScratchCopy→四业务HTTP→generation失效→receipt破坏拒绝→hold/revoke→原fixturebytes不变，第四审查agent交叉核对source/frozen identity/JSON数字。读路径完整保留原正文、outbox/hash、latest summary/version/id、legacy/current/stale分支及冻结角色身份；legacy snapshot写入不创建formal source。两个formal continuity写仍拒绝，不升级矩阵。native Lance无改动不重验，main不绑定，生产不动。只有四row→IN PROGRESS，**2/66/213**。

首次`74b9b96d`，[41454246646](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41454246646)失败于测试文件unused import；第二次`ad88fd5d`，[41455201247](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41455201247)原差分141/141及业务测试通过，但一个HTTP error message断言误把通用脱敏消息预期为code；修复测试期待、保留统一脱敏后第三次全过。两个失败run不计整体通过。JSON有限数按JS binary64舍入（真实bigint/underflow原case），非finite overflow/合法JSON孤立surrogate/超64层/非法UTF8或单字段超32MiB显式unsupported，不静默改写作者元数据；不是全表大小或RSS限额。见[GO-SUMMARY-CONTINUITY.md](GO-SUMMARY-CONTINUITY.md)和[evidence/go-summary-continuity-round22.json](evidence/go-summary-continuity-round22.json)。

## 第二十一轮：恢复候选正文与来源身份保护

最终资格源码`8d890fb6d1995a1f2810e932abce9fc7fc91b605`，[41449598057](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41449598057) **success：450顶层/4237含子用例PASS、0fail、3foreign-owner测试skip、18packages**（另cmd/novel-server无testfiles package skip）。恢复候选新增15顶层/70含子用例PASS、0fail/skip。原仓储实际55case差分、原始候选全字段后状态、Unicode有序sourceHash/contentHash、legacy sourceDraft captured bit、最新draft id/version身份检查、正文过滤/编辑、replacement边界与continued/discarded原版不对称语义。实际OpenScratchCopy→认证四项HTTP→蓝图变更拒绝edit/continue→discard→revoke→作者源fixture bytes不变通过。vet/race/原凭据decrypt/build/PATH无Node smoke均通过；精确测试计数见下面本轮证据文件。

首轮`7e880e03`，[41448240304](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41448240304)成功，450top/4237all PASS、0fail、3foreign-owner testskip、18packages。随后只读审查补强原始上下文恰好3fields（额外key不得被共享parser静默丢弃）和typed-nil HTTPError防panic/有限脱敏；中间`81166ff5` [41449262813](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41449262813)被后续push取消，不计通过；两项新增回归包含最终完整资格，没有失败测试被当成通过。`sourceCurrent`忠实原语义仅draft id/version，不等价正文CAS；continued只处置metadata，不授予AI继续或合并权威。四row仅→IN PROGRESS，**2/62/217**，main/production不变，不重验未变native Lance。详见[GO-RECOVERY-CANDIDATES.md](GO-RECOVERY-CANDIDATES.md)和[evidence/go-recovery-candidates-round21.json](evidence/go-recovery-candidates-round21.json)。

## 第二十轮：原生embedding双协议与UTF16分块

资格源码`7a2e50c6ca72c1bfb29b6ae4bd8d06f3e641026d`，普通[41443533098](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41443533098) **success：435顶层/4167含子用例PASS、0fail、3foreign-owner测试skip、18packages**（另cmd/novel-server无testfiles package skip）。embedding新包21顶层/428含子用例全通过、0fail/skip；原TS真实38response+16URL/body+12batch+240chunk共306差分case，12batch另以真实nativeegress HTTP重放。vet/race/原凭据解密/build/noNode smoke通过。

覆盖默认OpenAI50/Gemini100、显式clamp、两协议header/body、index覆盖重排/严格finite维数/跨批维数、nil partial、authority撤销/cancel、超时context、JSON/大小/数值预算、proxy/redirect隔离和Unicode分块。JS会产生孤立UTF16 surrogate的chunk输出明确failclosed拒绝，未以U+FFFD冒充兼容；query/fragment/userinfo/typed options等输入差异公开。没有KB写/检索/激活或public模型工作流绑定，本轮**不提升操作数，仍2/58/221**，main/生产不变。只改普通Go模块，不将它算native Lance重验。见[GO-EMBEDDING.md](GO-EMBEDDING.md)。

## 第十九轮：四项知识只读业务与不可变capture租约

普通Go修复源码`59176ff1b813348264c63ccf2de6c345690ae071`，[41439146355](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41439146355) **success：414顶层/3739含子用例PASS、0fail、3foreign-owner测试skip、17packages**；另有cmd/novel-server无testfiles package skip不混计。knowledgequery12顶层/40含子用例含原TS14组完整列表/统计/vectorless/copy读取差分；HTTP12顶层/47含子用例、KnowledgeManager8顶层均通过。vet/race/原凭据解密/build/noNode smoke通过。

首次普通[41438917975](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41438917975)在CI Oracle多行TypeScript返回类型提取阶段失败，修复提取完整函数后以上重验通过；不隐去失败，不将它计成功。native[41438917823](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41438917823)业务源码`52685005f1bfccc75be8f27dc2326e1660510621`（仅Oracle一行与修复SHA不同）**success**：Rust lockedbuild、native Go vet/build、8facade tests、7顶层/21含子用例projectaccess tests与1真实native OpenKnowledge→authenticated HTTP四读端到端test均通过，0fail/skip；11JS保全检查alltrue。capture cleanup后仍读/source无变/foreign tab拒绝/in-flight release拒绝/撤销后拒绝均实际通过。driver0.22s/maxRSS112368KiB仅微型存储driver，不当联合业务性能。四只读row→IN PROGRESS，合计**2/58/221**，main未绑定、不部署；见[GO-KNOWLEDGE-READS.md](GO-KNOWLEDGE-READS.md)。

## 第十八轮：document/copy关联与联合持锁预检

源码`edf80fb2c4e344c86c40939fce25e4d663a68a9a`，普通Go[41434132059](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41434132059) **success：378顶层/3617含子用例PASS、0fail、3foreign-owner skip、16packages**；vet/race/原凭据解密/build/noNode烟测通过。新增`TestLinks*`14顶层/32含子用例无skip通过，其中9个实际原TS copy lookup/current oracle核对managed/unmanaged/missing/dirty/marker suffix语义；非native Native配置提前拒绝。

独立native联合[41434132123](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41434132123) **success**：locked Rustbuild、native Go vet/build、8facade tests与5顶层/19含子用例真实SQLite+Lance preflight tests全通过，0fail/skip。10拒绝/late-drift/cancel/rename case均零Report与安全清理；4output-cleanup case保留unknown/symlink/replacement。原JS 11项schema/rows/保全/copylinks核对通过，实际PATH=/nonexistent driver0.23s/RSS111816KiB（微型fixture，非作者库性能）。driver自身仍有SQLite占位，不作为联合证据；真正联合来自`native-preflight-tests.jsonl`中原SQLite v7+真实native fixture Preflight测试。本轮不升级operation完成数、不接main、不部署。详见[GO-JOINT-KNOWLEDGE-PREFLIGHT.md](GO-JOINT-KNOWLEDGE-PREFLIGHT.md)。

## 第十七轮：typed native snapshot与canonical领域门

源码`54bc66125f52c18cfc2449b0d78dd1dd82ea989c`，普通Go[41429924274](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41429924274) **success：363顶层/3584含子用例PASS、0fail、3foreign-owner skip、16packages**；format/vet/race/原凭据解密/build/无Node烟测全过。新`knowledgedomain`12顶层/139含子用例（含真实原TS validateTables/tableHash 22case Oracle）均无skip通过。22case中一项明确记录原宽松emptyfieldtype接受而Go strictschema拒绝，不声称全输入域相同。

native[41429924196](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41429924196) **success**：Rust --lockedbuild、Go vet/build、8native tests无fail/skip、PATH=/nonexistent驱动、10项原JS核对/保全检查全过。Lance→domain typed sidecar→Go领域验证摘要6表/3docs/4chunks/4spaces/7embeddingrows/activeGeneration1，49entry/29322byte完整物理stage清理。微型fixture0.20s/maxRSS112200KiB；不外推作者工作负载。证据后续commit只吸收远端format/注释/文档，不宣称其新SHA另编译。详见[GO-KNOWLEDGE-DOMAIN.md](GO-KNOWLEDGE-DOMAIN.md)。本机仅编辑、静态检查及下载小型证据，不运行原Oracle/Go/Rust/gofmt/npm install。

## 第十六轮：知识资产stage与只读元数据

首轮源码`2b1f5dbd423472f904c0fedf39da3d5bc16f249e`，普通Go[41424195056](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41424195056) **success**：350顶层/3444含子用例PASS，0fail、3明确foreign-owner权限skip，15packages；vet/race/build/无Node烟测全部成功。新原TypeScript copy reader/current函数oracle7例实际通过，涵盖Unicode、edited-current、dirty、hashmismatch、empty和unknownrawfields。

独立native[41424195098](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41424195098) **success**：原Rust/Go锁定build+5native tests全部过；真实Go物理stage 49entries/29322bytes，metadata4spaces/3copies，native扫描同一副本6表/14行，由JS核对全部schema/rows，源/copy完整proof和自有文件cleanup通过。真实PATH=/nonexistent执行0.17s/RSS96764KiB（微型fixture不外推）。额外表domain/copy引用语义未实现。

只读审查发现取消+cleanup混合PathError可泄漏复制文件名；`04330543`规范化context sentinel、保留符号cleanup并新增确定性回归，普通Go[41425792641](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41425792641) **success（351顶层/3445含子用例PASS、0fail、3skip、15packages）**，native[41425792583](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41425792583) **success（5native test，0fail/skip，8项互操作与保全检查全通过）**。源码精确为`04330543876d169b2624f06878d9e33b506eb139`；最终format/documentation-only提交不另冒充编译过新SHA。详情：[GO-KNOWLEDGE-COPY-PREFLIGHT.md](GO-KNOWLEDGE-COPY-PREFLIGHT.md)。不升级操作完成数，不接main，不部署。

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

最终源码 `1eaff49c47400637a688ed75362ee68bfab16cc9`（包含最后两项option-presence/HTTP-error回归）已在 [Actions 41389284798](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41389284798) **完整success，4m41s：209顶层PASS /1766含子用例PASS，0 fail，1 skipped子用例，13 package通过**。vet/race/credential oracle/build/noNode smoke全绿。以下6d5f2f71为此前首次全绿证据，不替代最终源码验收。最后test文件与父agent并发staging存在时间差，发现未提交项后单独提交并重新GitHub验证，没有以先前green冒充最终tree。

源码 `6d5f2f7190b6240128f04cd5ed8dca9cc25b99ec`，[Actions 41388270772](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41388270772) **success**，job4m26s：**207 顶层PASS /1764含子用例PASS、0 fail、1 skipped子用例、13测试package通过**。该skip仍为非root runner不能chown的writerlock foreign-owner，未被隐藏；vet/race/build/credentials双向oracle/无Node smoke均通过。

- **154 Gemini原provider differential cases**（实际计数，不使用最初估算170）：请求、visible/legacy精确LF framing、rawusage/规范化/thoughts、finish、EOF、空与非法payload、HTTPcontext、null/omitted选项。Go仅将原JSON.parse engine-dependent SyntaxError安全固定为`Error: LLM_STREAM_INVALID`，测试对此prefix做显式局部转换，其余字段原样比较；该差异不伪装为完全字节一致。原OpenAI72/scanner922继续通过。
- **30 项原ProjectCoreRepository differential cases**：同原schema字节复制、完整返回及原始行变化、allowlist/readonly/threshold、11个源字段CASdrift。Go另覆盖独立连接池并发CAS和SQLabort原子性。
- 新authenticated HTTP私有副本test从原schema开始，通过冻结context/lease读取与更新主台账，两次相同source/target baseline提交恰1成功1冲突；cross-tab、stale、readonly、generated/recovery presence拒绝及inflight release阻止均通过。NULL/negative/REAL baseline和Node Buffer wire的额外transport回归通过。
- 初run41387677962失败在oracle driver的lazy native binding被过窄loader拦截，Go未执行；先在hook前初始化memory-only driver解决。run41387998418仅三项Gemini回调对照因显式空content和省略字段比较错误而失败，修正test adapter保留visible parser error空串后通过；未把实际provider错误删掉或放宽全局比较。

矩阵 **2完成、12进行中、267未开始**；三个core操作仅隔离业务实现（synopsis generated/recovery仍缺），无main绑定、持久作者保存或AI工作流完成声明。见 [GO-PROJECT-CORE.md](GO-PROJECT-CORE.md)、[GO-AI-TRANSPORT.md](GO-AI-TRANSPORT.md)。

## 审稿修稿第九轮

源码 `a5e38a20a85ea04ea02c6498fc3c31258976936b`，[Actions 41391829983](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41391829983) **success，4m42s：227顶层PASS /1996含子用例PASS，0 fail，1 skipped子用例，13 package通过**。该skip为`TestRejectUnsafeLockEntriesWithoutMutation/foreign-owner`（runner非root不能chown）；没有隐藏或把skip说成全覆盖。全部format/vet/unit/race/credential双向oracle/build/noNode smoke通过，本轮首次远端运行即通过；本机无Go执行、大安装或author数据访问。

- **162项原ReviewRepository/RevisionRepository差分**，使用真实review-cycle依赖，仅替换私有synthetic DB getter。每case恢复完整pre-operation DB字节，Go对照相同输入/返回/error及七张表完整副作用，仅排除created/updated时钟字段。
- 完整来源id/chapter/version/status/body与逐字段变化、missing/null/partial、BLOB不强制string、REAL快照读取；MAX序号事务分配、caller index忽略、nullable历史读取和pending替换通过。
- 合并原错误优先级、目标全文CAS+冻结来源检查、exact receipt和重放校验通过。独立pool多并发序号、不同revision同baseline仅一胜、同revision并发重放、作者后续编辑旧请求不得重写，SQL ABORT/ROLLBACK原子性与取消后pool恢复通过。
- authenticated cookie/CSRF/tab/frozenprojectcontext HTTP整链路通过：审稿、修稿、替换、合并、重放、lateredit、stalesource、cross-tab/stalelease、inflightrelease阻止与源fixture字节未改变。格式/owner preflight与provider诊断redaction也通过。
- 安全审查发现历史schema允许content_id共享；新增合并及普通正文CAS事务内独占检查。共享finalized draft/review/revision分别拒绝`CONTENT_OWNERSHIP_CONFLICT`、没有半合并/半字数变化，BLOB正文不转string保存。这是主动安全收紧，不能宣称复制原缺陷也是兼容。
- bound cycle图不在原oracle支持域。Go另以故障注入式结构性不完整cycle绑定验证不跳过owner，**不把这些单元图冒充准入有效/完整review-cycle验收**。真正generation root、作者确认、复查和recovery仍缺，见 [GO-REVIEW-REVISION.md](GO-REVIEW-REVISION.md)。

操作矩阵 **2完成、26进行中、253未开始**；新增14项全为隔离私有副本IN PROGRESS，未在main绑定、未发布作者文件、未部署生产。GitHubcache restore/save的服务故障warning不影响实际冷构建成功，非测试失败。

## 后处理账本与调用历史第十轮

最终源码`64d044fddf2eed1583c8fda86711df5d02c6013a`，[Actions 41395057332](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41395057332) **success，4m43s：244顶层PASS、2177含子用例PASS、0 fail、1 skip、13 package**。skip仍为非root runner的writerlock foreign-owner子用例，不是本轮oracle；132项原差分全部PASS。format/vet/unit/race/双向credential/build/noNode smoke全部通过。cache restore400/save服务可用性warning不影响实际冷构建通过，不冒充测试失败或隐藏其存在。

本轮原实现差分为**132项**：36 CreateRun、14 LatestRun、8 AllPassed、6 Steps、5 FailedStepLabels、13 MarkStepOK、13 MarkStepFailed、11 LogCall、9 Stats、17 History。全部使用原PostProcessRepository/LLMHistoryRepository，只有private DB getter stub；UUID在调用后透明canonicalize，不替换原crypto。Go对照调用前完整DB字节、返回/error和三张表原始副作用；时间字段局部规范化范围见 [GO-POST-HISTORY.md](GO-POST-HISTORY.md)。

- create原子性、重复step key标记全部回滚、summary触发器ABORT回滚、独立pool24次并发attempt无丢失，通过专用测试。步骤ok/失败和all_critical_passed不能分开成功。
- 定稿来源校验精确draft/chapter/status/latest version+ID、outbox finalizationID/hash/full snapshot/body和SHA256；支持历史REAL章号，拒绝BLOB冒充正文。没有读取Lance或发布文件，不能称后处理执行链通过。
- unknown usage NULL、known-zero、分量NULL独立SUM和finish CASE差分通过；History永不输出raw error_message，固定domain错误不泄露provider/path/SQL。
- 合成真实cookie/CSRF/tab/lease HTTP覆盖来源变化、fullidentity成功、关键重跑失败撤销汇总、可选步骤失败不影响关键汇总、跨cookie/tab/stale拒绝和hold阻止Release；原fixture字节不变。
- 首次 [41394277265](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41394277265) 因oracle一个case重复seed同run主键失败；修复为独立runID。随后 [41394461951](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41394461951) 揭示原schema source/step label允许NULL而Go校验过严，两项差分失败；修复存储保留NULL并新增回归。第三次 [41394769438](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41394769438) **132项原差分全过**，但旧invalid-input测试仍把null当错误，修正其测试数据为非文本false；不删原差分、不改expected成Go输出。

九项操作仅隔离账本/历史 **IN PROGRESS**，main未绑定，无持久作者保存；全操作矩阵2完成/35进行中/244未开始。完整runner、LanceDB既有正文、finalization publication、generation/billing owner/recovery仍未完成。本机仅source/JS syntax/git与读取远端文本证据，无Go执行、localoracle、大安装或生产数据读取。

## 叙事线索与作者豁免第十一轮

源码`f49dd968238829dbc277e3e48fd12b6eccbd6a80`，[Actions 41397459923](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41397459923) 首次远端执行即**success，4m50s：258顶层PASS、2353含子用例PASS，0 fail，1 skip，13 package**。skip仍为非root runner writerlock foreign-owner子用例，非本轮业务。format/vet/unit/race/凭据双向oracle/build/noNode smoke全部通过，cache restore400/save服务warning不影响冷构建通过。

- **160项真实原NarrativeThread/ConsistencyExemption repository差分**：26 CreatePlan、10 UpdatePlan、5 DeletePlan、4 GetPlan、42 ConfirmEvent、18 List、24 Relevant、8 Exemptions、15 SaveExemption、8 RevokeExemption。只替换private DB getter，不修改expected去迎合Go；完整调用前DB字节给Go独立副本，返回/error+三表完整副作用一致，只排除created/updated时钟字段。
- JS trim/UTF16中文emoji边界、FEFF/NEL区别、plan validation/range优先级、冻结outbox证据而非现编辑正文、source/evidence先于plan错误、原UNIQUE重复行为、input额外字段/响应id覆盖保留，均通过。Create/Update读回原子；confirm来源检查+plan+insert同BEGIN IMMEDIATE；FK cascade和ABORT/ROLLBACK故障后无半提交，取消/独立pool并发通过。
- List共享读事务快照，按章号+confirmationID取状态而非时间，隐藏非finalized/无outbox event，但所有finalized（含无outbox）计最大章号；dormant/overdue、terminal、Relevant区间/substring与历史BLOB展示匹配一致。
- 审查发现Go driver可把bool身份绑定成1，而原better-sqlite3拒绝，已在原查询优先级点拒绝并用原差分+HTTP证明没有插入；JSON number仍兼容安全decoder.UseNumber。Relevant原JS Array.join对历史BLOB的展示转换已补齐，仅匹配转换，不挪用于正文来源授权。
- cookie/tab/CSRF/lease HTTP覆盖作者计划→冻结证据→terminal→relevant过滤、豁免save/revoke/reactivate、并发upsert、跨session/tab/stale拒绝、hold阻止Release和原源fixture字节不变。未绑定main/作者持久保存/AI确认或完整一致性预检。
- 附加损坏flag单元测试临时关闭CHECK注入revoked=2仅验证严格1判断，不属于准入有效数据或原oracle兼容证据；非string损坏snapshot返回SOURCE_NOT_FINALIZED而非原JS TypeError，明确安全收紧，未伪称异常字句完全一致。

操作矩阵 **2完成、44进行中、235未开始**。见 [GO-NARRATIVE.md](GO-NARRATIVE.md)。本机无Go/gofmt/test/oracle执行、大安装或构建；仅源码、JS syntax、gitdiff和远端文本证据，生产容器、镜像、作者数据不变。

## 剧情树来源/快照第十二轮

源码`b0f1fb276727a3866ee7755298ba115170002fb2`，[Actions 41400535697](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41400535697) **success，4m50s：270顶层PASS、2986含子用例PASS，0 fail，1 skip，13 package**。format/vet/unit/race/凭据双向oracle/build/noNode smoke全部通过；skip仍为非root writerlock foreign-owner测试。

- 原PlotTreeRepository **60项**：35Read/20Save/5Clear。完整返回/原错误/project_core完整表副作用一致，54项原始有序facts JSON+SHA256记录，BLOB/NULL/REAL指数/Unicode/SQL TRIM摘要fallback/最新定稿/线程顺序纳入hash。只替换private DB getter，原migration schema约束保留。
- 原shared validator **271项**，float64/UseNumber各回放一次共**542 case**，加两层数值解析分组PASS=544，不能按测试名斜杠深度少算日期名含斜杠的4条。结构/角色/父子/来源状态/精确身份+章号、immutable上限vs当前来源边界、JS trim、无原不存在的数量或文本上限；所有期望由真实原shared模块产生。日期原Date.parse生成expect，不改expected迎合Go。
- Go额外覆盖源CAS、触发器ROLLBACK、取消恢复、独立pool并发聚合一致性；HTTP cookie/CSRF/tab/lease、read损坏隔离不改原值、explicit clear不变来源hash、generation save拒绝/无副作用、整调用hold阻止释放，源fixture字节不变。
- 首次[41400159254](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41400159254)通过60+542原差分但3个新增集成fixture缺project_core主行（空baseline UPDATE无效）；修正为显式INSERT main后全套通过，无生产代码/原预期改变，不隐去失败记录。
- read与clear只移至IN PROGRESS；public save仍原`GENERATION_GRAPH_ADMISSION_REQUIRED`，即使有事务Save原型也不能算generation owner已迁移，矩阵save保持NOT STARTED。main未绑定，无持久发布。
- **全量验收遗留编码边界**：无效UTF8 SQLite TEXT在Node/Go替换字符计数可能不同，孤立UTF16 surrogate JSON经Go decoder失真；尚未无损实现或显式准入拒绝，不宣称旧项目任意文本完全兼容。Date.parse只证明枚举ISO/commonlegacy域，非通用V8legacy语法。见[GO-PLOT-TREE.md](GO-PLOT-TREE.md)。

操作矩阵 **2完成、46进行中、233未开始**。本机没有Go/gofmt/test/oracle/build/install；生产容器、镜像、作者数据不变。

## 普通蓝图与幂等范围提交第十三轮

源码`c001a386e69762e860b359323eeb817c093c0557`，[Actions 41404142751](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41404142751) **success，4m58s：285顶层PASS、3165含子用例PASS，0 fail，1 skip，13 package**；format/vet/unit/race/凭据双向oracle/build/noNode smoke全部通过，skip仍非root writerlock foreign-owner权限子用例。

- 原BlueprintRepository **160项**：All18/Get16/Upsert24/UpsertMany12/Delete10/CommitRange54/PendingSync12/GetSync14。原TS递归依赖全部真实文件，仅DB access seam替换，完整调用前DB副本、返回/原错误+三表完整语义副作用；author notes/time不忽略，committed_at由两端相同fixture trigger固定后精确比较。
- **精确性边界没有隐藏**：仅characters/character_sync_input合法JSON字符串的键插入顺序未兼容，比较完整解析值/数组顺序，原export raw字符串不改；payload_hash仍严格原值匹配，包括ab/aB/Ab/AB+numeric own keys、Unicode值。2项原Unicode/punctuation canonical-key证据、4项畸形参数JS TypeError证据独立留存，**不计160通过兼容域**。Go unsupported canonical keys显式拒绝；畸形参数固定诊断、安全整数/10000章限制均为安全收紧。
- 事务范围包含full外部delete、author notes+timestamp分组保留、upsert读回一致、commit+sync双ledger；同ID同hash重放只读当前范围，不覆盖后来作者编辑。同ID异payload冲突，ABORT/ROLLBACK/取消与独立pool并发回放/竞争全部通过；旧ledger缺sync依原在effect tx外回填，不能假称pending查询绝无写副作用。
- HTTP owner/cookie/tab/CSRF/lease、UseNumber、整调用hold、原源字节不变；生成handle/authorRecovery无权通过普通commit，completed sync须真实未迁移权威证据故拒绝；clear-all/character-sync-complete不绑定，update-notes仍原finalization admission拒绝，不算完成。
- 首次[41403128338](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41403128338)失败：parent oracle cleaner把typed nil错误变{}，已修；原better-sqlite3会展开array位置参数/将object当named-bind而非通用拒绝，补blueprint-specific arity适配。未改原expected；正常commit原hash首次即过，修复后160全过。

最终加严fixture源码`81620db911f5448013e8d639c89c7ee309669c96`，[Actions 41405028511](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41405028511)再次**success，3m52s，同285/3165 PASS、0fail、1skip、13package、160原差分**；全部vet/race/credential/build/noNode通过。加严点：原第二次seedCommit会自动修补第一条sync，改为两次commit后再删除一条，使测试真正覆盖two-ledgers/one-sync缺失回填；不是改变业务或expected。

矩阵 **2完成、54进行中、225未开始**，新增8项仅隔离业务。见[GO-BLUEPRINTS.md](GO-BLUEPRINTS.md)。main未绑定、未发布作者durableDB。本机没有Go/gofmt/test/oracle/build/install，生产容器/镜像/作者数据未变。后续真实writer协调/Lance/portable权限准入排序记录在[GO-DURABLE-PREFLIGHT-NOTES.md](GO-DURABLE-PREFLIGHT-NOTES.md)，是方案而非已实现。

## 隔离作者准入与legacy共享锁第十四轮

源码`d5ba676f600d26ce27b71292343e4329444c1543`，[Actions 41408608534](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41408608534) **success，4m57s：316顶层PASS、3319含子用例PASS、0fail、3skip、14package**。format/vet/unit/race/原凭据oracle/build/无Node smoke全通过。精确skip为原writerlock与新增legacy两个foreign-owner子用例，以及scratch foreign-owner顶层夹具，runner非root不能chown；其余权限/祖先/type测试实跑，不能声称三个root所有权fixture已实测。

- `projectaccess` **8顶层/23含子用例通过**：同legacy锁持有到完整Stage→private SQLite→source/markers/assets复核和清理；第二个预检/gateway同名竞争busy；原DB字节不变、全成功零scratch残留；预算/schema/取消/manifest/DB/lock替换、portable marker前后创建全部零报告失败；scratch改名通过pinnedroot清副本而不删陌生replacement。初次MkdirTemp后Lstat失败只可能遗留无法归属空目录，显式cleanup错误。
- `writerlock`合计 **26顶层/79含子用例通过**（含旧测试）。真实util-linux flock/Go测试子进程**两个方向**互斥；0600/0644既有inode及bytes/mtime/ctime保全，0640/0666/symlink/hardlink/FIFO拒绝；Verify核对固定名字/完整parent/inode/ctime/mtime与Close并发。原`.novel-writer.lock`仍不同scope，不误称协调。
- `projectfs`合计 **36顶层/242含子用例通过**（含旧测试）：四个实际portable sidecar/journal marker任何type存在拒绝；原TS源码constant+纯restore journal hash函数oracle核对ASCII/Unicode/case路径。没有解析/采信原历史authority。固定Lance/registry/copies入口metadata准入与变化拒绝，不读取正文/向量，不枚举或宣称Lance格式兼容。
- 首次[41407717578](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41407717578) format/vet/原authority oracle及projectfs/writerlock通过，但新预检fixture失败并导致hold-handshake等待超时：Go `t.TempDir`在runner模式不保证0700，测试错把它当私有scratch；补测试显式chmod0700（不修改生产准入规则），握手加完成/超时分支防挂住；修复后全过。

本轮**不升级任何operation**：矩阵仍2/54/225；无durable原库能力，main不绑定，不创建/读写正式lock/作者DB，不部署。真正native Lance版本/全文、portable权限移植、populated迁移、全局/生成lifecycle及281浏览器完整验收仍未完成。详情[GO-AUTHOR-PREFLIGHT.md](GO-AUTHOR-PREFLIGHT.md)。

## 精确原生Lance隔离试验第十五轮（合成互操作已通过）

普通回归源码`07a17833cc1de7bc100ccfeafa632e5af6222f89`，[Actions41418579981](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41418579981) **success，4m56s，316顶层/3319含子用例PASS、0fail、3skip、14package**；format/vet/unit/race/凭据/build/noNode全过。普通CI不启用`lance_native`标签，不可把该结果当原生包已编译。

新增精确Rust0.22.3/Arrow56.2静态库、C ABI、Go直接链接驱动与独立GitHub任务，均只对私有合成副本。原Node0.22.3/Arrow18.1已实际生成6表/14行、多个embedding generation、空schema-bearing表和active表第二版本。首次原生[41418579889](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41418579889)编译到lance-encoding因缺protobuf标准include失败；修复系统包与PROTOC_INCLUDE后`bc14231b`的[41420077722](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41420077722) **success，约9m50s**：Rust locked build、Go vet、5项native测试0fail0skip、215M Go/CGO产物PATH=/nonexistent真实执行、完整Rust/Go/Node schema/全文/空表/reopen及source/copy bytes/membership/registry/copies保全全通过。active源v2→新库v1，原Node确认；不恢复历史号。独立普通Go[41420077714](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41420077714)也success。Rust build5m25s、target3.0G/staticlib841M、time最大RSS1619064KiB；微型native fixture0.10s/RSS109712KiB，不能外推作者规模。

锁文件在GitHub生成，下载审计591包sharedchecksum及Lance/Arrow/DataFusion版本后提交；root publishedcrate与upstream stale beta/root差异及license后续门有明确记录。原oracle实际发现vector-list childnullable在createTable和createEmptyTable均变true，按实际持久化schema严格校验，不把原构造Field当落盘schema。源码只读审查没有发现trustedfixture范围Critical/High；不是动态验收。详细[GO-LANCE-NATIVE-SPIKE.md](GO-LANCE-NATIVE-SPIKE.md)、`backend-go/native-lance/README.md`。

仍2/54/225；未绑定任何KB业务、未读取作者正文、未部署。全程编译/测试/依赖安装仅GitHub，本机只改源码和读取小型证据。

## 不代表什么

- 281 项中只有 2 项固定导航意图实现并绑定，279 项仍未迁移，详见 [GO-MIGRATION-MATRIX.md](GO-MIGRATION-MATRIX.md)。安全/租约/传输与草稿仓储模块通过，不等于用户业务端到端可用。
- 草稿正文 CAS/读取以及只读 v7 schema/物理路径核验已有 Go 源码和远端合成测试，但项目打开、旧库升级、完整作者内容证明与 HTTP 领域绑定尚未完成，不能对原作者数据库启用。
- AI 生成/审稿修稿定稿、后处理、LanceDB、归档、WebDAV、MCP 等仍待迁移。原 Node 通过的验收不能转成 Go 通过。
- 构建物明确标注非生产 scaffold；没有发布 Go 镜像或部署。正式容器保持原固定 Node 镜像；旧 Node 自动发布已改为仅手动 dispatch。

下一阶段继续完善数据准入的剩余兼容门并接入作品与草稿纵向业务；所有新增编译与测试继续交由 GitHub Actions。
