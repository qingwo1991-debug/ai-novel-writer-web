# Go 章节蓝图（隔离业务）

## 范围

`store.Blueprints`借已准入项目DB，实现All/Get、Upsert/UpsertMany/Delete、普通CommitRange、PendingSync/GetSync；`BeginBlueprints`整调用保持owner+cookie/tab+exact context/lease hold，`BindBlueprints`保留原RPC返回envelope。main未绑定、无持久项目发布、不启动Node。

- `db:blueprint-get-all/get/upsert/upsert-many/delete`：真实SQL业务与原row projection。
- `db:blueprint-commit-range`：只实现无generationRunHandle/authorRecovery的普通提交，生成/恢复owner请求拒绝`GENERATION_OWNER_REQUIRED`；不自动重采来源或移除owner后重试。
- `db:blueprint-character-sync-list-pending/get`：持久待办/回读，仅能解释尚未完成的凭据；完成条目需真实roster/proposal证据，Go未迁移则拒绝，不相信renderer完成声明。
- `db:blueprint-update-notes`：原公开controller始终要求`GENERATION_FINALIZATION_ADMISSION_REQUIRED`，继续拒绝，不把拒绝当实现完成。
- `db:blueprint-character-sync-complete`与`db:blueprint-clear-all`未绑定，后者必须同时处理generation recovery退休等事实，不能简单DELETE表冒充兼容。

矩阵最多8项移IN PROGRESS，其他3项仍NOT STARTED。所有生成/导入/角色身份完整业务和普通作者蓝图repository的存在不是同一权限。

## 数据与事务

读取NULL role/purpose/keyEvents/suspenseHook/userGuidance/notes/notesUpdatedAt只执行原`?? ''`投影，不把BLOB转字符串。characters使用原JSON.parse语义：合法非数组JSON仍返回scalar/object/null，不虚构旧schema没有的数组限制；损坏JSON返回[]。chapter bool拒绝driver绑定成数字1，保留正常JSON Number/HTTP UseNumber。

批量upsert同事务，后项失败全部rollback。普通upsert是原无CAS覆盖语义，**不得当作AI来源目标防覆盖方案**；原CommitRange在BEGIN IMMEDIATE中提交完整精确范围、检查回读、写入operation+character-sync两个事实并返回收据，原子失败不产生半提交。

- full模式从1开始，删除范围外蓝图；replace-range保留范围外。
- 保留作者字段：新userGuidance truthy才取新值，否则旧值；notes与notesUpdatedAt同组，不能新notes为空却用新timestamp覆盖旧摘要时间。
- 相同operationId+payloadHash重放不再产生写效果、不覆盖后来作者编辑；当前范围缺失或收据损坏按原错误失败。不同payload同ID冲突。
- read pending/get同原ensure逻辑，已有commit ledger缺sync时回填INSERT OR IGNORE；不是完全无副作用的只读查询。原DDL在Go不补建：schema准入必须先满足所有baseline表，不能在作者DB临时迁移。

## 明确兼容边界

- 原payload canonicalize递归localeCompare('en-US')排序，Go尚无完整ICU collation。仅ASCII字母/数字keys支持并有原混合大小写/数字key证据；任意Unicode/标点key**显式拒绝**`BLUEPRINT_COMMIT_CANONICAL_KEY_UNSUPPORTED`，绝不写入不同hash而假称幂等兼容。额外原Unicode证据单独留存，不列入Go已通过兼容域。
- Go采用安全整数与最多10000章范围边界，防止原Number.isInteger配合巨型Array.from的内存风险；这是安全收紧，不伪称原错误完全相同。完整generation requested range另有原owner限制，未迁移。
- JSON对象输入原键插入顺序在Go map中不可恢复，普通characters对象序列化顺序及不可枚举旧异常文本编码仍需完整兼容方案。不能用Go map排序替代任意JSON.stringify顺序后宣称完全兼容；默认characters应是字符串数组。原生非JSON Buffer对象参与canonicalize时原TS按numeric own keys而Go按Buffer JSON处理，当前不属于受支持的入参域；HTTP只收普通JSON，读投影保留BLOB不等于支持所有native Buffer写入。
- 无效UTF8/lone-surrogate和完整V8日期语法仍见`GO-PLOT-TREE.md`，全量发布前必须解决/显式拒绝准入并保留原数据，不能静默改作者值。

## 验证

GitHub-only从原BlueprintRepository生成私有baseline DB差分，完整返回/error及blueprints/commit/sync三表副作用比较；`characters`/`character_sync_input`内合法序列化JSON仅比较解析后的完整语义（保留数组顺序/值/所有字段），原JSON键插入顺序未兼容，不宣称原始DB字符串字节一致。payload_hash本身仍与原串严格一致，不把hash降成语义比较。notesUpdatedAt/notes_updated_at始终保留比较，不以时钟名误排除。committed_at通过两端共同的合成fixture trigger固定，证明相同约束/触发器行为；不能忽略持久收据字段。额外Go并发/rollback/Unicode/范围/replay测试与HTTP cookie/tab/CSRF/lease、作者字段保留、普通重放不覆盖、权限拒绝并验证源fixture字节不变。

本机不运行Go/gofmt/test/oracle/build/install，仅源码与轻量静态检查；源码`c001a386`通过[Actions 41404142751](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41404142751)：285顶层/3165含子用例PASS、0fail、1个非root foreign-owner权限skip、13package，160项原差分、vet/race/build/noNode全部通过。最终加严缺失sync回填fixture源码`81620db9`亦通过[Actions 41405028511](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41405028511)，相同285/3165/160用例全过。首次typed nil test cleaner与driver object/array binding差异已修，无原expected篡改。实际CI记录见GO-CI-ACCEPTANCE.md。正式容器与作者数据不变。
