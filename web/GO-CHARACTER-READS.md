# 原生 Go：角色卡、角色名册与公开身份只读切片

## 范围与边界（第23轮，多agent）

本轮目标是三个公开读取操作：`db:character-get-all`、`db:character-roster-read`、`character-identity:read`。公开身份读取是实际`CharacterProposalService.identitySnapshot()` DTO，不是仓储内部的原始SQL证据快照；公开结果没有`proposals`。身份操作原浏览器参数只有`[context]`，不能用多余path参数冒充原契约。

另提供内部原始identity证据快照与display-only currentDerivedFields用于差分和名册组合，不把内部函数算新公开操作。`db:character-roster-commit`仍拒绝：manual_edit需要未迁移的author scope，其余身份schema写需要ID写所有者；拒绝入口不计业务完成。本轮不做角色提案写/批准/撤销、不做AI来源提交、不做头像、不改main、不部署。

四个实施agent分别负责基础角色/派生字段、完整角色名册与hash、HTTP、原版Oracle；第五只读审查agent核对来源与接口；父代理整合实际私有会话、同一SQLite事务组合读取、GitHub工作流及最终证据。坚持文件归属，汇合前冻结。

## 契约与保全

- 原`CharacterRepository.getAll`原样保留角色CASE顺序与role alias（ASCII大小写而非Go的Unicode简单小写；实际MAİN/MİNOR保留JS unknown→supporting）、nullable/truthy列、chapter0动态状态、可选JSON对象provenance；M02只排除retired，内部identity证据仍包含retired。SQL BLOB按Node Buffer wire、整数按JS默认Number舍入，仅在声明支持的读取域内对齐。
- 名册读取只针对已有M02+名册元数据，不运行legacy ensure DDL或初始化。条目/关系排序、固定字段顺序、姓名版本meta哈希与公开稳定ID版本哈希分开；UTF16比较、嵌套provenance插入序、数字index枚举、duplicate key JSON.parse语义不能被Go map排序替代。语言切换保留已seal的历史语言投影，不能擅自重写作者兼容图谱。
- 公共identity DTO静态字段使用原String(row[key]??'')语义、retired布尔、revision/provenance、aliases与relationships，不泄露内部提案表。内部原始快照只用于合成差分。
- currentDerivedFields完整核对正文=outbox snapshot及SHA、当前finalized版本/id、精确summary source绑定、watermark范围及sourceOrder/epoch/strict keys；只授予有证据的动态字段显示标识。portable origin不在本轮能力范围，前置admission仍拒绝portable authority。没有修复、回放或产生权威。
- 名册和派生字段在同一个reserved BEGIN IMMEDIATE连接读取；派生投影失败按原controller返回空grant对象，原作者/历史角色仍可读，不把缺证据解释成current。
- 认证HTTP严格cookie/CSRF/tab和三字段冻结context；lease hold持续到读取及成功结果序列化结束。projectID从验证前捕获的冻结context而不是任意request payload获得。拒绝、错误、撤销都必须release，公开错误有限allowlist并保护typed nil。

## 明确不兼容输入域

非法UTF8、单字段超32MiB、合法JSON孤立surrogate、非有限浮点overflow或超64层JSON明确拒绝；普通坏JSON的原解析fallback与公开identity必须抛错的路径分别保留。原始Buffer wire读取可以保留任意字节，但公开identity静态字段转String时非法UTF8 BLOB明确unsupported，避免Go与Node多字节替换分组差异。名册哈希不能正确表示的BLOB/static unsafe scalar明确unsupported，不以不同hash冒充原版。原JS role alias继承Object.prototype的对象/函数键明确unsupported，而非将其悄悄变成supporting。单字段/深度限制不等于全项目行数、全结果大小或进程RSS硬限额。

## 验证状态

首轮源码`c383420b623e8ce77371e788386a76b4b17159dc`，[41463551803](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41463551803)生成31实际递归模块、140原版case（25All/29Roster/9内部raw identity/12公开identity/65derived）；另2个Go-only缺meta边界独立统计，全部142差分/边界子用例通过。整体run失败于3个合成测试断言：父会话bootstrap把空args编码成null，及rosterFixture基线没有project_core.main时UPDATE静默no-op导致两个投影断言失败。只修正测试夹具（空数组和INSERT OR IGNORE main），业务实现不变，随后重跑；失败run不计整体通过。

最终资格源码`058f98059c943f688bb77bf267dcc0d19bd66044`，[41464730633](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41464730633) success：**495top/4576all PASS、0fail、3既有foreign-owner testskip、18packages**（另cmd/novel-server无testfiles package skip）；本轮新增**26top/179all PASS、0fail/skip**。140实际原版差分+2独立native缺meta边界均通过，31递归原模块。vet/race/原凭据decrypt/build/PATH无Node烟测通过。只提升三个业务读取row为IN PROGRESS，矩阵**2/69/210**；最终文档/runner格式化提交不当成另一已编译源码SHA。证据见`evidence/go-character-reads-round23.json`和`GO-CI-ACCEPTANCE.md`。本地禁止编译、Go测试、gofmt或Oracle执行；仅源码读写、node --check语法检查及远端文本证据。矩阵提升依据上面的实际最终通过，而非静态审查或注册入口。测试均为实际schema的合成SQLite，不读取或上传作者数据/凭据。最终证据会记录准确源码SHA、case计数、所有失败及修复；完整迁移目标仍未完成。
