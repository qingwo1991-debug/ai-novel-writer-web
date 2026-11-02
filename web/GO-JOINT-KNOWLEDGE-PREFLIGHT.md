# 第十八轮：联合隔离预检与document-copy关系

本轮为第十四至十七轮只读基础设施组合，不是281操作完成、作者写能力或正式切流。源码`edf80fb2c4e344c86c40939fce25e4d663a68a9a`，普通Go[41434132059](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41434132059)、native联合[41434132123](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41434132123)均为**success**。普通378顶层/3617含子用例PASS、0fail、3foreign-owner skip；native facade8tests和联合preflight5顶层/19含子用例均无fail/skip。源码之后仅吸收远端format与文档，不另冒充新SHA编译。编译、格式化、测试、原Oracle执行只在GitHub；fixture使用公开合成内容，不读取正式作品、密钥或volume。

## 知识副本引用分类

`knowledgecopy.ValidateLinks`从已验证的documents.id/filePath计算管理关系。原`isDocumentCopyIndexCurrent`只看filePath是否以`knowledge-copy:`开头，**不要求marker后缀等于docId**；文件名始终是SHA256(UTF8(docId))+`.json`。本轮保留此行为，后缀为空/不同不会把它指向别的文件。

管理引用返回Present/Missing/Current/Dirty/Edited/HashMismatch计数；Current仍为`!indexDirty && SHA256(content)==indexedHash`，edited独立。Missing非fatal且Current为0，和原current predicate一致，不伪造已indexed就绪。非managed普通路径不要求copy；没有文档管理引用的copy计为Unreferenced并保留，不自动清除。所有存在文件先经过整个copiedmetadata验证器，未知有效字段和原bytehash保留。

这是一项**引用与状态分类**，不强迫dirty/edited正文与indexed canonical chunks相等，也不授予使用过期索引的许可。后续search/embedding/workflow必须分别根据Current判定可用性，不能把预检成功等同索引新鲜。

预算限制documents与copy数量、UTF8 docID/path大小及aggregateJSON；连续source/copy物理proof与writer排除仍必要。不从文件名逆推出docId，不把paths/prose/ID放进报告或错误。原TS纯函数CI oracle新增9case：UnicodeID、suffixignored、emptysuffix、missing、dirty、mismatch、unmanagedno-file/extra-copy和marker-not-prefix。

## 同一次持锁联合预检

可信管理员显式`Config.Knowledge.Native=true`才启用；普通非native编译提前拒绝配置，不尝试fallback Node，不读写DB、不创建lock/scratch。默认nil或Native=false保持既有metadata-only预检，不升级业务状态。

native编译下：

1. 同一legacy gateway flock持续持有，校验portable authority拒绝门及原项目physical身份。
2. 将SQLite DB/WAL/SHM/journal完整stage为私有副本，知识固定资产完整stage并metadata验证。
3. 仅在已stage副本上native checkout/query，输出到同一私有scratch内独立空目录；有界decode typed snapshot→Go canonical表/domain验证→document/copy关系分类。
4. native输出在调用完成后固定白名单/descriptor限定清理（含失败的partial文件）；未知/替换对象保留且报告失败。不在native仍工作时抢先取消释放资源。
5. SQLite仅打开stage副本进行schema/domain验证；最后复核原SQLite/知识/portable/legacylock及所有proof，清理知识/SQLite scratch，再解锁。任何失败包括cleanup均零Report。

不存在Lance根时不connect、不创建空库；原registry存在且声称space却无tables必须被领域门拒绝。报告仅增anonymous domain/counts/copy状态，不返回私有正文、原FD、repo或未来可写令牌。

native输出清理的provenance边界仍是独占、静止、attempt-owned私有目录；不声称抵御恶意同UID写入白名单文件。flock只约束合作进程，旧desktop仍需外部停机。版本钉选仍逐表，不是跨SQLite/Lance事务。

## 验证设计和未完成门

GitHub nativejob额外通过原TS migration registry创建真实合成SQLite v7，和原Node Lance0.22.3合成6表一起喂入Preflight；联合证据来自`native-preflight-tests.jsonl`，不使用opaque SQLite假占位冒充联合通过。单独`lance-interop` driver的SQLite仍是opaque占位，仅为物理stage/typed/domain/copylinks证据，不另冒充联合预检。验证成功源bytes不变/scratch空/锁释放；测试拒绝native域漂移、SQLite坏数据、late cancellation/late source mutation，以及输出未知对象/rename安全清理。普通job验证无native拒绝门与原9case links oracle。

机器可读证据见[evidence/go-joint-knowledge-round18.json](evidence/go-joint-knowledge-round18.json)。原JS 11项互操作/保全检查全真；driver输出copylinks为3managed/3present/3current/0missing/0unreferenced。PATH=/nonexistent driver微型fixture0.23s/RSS111816KiB，不是联合preflight或作者库性能。所有源码与负路径测试在GitHub首跑通过，本机只做node --check/git diff检查并下载文本证据。

仍不含Go精确JS logicalHash、作者任意大库、populated M03/M04、portable历史authority、durable open/save、generationfinalization/AI业务、KB search/embedding写入或浏览器全验收。当前2完成/54进行中/225未开始；生产不变。
