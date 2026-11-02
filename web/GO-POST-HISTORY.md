# Go 后处理跑批账本与 LLM 调用历史（非执行器）

## 实现范围

`store.PostHistory` 移植原PostProcessRepository/LLMHistoryRepository；`transport.BindPostHistory`提供六个post-process和三个LLM history RPC。只接入已准入的私有scratchcopy，每次操作保留cookie+tab+projectUUID/path+lease hold。main不绑定，不发布作者库，不调用模型或LanceDB。

“账本步骤ok”仅是原API状态，不是外部知识文档、人物同步、定稿发布或审稿副作用的执行凭证。实际后处理runner、跨存储幂等、finalization owner、recovery和generation accounting仍缺，不能将这九项宣布业务全完成。

## 后处理语义

- CreateRun使用Go crypto/rand v4 UUID，无外部ID覆盖；新run和全部steps在同一BEGIN IMMEDIATE内提交。重复step key与旧schema一样可创建；标记时若匹配不恰好一行，整次事务回滚，不会修改多个步骤。
- supplied finalizedSource 必须与同事务内最新finalized draft身份、chapter、outbox finalization ID/hash/snapshot、现正文和现正文SHA256全部相符。source type必须chapter_finalize，source ID必须原章号的JavaScript Number字符串（含历史REAL章号）；成功保存为`finalization:<id>`。已带finalization:前缀但无冻结来源拒绝，不允许凭字符串自称定稿来源。
- 标记成功/失败与all_critical_passed汇总同事务变化。成功清error、写completed/lastAttempt、attempt_count+1；失败清completed、保留error、更新lastAttempt和attempt。关键步骤从成功重跑失败时汇总必须回到false；非关键失败不拉低汇总。
- 新run默认false，空步骤不自动算成功；首次有效标记后按原SQL关键失败计数计算，不发明自动完成。
- latest按created_at DESC,rowid DESC；chapter_finalize digit-only来源沿原JOIN查当前outbox ID（不是无outbox的新draft替代既有outbox）。这是展示查询，不构成执行权；CreateRun来源校验独立而更严格。
- 不存在项目时原读取null/[]、mark no-op；不存在步骤的已打开项目mark失败。caller仍必须持有有效项目会话，nil默认不作为HTTP权限fallback。

## LLM 历史语义与安全

- LogCall保留nullable token，未知不是0；Stats以total_tokens非NULL计knownUsageCalls。全部未知SUM仍null，真实已知0仍0；独立prompt/completion SUM不强迫相互一致，不拿历史当billing authority。
- History保留原success 0/1、按id DESC、默认50及SQLite LIMIT 0/negative行为，finishReason只按原CASE映射。raw error_message永不返回，provider可能回显prompt/key，未知raw错误不向浏览器泄漏。
- 原API无新secret存储或模型调度，禁止用这些手工记录确认预算结算、generation成功或持久正文。
- SQL raw值不强制转string，Buffer wire保持Node JSON；legacy NULL的标签/错误/时间默认遵循原nullish规则，不用falsy吞掉合法值。
- 公开错误仅固定domain codes；日志写入失败保持原`{success:false}`无error字段，不输出rawSQL/path/upstream内容。未声明完整的任意损坏请求coercion兼容。

## 原实现对照

GitHub执行原TypeScript两个repository，仅替换getProjectDb指向合成临时库。132个实际case保存完整调用前DB字节，原调用后返回和三张表副作用给Go对照。随机UUID只在case捕获后把实际新建UUID替换为固定测试UUID；不patch原crypto，不让生产调用指定UUID。Go测试通过package-private newRunID实现同一对照ID。

created/updated时钟字段不比较；completed/lastAttempt非空字符串局部替换为`<timestamp>`，保留空/NULL区分，其他来源、正文、step、attempt和usage一律原样比较。临时原fixture源字节不可改变。最终源码`64d044fd`已通过 [Actions 41395057332](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41395057332)，244顶层/2177含子用例PASS，0fail、1个非root writerlock foreign-owner权限skip，13package；132项原差分、vet/race/build/noNode全部通过。测试均GitHub，CI故障修复过程与结果见GO-CI-ACCEPTANCE.md；不使用作者数据或真实凭据。
