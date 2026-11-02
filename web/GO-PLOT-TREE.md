# Go 剧情树来源与快照（隔离验证）

## 实现及明确门禁

`store.PlotTree`聚合现作品来源，读取/隔离存储快照，提供事务化Save原型和显式Clear；`BindPlotTree`仅借已准入私有scratch项目并保留全调用lease hold。main未绑定，不持久发布作者文件，不启动Node。

原`db:plot-tree-save`公开controller始终返回`GENERATION_GRAPH_ADMISSION_REQUIRED`。Go保留该门禁，先验证真实owner/lease/path，再拒绝；不调用repository.Save，不从server重读baseline替浏览器重试。**拒绝响应不是操作迁移完成**，该矩阵行仍NOT STARTED，直到完整generation graph owner接入。read/clear有真实隔离业务，最多IN PROGRESS。

## 来源聚合与指纹

- 同一只读事务读取project_core、按章排序blueprints、现finalized每章最新version/id稿件与narrative事件，避免多个独立SQL拼出不同事务的混合来源。
- title严格SQLite COALESCE(outbox.chapter_title,blueprint.title,'')，空串不被fallback吞掉。summary按原NULLIF(snapshot.chapter_notes,'')，再仅有非空SQL TRIM(notes_updated_at)的blueprint.notes；SQL TRIM仅ASCII空格，不换成JS trim。NULL/BLOB保留，不转正文string。
- narrative聚合复用同一queryer的状态计算，但指纹仅选原字段，排除dormant/overdue和createdAt等；不加入新的全局状态或当前快照。
- SHA256输入保持原JSON.stringify声明顺序：writingLanguage、synopsis:{content}、blueprints、finalizedChapters、narrativeThreads；内层字段同原对象声明顺序。Go map排序、HTML escaping、U+2028/2029 escaping、Buffer base64都不可替代。
- 原始BLOB以Node Buffer JSON编码；NULL是null，数值保留JS exponent/fixed切换与零表示，摘要/中文/emoji/控制字符原样参与指纹。默认语言仅严格en-US，否则zh-CN。当前plot_tree_snapshot不参与sourceRevision。

## 快照校验

Stored校验只要求快照结构/immutable章号天花板4294967295，不要求历史来源仍存在；历史有效快照在原来源删除后仍可读。Current校验另要求语言/指纹一致、每个blueprint/finalized/thread/event引用真实存在、事件状态与来源匹配、轨道范围位于当前有效来源domain。

轨道id、父子角色、重复id、事件range及来源引用遵循原shared模块。原模块没有max tracks/events数量上限，不虚构旧限制。文本JS trim，非空，没有新增长度上限。原String(role/status)的JSON数组coercion怪异行为仅在校验中复刻，不赋予写者权限。

Date.parse兼容覆盖原oracle中的ISO和常见历史日期形式，不宣称穷尽V8所有implementation-defined日期字符串；Go无JS运行时依赖。尚未测试的日期形式是兼容边界，不可以借Node进程在运行时补齐。

只验证有效Unicode文本域及原始BLOB编码。损坏SQLite TEXT中的无效UTF8多字节截断，Node与Go替换字符计数可能不同，从而指纹不同；JSON中孤立UTF16 surrogate经Go标准JSON decoder会丢失为U+FFFD，不等于JS保留原code unit。尚未实现无损跨端编码，**属于全量迁移验收前必须解决/显式拒绝准入而保留原数据的遗留兼容项**，不能声称已完全兼容所有旧项目。不得静默替换作者原值。

读取损坏/不安全快照返回snapshot:null + storedSnapshotInvalid:true，不删除、不重写raw值。尾随无效JSON也隔离，不仅解析第一个JSON值。显式Clear才清字段，不改synopsis等来源。BLOB存储JSON遵循原JSON.parse的Buffer文本显示转换，不用于正文来源授权。

## 原型Save和事务

Repository.Save在BEGIN IMMEDIATE内重新聚合完整来源、比较expectedSourceRevision，再校验当前来源并写入规范化JSON；错误优先级sourcechanged先于快照invalid。不会sourcechanged后自动取新revision重试。标准化JSON字段顺序与原TS一致，以便完整DB副作用比较。触发器ABORT/ROLLBACK、取消不产生半保存，池仍可用。

这不是generation owner或AI模型调度，也不解决全局writer/旧版本持久项目发布或LanceDB已有正文问题。

## 验证

GitHub-only执行原PlotTreeRepository及原shared validators，私有合成baseline，全量调用前DB字节复制。repository返回/error与project_core完整表副作用比较，只忽略created/updated时钟字段；sourceHashEvidence保存真实JSON.stringify facts和原SHA256，不把Go输出当expected。shared结构/来源校验另有271项独立原实现差分，每项分别用float64与UseNumber解析执行，共542个case回放；repository为60项（35Read/20Save/5Clear），54项额外保存原始指纹字节证据。

认证HTTP验证read/clear、损坏值只隔离、生成保存拒绝且无副作用、跨cookie/tab/stale拒绝和hold保护。Go额外验证source-CAS、触发器rollback、取消恢复、并发来源快照一致。本机不执行Go/gofmt/test/oracle/build/install，最终源码`b0f1fb27`通过[Actions 41400535697](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41400535697)，270顶层/2986含子用例PASS、0fail、1个非root foreign-owner skip、13package，vet/race/build/noNode全部通过。首次3个集成fixture缺main主行已修正为显式INSERT；原60+542差分自首次执行即通过。实际CI见GO-CI-ACCEPTANCE.md。
