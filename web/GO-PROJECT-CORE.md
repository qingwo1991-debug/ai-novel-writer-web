# Go 项目主台账与 synopsis CAS（隔离业务验证）

`store.ProjectCore` 移植原 `ProjectCoreRepository.get/update/commitSynopsis`，不是占位success。`transport.BindProjectCore` 支持对应三个RPC，但 **main尚未绑定，只有已准入的私有scratchcopy HTTP集成**。作者目录不会被打开为可写SQLite，也不把scratch修改发布回去。

## 业务语义

- 23个原camelCase字段，无id/timestamps/plot_tree_snapshot额外暴露；writingLanguage/creativeStrategy/dormant-threshold按原helper规范化。其余SQL NULL/raw值保留，HTTP BLOB用Node Buffer JSON而不是Go默认base64。
- Partial update固定原字段allowlist，unknown keys忽略。`charactersArch`只要key存在（包括null）即拒绝整个更新，不能通过普通配置写绕过人物投影owner。
- 普通synopsis提交用**单条原SQL UPDATE**比较11个来源/目标字段，包括旧synopsis、premise、charactersArch、worldbuilding等；不预读baseline、不按timestamp、不fetch最新再重试。writingLanguage比较保持原CASE逻辑。任一变化返回`PROJECT_CORE_SYNOPSIS_CHANGED`。
- NULL来源仍按原SQL `=` 得到冲突；numeric baseline保留legacy负数/REAL/JSONnumber而非误当编号分配要求safe positive int。缺失baseline、非法shape必须拒绝。
- `generationRunHandle`/`authorRecovery` key存在（含null）均拒绝；generated commit操作完全未绑定。AI source authority、receipt、recovery和idempotency尚未移植，不能把普通CAS当作其替身。
- provider返回repository时必须验证cookie+tab+projectUUID/path/lease并hold至调用结束；跨tab/stale/release-inflight拒绝。legacy路径参数只做一致性检查，从不作为打开权限。

## 原实现对照与资源限制

CI执行原TypeScript repository，getProjectDb stub禁止implicitDB，每次显式传递从同一原schema复制的合成数据库。比较全返回shape、原始行变化（只排除时钟updated_at）、所有11字段CASdrift、threshold取值、read-only/unknown/no-op行为。Go另覆盖独立连接池并发CAS、SQLabort原子性、源fixture字节不变。所有编译/测试均GitHub，[Actions 41388270772](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41388270772) 源码6d5f2f71的30项原repository差分、并发/故障与真实authenticated HTTP/CAS通过；全套207顶层/1764含子用例PASS、0 fail、1个非root writerlock权限case skip。详见GO-CI-ACCEPTANCE.md。

controller error不透出SQLite SQL/path/raw值，update保持原business envelope，读取/提交走nested error。与旧String(err)原样泄漏不同的固定symbolic diagnostics须最终兼容审定。

当前仍无持久作品open/create/save、全局单写者协调、recoverypublication或library状态；只将这三项计IN PROGRESS，不计完成。不能用此模块覆盖人物、AI或LanceDB内容。正常原Get如果没有main row返回null，Update不隐式init/schema迁移。
