# 原生Lance兼容试验（隔离合成数据互操作已通过，非生产能力）

## 第十五轮执行状态

- 源码起点`d1ac97b5`，最终通过远程原生试验源码`bc14231b`；`backend-go/native-lance/`为精确0.22.3 Rust staticlib，Go/CGO进程内调用，只有`lance_native && cgo`显式构建标签可用，没有接入main/RPC/作者预检。
- [首次原生编译41418579889](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41418579889)：原Node0.22.3/Arrow18.1合成6表与stream IPC生成通过；Rust编到lance-encoding后缺`google/protobuf/empty.proto`失败（5m31s，time记录最大RSS1275704KiB，不代表完整build峰值）。已添加libprotobuf-dev并强制/usr/bin/protoc、PROTOC_INCLUDE=/usr/include，提交`bc14231b`，[锁定依赖重试41420077722](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41420077722) **全绿（约9m50s）**：Rust --locked build、Go vet、5项native测试0fail0skip、Go静态链接产物在PATH=/nonexistent执行、原Node完整schema/rows/reopen与源/副本bytes/membership/registry/copies保全全部通过。独立普通Go[41420077714](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41420077714)也success。
- GitHub生成591包Cargo.lock已下载审计并提交，SHA256`a6252b1526248b36ff4168826cf3d91b7b43dc54b69ac600854d8d5bac705365`；所有shared注册表包checksum与Lance/Arrow/DataFusion家族版本符合immutable上游，注意physical-expr-adapter原锁就是50.0.0。上游release workspace锁root仍是0.22.3-beta.5，实际发布crate0.22.3独立与crates.io checksum核对；仅新增published lancedb与shim两个name/version pair，不宣称root锁字节相同。metadata不等于最终链接SBOM；MPL-2.0 option-ext及其他notice仍需分发审查。
- [普通Go回归41418579981](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41418579981)已全绿：316顶层/3319含子测试PASS、0fail、3skip、14packages；format/vet/unit/race/凭据原版解密/build/无Node运行均通过。此任务不编译显式标签native包，不能替代原生验收。
- 原生workflow初次因job级env不能使用runner context而未入场；改运行步骤注入固定RUNNER_TEMP路径。原oracle随后两次真实发现createTable及createEmptyTable持久化vector child nullable=true，虽原传入Field为false；已按实际持久化schema建立明确预期，未放宽字段/行比较。
- 当前真实覆盖：documents、canonical chunks、active2D/inactive3D/building2D与空2D表，共6表/14行，Unicode/null/Float32，active表append成第二版本。没有历史override checkout、删除、FTS/旧layout或作者数据；不得把下文研究目标全算已实现。
- 两个互操作方向独立：原版IPC→native新Lance→原版重开；native读物理副本→IPC→JS Arrow校验。当前不是将native-export IPC再送native-import的组合自往返。
- ABI同步无handle、Rust Box bytes配套free、返回仅状态token，Go只保留数字status；catch_unwind不屏蔽默认panic hook的stderr，也不防abort/OOM/UB。私有合成目录独占条件是前提，IPC64MiB/表与128MiB总保留输入不代表解码后内存硬上限。
- 部署、作者卷、281操作矩阵均未推进：仍2 IMPLEMENTED/54 IN PROGRESS/225 NOT STARTED；原生试验基础设施不是业务操作实现。

### 实测成本与精确边界

- Rust dev/debug0/incremental=false/jobs2编译5m25.01s，time最大RSS1619064KiB；target3.0G，staticlib841M，strip Go可执行215M。这不是tiny binary，后续发行体积/优化待单独评估，未把编译挪回本机。
- 6表/14行三次native调用合计约0.10s、最大RSS109712KiB；仅微型fixture，不是作者规模性能承诺。ldd只列libm/libgcc_s/libc与动态loader（加linux-vdso），没有Node/Python运行库；真实PATH隔离执行通过。Rust静态库链接不等于整个ELF全静态。
- active源版本2导入后版本1，再由原Node重开确认；显式不保留原历史版本号。全部5项native单测通过，普通Go race通过但native标签包本轮没有race门。
- 长期小证据见[web/evidence/go-lance-native-round15.json](evidence/go-lance-native-round15.json)。完整短期artifact只含合成契约/metadata/日志/manifest，不含作者数据或大二进制。

### 下一步限定块（只读源码调查，不是已实现）

1. `projectfs.StageCanonicalKnowledge`固定资产有界递归复制与源/副本proof：lancedb/registry/knowledge-copies；原`vector-migration-snapshot.ts:physicalManifest`不含copies，不能原样照搬冒充完整。原readSnapshot拒绝vectors.json或migration-journal存在，历史vectors.json.migrated需opaque保留。
2. 在AcquireLegacy+外部旧desktop排除期间包住SQLite与知识资产stage、native同步scan、两个proof复核、cleanup；版本只从冻结副本取，不打开原库，不因context取消提前删scratch或解锁。逐表checkout不是跨表事务或句柄quiescence证据。
3. port原validateTables与registry/copy关系：uniqueIDs、文档chunkCount/序列、向量dimension/finite/docId/text、active full generation及building prefix。registry只读，不调用会推断写入的getEmbeddingSpaces。knowledge-copy引用是SHA256(docId).json；edited/indexDirty可合法不同于indexed chunks，分类不修复、不要求content==joined chunks。报告仅counts/versions/digests/status，不含正文。

以下版本研究与未来验收目标保留，不能误读为已经通过的证据。

第十四轮仅inventory固定入口metadata，**没有读取或验证Lance格式/表/正文**。下一轮必须先用原Node0.22.3产生的合成populated fixture证明原生兼容，再接任何作者项目。不能清空、重建或用Node/Python运行时桥接。

## 已核实版本关联

- [npm @lancedb/lancedb0.22.3元数据](https://registry.npmjs.org/@lancedb/lancedb/0.22.3) gitHead为`e1906a328d25c74fe182b5b13a112a77b992180a`，原[Node/Rust v0.22.3 release](https://github.com/lancedb/lancedb/releases/tag/v0.22.3)对应此commit。候选Rust依赖必须是**lancedb=0.22.3**，不是lance=0.22.3。
- 该release依赖Lance family0.39.0、Arrow56.2.0，工具链1.90.0；不能只相信较旧manifest MSRV字段。最终仍需GitHub锁定Cargo.lock并编译验证，不因源码版本同名而称格式已通过。
- 官方[Go SDK](https://github.com/lancedb/lancedb-go)确实存在，提供Rust native library+C header，不能声称“无官方Go binding”。但父代理逐一读取[main/rust/Cargo.toml](https://raw.githubusercontent.com/lancedb/lancedb-go/main/rust/Cargo.toml)，当前main依赖lancedb Git v0.24.0+Arrow56.2；读取[latest tag列表](https://api.github.com/repos/lancedb/lancedb-go/tags?per_page=10)及[v0.1.2@ba58a1cbd2d9c145c4711c236df3628eb3f9b157 Cargo.toml](https://raw.githubusercontent.com/lancedb/lancedb-go/ba58a1cbd2d9c145c4711c236df3628eb3f9b157/rust/Cargo.toml)，该tag依赖lancedb Git v0.22.1+Arrow55.1，并非当前源0.22.3精确组合。不能直接拉latest预编译库宣称无升级兼容。

## 原Arrow流格式陷阱（源码研究，未运行）

原`vector-migration-snapshot.ts`使用JS Arrow18 `tableToIPC(arrow)`，[其默认输出是STREAM](https://github.com/apache/arrow/blob/apache-arrow-18.1.0/js/src/ipc/serialization.ts)。[Rust0.22.3 ipc helper](https://github.com/lancedb/lancedb/blob/v0.22.3/rust/lancedb/src/ipc.rs)是FILE格式且空batches失败；官方Go当前[data.rs](https://github.com/lancedb/lancedb-go/blob/main/rust/src/data.rs)用FileReader、[query.rs](https://github.com/lancedb/lancedb-go/blob/main/rust/src/query.rs)用FileWriter且empty返回NULL/len0会丢schema。不能直接拿这些helper验证原STREAM；shim应明确StreamReader/StreamWriter，空表也输出schema。

原logicalHash用Arrow type.toString/field顺序+nullable/children、原String(id).localeCompare排序和JS JSON序列化。普通Rust/Go JSON与字节ID排序不自动等价，第一轮应让原TS verifier作为CI-only权威或完整语义Arrow交叉比较，不能自称精确logicalHash移植完成。连接API可能创建缺失路径，须先验证已有私有副本且缺库不connect；[原listing实现](https://github.com/lancedb/lancedb/blob/v0.22.3/rust/lancedb/src/database/listing.rs)无通用只读connect保证。

## 一个隔离轮次的验收目标

1. GitHub CI用固定原@lancedb/lancedb0.22.3+Arrow18（现JS业务依赖）生成documents、canonical chunks、active/inactive/building `chunks__space_N`、registry及knowledge-copies合成物理目录；至少多版本append/delete/checkout、Unicode/emoji/空串/null、Float32向量、FTA索引/旧layout样本。原oracle记录每表version/Arrow IPC/schema/排序logical行摘要与完整物理文件哈希；不使用作者正文/keys。
2. 评估官方Go绑定可否以**明确版本锁定并补全version checkout+raw Arrow IPC**达到要求；若接口/版本不匹配，先实现极小Rust staticlib/C ABI shim，精确lancedb0.22.3/Lance0.39与锁定依赖，由Go CGO直接调用，不经子进程、不需Node/Python运行时。shim只操作私有物理副本。
3. 最小接口为受限local path连接、列table/version、显式checkout(version)、schema/record batch输出ArrowIPC、关闭/释放buffer/符号错误。句柄opaque、caller不持Rust内存裸引用，buffer由Rust allocate/free成对，panic不越过FFI边界，长度上限/UTF8路径/NUL/无remoteURI、取消与scope生命周期明确。Arrow Rust56 IPC与JS18的可读性必须实际交叉验证，不能比crate major猜兼容。
4. 只读基线验证前后物理目录hash+版本/rows不变；connect/openTable可能有目录创建或本地状态副作用，必须用已有副本、read-only文件权限/不创建路径策略+原源beforeafter证明，不能把默认connect当只读原库opener。缺库/不兼容必须错误，不自动创建空库。
5. 第二阶段在**另一私有副本**用native append/delete/version变化再用原Node读回，证明canonical全文、registry/copied text一致，不只是向量近似。snapshot逻辑对照参考`electron/services/vector-migration-snapshot.ts`物理copy→version checkout→ArrowIPC/registry/hash流程。
6. GitHub专用工作流编译Rust/Go、运行nativeinterop、隔离无Node PATH验证Go消费；记录license/SBOM、工具链/Cargo.lock/native ABI、实际RSS/磁盘/构建耗时/二进制大小。Rust/DataFusion编译可能很重，不挪回本机、不扩散到现轻量Go CI每次都重编；不上传作者数据或巨型fixture，不接main，不操作生产。

## 构建/FFI的具体限制

- Rust1.90.0、direct Arrow array/schema/ipc=56.2.0与exact lancedb0.22.3；首次Cargo.lock仅在GitHub生成，下载小文本lock审计shared Lance/Arrow/DataFusion包版本与release lock，再提交并使用--locked；root shim新增意味着不能谎称lock与上游字节相同。
- catch_unwind覆盖所有export/free/close且panic=unwind，仍不防abort/OOM/UB；opaque handle+generation拒绝stale/doubleclose、连接有子表/在途操作时不提前destroy。Cgo取消不会自动中断native query，不得Go timeout返回后并发释放仍执行的native handle。无retained Go pointer/callback，Rustbuffer只由Rustfree，C.GoBytes的int长度需限制。
- 不预估未经测量的tiny binary。官方npm linux x64 native package整体unpacked约116.5MB；[官方Go多平台release bundle](https://api.github.com/repos/lancedb/lancedb-go/releases/latest)约474MB compressed，两者都不是shim单binary大小。先45–60min单Ubuntu任务、jobs2/debug0/incrementalfalse、boundedcache；不得--all-features，不保证default-featuresfalse完全移除transitive cloud代码。[上游Rust CI](https://github.com/lancedb/lancedb/blob/v0.22.3/.github/workflows/rust.yml)已有磁盘/large runner/swap成本提醒。
- 需要记录proto/clang/SSL依赖、SBOM、a/strippedGo大小/RSS/compile磁盘时间。跨平台CGO不只是GOOS：Windows MinGW Go与Rust MSVC CRT不匹配，macOS frameworks/签名、Linuxglibc/musl均独立后续资格门。

## 完成边界

原生reader/spike通过也不是完整KB实现：仍缺版本冻结、write journal、index/embedding空间切换、文本/SQL receipt/copies一致、错误/取消恢复、bounded大库扫描、search语义、生产包装及全量281浏览器验收。原作者Lance始终保留，portable archive投影不替代整卷rollback备份。
