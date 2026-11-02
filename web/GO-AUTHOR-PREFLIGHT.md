# 第十四轮：隔离作者准入预检（非durable open）

`backend-go/internal/projectaccess.Preflight`只返回完整性报告；不返回源文件描述符、repository、会话或原数据库写权限，不注册RPC，不接入main。当前Go仍拒绝正式dataRoot/非loopback绑定。281矩阵不因拒绝门或基础设施测试而升级。

## 第十六轮opt-in扩展

设置可信`Config.Knowledge`后，在同一legacy锁内增加知识资产完整物理复制、raw registry/copies只读结构与状态校验、源/副本完整manifest复核和descriptor安全清理；报告只增counts/digests/status。nil维持下文第十四轮固定入口metadata行为。详见[GO-KNOWLEDGE-COPY-PREFLIGHT.md](GO-KNOWLEDGE-COPY-PREFLIGHT.md)。原生隔离reader在第十五轮已通过实际合成互操作，但`Preflight`仍未调用native，不得把metadata校验当作Lance表/domain或durable open准入。

## 与现gateway真正互斥

旧`docker/web-entrypoint.sh`29–30使用`$NOVEL_DATA_ROOT/.native-web.gateway.lock`。新增`writerlock.AcquireLegacy`固定复用这个名字，不再把不同的`.novel-writer.lock`误当互斥证明。原Acquire仍保留旧名与严格0600行为，两个命名空间互不代替。

Legacy允许已存在的empty regular、euid所有、single-link inode，模式仅0600或0644；新增仅O_EXCL0600。现entrypoint umask077通常生成0600，0644只兼容历史默认umask。绝不chmod、chown、截断、删锁或重建已有文件；不采用PID“陈旧锁”猜测。flock busy立即返回符号错误。`Lock.Verify`串行于Close，核对所有物理父目录、原inode、固定name、mode/euid/nlink/size与ctime/mtime；锁被改名/替换后不能继续宣布排他。

**边界：**flock只约束合作进程。旧desktop不参加，维护窗口须先停；不防恶意管理员、同UID文件交换、mount攻击或不合作写者；Verify是观察边界，不是连续监控。此轮没有改变生产锁文件/服务，也没有将main接进这套guard。

## 预检顺序

1. 仅可信管理员配置：canonical DataRoot、其严格子目录LibraryRoot、直接子项目；私有0700/euid scratch必须在DataRoot之外，所有scratch祖先物理无symlink且安全所有权/无可写位（root sticky临时祖先例外）。禁止DataRoot为系统根。
2. AcquireLegacy并持锁，打开现有read-only projectfs能力；canonical manifest、.vela双根、未完成migration journal按既有规则拒绝。
3. `VerifyNoPortableAuthority`在锁内检查原实现四个固定位置：storage/runtime-freeze、storage/transfer-authority、项目restore-reservation、library下该目标hash对应restore-journal。**任何存在**（合法/畸形/过大/symlink/dir/hardlink/FIFO）一律PORTABLE_AUTHORITY_UNSUPPORTED，无解析、无marker正文读取。原TS常量与restore journal路径函数在GitHub合成oracle核对，含中文/emoji/case路径；不代表已移植portable重放权限。
4. 仅inventory三个固定canonical成员`lancedb`目录、`embedding-spaces.json`regular/singlelink文件、`knowledge-copies`目录。保存entry metadata并最终重查；不枚举/打开Lance，不读取正文/向量/key。
5. 在独占私有临时目录StageDatabase完整DB/WAL/SHM/journal→受信schema contract的VerifySnapshot（只SQLite打开副本）→source proof.VerifyUnchanged。持同一锁到marker/asset/lock复核和scratch清理完成。
6. 返回projectId、SQLite schema/counts/physical identity、三个asset entry存在与否。没有正文/路径/写能力，不能将报告当成未来持久写凭据。所有失败返回零报告。

## 清理与未证明部分

正常/取消/SQL拒绝：移除私有副本四个固定名字、关闭root、只删除identity匹配的空临时目录，再放锁。观察到scratch改名时，仍通过已pin root删副本，原path的陌生replacement不删除，返回CLEANUP_FAILED。初次MkdirTemp后立即Lstat失败可能留下无法安全归属的空目录，显式CLEANUP_FAILED，不盲目RemoveAll。

AssetInventory只是**固定入口metadata**，不是目录递归hash、Lance版本/Arrow/schema/表/行证明；后代文件原地改动可能不改变目录metadata。必须持续外部排除不合作写者，原生Lance格式和canonical全文兼容仍硬阻塞。缺失Lance三项不被补空重建。

还未实现populated旧schema迁移、avatar移动、import owner fencing、portable历史authority采用/冻结重放、global lifecycle、project:open/save、archive/restore、AI workflow和前端完整验收。本轮不部署、不升级操作完成数。

## GitHub-only合成验证

新增真实util-linux flock对Go测试子进程双向竞争、legacy0600/0644 inode/mtime/ctime/字节保全、错误type/mode/link拒绝、持锁元数据/父目录替换检测、32并发只有一个成功及Close/Verify并发。foreign-owner仅root可执行夹具在普通runner明确skip，其余权限测试不skip。

预检覆盖持锁期间第二个预检/gateway失败、全成功后源字节不变和scratch清空、取消/限额/错误schema/DB与manifest改动、lock替换、portable marker前后新增、双根/journal/symlink/scratch模式/祖先/所有权、改名清理不删replacement。原TS只是CI常量/纯路径函数oracle，非运行时桥接。本机只编辑与静态检查，Go编译/gofmt/tests/race均由Actions执行；最终run见GO-CI-ACCEPTANCE.md。
