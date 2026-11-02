# 第十七轮：合成 native typed-domain 验证

本轮目标：在现有Go完整物理知识stage之后，从同一批native table query batches生成有预算的typed rows/schema，交给独立Go领域validator。**不是作者库准入、可写Lance、完整知识库或281业务操作完成。** 源码`54bc66125f52c18cfc2449b0d78dd1dd82ea989c`已推送，普通Go[41429924274](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41429924274) **success：363顶层/3584含子用例PASS、0fail、3明确foreign-owner skip、16packages；vet/race/build/无Node烟测全过**。新领域包12顶层/139含子用例PASS（含真实原Oracle22case对照），未skip。native[41429924196](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41429924196) **success：Rust locked build、Go vet/build、8native tests无fail/skip、PATH=/nonexistent真实驱动、10项JS schema/rows/domain与保全检查全通过**。native→Go领域摘要为6表/3docs/4canonicalchunks/4spaces/7embeddingrows/activeGeneration1，包含空schema表；source/copy全部字节与metadata不变且Go stage清理完成。微型fixture0.20s/maxRSS112200KiB，不外推作者库性能。后续commit仅吸收GitHub format、注释和证据，不冒充重新编译新SHA。

## 新链路

`lancenative.Export`仍输出原Arrow stream IPC及原manifest，另在空私有输出目录exclusive创建0600 `domain.json`。同一已checkout(version)的batch同时用于IPC与typed领域sidecar，不重查活动表；没有Node/Python运行桥。支持实际canonical字段Utf8/Int32/Float32/Float64/FixedSizeList，原Arrow metadata存在状态显式保留。Float32按实际值先提升Float64再JSON编码，不使用短Float32十进制损失精度。

native额外hard caps：32表、10000总行、200000含vector元素cell、32MiB字符串、128总field/child、16层递归和64MiB序列化JSON；原IPC每表64MiB限制保留。native Arrow/Lance batch可能事先分配，因此不是RSS承诺。为供领域验证累积一个有明确大小上限的私有快照，不声称任意作者大库流式external-sort已实现。超过限额fail-closed，不能以拒绝大库冒充完成兼容。

`ReadDomain`固定文件读取、private/euid/singlelink/regular/nofollow/nonblock、64MiB限制与符号错误，调用者保持输出目录独占；native原同步调用context只在前后检查，不声称可抢占native task。

## Go领域门

`internal/knowledgedomain.Validate(ctx, Snapshot, Budget)`没有filesystem/database能力，不读原作者库，不改传入maps。所有预算必须显式正数且不大于hard caps：32表/10000行/200000cells/32MiB strings+registryraw。先资源预检再建有界ID索引；输出只有counts和activeGeneration数字，所有失败零Summary。

- table名白名单和唯一、schema/field/child metadata拒绝、字段白名单与required列、唯一fieldname、row恰好匹配schema字段。
- scalar类型/nullability符合本轮窄schema；Utf8有效、number有限，Int32在范围内，vector FixedSizeList且元素finite Float32范围、禁止null vector元素。
- 各表id非空唯一；document/chunk string字段与corpusKind；canonical orphan拒绝；每doc chunkCount精确、chunkIndex完整唯一0..n-1，totalChunks一致，允许零chunkdoc。
- registry generation/table唯一safeinteger、实际table存在、dimension吻合、合法status/fingerprint/metric/createdAtstring；space每row与canonical同id/docId/text；inactive/building允许子集；active最多一个且全量，activeGeneration精确等于active或null；不允许未注册的chunks__space_N。
- JSON wire registry null表示资产缺失（不是原文件存在且内容为`null`的合法声明）；完整stage的knowledgecopy metadata validator在领域门前仍先验证原file，已有非法null文件不会靠wire缺失绕过。

## 与原验证器的对照边界

GitHub-only `knowledge-domain-contract.mjs`从原`vector-migration-snapshot.ts`提取未改的plain/rows/fieldSchema/tableHash/validateTables，真实Arrow18建立22个合成snapshot并执行原validateTables。case包含orphan/count/order/duplicateid、space正文/doc漂移、active不完整/多active/pointer/dimension/未注册space、未知/缺列/metadata、zerochunk和空库。Go相同wirecases对照accept/reject；额外一个empty-table text Float64原宽松validator接受而本轮Go strict-schema拒绝，明确记为不同域，不伪装全输入等价。

本轮不实现Go精确原`tableHash/logicalHash`。原Oracle先用原函数生成各casehash，只是让原validator实际运行；Go验证关系不宣称hash编码/locale/ECMAScript JSON兼容。JS additionally compares native typed sidecar全schema/records与Arrow IPC同一真实合成Lance来源，避免只验证手写JSON夹具。

## 本轮不会提前接线的部分

- `projectaccess.Preflight`尚不调用native domain validator，main/RPC不变；native CI驱动仍使用opaque假SQLite占位，不构成跨引擎准入。
- document的knowledge-copy marker/docId→SHA256filename关系尚无验证；dirty/edited正文必须保留、不可强制与indexed chunks相等。
- 不含FTS/search/distances/embeddings写入、历史version override、legacy向量迁移、portable authority、populated M03/M04、durable lifecycle。
- 生产容器、作者卷、凭据不变；操作矩阵仍2完成/54进行中/225未开始。
