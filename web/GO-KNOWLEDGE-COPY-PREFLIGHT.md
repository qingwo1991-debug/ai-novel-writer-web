# 知识资产私有复制与只读元数据预检

第十六轮隔离实现。**不是项目打开、可写Lance库、完整知识库或281操作迁移完成。** 未接main，不读取正式作者数据。编译、测试和依赖安装只在GitHub，本文中的源码范围不自动视为验收通过。

## 固定物理资产

`Project.StageCanonicalKnowledge(ctx, target, KnowledgeBudget)`只读取已通过物理项目身份检查的`.ai-novel`固定入口：

- `lancedb/`，完整递归原字节，不解析Lance、不重建或丢弃旧版本/索引。
- `embedding-spaces.json`，原始字节。
- `knowledge-copies/`，完整递归原始字节（物理层不把它当可重建缓存）。
- `vectors.json.migrated`，历史opaque receipt原样保留，不采信或重放。

所有入口可缺失；缺Lance时保持缺失，不调用connect创建。`vectors.json`或`vectors.json.migration-journal.json`任何类型存在均拒绝，不解析、不自动转换。原TypeScript vector migration physicalManifest不包含knowledge-copies，因此本层有意比原固定列表更完整，不冒充原hash格式一致。

目标是整个源library外的已存在空0700/euid私有目录。沿物理祖先逐层open，目标Root.Name必须与实际物理打开对象SameFile，不单独信任字符串。源和目标禁止symlink/hardlink/specialfile；文件O_NOFOLLOW|O_NONBLOCK，子目录逐层descriptor固定。新目录0700、新文件0600且exclusive，不覆盖现存对象。

## 预算、证明和清理

- 全量bytes/entries/depth正预算，hard ceiling分别1TiB/10000/64；128KiB复用读buffer，目录ReadDir最多128一批，预算外的sentinel不追加到集合。
- 三次源扫描：参考hash、边复制边hash、最后重新hash；目标再扫。检查全部固定入口缺失状态、目录成员、每文件SHA256、identity/mode/size/mtime/ctime。返回digest涵盖内容与成员；不是原JS logicalHash，不是数据库事务一致性证明。
- `VerifyUnchanged`复核源完整manifest+项目身份；`VerifyCopy`复核目标完整manifest+原物理anchors。原始prose不会进入报告或错误，digest/counts不等于语义正文准入。
- `Cleanup`反向只删本次创建且仍same-inode/single-link的对象，不用RemoveAll；不删替换inode或陌生成员。目标被改名后仍通过pinned descriptor清自建对象。全部保留描述符在成功/失败均关闭；重复Cleanup返回首次结果，失败后不保留FD重试能力。
- 为避免unlinked inode号复用误删，创建文件FD固定至Cleanup；最多可能保留接近entries预算的FD，管理员必须配置适合平台ulimit的预算。资源不足失败/清理，不承诺只占一个FD或跨任意大目录。
- 调用者必须从stage前到源/副本复核和cleanup结束持续排除writer/同用户filesystem mutation；metadata采样不能抵御恶意swap-away-and-back，也不是一致SQLite backup。

## 副本元数据验证

`knowledgecopy.Validate`只接收可信私有副本Root，固定registry/copies入口。总原始JSON预算<=64MiB，copies数量<=100000，32KiB分块读、目录128批读；parser深度128、context节点检查。不是解码后进程RSS硬限制。

原registry局部语义保留version1、generation>=0、dimension>0、非空fingerprint、createdAt字符串（不假称V8 Date.parse）、activeGeneration缺失/null或引用已有generation；status计数active/inactive/building，局部validator不把status与pointer强绑。预检域有意更严：safeinteger、唯一generation/table、chunks/chunks__space_N、l2/cosine/dot。未知合法字段原样保留。未移植完整validateTables的active完整代/文档关系，不推断missingregistry或调用会写入的getEmbeddingSpaces。

copy名称为64位lowerhex`.json`，正文必须content/indexedHash字符串和edited/indexDirty布尔。indexedHash即使空或畸形仍按原逻辑分类hashMismatch而非伪造成功：`Current = !indexDirty && SHA256(UTF8(content)) == indexedHash`，edited独立，Dirty和HashMismatch可重叠。edited/dirty副本可合法不同于indexed chunks，绝不自动normalize或修复。尚未从documents.filePath验证docId对应SHA256文件名，计数只证明已出现副本格式。

额外拒绝duplicateJSON keys、无效UTF8、孤立UTF16surrogate和非单个JSON值，避免Go替换字符导致不同正文hash；这是显式安全收紧，不声称原JSON.parse任意输入兼容。raw registry SHA256与有长度边界的排序副本raw聚合digest保留空白/未知字段差异。报告不暴露路径/ID/prose/fingerprint。

## 隔离预检组合

`projectaccess.Config.Knowledge`显式opt-in；nil保持第十四轮metadata inventory接口。启用后：AcquireLegacy→portable拒绝→固定asset inventory→私有完整SQLite stage→知识资产stage+metadata→SQLite scratch准入→源/副本完整proof复核→knowledge/SQLite安全清理→释放锁。任何错误包括cleanup均零报告。

这不是native semantic扫描已绑定作者预检；`Preflight`仍不调用native库。另一个GitHub-only驱动以原Node合成6表为源，执行Go物理stage→metadata校验→native scan→原JS schema/rows核对→proof与cleanup验证，以证明复制后native读取的组合，不是author可写业务。

## 当前验收状态

源码`2b1f5dbd`：普通Go[41424195056](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41424195056) **success：350顶层/3444含子用例PASS、0fail、3skip、15package**；format/vet/unit/race/原凭据解密/build/noNode全过。projectfs合计46顶层/278含子用例、knowledgecopy18顶层/65含子用例、projectaccess14顶层/47含子用例PASS（均包含旧测试），另scratch-owner顶层与两个writerlock foreign-owner跳过。原copy reader/current函数7项CI对照真实执行。native组合[41424195098](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41424195098) **success**：6表/14行全部schema/rows由JS复核、源/copy字节不变、Go知识stage完整49entry/29322bytes、metadata4spaces/3currentcopies、原始registrydigest一致、native读后源/副本proof通过、已复制资产全部清理。PATH=/nonexistent真实执行0.17s、RSS96764KiB（微型合成fixture，不外推）。所有第十五轮测试门仍通过。

独立只读审查发现Low级mixed cancellation+Cleanup raw PathError可能泄漏copied文件名；`04330543`把context错误规范成sentinel并只保留符号cleanup标记，新增回归。不改原业务、源字节或通过域。最终源码普通Go[41425792641](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41425792641) **success：351顶层/3445含子用例PASS、0fail、3skip、15packages**；native[41425792583](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41425792583) **success：5native tests、0fail/skip，全部8项互操作/不变式检查通过**。后续提交只吸收GitHub生成format与文档，不冒称另编译新SHA。

仍2完成/54进行中/225未开始；生产镜像和作者卷不变。后续还需canonical表schema/domain、document/chunk序列、embedding active完整性及copy引用关系、JS精确logicalHash、portable权限与populated迁移、实际durable lifecycle及完整浏览器验收。
