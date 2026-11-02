# 原生 Go 普通生成执行生命周期（第二十五轮，合成普通 lane 资格通过）

本轮继续四个并行 agent：普通执行服务与真实 SQLite 生命周期测试、可信 usage 结算、实际 TypeScript 差分 Oracle、独立只读安全审查。父整合原生 OpenAI/Gemini provider 接口、真实 HTTP→账本测试、GitHub 工作流与证据。所有编译、测试、race、格式化、Oracle 执行仍只在 GitHub，本机仅改源码和轻量文本检查。

## 交付范围

`backend-go/internal/generation` 以捕获的内部 `Ledger.CallJSON` 为依赖，组合上一轮实际 generation root/run/attempt/artifact 账本：

- `SettleProviderUsage`：included / separately billed / unknown reasoning 策略；原版 JS truthiness、缺失与 null 区别、安全整数及一致的 reported-total 校验。无效证据保留预留量，实际超估不截断；不增加 token liability 刹车。
- `Service.Execute`：nonce/hash 幂等重放及同请求并发加入，reserve→dispatch 标记→普通流式正文→全文快照→settle；delta/cumulative 前缀、eventId 冲突及 split-think 拒绝。推理仅 display 回调，不写候选正文。
- 可见内容按 UTF-8 字节差达到 4096、默认一秒间隔及结束时持久化。完整正文与 revision 由真实 ledger 验证。UI 回调异常不能改变持久化记账。
- Pause/Cancel、取消响应、项目关闭前 flush→保守中断分类→捕获 receipt→abort、迟到事件围栏、存储失败后的完整可复制尾部和 root 内存失败围栏。显式持久丢弃候选成功后才清除内存尾部；失败的关闭可重试。
- `NativeProvider`：原生 OpenAI/Gemini streaming 适配，强制 VisibleOnly，避免重复追加累计 OnDone 内容。仅允许与内部 adapter 一致的已知协议 usage，OpenAI nullable pointer 数字解引用，Gemini 使用 raw candidates / thoughts，避免重复记 reasoning。错误只返回稳定分类，不复制 provider 文本或凭据。

## 不授予的权限与明确缺口

**不是公开 AI handler、当前 generation owner 或可部署的完整服务。** 没有连接 main、浏览器 RPC、公开 SSE 或正式作者数据。281 操作矩阵不因内部模块提升状态。

1. 调用者必须先在写者排除下完成原项目/portable sidecar 预检，保留捕获数据库的完整借用生命周期。此服务不自行选择目录、开作者原库、授予 durable session 或迁移 schema。
2. scratch cookie/tab lease 不是 generation epoch、来源 manifest currentness、模型租赁、source/target 防覆盖或恢复授权。可信 owner 尚待移植，不能将内部构造的 Request 或 trusted usage 暴露给浏览器。
3. `ProviderRequest` 只在内存传给可信适配器；不写入账本。provider key、URL、prompt 不能通过内部接口变成公开输入。传入 HTTP Dispatch 的可信 owner 仍必须每次检验 URL/origin、解析后地址、模型凭据和取消信号；不能使用允许任意目的地的默认 client 冒充安全 egress。
4. Agent 工具协议、规划/blueprint/作者 effects、portable history、composition、最终定稿/后处理未覆盖。artifact 是候选，不是作者正文保存或 commit receipt。
5. Resume 内部默认拒绝；外部 recovery validator 必须由未来可信 owner 提供。回调在锁外运行，重新加锁后再次检查 run。此轮不将 Resume 的源码或 native 测试当成实际原版完整恢复差分资格。
6. 取消不等待不合作的自定义 Dispatch 返回；迟到回调被围栏，不能写已关闭 DB。服务和 NativeProvider 在调用前检查取消，但**不能承诺任意恶意/不合作外部 Dispatch 绝不在取消后自行发送网络请求**。未来模型/egress owner 仍须在真实网络边界检查 authority/context。
7. service 可见正文/去重事件集合和自定义 provider goroutine 没有整体 RSS/并发配额证明；ledger JSON 字段边界不能冒充完整服务内存上限。超 Go Duration 范围的安全整数 deadline 按可表示最大时长封顶，不宣称超世纪计时的原版完全等价。
8. 本轮普通 Go 工作流不重编或重验原生 Lance，不覆盖作者库升级、durable lifecycle 或最终本地部署。

## 静态审查与远端验证

独立审查与父整合修复了：finalization 与 Cancel 的竞争、恢复校验回调持锁重入死锁、取消请求误将健康 root 永久标记存储失败、内存 unsaved tail 无持久确认出口、成功 provider 最后一次快照失败时的 finish/usage 语义、原生 OpenAI usage pointer/typed nil、Gemini reasoning 双算风险、终端 context-length 与网络错误分类，以及可见回调 pre/post-flush 顺序。

实际 Oracle 递归加载未改写的原 TS `generation-run-service`、repository 及 shared 模块，只替换 synthetic DB 准入、repository clock、randomUUID；provider 事件为受控合成输入。每场景使用原迁移 baseline-v7 的私有副本，不创建替代业务 schema，不 mock repository 业务。完整 receipt、get/readAttempt、snapshot、reasoning、4096 UTF-8 边界中间全表 SQL、最终全表 SQL、UUID、dispatch/abort 和输入不变均对照。仅生成 UUID 重标记，声明的 ledger JSON 列做结构比较；作者文本/hash/时钟不删除。null-policy 原 TypeError 单独列出，不冒充 Go 错误兼容。

最终资格源码 `d7780031e7e3c7b147a87c01bc107518f9365c14`，[41539542139](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41539542139) 完整 success：**524 顶层／5819 含子用例 PASS，0 失败，19 packages**；另有 3 个既有 foreign-owner 测试 skip 和 cmd/novel-server 无测试文件 package skip。新增 **14 顶层／551 含子用例全通过，无失败或 skip**。vet、race、原版凭据 decrypt、Go build、无 Node 烟测均通过。

实际运行生成 **444 usage 案例＋60 生命周期 trace、12 个递归原模块**。505 个 Oracle 顶层及嵌套用例＝1＋444＋60；独立 usage 3 顶层及 28 子用例共 31；服务 native 8 顶层及 3 子用例共 11；父原生 provider 2 顶层及 2 子用例共 4，总计新增 551。实际 null-policy TypeError 仅作独立不支持域证据，不计这些通过案例。

首轮 [41538480795](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41538480795) 在 Oracle loader 延迟加载 better-sqlite3 native addon 时失败，尚未执行 Go；修复是在注册严格原模块 hooks 前预热 SQLite addon，不扩大 hook 的业务依赖准入。第二轮 [41538840715](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41538840715) 原版 fixture、编译/vet 通过，444 usage 通过；59 生命周期 trace 到最后输入不变断言时因测试自身 UUID 非对称归一化失败，另 1 个 suspend trace 发现缺少 dirty-flush 通知。修复为原始 request JSON bytes 前后精确比较，以及在释放所有锁后发出捕获的 suspend 快照。最终第三轮全绿；前两轮不计通过。

本机只应用 GitHub 生成的本轮新文件格式化 patch，最终文档/格式化 SHA 不冒充另一个独立已编译源码。证据见 `evidence/go-generation-service-round25.json`。矩阵仍为 **2／69／210**，main/production/native Lance 不变。

## 最终交付门

用户最新明确要求：**全部完成，直到拉取完整 Go 镜像部署在本地并通过验收。** 这是总体终点，不是当前内部切片的完成声明。全量 281 操作、浏览器及作者数据安全验收前继续保留正式 Node 容器；通过后备份作者数据，拉取 GHCR 固定 digest 的完整 Go 镜像，在现有本地部署替换、检查关键业务与数据保全，保留原镜像回滚。不会部署本轮不完整 Go 或新 Node 修复镜像。
