# Go 数据准入：物理安全、只读快照与真实旧 schema 对照

状态：当前只读实现已由 [GitHub Actions 41376594444](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41376594444) 验证，81 顶层 / 237 含子用例、0 fail、0 skip，vet/race/build/smoke 通过。不是生产可用或 281 项业务完成声明；剩余兼容门见下文。

## 本阶段边界

- `backend-go/internal/projectfs`：管理员配置的作品库根，以 `os.Root` 描述符逐级限定；作品仅接受规范绝对路径下的直接子目录。拒绝越界、符号链接、硬链接、特殊文件、旧 `.vela`、异常迁移 journal、项目身份更换。项目 manifest 保持六个字段，拒绝重复 JSON 键。
- `backend-go/internal/admission`：只对调用者拥有的私有副本使用 SQLite；检查当前版本 7、确切 DDL 指纹、完整性、外键以及 M02 角色身份域不变量。版本号正确但 DDL 被改、空库伪造版本号，均不能准入。不执行修复、建表或迁移。
- `backend-go/internal/projectdata`：组合以上两层；复制 DB、WAL、SHM、rollback journal 全套已存在物理文件到私有 0700 临时目录，文件 0600、独占创建；只连接副本，结束清理。验证前后再比较原物理文件集，不能丢失仅在 WAL 的正文。

这是**检查能力**，不授予原库可写访问，不注册 `project:open` 或 `db:*` HTTP handler，也不允许当前二进制访问正式挂载。作品打开还需要租约、全局单写者排除、版本升级、LanceDB/全局配置协调和安全失败回滚。

## 必须由上层实现的前置条件

调用者必须在整个快照、验证和最终源身份复查期间排除所有源写入者/检查点进程。采样的三遍 SHA256、inode/链接数、size/mtime/ctime、文件集比较可检测漂移，**不能替代写者排除，也不是对并发多文件 SQLite 的原子备份证明**。不可信本地进程拥有目录写权限时，还必须在部署/文件所有权层排除它；Go 路径约束不是对恶意管理员的隔离。

SQLite 即使只读连接也可能写 SHM 读者标记，因此绝不将原库文件名交给 SQLite。私有副本使用 `mode=ro`，不使用会忽略 WAL 的 `immutable=1`，不改变 journal mode，不 checkpoint，不恢复原 journal。需恢复或迁移的库明确失败，不伪造成空项目。

## 独立兼容性证据来源

GitHub runner 单独安装固定 `better-sqlite3@13.0.3`，通过 CI 专用 TypeScript loader 直接运行仓库中原 `desktop-registry.ts` 和 M00–M06。由原实现生成：

- 基准版当前 v7 合成库；
- 保留历史 avatar 列的 donor 当前 v7 合成库；
- 基准版 v6 合成库，Go 必须明确要求迁移，不能打开后偷偷盖章升版本；
- 原实现产生的版本/确切 DDL 指纹目录。

Go 的指纹 JSON 顺序和转义与原 `JSON.stringify` 对照，而不是用 Go 自己产生的指纹充当兼容性证明。CI loader/Node/SQLite 原实现仅是测试 oracle，**不进入 Go 应用运行时**。Go 单元测试另覆盖异常输入和域不变量；组合测试在临时合成库写入未 checkpoint WAL 行后，检查副本可见该行、源 DB/WAL/SHM 字节不变且临时副本被清理。

本机不运行任何编译、测试、构建或大型安装；CI 不读取作者目录、生产卷、模型凭据或真实 key。跨语言文件仅在 runner 的临时目录生成，不下载这些数据库到本机。

## v6→v7 私有副本升级（窄范围实现）

`admission.UpgradeSnapshotV6` 已移植原 M06 的确切 `review_cycle_merges` DDL，仅支持 `review_cycles`/`review_findings` 均为空的 lane。非空数据需要完整 M03 历史证明与 merged body 回填，目前明确拒绝，不能伪造完成。

要求来源受信的私有复制库、0700 parent、0600 单链接 DB、非 WAL header、无 sidecars。transaction 内检查 trusted exact v6 fingerprint、integrity/FK/M02，再执行 DDL 和 `user_version=7`；原表行级内容证明不变且目标指纹确切匹配才提交。目标指纹失败同时回滚 DDL/版本。提交后最终 I/O/取消仍可能报错，因此任意失败都应丢弃私有副本，禁止拿原作者库重试。

[GitHub Actions 41381280660](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41381280660) 已验证：oracle 从**相同已关闭 v6 源字节**分别交给原 Node M06 和 Go，比较 baseline/donor 两种 schema 与完整逻辑 proof；不是另造一份近似数据，也不把物理文件 hash 相同当作要求。尚未连接作品打开或迁移发布；历史备份、恢复 journal 与全链路切换仍缺失。

## v4→v5 / v5→v6 窄范围升级

`UpgradeSnapshotV4`、`UpgradeSnapshotV5` 已按原 M04/M05 确切 DDL 实现并通过 [Actions 41383135731](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41383135731) 的 baseline/donor 原 Node 对照。两者均要求审稿周期/发现项、导入 run/receipt 为空（v5 source 的 effect ledger 也须空），使尚未移植的复杂历史 verifier 可明确拒绝而非假装通过。逻辑原表内容、source/target fingerprints、FK/integrity/M02 在事务内检查；目标失败回滚 DDL/版本。M05 这里只装 schema，不迁移头像文件或建立 binding。返回 intermediate stage evidence，不能令 v5/v6 绕过当前 v7 admission。

## 尚未达到的兼容范围

- v0–v3 升级及历史发行版规范化尚未移植；v4/v5/v6 仅以上空历史限定私有副本 lane，不能视为旧库全兼容。常规 VerifySnapshot 仍只读核验 v7。
- manifest 日期先覆盖 ISO/RFC3339 等常见形式；暂时拒绝 JS `Date.parse` 宽松接受的本地格式、日期溢出、扩展年份、24:00；重复键也比原解析严格。正式兼容前须消除/审定这些差异，不能静默重写 manifest。
- 新 `StageDatabase` 流式复制使用固定 128 KiB buffer，调用者指定总物理字节预算，不受旧 `SnapshotDatabase` 的 64 MiB ceiling 限制；`projectdata.Inspect` 已改用新 API。旧内存接口暂保留兼容测试，不用于当前检查流程。新 API 已通过 [GitHub 41378784128](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41378784128) 的 65 MiB 文件、取消/漂移/回滚回归，不代表正式大作品端到端已验收。
- Linux descriptor 安全实现；其他平台失败关闭。目标容器为 Linux，不代表所有桌面平台均兼容。
- 物理 DB/WAL SHA256 不等于现 Node 行级 `authorContentHash`。新增 opt-in `VerifySnapshotWithDomain` 移植行级证明：SQLite 整数 decimal wrapper、BLOB Buffer JSON、JS UTF-16 行排序、非空表计数及逐表哈希；GitHub 用原 `probeProjectSqlite` 输出对照。已实现 chunk/8路归并私有外部排序，独立 Node 对照的 >64 MiB 总编码行量测试通过；单行仍有保守64MiB upper-bound、merge内存峰值和磁盘配额/失败残留清理限制，见 [GO-CONTENT-PROOF.md](GO-CONTENT-PROOF.md)，不声称任意大库无资源限制；跨版本保全、幂等 receipt、导出权威尚待移植。常规 schema 检查不触发全文排序。
- LanceDB 文档/分块正文仍未迁移或验证，保留原文件，不重建成空索引。
