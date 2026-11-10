/**
 * 正文未保存修改的 localStorage 快照。
 *
 * 每次正文内容或保存基准变化时写入 `{ content, savedContent }`，按 `projectKey + filePath` 记录；
 * 刷新/重开这一稿时用它恢复或提示作者有未保存改动。格式沿用账本的「版本化 JSON + 命名空间键」风格。
 */

const STORAGE_KEY = 'ai-novel-writer-manuscript-snapshots'

export interface ManuscriptSnapshot {
  content: string
  savedContent: string
  savedAt: number
}

interface ManuscriptSnapshotLedger {
  version: 1
  entries: Record<string, ManuscriptSnapshot>
}

function storage(): Storage | null {
  return typeof window === 'undefined' ? null : window.localStorage
}

function emptyLedger(): ManuscriptSnapshotLedger {
  return { version: 1, entries: {} }
}

function readLedger(): ManuscriptSnapshotLedger {
  try {
    const raw = storage()?.getItem(STORAGE_KEY)
    if (!raw) return emptyLedger()
    const parsed = JSON.parse(raw) as { version?: unknown; entries?: unknown }
    if (parsed.version !== 1 || !parsed.entries || typeof parsed.entries !== 'object' || Array.isArray(parsed.entries)) {
      return emptyLedger()
    }
    return { version: 1, entries: parsed.entries as Record<string, ManuscriptSnapshot> }
  } catch {
    return emptyLedger()
  }
}

function writeLedger(ledger: ManuscriptSnapshotLedger): void {
  try {
    storage()?.setItem(STORAGE_KEY, JSON.stringify(ledger))
  } catch {
    // 存不下时只在本次会话里保留。
  }
}

export function manuscriptSnapshotKey(projectKey: string, filePath: string): string {
  return `${projectKey}\u0000${filePath}`
}

export function readManuscriptSnapshot(projectKey: string, filePath: string): ManuscriptSnapshot | null {
  const entry = readLedger().entries[manuscriptSnapshotKey(projectKey, filePath)]
  if (!entry || typeof entry.content !== 'string' || typeof entry.savedContent !== 'string') return null
  return {
    content: entry.content,
    savedContent: entry.savedContent,
    savedAt: typeof entry.savedAt === 'number' ? entry.savedAt : 0,
  }
}

export function writeManuscriptSnapshot(
  projectKey: string,
  filePath: string,
  snapshot: { content: string; savedContent: string },
): void {
  const ledger = readLedger()
  ledger.entries[manuscriptSnapshotKey(projectKey, filePath)] = {
    content: snapshot.content,
    savedContent: snapshot.savedContent,
    savedAt: Date.now(),
  }
  writeLedger(ledger)
}

export function clearManuscriptSnapshot(projectKey: string, filePath: string): void {
  const ledger = readLedger()
  delete ledger.entries[manuscriptSnapshotKey(projectKey, filePath)]
  if (Object.keys(ledger.entries).length === 0) {
    try { storage()?.removeItem(STORAGE_KEY) } catch { /* 清理失败不影响编辑。 */ }
  } else {
    writeLedger(ledger)
  }
}
