# 第二十一轮：恢复候选正文的隔离仓储与认证HTTP

本轮实现db:recovery-candidate-record/list/update/resolve四项领域行为，仅在现有已准入私有SQLite scratch会话中使用，不绑定main，不公开project:open，不把恢复候选元数据当AI生成/正文合并/导入receipt权威。正式容器、作者原库、凭据不动。

## 内容与来源保护

记录使用crypto UUID，chapter/run/step/project身份校验，sourceDraft必须null或有效id/version。HTTP拒绝renderer projectId及未知字段，将被冻结上下文且provider核对过的projectId注入clone；输入对象不被修改。全调用保持同cookie session+tab+projectId+leaseId+path hold，foreign/revoked拒绝。

来源快照按原JSON.stringify固定8字段顺序：chapterNumber/title/role/purpose/keyEvents/characters/suspenseHook/userGuidance；使用既有有序JS serializer，正文不HTMLescape或标准化Unicode，SHA256按原UTF8字节计算。nullish可选字段→空字符串，characters仅接受string数组，任意scalar/对象解析差异明确拒绝，不用Go map排序哈希冒充原源指纹。

visibleOnly复现原<think>配对/未闭合/孤立close过滤与ECMAScript trim，visible正文非空，独立contentHash。失败码/理由按原160/2000 UTF16长度截断；恰好切开surrogate时整个请求INPUT_UNSUPPORTED，绝不以U+FFFD替换。UTF8/每字段32MiB等有界typed输入门；不声称原JS所有动态类型可接受值均兼容。

list按rowid稳定返回pending数组，并核对全部row source_snapshot/content两个哈希；任一损坏整次失败，不能半数组伪成功。sourceCurrent要求source_draft_identity_captured==1、当前蓝图8字段序列化hash相等、当前latest草稿id/version完全一致；源未捕获历史行false。**这是原草稿身份检查，不是全文CAS**：同id/version的body/status编辑不会变false；后续AI继续执行必须重新抓正文full-body/source-target proof，不能据此放行覆盖。无草稿的双方必须同时null，出现新草稿会失效。

编辑先pending+完整性+sourceCurrent，然后visibleOnly和新contentHash；来源改变拒绝。处置保留原不对称：continued只验证sourceCurrent（不额外toCandidate完整性）；discarded允许陈旧/损坏候选从pending退出。这是元数据标记，不生成或保存草稿正文。replacement只允许同run/project/chapter候选，不自动resolve旧候选、不冒充幂等记录receipt。

## 原子性与授权

所有read/check/write/readback在同一reserved connection BEGIN IMMEDIATE内完成，避免来源检查与写入中间被另一个SQLite writer插入。record全持久字段readback防trigger静默改写；update检查期望body/hash及source仍current，resolve核对状态/时刻。不在db.SetMaxOpenConns(1)时嵌套db查询死锁；取消/失败rollback，返回nil结果。原同方法非事务的trigger行为不作为兼容承诺，Go在这些异常情况下明确更严格。

portable freeze/authority项目仍由前置admission拒绝；本实现不推断旧row可豁免sourcehash，不重播历史生成权威。可信provider必须来自已准入仓储且借用全程hold，不能用renderer path新开DB。严格有限error allowlist，provider/SQLite原始异常不回显；record/update/resolve原success envelope、list原bare array保持。未知操作仍未实现，不批量注册空成功。

## 验证计划与范围

GitHub-only实际RecoveryCandidateRepository及真实递归BlueprintRepository/DraftRepository模块Oracle，唯一environment seam为copied DB/currentProjectPath=null（无portablemarker）。保存真实before SQLite v7文件，Go对同bytes执行，比较完整返回/原错误码/全部raw候选后状态；随机UUID只按rowid映射，created/resolved时钟字段排除，其余原sourceHash/contentHash保留。原始来源序列化由原函数真实生成，不在Oracle手写替代。

额外仓储测试覆盖Unicode/order/hash/think过滤、legacyflag、sourceDraft null/stale/newversion/bodyonly变化、同run/project/chapterreplacement、integrity全表失败、两个pool并发resolve、trigger失败/静默改写rollback、取消。认证HTTP测试覆盖冻结参数/未知字段/恶意projectId、cookie/CSRF/origin/tab、release-on-providererror、有限错误；实际Manager.OpenScratchCopy→四项HTTP→源蓝图变更拒绝继续→discard→revoke→原SQLite源bytes不变。

首轮资格源码`7e880e038c237dacd72d73e77c0971410b6ea424`，[41448240304](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41448240304) success：450top/4237all PASS、0fail、3foreign-owner测试skip、18packages，vet/race/凭据decrypt/build/noNode smoke通过，原仓储55case全部通过。随后审查发现并补强原始frozen context多余字段拒绝及typed-nil HTTPError脱敏防panic；最终`8d890fb6d1995a1f2810e932abce9fc7fc91b605`，[41449598057](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41449598057) success：450top/4237all PASS、0fail、3foreign-owner testskip、18packages，新增恢复15top/70all无fail/skip，55原差分全过。vet/race/凭据decrypt/build/noNode smoke通过。中间strict-context run被新push取消，不当通过计数。最终证据见GO-CI-ACCEPTANCE.md和evidence/go-recovery-candidates-round21.json；随后只吸收GitHub生成的本轮新文件format与文档证据，不宣称最终文档SHA另编译。只用合成fixture，不运行本地Go编译/test/race/gofmt、原Oracle或大型安装。native Lance无改动且不重验。

## 剩余门

未实现AI失败流自动保存候选、恢复run/attempt/generation root所有权、审稿循环继续/merge、正式正文CAS、idempotency receipts、durable作者会话和发布；不把这四项隔离业务计IMPLEMENTED。本轮只有实际GitHub通过后才将四row从NOT STARTED→IN PROGRESS；main/production不变。
