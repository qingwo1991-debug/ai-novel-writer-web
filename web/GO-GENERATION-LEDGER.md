# 原生Go生成账本基础（第二十四轮，合成普通lane资格已通过）

这一轮四个并行agent：实际GenerationRunRepository普通执行lane、shared generation-contract、实际递归TypeScript差分Oracle、独立只读安全审查；父整合私有会话、工作流与资格证据。明确文件归属，父协调有序JSON输入接口，不把Go map排序后的JSON冒充原版JSON.stringify顺序。

## 范围与不授予的权限

`GenerationLedger`只借用已准入的项目SQLite连接，不做DDL、迁移、选目录、全局数据库重定向或作者原库写入。已实现普通生成root/run/attempt/artifact事务：根幂等key、原预算/物理预留、调用nonce重放、dispatch标记、完整可见正文SHA256+revision/prefix CAS、可信调用者settle、保守中断恢复与候选丢弃。实际差分资格覆盖open/get/budget/findInvocation/reserve/markDispatched/snapshot/settle/block/pause/receipt/recoverInterrupted/discardCandidate及五个shared契约；attemptCount/hasInFlight由native集成覆盖。resume/replaceUnstartedBinding虽有内部源码，但本轮未有实际原版差分资格，不列作已验证工作流；listCandidates/composition/规划等缺失。

- **不是公开generation handler，不提升281矩阵。** 不连接provider、模型凭据、Agent工具、流式owner、浏览器或定稿。
- scratch lease只保护数据库借用期，不是当前generation epoch/source manifest/模型租赁的授权。绑定和trusted usage必须由未来可信generation owner/service审查；repo内部比较run/root/artifact epoch不等于当前owner epoch检查。
- 可见artifact只是执行候选，不是作者正文save、审稿修稿commit、定稿receipt或portable authority。
- 不支持planning/blueprint工作流、作者效果、portable冻结历史；普通lane不能从同root的另一个run绕过保护。当前合成普通库恢复在发现任何unsupported root时**整事务拒绝、无修改**，不宣称实现原版混合库逐root skip；该差异独立披露，不计原版兼容。
- `OpenScratchCopy`的SQLite schema准入本身不读取portable sidecar。调用者仍必须先在写者排除下执行`projectaccess`预检、拒绝portable authority；`WithFreezeGuard`只是可信附加围栏，不是sidecar解析或自动来源准入。没有公开handler，因此不能由浏览器将裸DB绕过此先决条件。
- 预算按原ADR0023：tokens仅记账，不以累计token liability刹车；实际超预留量不得截断。物理请求数与活跃时间限制仍执行。不同sibling run可以共享root并发，same-run新nonce不能与live reservation重叠。
- `CallJSON`保留fingerprint/budget键顺序，以忠实原版重放比较；不能从无序Go map恢复的顺序显式拒绝，不暗中排序、伪造稳定成功。

## 审查重点

原版`open`根key排除epoch，nonce相同但source/fingerprint/context/budget不同必须冲突，sourceRefs忽略epoch但数组顺序不忽略。`reserve`精确重放在fresh token/blocked/inflight检查之前；不同hash冲突。snapshot先验证完整旧artifact与reservation identity，再epoch，再terminal之前的精确幂等重放，最后revision/prefix/think检查。settle只能由可信内部usage归属方调用，浏览器不能提交trusted=true。

现有项目lease/source-copy机制不自动满足generation owner。`BeginGenerationLedger`只用于合成私有副本集成，不能当全量迁移完成。原版日期、provider预算规划、recovery validateRecovery、composition/规划/目录恢复、最终副作用仍待后续移植。

## 验证状态

最终资格源码`2d4f8a9e440732e827a61854749786b97575da0b`，首次[41471547497](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41471547497)完整通过：**510top/5268all PASS、0fail、3既有foreign-owner testskip、18packages**（另cmd/novel-server无testfiles package skip），新增**15top/692all PASS、0fail/skip**。vet/race/原版凭据decrypt/build/无Node烟测通过。

实际递归原版Oracle加载10模块，**225案例=143pure+82ledger、417操作步骤**；另3个明确独立的Go-only native-shape边界，不冒充原版TypeError兼容。225fixture+3boundary+417step=645契约子用例，外加契约顶层1；4native账本测试+9pure顶层及32子用例（pure合计41）+父实际私有会话测试1=新增692all。完整结果/错误/每步骤全部SQL表后状态/输入不变/UUID调用次数均对照；仅生成UUID改为稳定标签，声明的generation JSON列做结构比较，不去除author timestamp/hash/text。字节不变只在原版实际不改bytes的步骤要求，不把正常事务的物理布局当逻辑差分。

四个native账本测试额外证明trigger中途失败后reservation/artifact无部分写、fullbody hash/terminal replay、budget键顺序冲突、root/run/attempt freeze拒绝、raw null effects拒绝、lone-surrogate/cyclic输入拒绝和损坏复合identity不panic。父`OpenScratchCopy`→借用→reserve/dispatch/Unicode snapshot→refresh→手动中断分类→revoke→源合成库bytes不变；不是完整作者服务重启验收。

审查具体修复：Go复合interface直接比较panic→scalar安全比较；多SELECT读取→同连接read transaction；cancel清理独立background rollback；普通map递归先拒绝cycles；binding负零按JS stringify归零；snapshot精确重放→terminal→CAS的错误顺序；替换next预验证及resume原始extra字段保留。后两项仍不当成本轮原版差分已验收。

本机不执行编译/测试/gofmt/Oracle，也不下载二进制。只应用GitHub产生的本轮新文件格式化patch；最终文档SHA不冒充另一已编译源码。证据见`evidence/go-generation-ledger-round24.json`。矩阵维持**2/69/210**，main/production/native Lance均未变化。
