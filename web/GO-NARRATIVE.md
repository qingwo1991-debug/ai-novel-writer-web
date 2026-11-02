# Go 叙事线索与一致性豁免（隔离验证）

## 范围

移植原`NarrativeThreadRepository`与`ConsistencyExemptionRepository`，提供六项narrative RPC、三项consistency exemption RPC。正文不迁入新表，不读写Lance，不调用AI；保存作者计划/作者确认的证据，以及原豁免状态。

每次RPC依赖admitted private scratch project，cookie+tab+projectId/path+lease hold包围完整操作。main未绑定，不持久发布作者项目；9项只计IN PROGRESS，不能宣称整个一致性预检、剧情树、AI处理或审稿工作流完成。

## 叙事计划与状态

- title/type/authorIntent按JavaScript trim和UTF16长度限制120/60/1000校验；targetStart/End为safe positive整数、End≥Start。中文/emoji不是按UTF8字节计长；FEFF属于JS空白，NEL不是。
- create/update的写入和返回读取同事务；delete保留原外键cascade删除confirmation。缺计划update/delete原符号错误，不假装成功。
- List按planID排列；event仅保留现status=finalized且有outbox的draft，按chapter ASC、confirmation ID ASC；最后一条type定义状态，没有则planned。不是按最近写入时间排序，也不额外改成仅最新定稿版本。
- dormant=max(0,所有finalized稿件最大章号-最后event章号或targetStart)；resolved/abandoned为0，overdue仅非terminal且最大finalized章号>End。没有outbox的finalized仍参与最大章号，符合原实现。
- Relevant排除terminal，先判断包含区间；区间外仍可由trim后character在thread title/type/authorIntent/event证据原因的substring匹配，或长度≥2 UTF16的thread.title在当前title+newline+keyEvents中匹配。不是语义向量检索，不发明tokenization。

## 作者确认的来源与事务

- confirm输入type只允许planted/progressing/resolved/abandoned；trim evidence/reason、UTF16≤240/500，非空。
- 原校验优先级：输入→draft现finalized并有outbox→冻结content_snapshot去JS空白后包含evidence→plan存在→insert。Go把这些来源检查与insert放同一BEGIN IMMEDIATE，不让并发改状态跨检查提交。
- 证据绑定**不可变outbox快照**，不是当前contents正文，也不新增SHA256/currentbody与原接口不符的门。完整定稿生成/发布owner仍待迁移，不能把线索确认当定稿提交。
- confirmation UNIQUE(plan_id,draft_id,event_type,evidence)：原字面相同证据/类型重复失败；不同事件或字面空白不同仍按schema处理，不偷偷去重或重放成功。
- 返回保持原object spread附加字段及覆盖顺序；caller input.id可能覆盖响应的id，但不改变数据库自动分配主键，不把它当保存授权或幂等凭据。
- 原始BLOB不强转string冒充来源正文。Relevant仅在原JS join/substring展示匹配处按Buffer UTF8解码，输出原存储仍不变，不把展示转换借给来源校验。ConfirmEvent身份参数拒绝原better-sqlite3不接受的boolean/object/array，防止Go数据库驱动把true悄悄绑定成现实体1；原SQLite可绑定的字符串数字不因此强改整数。异常存储不造成panic；不声称支持任意损坏值的JavaScript运行时异常字句。

## 一致性豁免

stableFactKey/reason按JS trim非空、UTF16≤500。保存用原SQL upsert，以稳定key为唯一权威，更新reason并revoked=0；撤销设revoked=1，不删除key，撤销不存在key无错误。重新保存重新启用。list按原SQLite key排序，revoked仅数字1映射true，原BLOB/NULL存储读取不偷偷改值；schema CHECK/FK不绕开。

这些记录只是作者豁免输入，不表示Go一致性预检/模型检查执行完毕。HTTP豁免写入沿原success/error envelope；narrative原handler异常沿RPC error。未知SQL/path/provider错误只返回固定诊断，不回显私人内容。

## 验证方法

GitHub-only oracle运行真实原TS两个repository，只替换private DB getter。160个实际case每项恢复完整调用前合成DB字节，Go独立拷贝执行同调用；对照返回/error及plans/confirmations/exemptions三表完整副作用，仅排除created/updated时钟字段。Buffer按Node JSON，不把来源或证据规范化成expected。

Go合成测试额外覆盖事务故障、取消/并发、冻结快照、会话隔离与Unicode边界；一个额外损坏存储flag测试暂时关闭CHECK注入revoked=2仅用于验证严格数字1判断，明确不是schema准入有效数据或原oracle支持域；原fixture字节保持不变。本机无Go/gofmt/test/oracle执行或大型安装。源码`f49dd968`首次远端执行即通过 [Actions 41397459923](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41397459923)，258顶层/2353含子用例PASS、0fail、1个非root writerlock foreign-owner权限skip，13package；160项原差分、vet/race/build/noNode全部通过。实际CI结果见`GO-CI-ACCEPTANCE.md`。
