# Go 审稿与修稿：来源快照、原子替换、作者合并

## 实现范围（隔离业务，非生产绑定）

`internal/store/reviewrevision.go` 提供审稿create/list/latest/full/next-index、修稿create/replace-pending/list/pending/full/next-index/merge/mark-merged/mark-discarded。`transport.BindReviewRevisions` 对应14个公开RPC；`projectsession.BeginReviewRevisions` 仅借用准入后的私有scratchcopy并持有完整会话lease。main仍未绑定，不能持久保存作者作品。

这不是AI审稿/修稿工作流的完成：模型调度、generation root/run、review cycle的作者确认/生成绑定/复查、billing/reservation、恢复与receipt等仍未移植。

## 来源与事务

- Create必须提交id、chapterNumber、version、status、**完整content**的冻结来源。原JOIN读取与来源比较在同一`BEGIN IMMEDIATE`连接内执行。缺失、null、部分或不匹配来源返回原`SourceDraftChangedError: SOURCE_DRAFT_CHANGED`，controller附`errorCode`；不fetch最新来源替调用方重基线。
- 与原逻辑相同，创建审稿/探索修稿并不要求working状态，只要完整来源匹配；真正合并才要求draft/revised/reviewed。不能据“创建成功”允许更改finalized正文。
- 每次创建在事务内`MAX(index)+1`分配，不信调用方index，内容池与元数据一起提交。next-index只是预览，不具备序号分配权。
- replace-pending分配新内容/元数据和弃用旧pending是同一事务。任一步失败不得留下新内容或半弃用；不删除旧报告/分支正文。
- 取消也必须完成有界background rollback；回滚状态不确定丢弃连接，不把未完成手动事务交回pool。

## 合并与幂等

作者提交`revisionId,targetDraftId,expectedDraftContent,mergedContent,wordCount`。合并在单事务内按原错误顺序验证目标、修稿、同目标、pending、可编辑状态、目标全文CAS，以及修稿冻结的chapter/version/status/content。成功写content、draft状态/字数和revision关系，不修改AI候选来假装已合并。

同一revision只有在当前目标仍为同正文、revised状态、同字数和同合并关系时才返回`idempotent:true`，不重写；作者后来改过正文，旧请求重放必须`REVISION_ALREADY_MERGED`，不能恢复旧正文。两个不同分支从同一来源并发合并只能一个成功，另一个不得自动重试覆盖新结果。

## 显式安全差异与未支持边界

- 不跳过review-cycle效果：数据库存在cycle表不是一律禁用；先证明revision以及reviewSourceId的原review/confirmation角色均未绑定才允许写。任何参与绑定的合并、discard、replace均`REVIEW_CYCLE_OWNER_REQUIRED`，直接mark-merged的revision绑定保留`REVISION_MERGE_REQUIRED`。该规则比仅revision_id无绑定更保守，不宣称完整周期兼容。
- structured请求只要出现`reviewCycleId`/`generationRunHandle`/`authorRecovery`（包括null）就拒绝。repo/HTTP均failclosed，不把任意presence当普通作者提交。
- 历史schema没有content_id唯一约束，两个draft（甚至含finalized）、review/revision可能共享一个内容池ID。旧实现会就地改写共享body。Go合并及普通draft正文CAS在同一写事务内要求目标content独占，否则`CONTENT_OWNERSHIP_CONFLICT`；不克隆、改引用、修复或覆盖其他实体。这是显式安全收紧，不是原缺陷的字节兼容。只检查现有表，不做schema迁移；任何存在表的检查失败仍拒绝。
- SQL BLOB不强制转string：读取使用原Node Buffer JSON；source BLOB与字符串baseline不能相等。SQLite REAL来源快照读取保持原typeofnumber投影；新create来源章号/version要求正常正safeinteger，历史0/负数/REAL来源的写入尚未完整兼容，不冒充自动修复。
- 不透出原SQLite/path/prompt diagnostics；只有固定domain codes允许出站，其他为`REVIEW_REVISION_OPERATION_FAILED`。这与旧任意`String(error)`不同。
- direct mark-merged保留旧未绑定分支的metadata-only语义，不据此更新草稿正文或绕过cycle merge；最终工作流审定仍需确认该旧API是否应继续公开。

## 验证方法

`contract/reviewrevision-contract.mjs` 在GitHub运行原ReviewRepository、RevisionRepository及其真实review-cycle依赖，只替换`getProjectDb`以指向显式的合成私有连接。每case从原baseline-v7拷贝，记录原调用后结果、contents/drafts/reviews/revisions/cycle三表，再恢复完整调用前字节交给Go。只剔除created/updated时钟字段，其余nullable、BLOB、REAL、source、正文、状态和关联均比较。

全部162个原对照只覆盖未绑定cycle图；不伪造“unsupported也是原行为”。另用Go故障注入、独立pool并发与真实cookie/CSRF/tab/lease HTTP回归验证防覆盖和源fixture未改变。源码`a5e38a20`已在 [Actions 41391829983](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41391829983) 全部通过，227顶层/1996含子用例PASS、0 fail、1个writerlock foreign-owner权限skip、13package，含vet/race/build/noNode smoke。实际CI记录见`GO-CI-ACCEPTANCE.md`，只在GitHub执行编译/测试，不使用作者数据、真实凭据或生产服务。
