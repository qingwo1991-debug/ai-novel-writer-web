# Go 原生 AI 传输迁移（未绑定业务）

本模块不是 Node worker 代理。Go 直接构造 OpenAI-compatible HTTP 请求、解析 SSE、分离可见正文与推理，并返回语义 finish evidence。**本页源码范围仍不等于 AI 写作/审稿/修稿/定稿端到端完成**，operation 矩阵不因 provider 模块存在而升格。

## 当前实现与准入边界

- `internal/llm/openai.go`：非流式/流式请求、可选 temperature（未定义不 fallback）、token cap field、已解析 reasoning directive 的序列化、NovelAI narrow payload、finish normalization、usage evidence、显式 DONE 必须出现、协议错误与 partial visible candidate。
- `internal/llm/visible.go`：原 VisibleStreamFilter 与 InBandReasoningStream 的增量 scanner，保留空白、nested tags、partial tags 不泄漏、UTF16拆分与大小写细节。协议 reasoning_content 只走临时reasoning回调，不混入 visible candidate。
- `[DONE]` 只证明 transport结束；`unknown`/`length`/`content_filter` 不是允许持久化正文的成功证据。creative owner 后续仍须检查 semantic stop、full-body source/target CAS、reservation/receipt/idempotency。provider 不持有数据库也不替 owner 保存。
- `internal/egress`：独立native HTTP client每次检查最终目标的精确管理员origin白名单，无默认网关/环境proxy、无任何redirect、保持context取消。它允许管理员显式配置LAN/loopback；DNS跟随已批准hostname，不声称全局禁止私网或抵御恶意管理员DNS。当前host语法保守（ASCII/punycode、canonicalIP），与旧WHATWG宽松hostname尚有兼容边界。
- Dispatch依赖注入；未来业务只能注入受控egress，不能接受请求指定transport。现在没有RPC binding，也不把已有credential/key读取接到provider。

## 原实现对照

`contract/openai-contract.mjs` 在 GitHub运行实际 `OpenAIProvider`，只有明确声明的synthetic fetch、已解析mapping和package version加载stub；不使用真实endpoint/secret。72个case（46 stream/26 generate）捕获原请求与orderedcallbacks，包括CRLF/单byte UTF8、多行event、usage、hidden reasoning、invalidprotocol、partialerror、finish和narrowpayload。

`contract/visible-contract.mjs` 直接运行原scanner，UTF16 unit数组避免JSON解析丢失未配对surrogate。细粒度split、nested/unmatched/Unicode、每次emission和临时reasoning回调均对照。

合成真实HTTP测试通过原生egress连接httptest server，覆盖增量stream、取消、未获准目标不dispatch。[Actions 41385531191](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41385531191) 在源码73244fa7上通过72个原provider +922个原scanner differential cases，以及vet/race/build/无Node smoke；全套183顶层/1512含子用例PASS、0 fail、1个writerlock权限case skip（非AI用例）。所有编译/测试只在GitHub，完整记录在 `GO-CI-ACCEPTANCE.md`。

## 未迁移/未验证

Gemini、模型配置/凭据绑定、真实provider presets与generation-parameter-policy、discovery/embeddings、精细transport diagnostics/timing、tokens/reservations/accounting owner、AI工作流、历史恢复、SSE到浏览器、正文source/target CAS、LanceDB、所有业务operation绑定仍待实现与契约验收。

本轮provider显式增加了8MiB SSE event、64MiB累计raw wire、累计输出及非流式响应预算（comment/metadata也计入raw wire，避免心跳逃逸预算）；这不是与无界原transport完全相同的资源策略。非流式异常映射固定symbolic code而非旧 String(error)，避免原URL/凭据泄漏，需在最终兼容验收审定。原HTTPFailure.body仍保留provider原始错误payload，仅供未来受控diagnostics owner处理；上层必须redact，禁止直接送浏览器或日志（上游可能回显prompt/key）。所有body/候选只在调用生命周期内存，不写作者库。不可把基于mock或合成HTTP的通过声称为真实外部服务已验证。
