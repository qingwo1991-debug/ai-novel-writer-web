# 后续durable准入排序（第十三轮只读研究，非已实现能力）

不能将以下方案当成已开放的project:open/save。正式服务/作者数据不动，下一轮先解除真实准入阻碍，不发布scratch回原文件。

## 1. 同一进程间锁

现docker/web-entrypoint.sh使用`$NOVEL_DATA_ROOT/.native-web.gateway.lock`持有util-linux flock；Go writerlock固定`.novel-writer.lock`，**两个文件互不排他**。旧desktop不加入flock。必须维护窗口排除非合作进程，再将Go与legacy gateway协调到同一持久inode、整个准入/写DB生命周期持锁；不得删除/重建锁inode“解锁”，不得把Go当前严格empty0600/euid/nlink规则未经旧文件兼容测试就称为互操作。内存cookie/tab lease是另层，不替代进程锁。

## 2. 私有快照不是durable发布

`projectfs.StageDatabase`复制完整DB/WAL/SHM/journal要求外部持续排他到snapshot verify+source unchanged proof完成；多次hash不是原子备份。`projectsession`现在只有scratch mounts，释放删除。不允许将scratch数据库rename覆盖仍被Node/desktop打开的原DB。

## 3. 必须保留的Lance正文

`electron/vector-store.ts`的chunks是canonical全文，不是可清空重建缓存。下一阶段必须用Go可调用原生Lance binding或Rust/C ABI，对原Node0.22.3生成的合成populated Lance证明documents/chunks、版本/Arrow schema、embedding-space注册与knowledge-copies。参考`electron/services/vector-migration-snapshot.ts`的物理副本→checkout版本→ArrowIPC/registry+logicalhash检验。不得运行时转给Node/Python冒充Go，也不得用空Lance上线。

## 4. schema之外的历史权限

Go尚未实现`portable-runtime-freeze`与`portable-transfer-authority`。原`electron/services/portable-runtime-freeze.ts`111–159将恢复历史draft/generation/finalization记录标成nonreplayable，要求mutable guard。未来准入先拒绝这些marker项目直到完整移植；仅schema fingerprint不代表可以重放旧authority。

还缺populated M03/M04/review/import migration、filesystem avatar迁移、import owner fencing、转移角色continuity adoption、重复finalization repair和全局generation生命周期。project:open不是简单返回ProjectCore.Get；project:save原语义是受会话保护的事务core更新，近期列表后处理失败可success+warning，不是dump scratch。

## 5. portable archive不是rollback backup

`.ainovel` ZIP/ZIP64 formatVersion1/sourceSchemaVersion7，manifest entry含path/size/SHA256/disposition与generation/counts/omissions/receiptIDs，边界100k项/2GiB单项/16GiB总/ratio200。

portable导出是策略投影SQLite+允许原始assets+portable-knowledge-source.json，不包含原Lance/registry；restore分配新projectID、恢复canonical text但embeddingstale、安装runtime freeze，manifest-last可恢复journal。它不是含credential key、DB sidecars和原Lance/assets的整卷离线rollback备份。

## 第十四轮已实现的隔离部分

以下建议现已形成`internal/projectaccess.Preflight`隔离模块：同gateway锁名、持锁元数据校验、原portable四marker拒绝、固定canonical入口metadata inventory、私有SQLite准入与源未变证明、取消与清理。**未接main、无durable写能力**；Lance子树/表/正文、populated迁移及完整portable authority依然未实现。最终CI见[GO-CI-ACCEPTANCE.md](GO-CI-ACCEPTANCE.md)，范围见[GO-AUTHOR-PREFLIGHT.md](GO-AUTHOR-PREFLIGHT.md)。

## 原建议：一个安全后续轮次

实现隔离AuthorAdmissionPreflight/WriterGuard capability：同legacy-gateway inode flock互斥、physical anchor证明、canonical根/资产清单、拒绝双根/未完成journal/portable markers，在持锁下StageDatabase→trusted VerifySnapshot→VerifyUnchanged，只返回准入证据，**不授予作者写能力**。

GitHub-only合成util-linux flock/Go子进程竞争、inode保全、marker拒绝、取消失败清理；main继续拒绝生产/dataRoot，不接作者真实目录，无operation因拒绝而升级IMPLEMENTED。随后做原生Lance格式兼容证明，再journaled writable opener与完整迁移/281浏览器/打包验收。
