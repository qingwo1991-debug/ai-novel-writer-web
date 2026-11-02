# Go 跨进程单写者锁（待上层集成）

`internal/writerlock.Acquire(directory)` 使用 Linux `flock(LOCK_EX|LOCK_NB)`，不是仅内存Mutex。`Close` 幂等、并发安全，只解锁/关descriptor，不unlink lock文件，避免两个进程持有不同inode的伪互斥。

- directory必须管理员配置的既有canonical absolute物理目录，绝不接受RPC路径。保留全部祖先descriptor并在准入前后核对physicalanchors；final目录owned且不允许group/world写（推荐0700，安全0755亦接受），祖先须euid/root owned且不允许不受控写，允许root-owned sticky temp祖先。
- 固定 `.novel-writer.lock`，缺失才openat+O_EXCL建0600；现存必须empty regular、euid owned、nlink1、exact0600。真实syscall.Openat含NOFOLLOW/NONBLOCK/CLOEXEC；不读取凭据，不repair/chmod/truncate/unlink异常条目。
- 非Linux failclosed。sameprocess独立Acquire也互斥，Close后原inode可重用。进程退出/被kill由内核释放，CLOEXEC不把锁传给外部工具。
- Advisory/trusted-admin边界：不能对抗忽略flock的旧程序、恶意sameUID/admin、mount改变或Acquire返回后的namespace替换；合法empty0600同UID预创建与上次运行残留不可区分。

## 重要：未接到作者数据

第十四轮新增`AcquireLegacy(directory)`固定使用现entrypoint的`.native-web.gateway.lock`，兼容empty regular/euid/nlink1的0600或0644旧inode，新增仍0600；不修改/删除/重建现存entry。`Lock.Verify()`持mutex核对全部pinned parent、正确命名entry/inode与ctime/mtime/权限，closed或观察到替换拒绝；此检查不是连续监控，仍不防恶意sameUID/admin。

`internal/projectaccess.Preflight`在同一legacy锁内stage→私有SQLite验证→原源未变→marker/asset复核→清理，只返回无写能力报告；真实util-linux flock/Go测试子进程双向竞争验证的是同名持久inode，不是两个名字假互斥。详见`GO-AUTHOR-PREFLIGHT.md`。原Acquire的`.novel-writer.lock`仍完全独立，不能冒充gateway排他；desktop不合作须外部停止。

没有创建正式authorroot里的lock、不启动/停止任何现有容器。main未绑定预检或durable opener；上层仍需全局key/config generation、SQLite、LanceDB、library/recovery publication的共享scope与shutdown/recovery生命周期。

现在projectsession只管隔离scratchcopy，不能以本模块存在宣称已获准持久作者库打开。正式迁移前需要明确shutdown顺序、全局/项目锁顺序、at-most-onewriter、recoveryjournal、writer exclusion跨snapshot+verification持续持有的集成证据。

## 验证范围

合成tests覆盖goroutine/多个Go自执行child只有一赢家、release/reacquire、processdeath、CLOEXEC、文件/祖先/entry替换拒绝、symlink/hardlink/FIFO/权限/大小/owner拒绝和inode保全。[Actions 41385531191](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41385531191) 已通过模块tests和race；foreign-owner filesystem case因runner非root不能chown而明确skip，不能声称该条已实测。测试只在GitHub运行，完整记录见 `GO-CI-ACCEPTANCE.md`。本机只编辑源码，不读取正式lock或数据库。
