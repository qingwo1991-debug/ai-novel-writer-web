# 第十九轮：有界私有知识快照的四项只读业务

本轮移植kb:list-documents、kb:stats、kb:get-vectorless-count、kb:read-document-copy的实际Go只读领域逻辑与认证HTTP。不是完整知识库，不接main，不升级为IMPLEMENTED，不开放原作者库写能力；只在GitHub合成资格通过后将四项计IN PROGRESS。

## 实际业务

`knowledgequery.View`构造时先通过既有fixed-budget canonical domain门，再对同一私有物理stage读取全部managed副本，捕获不可变值。文档保持native query顺序，公开6字段；corpusKind缺失投影unknown。统计只返回documentCount/totalChunks/vectorDimension三字段（不误返回vector-store内部hasVectors）。没有chunks表时统计全零，即使存在零chunk文档。

无向量计数严格选择active空间，否则registry数组中第一个building空间；不把inactive或第二个building当补齐。没有选中空间则所有canonical chunks计missing；active领域门已要求全量，所以正常active计0。不会推断legacy embedding registry或创建文件。

副本读取仅managed文档存在且SHA256(docID)文件可用时返回available/content/contentHash/edited/indexStatus；current为!dirty&&hash一致，dirty/错hash为stale；unknown/unmanaged/missing返回严格`{available:false,indexStatus:'unavailable'}`。hash使用原UTF8内容，不重排正文、不隐式reindex。副本不与canonical chunks强制相等，编辑合法。错误只返回符号码，正文仅通过受认证read操作有意返回。

## 数据来源与会话

`projectaccess.InspectKnowledge`沿同一legacy排他Preflight，复制SQLite与所有知识资产，native版本钉选→领域验证→构建View及copied SQLite finalization doc→chapter投影→最终原源/copy/marker/lock验证→清理scratch→unlock。只有全部完成才返回private KnowledgeRead；任何late错误/cleanup/lockclose失败清空capture和Report。无原root/FD/repo能力逃逸，View不保留caller map/JSON/root/FD，scratch删除后仍可读取。

独立`projectsession.KnowledgeManager`只管理不可变capture，和原可编辑scratch Manager明确分离。可信管理员OpenKnowledge调用真实Inspect，不接受renderer传filesystem配置；每次读取匹配cookie session+tab+projectId+leaseId+captured path，整个读期间hold。Release/Close在有in-flight读时拒绝，不能偷撤其他标签。

同owner refresh保留同capture/lease，不读源、不应用新配置，**不代表current author state**。Release后再open才能recapture。capture是静态隔离读权限，不是durable project:open、author writer lease、全局currentDB或自动更新视图。未来接入正式生命周期时仍需统一freshness/generation策略，不能并排拼装这些独立manager冒充全量可写项目。

HTTP仅显式BindKnowledge四项（main不调用），严格位置args+冻结context+expectedPath一致性；provider返回同捕获项目的View与finalized章号，list在匹配docID时加finalizedChapter。未知/保存/search/reindex/import/delete不以空成功替代，仍501。

## 验证与未完成

首个普通任务[41438917975](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41438917975)在CI-only Oracle提取阶段失败：`readDocumentCopy`的多行对象返回类型含行首`}`，旧提取器误把类型结尾当函数结尾。修正为整行`}`后跟换行，保留完整原函数；修复源码`59176ff1b813348264c63ccf2de6c345690ae071`，普通重验[41439146355](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41439146355)。native任务[41438917823](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41438917823)针对原实现源码`52685005f1bfccc75be8f27dc2326e1660510621`，两者业务源码相同，仅CI Oracle提取一行不同。普通重验已**success：414顶层/3739含子用例PASS、0fail、3foreign-owner测试skip**（另有cmd/novel-server无testfiles package级skip，不计测试skip）；17packages通过。新knowledgequery12顶层/40含子用例全部通过，其中14原TS完整querycases；新HTTP12顶层/47含子用例全部通过，KnowledgeManager8顶层通过；vet/race/build/原凭据解密/noNode smoke通过。native亦已**success：8facade tests、7顶层/21含子用例projectaccess tests、1真实native capture→session→HTTP端到端tests全部PASS，0fail/skip**；locked Rustbuild、native Go vet/build、11项原JS schema/fullrows/physical preservation核对通过。driver合成微型fixture0.22s/maxRSS112368KiB仅存储driver，不是联合HTTP性能。真正业务证据来自独立`native-knowledge-http-tests.jsonl`，包含四读、外tab拒绝、active hold拒绝release、撤销后拒绝；`native-preflight-tests.jsonl`覆盖cleanup后capture仍读及source不变。失败任务不算通过证据。

本机只编辑、node --check和git diff检查；不运行Go/Rust构建/test/race/gofmt、原Oracle或大型安装。所有secret/prose fixture均为公开合成，正式volume未读取。

原TS纯函数Oracle提取实际listDocumentsInternal/getStatsInternal/getChunksWithoutVectorsInternal及knowledge-base.readDocumentCopy，在GitHub以query adapter+真实合成rawcopy文件执行14组完整案例；每组核对列表/统计/计数/两次读取。queryadapter仅为逻辑差分，不替代native存储证明。

独立nativejob另测真实原SQLite v7+Node Lance0.22.3六表→持锁Inspect→scratch清理后View读取；真实OpenKnowledge→冻结租约→authenticated HTTP四读→in-flight release拒绝→foreign tab拒绝→撤销后拒绝。普通job覆盖copy unsafe/预算/identity/cancel、immutable map、manager owner/并发close/borrow、HTTP auth/CSRF/参数/错误脱敏与原14组对照；全部编译测试只在GitHub。

仍缺知识库search排序/score与模型调用、embedding写入/回填/激活、copy保存全文CAS、reindex/delete/clear、importreceipt/执行owner、native任意大库、精确JS logicalHash、durable作者会话、浏览器端全验收。保持正式容器不变。
