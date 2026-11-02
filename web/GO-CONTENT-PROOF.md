# Go 行级内容证明与私有外部排序

`admission.VerifySnapshotWithDomain` 是原 Node migration verifier 的行级 `authorContentHash` 兼容实现，不是 DB/WAL 物理 hash。默认 schema 检查不扫描全文。

## 编码契约

- 用户表名称排序；逐行保持 SQLite column 顺序与 storage class。
- INTEGER 使用 decimal-string wrapper，BLOB 使用 Node Buffer JSON。
- TEXT 按 JS JSON.stringify 转义，行按 UTF-16 code units 排序。
- 仅非空表计数，逐表哈希 JSON `[table,encodedRows]`；重复行不去重。
- oracle 使用原 TypeScript 的实际 domain 输出，另用独立 Node serializer 覆盖特殊存储类型和大表，不向生产 recognized schema 添加测试 schema。

## 本轮外排实现

`rowsort.go` 每个 chunk 最多 4 MiB 编码行或4096行（单行超 chunk 时单独处理），私有0700目录/0600 runs，8路归并，分层合并避免把全部 run metadata 留在内存。每个 run 携带预期行数，完整记录边界截断也必须失败，不能静默漏行。

SQLite 在 Go driver fetch 前先保守检查每行 JSON upper bound，拒绝超64MiB行；编码过程中再次检查。总数据库/表不再受原累计64MiB上限。**这不是4MiB进程内存保证**：归并仍可持有8个最大行头，另有driver/编码/JSON转义临时副本；当前保守upper-bound可能拒绝实际编码能装入的特殊大行。spill磁盘消耗与待排序数据量成比例，没有无限资源保证。

取消在读取/归并循环检查，不保证在一次大行编码或排序比较期间即时中断。临时明文 runs 正常/失败/取消均尝试清理；清理失败会让证明失败而不是返回成功，但目前没有跨重启的 spill cleanup journal。部署前还需明确私有 scratch配额、失败残留清理和磁盘告警策略。不得把系统共享可读目录或请求参数作为临时权限配置。

## GitHub 验证

新增 >64MiB 编码行总量的合成SQLite表与独立 Node hash；Go外排结果必须一致。另覆盖多层merge、重复行、astral/私用字符排序、部分/边界截断、取消、visitor失败、私有权限与正常清理。

所有生成/运行只在GitHub；本机不下载数据库、不运行Go/Node oracle，不读取作者正文。[Actions 41383135731](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41383135731) 源码30373e51已通过：全套151顶层/465含子用例、0 fail、0 skip，vet/race/build/smoke均通过；完整记录在 `GO-CI-ACCEPTANCE.md`。
