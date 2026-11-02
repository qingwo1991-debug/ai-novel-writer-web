# 第二十轮：原生Embedding双协议与UTF16分块

本轮补齐知识导入/重建/检索未来需要的纯Go embedding基础；不是KB写入或工作流完成，不提升任何operation状态、不绑定main、不部署。源业务来自`electron/embedding.ts`和`src/shared/embedding-options.ts`，使用原函数GitHub-only Oracle差分；本机不执行Go/Rust编译、test、gofmt、原Oracle或大型安装。

## 原生请求和校验

`internal/embedding.Client`只持有显式配置的`*egress.Client`，请求为Go net/http，不接Node/Python、provider SDK或ambient HTTP/proxy。实际dispatch经既有exact HTTP(S)-origin白名单，重定向在边界拒绝，credential-bearing URL/query/fragment/control字符配置不接受。明确配置的LAN/loopback服务允许；这不是DNS/IP全局防重绑定系统。API key仅进协议header，不进URL、错误、日志或输出。

OpenAI保留原knownroots→/v1/embeddings、custom路径append/embeddings、已有endpoint不重复；原11434/api原生Ollama地址明确EMBEDDING_OLLAMA_NATIVE_URL，不擅自改路由。缺省text-embedding-3-small，Bearer header，input字符串数组。响应按整数index覆盖/去重/重排，不能凭返回顺序配正文。

Gemini保留仅去掉baseUrl**一个**末尾斜杠的原语义，/v1beta/models/{model}:batchEmbedContents、默认gemini-embedding-001，x-goog-api-key，requests内models/{model}及RETRIEVAL_DOCUMENT。按服务端embeddings顺序取values。两个协议都要求数量完全一致、非空数组、每个值为有限数、维数一致；Generate最后跨批次维数也一致。零向量值合法，不把全0替换为假成功或拒绝。

Generate空数组返回[]且无网络；OpenAI默认50/批、Gemini默认100/批，显式配置clamp1..50（typed整数入口，不声称JS任意coercion的options层已迁移）。每批前后assertActive，取消/authority撤销/后批失败均nil结果，绝不返回前批partial。callback仅内部可信执行租约检查，不直接接renderer。原生每请求60秒上限+调用者context，HTTP状态/非法JSON/网络异常只固定符号，不回显provider response正文/credential/URL。callback自身error保留供可信上游识别，未来public handler仍须有限allowlist，不能直接String(error)。

## 显式边界与预算

请求最多10000条、每条1MiB、总input32MiB、编码request64MiB。响应16MiB、JSON深度64、单vector最多65536维、总数最多1000000数值；Generate总结果数值预算同样检查。这是有界资格，不代表整个作者库一次嵌入无限可用。UTF8不合法、query/fragment/userinfo等原JS可能接受的输入明确拒绝，不静默规范化。未移植release-smoke token/vector伪provider。

## 分块保真

`ChunkText`内部按UTF16 codeunit计算长度与slice，不用Go rune数冒充JS string.length；ECMAScript whitespace/trim、段落\n\s*\n、句末标点后的\s*、强切/重叠均与原逻辑核对。保留原overlap=0时slice(-0)等于完整字符串的真实行为；空/纯空白返回[""]，不擅改为[]。API显式size1..4000、0<=overlap<size；正常应用options最低100的归一化层尚未接入。

JS slice可能把一个astral字符劈成unpaired surrogate。Go UTF8接口不能无损表示该返回值：本实现内部允许临时UTF16片段，但任何最终chunk包含孤立surrogate时**整次nil+EMBEDDING_CHUNK_UTF16_BOUNDARY**，不以U+FFFD/改变边界冒充兼容。原Oracle明确标识这些拒绝差异；不会因astral出现本身拒绝未被劈开的合法文本。下一步正式导入策略必须给作者可理解错误或显式版本化安全切分，不能默默改变已有canonical chunks。

input/output各32MiB、最多10000chunks、循环检查context，所有错误nil结果。空白表、BOM/非JS空白NEL、CRLF、组合字符、astral边界、标点、超长单句和overlap0均纳入差分。

## 验证状态

GitHub [41443533098](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41443533098)，实际资格源码`7a2e50c6ca72c1bfb29b6ae4bd8d06f3e641026d` **success：435顶层/4167含子用例PASS、0fail、3foreign-owner测试skip、18packages**；另有cmd/novel-server无testfiles package skip不混计。新embedding包**21顶层/428含子用例全部通过，0fail/skip**，vet/race/原凭据解密/build/noNode smoke通过。仅普通Go任务；本轮未改native Lance，也未执行native Lance重验。

原TS实际函数38响应校验+16endpoint/body+12批处理+240分块=306case全部验证通过。其中明确lonesurrogate输出作为Go failclosed差异，不算任意输入无差异。原12batchcases另经实际httptest+nativeegress执行；真实HTTP测试覆盖header/body、badindex/JSON/status/维数、出站拒绝/重定向、ctx取消/authority撤销/后批失败零partial。资格与精确SHA见`web/evidence/go-embedding-round20.json`，不把源码存在/编译骨架当通过。最终证据提交只吸收远端生成format及文档，不声称该新SHA另编译。

## 仍缺的业务

没有KB import/reindex/search/activate embedding generation、canonical表写事务、copy CAS和source-target generation guard；没有public llm:test-connection/model policies、持久化全局配置/凭据owner或AI执行租约。这里只提供可由未来可信工作流调用的原生模块。native Lance代码本轮未改，不把普通Go tests算Lance重验。当前operation仍2 IMPLEMENTED /58 IN PROGRESS /221 NOT STARTED，正式容器和作者volume不变。
