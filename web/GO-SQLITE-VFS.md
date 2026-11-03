# Confined SQLite source writer: required VFS work

The Go migration does **not** yet have authority to write an author's source
`project.db`. `projectfs.SQLiteWriteCandidate` pins the existing database inode
under the legacy gateway lock, and `AssessSQLiteConnector` inventories existing
sidecars. `OpenConfinedSQLite` deliberately returns
`SOURCE_SQLITE_VFS_UNCONFINED`. No route should turn a private-copy transaction
into a successful author save.

## Why the bundled connector is insufficient

The pinned `github.com/mattn/go-sqlite3` v1.14.32 driver opens a pathname with
`sqlite3_open_v2(..., SQLITE_OPEN_READWRITE|SQLITE_OPEN_CREATE, ...)`. Its `vfs=`
DSN option selects a registered VFS, but `ConnectHook` runs **after** this open.
`mode=rw` can prevent creation of a missing main database; it does not attach
the already-pinned descriptor. The bundled Unix VFS uses `O_NOFOLLOW` for its
final `open()` component and for WAL shared memory, but it still resolves the
parent path and opens `project.db-wal`, `project.db-shm`, journals and temporary
files itself. A pre-open check or a VFS wrapper that checks a path and then
delegates `xOpen` to the Unix VFS has a check/open race on the parent directory.
An after-open inode check can detect a mistake only after an unintended file
may have been created or modified.

SQLite's [open API](https://www.sqlite.org/c3ref/open.html) documents the VFS
selection and `mode=rw` semantics. Its [VFS interface](https://www.sqlite.org/c3ref/vfs.html)
defines `xOpen`, `xDelete`, `xAccess` and `xFullPathname`; the
[file methods](https://www.sqlite.org/c3ref/io_methods.html) also include
locking, sync and WAL shared-memory operations. Existing WAL databases need
those [shared-memory methods](https://www.sqlite.org/wal.html).

## Required implementation boundary

One viable path is a Linux-only VFS owned by a single write session. It must:

1. Register a private VFS instance before opening SQLite. The VFS must retain
   duplicated descriptors for the pinned `.ai-novel` directory and a separate
   private scratch directory. The caller supplies only a server-generated
   instance identifier; no browser path or arbitrary SQL URI can select a
   source root. Unregister after every connection closes.
2. Implement `xOpen`, `xDelete`, `xAccess` and `xFullPathname` without reopening
   an absolute author path. Classify the exact main database, its allowed WAL,
   SHM and rollback-journal names, and SQLite temporary files. Open existing
   source entries through `openat`/`openat2` beneath the pinned directory with
   `O_NOFOLLOW|O_CLOEXEC`; verify type, owner, link count, permissions and
   expected inode. Create only an allowed sidecar with exclusive creation and
   owner-only mode. Confine temporary files to the private scratch directory.
   Reject unexpected suffixes, `ATTACH` targets, alternate VFS names and path
   escape requests. Delete with `unlinkat` against the same pinned root and
   sync that directory when SQLite requests it.
3. Provide complete `sqlite3_io_methods` semantics for the pinned descriptors:
   short-read zero fill; write/truncate/sync ordering; cross-process database
   locks; correct file controls; and WAL `xShmMap`, `xShmLock`, `xShmBarrier`,
   `xShmUnmap`. File controls and mmap must not escape to a pathname-based
   implementation. If the VFS cannot preserve the old SQLite locking protocol,
   it must refuse WAL rather than switch journal mode on author data.
4. Hold and repeatedly verify the exact legacy gateway lock and project
   identity from before SQLite open through commit, readback and close. Stop
   on external replacement or unknown commit outcome. A VFS only confines
   file access; it does not provide a recoverable author publication protocol.

The alternative is to transact on a private copy and publish an entire SQLite
file set using an atomic, crash-recoverable directory exchange. That approach
must prove preservation of every other `.ai-novel` entry, existing WAL replay
semantics, identity, fsync order and restart recovery. Merely renaming
`project.db` while old WAL/SHM entries remain is unsafe.

## Qualification before source writes

- Verify a custom VFS against the exact pinned SQLite amalgamation and Go
  driver version in GitHub Actions, including ordinary and WAL-mode author
  fixtures, rollback journals, hot-journal recovery, checkpoints and reopened
  connections. Keep `mode=rw` and assert `sqlite3_db_readonly() == 0` before a
  transaction.
- Fault-inject every source open/create/delete/sync, WAL map and lock, commit,
  readback and close step. On a crash or uncertain result, retain a durable
  PREPARED intent and classify the source without reporting save success.
- Exercise symlink and hardlink sidecars, ancestor replacement, a swapped main
  database, disallowed `ATTACH`, hostile suffixes and concurrent processes.
  Confirm no unintended path is created or modified and the previous project
  remains recoverable.
- Join the VFS with a **restorable** backup of the full DB/WAL/journal set, the
  in-transaction draft CAS and finalization receipt fence, post-commit source
  proof, and exact session/lease ownership. The current sealed snapshot and
  publication journal are evidence and recovery classification only; they do
  not restore an author project or resolve an unknown commit.

Until these gates pass, `OpenConfinedSQLite` must continue to deny source
connections and `project:save` / durable draft-save routes must remain unbound.
