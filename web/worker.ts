import fs from 'node:fs'
import path from 'node:path'
import { EventEmitter } from 'node:events'
import { AsyncLocalStorage } from 'node:async_hooks'
import { randomUUID } from 'node:crypto'
import { configurePlatform, operations, type ClientSink, type InvokeEvent } from '../electron/platform'
import { APPEARANCE_STORAGE_KEY } from '../src/shared/appearance-profile'

/** Trusted child-process protocol. This file is never an HTTP entry point. */
type Init = { type: 'init'; userData: string; migrationUserData: string; appData: string; canonicalTarget: string; legacySource: string;
  generation?: string; dataRoot?: string; projectRoot?: string; libraryRoot: string }
type Command = { type: 'invoke'; id: string; channel: string; args: unknown[]; clientId: number }
const invocation = new AsyncLocalStorage<string>()
const clients = new Map<number, Sink>()
const pendingDialogs = new Map<string, { resolve(value: unknown): void; reject(error: Error): void; timer: NodeJS.Timeout }>()
let initialized = false
let projectRoot: string | undefined
let generation: string | undefined
let globalDataRoot: string | undefined
let projectData: unknown
const emit = (message: unknown) => { if (process.connected) process.send?.(message) }
class Sink extends EventEmitter implements ClientSink {
  destroyed = false
  constructor(readonly id: number) { super(); this.setMaxListeners(0) }
  send(channel: string, ...args: unknown[]): void { if (!this.destroyed) emit({ type: 'event', clientId: this.id, channel, args }) }
  isDestroyed(): boolean { return this.destroyed }
  destroy(): void { this.destroyed = true; this.emit('destroyed'); this.removeAllListeners() }
}
function client(id: number): Sink {
  if (!Number.isSafeInteger(id) || id <= 0) throw new Error('CLIENT_INVALID')
  let sink = clients.get(id)
  if (!sink) { sink = new Sink(id); clients.set(id, sink) }
  return sink
}
function browserDialog(kind: string, options: unknown, event?: InvokeEvent): Promise<unknown> {
  if (!event) throw new Error('DIALOG_CALLER_REQUIRED')
  const id = randomUUID()
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => { pendingDialogs.delete(id); reject(new Error('BROWSER_DIALOG_EXPIRED')) }, 10 * 60 * 1000)
    pendingDialogs.set(id, { resolve, reject, timer })
    emit({ type: 'dialog', id, kind, options, clientId: event.sender.id, requestId: invocation.getStore() })
  })
}
async function init(input: Init): Promise<void> {
  projectRoot = input.projectRoot
  process.env.AI_NOVEL_RUNTIME = 'web'
  if (projectRoot) process.env.AI_NOVEL_WORKER_PROJECT = projectRoot
  configurePlatform({
    app: { getLocale: () => 'zh-CN', getPath: name => {
      const paths: Record<string, string> = { userData: input.userData, appData: input.appData, home: path.dirname(input.libraryRoot), temp: '/tmp' }
      if (!(name in paths)) throw new Error('APP_PATH_UNSUPPORTED')
      return paths[name]!
    }, isPackaged: false, getVersion: () => '1.4.0-web' },
    dialogs: {
      showOpenDialog: (options, event) => browserDialog('open', options, event) as Promise<{ canceled: boolean; filePaths: string[] }>,
      showSaveDialog: (options, event) => browserDialog('save', options, event) as Promise<{ canceled: boolean; filePath?: string }>,
      showMessageBox: (options, event) => browserDialog('message', options, event) as Promise<{ response: number }>,
    },
    navigation: { async openExternal(url, event) {
      const parsed = new URL(url)
      if (parsed.protocol !== 'https:' || parsed.username || parsed.password) throw new Error('EXTERNAL_URL_DENIED')
      if (!event) throw new Error('CLIENT_REQUIRED')
      event.sender.send('browser:open-external', { url: parsed.href })
    } },
  })
  const locator = await import('../electron/services/app-data-locator')
  if (input.generation && input.dataRoot) {
    locator.installGlobalDataLocator(input.dataRoot, input.generation, input.legacySource)
    generation = input.generation; globalDataRoot = input.dataRoot
  } else {
    const { runGlobalDataMigration, activateGlobalData } = await import('../electron/services/global-data-migration')
    const migrated = runGlobalDataMigration({ userData: input.migrationUserData, canonicalTarget: input.canonicalTarget, legacySource: input.legacySource, exclusiveAccess: true })
    if (migrated.state !== 'ready') throw new Error(migrated.code)
    activateGlobalData(migrated)
    generation = migrated.globalGeneration; globalDataRoot = migrated.dataRoot
  }
  const { ensureVelaHome } = await import('../electron/utils/config-utils')
  ensureVelaHome()
  const { registerIPCHandlers } = await import('../electron/ipc-handlers')
  registerIPCHandlers()
  const { registerMCPHandlers } = await import('../electron/mcp/mcp-ipc-bridge')
  registerMCPHandlers()
  const { mcpManager } = await import('../electron/mcp/mcp-manager')
  mcpManager.setCallbacks({
    onStatusChange: (serverId, status, error) => { for (const sink of clients.values()) sink.send('mcp:status-change', { serverId, status, error }) },
    onToolsChange: tools => { for (const sink of clients.values()) sink.send('mcp:tools-change', tools) },
  })
  const { externalFileGrants } = await import('../electron/services/external-file-grant-service')
  const { projectPeekService } = await import('../electron/services/project-peek')
  const { registerRecentProject } = await import('../electron/controllers/project-controller')
  operations.handle('web:resolve-project-grant', (event, grantId: string) => externalFileGrants.resolveDirectoryPath({ grantId, webContentsId: event.sender.id, operation: 'project-open' }))
  operations.handle('web:library-list', () => {
    if (projectRoot) throw new Error('GLOBAL_OPERATION_REQUIRED')
    const output: Array<{ name: string; path: string; projectId: string; updatedAt: string; previewCapabilityId?: string }> = []
    for (const entry of fs.readdirSync(input.libraryRoot, { withFileTypes: true })) {
      if (!entry.isDirectory() || entry.isSymbolicLink()) continue
      const root = path.join(input.libraryRoot, entry.name)
      try {
        const manifestFile = path.join(root, '.ai-novel', 'project.json')
        const info = fs.lstatSync(manifestFile)
        if (!info.isFile() || info.isSymbolicLink()) continue
        const manifest = JSON.parse(fs.readFileSync(manifestFile, 'utf8'))
        if (typeof manifest.projectId !== 'string') continue
        const capability = projectPeekService.issueCapability(root)
        const updatedAt = fs.statSync(path.join(root, '.ai-novel', 'project.db')).mtime.toISOString()
        registerRecentProject({ name: entry.name, path: root, projectId: manifest.projectId, updatedAt })
        output.push({ name: entry.name, path: root, projectId: manifest.projectId, updatedAt,
          ...(capability ? { previewCapabilityId: capability.capabilityId } : {}) })
      } catch { /* An incomplete or legacy directory is not an open capability. */ }
    }
    return output.sort((a, b) => b.updatedAt.localeCompare(a.updatedAt))
  })
  operations.removeHandler('project:peek-overview')
  operations.handle('project:peek-overview', (_event, capabilityId: string) => projectPeekService.peek(capabilityId))
  const revisions = new Map<number, number>()
  operations.handle('startup:get-state', event => ({ state: 'ready', globalGeneration: generation, appearanceRevisionFloor: revisions.get(event.sender.id) }))
  operations.handle('startup:appearance-ack', (event, input: unknown) => {
    if (!input || typeof input !== 'object') return false
    const ack = input as Record<string, unknown>
    if (ack.storageKey !== APPEARANCE_STORAGE_KEY || ack.globalGeneration !== generation
      || !Number.isSafeInteger(ack.profileRevision) || Number(ack.profileRevision) < 1) return false
    revisions.set(event.sender.id, Number(ack.profileRevision)); return true
  })
  // Internal initialization calls never cross the network. A project worker
  // opens exactly one canonical project for its complete lifetime.
  if (projectRoot) {
    const opened = await operations.invoke('project:open', { sender: client(1) }, projectRoot, randomUUID()) as { success: boolean; project?: unknown; error?: string }
    if (!opened.success) throw new Error(opened.error ?? 'PROJECT_OPEN_FAILED')
    projectData = opened
    // Reopening from another tab must not rotate the active worker lease.
    operations.removeHandler('project:open')
    const { ProjectCoreRepository } = await import('../electron/repositories/project-core-repository')
    operations.handle('project:open', (_event, _target: unknown, requestToken: string) => {
      const cached = projectData as { project: { novelConfig: Record<string, unknown>; name: string; characterStates: string } }
      const core = ProjectCoreRepository.get()
      if (!core) throw new Error('PROJECT_CORE_MISSING')
      const novelConfig: Record<string, unknown> = { ...cached.project.novelConfig }
      for (const key of Object.keys(novelConfig)) {
        const sourceKey = key === 'narrativePOV' ? 'narrativePov' : key
        if (sourceKey in core) novelConfig[key] = core[sourceKey as keyof typeof core]
      }
      return { ...(projectData as object), requestToken, project: { ...cached.project, name: core.projectName, characterStates: core.characterStates, novelConfig } }
    })
  }
  initialized = true
  emit({ type: 'ready', generation, dataRoot: globalDataRoot, channels: operations.channels(), project: projectData })
}
process.on('message', async (message: Init | Command | { type: 'dialog-result'; id: string; result: unknown } | { type: 'release-client'; clientId: number }) => {
  if (message.type === 'dialog-result') {
    const pending = pendingDialogs.get(message.id)
    if (pending) { clearTimeout(pending.timer); pendingDialogs.delete(message.id); pending.resolve(message.result) }
    return
  }
  if (message.type === 'release-client') { clients.get(message.clientId)?.destroy(); clients.delete(message.clientId); return }
  if (message.type === 'init') {
    try { await init(message) } catch (error) { emit({ type: 'fatal', code: error instanceof Error ? error.message : 'WORKER_INIT_FAILED' }); process.exitCode = 1 }
    return
  }
  if (message.type !== 'invoke' || !initialized) return
  try {
    const result = await invocation.run(message.id, () => operations.invoke(message.channel, { sender: client(message.clientId) }, ...message.args))
    emit({ type: 'result', id: message.id, result })
  } catch (error) {
    // Full stack remains server-local. Responses do not disclose configuration.
    console.error('[web-worker]', message.channel, error instanceof Error ? error.stack : 'operation failed')
    emit({ type: 'result', id: message.id, error: { code: error instanceof Error && /^[A-Z0-9_]+$/.test(error.message) ? error.message : 'OPERATION_FAILED' } })
  }
})
process.on('disconnect', () => process.exit(0))
for (const signal of ['SIGTERM', 'SIGINT'] as const) process.on(signal, async () => {
  for (const pending of pendingDialogs.values()) { clearTimeout(pending.timer); pending.reject(new Error('SHUTTING_DOWN')) }
  try { const { closeProjectDatabase } = await import('../electron/database'); closeProjectDatabase() } catch { /* shutdown is best effort */ }
  process.exit(0)
})
