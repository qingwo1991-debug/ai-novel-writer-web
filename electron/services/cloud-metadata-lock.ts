import fs from 'node:fs'
import path from 'node:path'
import { randomUUID } from 'node:crypto'
import { performance } from 'node:perf_hooks'

import { assertGlobalPathAccess } from './app-data-locator'

const WAIT_MS = 20
const STALE_AFTER_MS = 1_000
const DEFAULT_TIMEOUT_MS = 5_000
const heldLocks = new Set<string>()
const waitSignal = new Int32Array(new SharedArrayBuffer(Int32Array.BYTES_PER_ELEMENT))

interface LinuxIdentity {
  boot: string
  namespace: string
  start: string
}
interface Owner {
  version: 1
  pid: number
  token: string
  linux: LinuxIdentity | null
}

function errno(error: unknown): string | undefined {
  return (error as NodeJS.ErrnoException).code
}

function processStart(pid: number): string {
  // comm may contain spaces and parentheses. Fields after its final ')' start
  // at field 3; Linux /proc stat field 22 is the process start time in ticks.
  const stat = fs.readFileSync(`/proc/${pid}/stat`, 'utf8')
  const start = stat.slice(stat.lastIndexOf(')') + 2).split(/\s+/u)[19]
  if (!start || !/^\d+$/u.test(start)) throw new Error('PROCESS_IDENTITY_UNAVAILABLE')
  return start
}

function linuxIdentity(): LinuxIdentity | null {
  if (process.platform !== 'linux') return null
  try {
    return {
      boot: fs.readFileSync('/proc/sys/kernel/random/boot_id', 'utf8').trim(),
      namespace: fs.readlinkSync('/proc/self/ns/pid'),
      start: processStart(process.pid),
    }
  } catch {
    // A restricted /proc disables automatic recovery, never exclusivity.
    return null
  }
}

function isDeadOwner(owner: Owner, self: LinuxIdentity | null): boolean {
  if (!self || !owner.linux || owner.linux.boot !== self.boot || owner.linux.namespace !== self.namespace) return false
  try {
    // A reused PID is not the recorded owner, even if the new process is alive.
    return processStart(owner.pid) !== owner.linux.start
  } catch (error) {
    if (errno(error) !== 'ENOENT') return false
    try { process.kill(owner.pid, 0) } catch (error) { return errno(error) === 'ESRCH' }
    return false
  }
}

function recoverDeadOwner(lockPath: string, self: LinuxIdentity | null): void {
  if (!self) return
  try {
    const directory = fs.lstatSync(lockPath)
    if (!directory.isDirectory() || directory.isSymbolicLink()) throw new Error('CLOUD_METADATA_LOCK_INVALID')
    const entries = fs.readdirSync(lockPath)
    // Empty/partial, unknown, foreign-namespace and malformed locks are never
    // age-stolen. An operator can remove them only with all writers stopped.
    if (entries.length !== 1 || !/^owner-[0-9a-f-]{36}\.json$/u.test(entries[0]!)) return
    const ownerPath = path.join(lockPath, entries[0]!)
    const info = fs.lstatSync(ownerPath)
    if (!info.isFile() || info.isSymbolicLink() || info.size > 4_096 || Date.now() - info.mtimeMs < STALE_AFTER_MS) return
    const owner = JSON.parse(fs.readFileSync(ownerPath, 'utf8')) as Owner
    if (owner.version !== 1 || !Number.isSafeInteger(owner.pid) || owner.pid <= 0
      || entries[0] !== `owner-${owner.token}.json`
      || !owner.linux || typeof owner.linux.start !== 'string'
      || !/^\d+$/u.test(owner.linux.start) || !isDeadOwner(owner, self)) return
    // Delete only the unique, positively dead owner's marker. Two reapers
    // cannot remove a new holder: the loser must successfully unlink THIS
    // marker before attempting nonrecursive rmdir, and new markers differ.
    fs.unlinkSync(ownerPath)
    try { fs.rmdirSync(lockPath) } catch (error) {
      if (!['ENOENT', 'ENOTEMPTY', 'EEXIST'].includes(errno(error) ?? '')) throw error
    }
  } catch (error) {
    if (error instanceof Error && error.message === 'CLOUD_METADATA_LOCK_INVALID') throw error
    // Inspection can race with a holder releasing, or encounter damaged state.
    // Neither observation authorizes deletion; acquisition remains bounded.
  }
}

/**
 * Serialize a complete synchronous read/validate/CAS/atomic-write transaction
 * for ONE metadata document across global/project processes. This deliberately
 * is NOT a global-owner service or a transaction across credentials+bindings.
 * All writers must use this helper on the same local filesystem; do not use
 * separate PID namespaces against the same store. No network work/await inside.
 *
 * Atomic mkdir owns the lock; only the holder's unique marker is unlinked and
 * the directory is never recursively removed. Waiting is bounded (5 seconds).
 * Linux stale recovery requires >=1s age AND positive process death/PID reuse
 * in the same boot+PID namespace, never age alone. Unknown/partial locks,
 * restricted /proc and other platforms fail closed and need operator recovery
 * with every writer stopped. Metadata itself still uses fsync+atomic rename.
 */
export function withCloudMetadataLock<T>(
  filePath: string,
  transaction: () => T,
  options: { timeoutMs?: number } = {},
): T {
  assertGlobalPathAccess(filePath)
  const timeoutMs = options.timeoutMs ?? DEFAULT_TIMEOUT_MS
  if (!Number.isFinite(timeoutMs) || timeoutMs < 0 || timeoutMs > 60_000) throw new Error('CLOUD_METADATA_LOCK_OPTIONS_INVALID')
  const parent = path.dirname(path.resolve(filePath))
  fs.mkdirSync(parent, { recursive: true, mode: 0o700 })
  const lockPath = path.join(fs.realpathSync(parent), `.${path.basename(filePath)}.lock`)
  if (heldLocks.has(lockPath)) throw new Error('CLOUD_METADATA_LOCK_REENTRANT')
  const token = randomUUID()
  const ownerPath = path.join(lockPath, `owner-${token}.json`)
  const identity = linuxIdentity()
  const deadline = performance.now() + timeoutMs

  for (;;) {
    try {
      fs.mkdirSync(lockPath, { mode: 0o700 })
      break
    } catch (error) {
      if (errno(error) !== 'EEXIST') throw error
      const info = fs.lstatSync(lockPath, { throwIfNoEntry: false })
      if (info && (!info.isDirectory() || info.isSymbolicLink())) throw new Error('CLOUD_METADATA_LOCK_INVALID')
      recoverDeadOwner(lockPath, identity)
      if (performance.now() >= deadline) throw new Error('CLOUD_METADATA_LOCK_TIMEOUT')
      Atomics.wait(waitSignal, 0, 0, Math.min(WAIT_MS, Math.max(1, deadline - performance.now())))
    }
  }

  let ownerCreated = false
  try {
    const owner: Owner = { version: 1, pid: process.pid, token, linux: identity }
    const descriptor = fs.openSync(ownerPath, 'wx', 0o600)
    ownerCreated = true
    try { fs.writeFileSync(descriptor, JSON.stringify(owner), 'utf8') } finally { fs.closeSync(descriptor) }
    heldLocks.add(lockPath)
    return transaction()
  } finally {
    heldLocks.delete(lockPath)
    // Never remove another token or any directory contents recursively.
    try { if (ownerCreated) fs.unlinkSync(ownerPath) } finally { fs.rmdirSync(lockPath) }
  }
}
