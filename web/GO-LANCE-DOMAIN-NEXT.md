# Native Lance域准入下一关（设计，不是已实现）

前置已达：native0.22.3/Arrow stream双向schema/rows/reopen，Go有界完整物理assetstage、rawcopymetadata、源/副本proof和安全cleanup。下一步不能直接把此结果变成project:open。源码参考原`electron/services/vector-migration-snapshot.ts:68–139`和`electron/vector-store.ts:135–173`。

## 一次私有副本中的schema/stream域验证

应让native在**当前固定私有copy**上逐表钉version，输出有预算的schema+typed rowstream，不先把任意大整个库JSON/IPC无限装载。Go側消费typed cells，累计领域摘要并尽早失败；建立有界id外排序/磁盘暂存或显式小预算，不以总库无界map作为作者准入。

1. table白名单、unique；Arrow表/field/child metadata空门；原allowed/requiredfields精确覆盖，Unicode正文禁止隐式替换。额外schema收紧必须用原Oracle明确差异。空表保留schema。
2. 每table非空unique string id；documents fileName/importedAt/filePath、chunk docId/fileName/text/importedAt必须string；corpusKind只reference/project-knowledge/unknown。
3. canonical chunks引用existingdoc；每doc safeinteger chunkCount实际相等；chunkIndex完整唯一0..count-1、totalChunks==count，含零chunkdoc。
4. registry generation唯一safeint、table唯一且确实存在、status/fingerprint/createdAt/metric/dimension局部门。registeredtable必须fixedsizelistdimension吻合且vector元素有限float；rowid存在canonical且docId/text精确一致。inactive/building可合法子集；active恰好一个或null、active全量且activeGeneration指针一致。未注册chunks__space_N拒绝，不清除历史空间。
5. copy引用独立门：由documents.filePath的knowledge-copy marker确定需要的docId→SHA256filename，不用目录文件名倒推docId；既保留合法dirty/edited当前内容，又区分是否满足索引使用条件。原snapshot并未覆盖这一关系，需明确设计缺失/多余/索引过期的拒绝或降级策略，不擅自“repair”。

## 原logicalHash风险（需专门对照）

原tableHash是`JSON.stringify([name,schema,rowsSortedByStringIdLocaleCompare])`，最后tablepair默认sort和registry原bytehash再封装。Go普通JSON编码与locale不是同协议：

- JS Number格式、-0、largeinteger与float32转double；Go encoder HTMLescaping、U+2028/U+2029、slash及Unicode；未知字段顺序必须由Arrow schema导出。
- `String(id).localeCompare`受Node/ICU默认locale，非UTF8/UTF16简单字典序；要么实现有确切版本/locale约束的比较和ECMAScript JSON，或单独批准新的明示格式，只能在同协议下比较。不能把物理manifest SHA或rawIPC hash冒充logicalHash一致。
- 每表Arrow field type.toString/nullability/childschema完全来自真实持久schema；stream/file差异和空batch schema已验证但不涵盖任意metadata类型。

GitHub应提取原validateTables/tableHash纯函数，固定实际Arrow依赖，仅对合成数据执行。变异cases覆盖缺列/错类型/metadata/重复id/orphan/count/order/activepointer/dimension/nonfinite/copydirty，断言各拒绝域和无源变更；不要把本地重新写的JS期待值当原Oracle。

## 生命周期最终接线仍后置

SQLite snapshot和Lance版本钉选不是跨表/跨引擎事务。必须维护旧gateway同锁+桌面关闭+整个项目quiescence到所有proof复核，任何不合作writer仍是拒绝/维护窗口前提。验证器只返回匿名counts/digest/可分类状态，不返回原root或写能力。先与Preflight opt-in形成一次受控闭包再讨论durable authority；main/RPC生产仍不接线。
