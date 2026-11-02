# 第二十二轮：摘要快照与定稿连续性读取

本轮多agent按不重叠文件并行推进仓储、HTTP、原TypeScript差分Oracle，父代理负责真实私有会话整合和GitHub验证。所有编译、gofmt、测试、race和原Oracle执行均在GitHub；本机只源码编辑/轻量语法与证据检查。不部署、不切流量、不读取作者正文或密钥。

## 四项实际业务，两个拒绝入口

- `db:save-summary-snapshot`：只追加`draft_id IS NULL`的旧式角色状态快照，保留原字符串，包括空白、Unicode、JSON样式文本；不是正式定稿来源绑定数据、AI提取证据或生成receipt。
- `db:get-latest-summary`：按id倒序只取旧式快照，忽略所有`draft_id IS NOT NULL`正式投影；未找到返回null。
- `db:continuity-read-source`：读取已定稿不可变正文，原三分支`valid/legacy/invalid`；valid需要实际outbox、finalized状态、非空finalizationId、小写SHA256、全文body==content_snapshot、实际hash相符，以及continuity watermark；只有没有outbox的历史定稿才能legacy fallback，损坏receipt不得冒充legacy。
- `db:continuity-list-before`：按原SQL选目标章之前最新非空summary，来源可为finalized/superseded；当前定稿按version/id排序，独立校验source/body/hash、projection generation/stale_from_chapter与character identity frozen snapshot，返回`current/legacy/stale`分类及安全fallback当前定稿id。保留原事实/候选JSON容错投影语义，不修复原记录。

`db:continuity-save-finalized`和`db:continuity-save-character-state-candidates`继续原版明确拒绝`GENERATION_FINALIZATION_ADMISSION_REQUIRED`，即使传入看似有效request也不写。它们的拒绝handler、HTTP200或测试不算迁移完成，矩阵不提升。这一轮也不实现character proposal决策或正式角色状态投影写入。

## 来源和身份边界

read-source的valid snapshot包含draftId/finalizationId/chapterNumber/contentHash、原章节标题、全文、projectionGeneration；不从发布文件或知识库猜补正文，不规范化Unicode、不裁剪正文。投影失效不抹去仍真实有效的定稿source。list的latest-summary压制规则按原summary id与非空notes独立于draft状态，不改成“看起来更合理”的chapter/version聚合。

事实解析保留原分类、sourceChapter、ECMAScript trim、entities去重及characterRefs检查；一个非法fact使该行facts整体退回[]，不当成author-confirmed事实。候选支持历史数组和version2 pending，绑定metadata不完整的候选丢弃；合法source绑定候选保留value空白，legacy value按原trim，任意rawValue只作不具权威的JSON显示值。

identity schema仅按原`character_identity_meta`表存在性判断。当前来源有identity schema时，需要真实不可变`fcs:<finalizationId>`快照及原始raw哈希/来源/identityRevision/characters检查；缺失或无id候选→legacy，坏快照、错误/歧义引用→stale。不会把显示名称自动映射为新身份，不以最新角色表覆盖冻结身份，也不把legacy显示输出当新写入权限。

## 事务、输入与会话

仓储借用已准入DB、不DDL、不按renderer路径开库。读路径在同一connection一致事务中处理，不混合多个writer版本；SaveSnapshot采用事务+readback，异常trigger或取消rollback，不能留半次追加或把legacy快照改写为正式来源行。typed native输入明确只接受safe integer chapter（legacy保存允许负数/0，source/list只正整数）和有界有效UTF8文本；原DB返回文本同样拒绝非法UTF8或超32MiB，合法JSON中的孤立escaped UTF16 surrogate整次拒绝`SUMMARY_INPUT_UNSUPPORTED`，不让Go decoder/HTTP静默变成U+FFFD；普通坏JSON语法仍保留原空投影fallback，正确surrogate pair与literal escaped backslash照常接受。JSON数字按原JavaScript binary64舍入，含unsafe整数、有限小数和有符号underflow；浮点overflow域显式拒绝而不以Infinity/null冒充，JSON容器最大64层；原JS动态类型/SQLite可接受的fraction/null/string等不冒充完全相同输入域。

这些界限是单字段／JSON深度限制，不是整个项目行数、全列表输出大小或进程RSS硬上限，不能据微型fixture承诺任意大小作者库可用。

认证HTTP复用cookie/CSRF/origin/tab，原始frozen context恰好3字段，path/projectId/leaseId由已核对provider借用验证，全调用保持hold，release/close在途拒绝；foreign/revoked context不能读正文。失败只回有限公开error code，unknown provider/SQL错误和typed-nil HTTPError不可泄露或panic。main不绑定本组。

## 验证与未完成门

GitHub-only执行真实SummaryRepository及真实递归依赖，仅替换DB环境入口。每case从实际baseline-v7合成数据库种子导出before bytes，Go执行同bytes，比较完整返回/原错误/summary raw后状态；只排除created时钟，不排除source/body/hash。另有trigger失败/静默修改、取消、一连接防嵌套死锁、同owner refresh保留scratch、source valid→generation stale→receipt invalid、foreignowner/revoke和作者fixture bytes不变等测试。

首轮`74b9b96d`，[41454246646](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41454246646)在远端vet发现transport测试有未使用net/http import，修复后重跑。第二轮`ad88fd5d`，[41455201247](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41455201247)实际141原仓储差分与所有业务/会话测试通过，但一个closed DB错误断言错误期待message=code，而实际安全层使用通用中文脱敏message；仅修正测试期待，不放宽或改动生产错误脱敏，随后重跑全部资格。两个失败run不计整体通过，race/build烟测不能借用未到达阶段的假证据。

最终资格源码`02c04cb6fa2e4202cf8f80eeb95aea6a2699bbb5`，[41457578715](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41457578715) success：469top/4397all PASS、0fail、3foreign-owner测试skip、18packages（另cmd无testfiles package skip），新增摘要19top/160all无fail/skip；141实际原仓储case全部通过，独立Go-onlyFAIL事务fixture亦通过。vet/race/原凭据decrypt/build/noNode smoke通过。准确资格/失败历史见GO-CI-ACCEPTANCE.md及evidence/go-summary-continuity-round22.json；后续只吸收GitHub生成的本轮新文件格式和文档证据，不宣称最终文档SHA另外编译。仅四项业务→IN PROGRESS，矩阵2/66/213；无durable author save、main业务绑定、AI语义提取/角色状态commit、generation/finalization admission、跨存储定稿或完整作者库验收，正式迁移目标仍未完成。
