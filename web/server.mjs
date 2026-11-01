import http from 'node:http'
import fs from 'node:fs'
import path from 'node:path'
import crypto from 'node:crypto'
import { fork } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { HttpError, Sessions, inside, safePath, safeFilename, body, jsonBody, json as writeJson, validateArguments } from './security.mjs'
import { directoryZip } from './downloads.mjs'
import { registerSecret, sanitizePublicResponse, publicModels } from './public-response.mjs'
import { ClientDepartures } from './client-departure.mjs'
function json(res, status, value) { return writeJson(res, status, sanitizePublicResponse(value)) }

const here = path.dirname(fileURLToPath(import.meta.url))
const appRoot = path.resolve(here, '..')
const data = path.resolve(process.env.NOVEL_DATA_ROOT ?? '/data')
const library = path.join(data, 'workspaces')
const transfers = path.join(data, 'web-transfers')
const userData = path.join(data, 'config', 'ai-novel-writer')
// Reuse the committed generation from the former desktop deployment. The
// migration control profile is distinct, but the canonical data never moves.
const canonicalTarget = userData
const migrationUserData = path.join(data, 'config', 'ai-novel-writer-web-runtime')
const legacySource = path.join(data, 'legacy-global')
const dist = path.join(appRoot, 'dist-web')
const indexSource = fs.readFileSync(path.join(dist, 'index.html'), 'utf8')
const inlineScriptHashes = [...indexSource.matchAll(/<script(?:\s[^>]*)?>([\s\S]*?)<\/script>/g)].filter(match => match[1].trim()).map(match => `'sha256-${crypto.createHash('sha256').update(match[1]).digest('base64')}'`).join(' ')
const contentSecurityPolicy = `default-src 'self'; script-src 'self' ${inlineScriptHashes}; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'`
const port = Number(process.env.PORT ?? 6081)
const origins = new Set((process.env.NOVEL_ALLOWED_ORIGINS ?? 'http://192.168.1.15:8093,https://192.168.1.15:8445').split(',').map(x => x.trim()).filter(Boolean))
const allowedEndpoints = new Set((process.env.NOVEL_ALLOWED_ENDPOINTS ?? 'http://192.168.1.15:8317').split(',').map(x => x.trim()).filter(Boolean))
const sessions = new Sessions()
const workers = new Map()
const clients = new Map()
const dialogs = new Map()
const uploads = new Map()
const exports = new Map()
const streams = new Map()
const downloadTokens = new Map()
const grantWorkers = new Map()
const executionLeases = new Map()
const cloudOperations = new Map()
const adminUsers = new Set((process.env.NOVEL_ADMIN_USERS ?? 'admin').split(',').map(x => x.trim()).filter(Boolean))
function clientBusy(client) {
  return client.inflight > 0
    || [...workers.values()].some(worker => [...worker.pending.values()].some(pending => pending.client === client))
    || [...streams.values()].some(stream => stream.client === client)
}
const departures = new ClientDepartures({ release: releaseProject, busy: clientBusy })
let nextClient = 10
let stopping = false
let globalInit
for (const directory of [library, transfers, userData, canonicalTarget, legacySource]) fs.mkdirSync(directory, { recursive: true, mode: 0o700 })
// Production entrypoint holds a kernel flock for the process lifetime. Unlike
// PID/hostname JSON markers it releases after SIGKILL/container replacement and
// cannot age-steal another live gateway. Stop the former desktop writer first.
if (process.env.NODE_ENV === 'production' && process.env.NOVEL_VOLUME_LOCK_HELD !== '1') throw new Error('DATA_VOLUME_LOCK_REQUIRED')
const policy = JSON.parse(fs.readFileSync(path.join(appRoot, 'web', 'operations.json'), 'utf8'))
const operationPolicy = new Map(Object.entries(policy.operations))

function bindIssuedGrants(value, worker, client, requestId, depth = 0) {
  if (!value || typeof value !== 'object' || depth > 6) return
  if (typeof value.grantId === 'string') {
    const staging = [...exports.values()].find(item => item.requestId === requestId && item.client === client && item.worker === worker)
    grantWorkers.set(value.grantId, { client, worker, exportRoot: staging?.directory, exportFile: staging?.file, created: Date.now() })
  }
  for (const child of Object.values(value)) bindIssuedGrants(child, worker, client, requestId, depth + 1)
}
function referencedGrants(args) {
  const found = new Set()
  const visit = (value, depth = 0) => {
    if (depth > 6 || !value) return
    if (typeof value === 'string') { if (grantWorkers.has(value)) found.add(value); return }
    if (typeof value === 'object') for (const item of Object.values(value)) visit(item, depth + 1)
  }
  for (const value of args) visit(value)
  return [...found]
}
function event(client, channel, args) {
  const entry = { id: ++client.sequence, channel, args }
  client.events.push(entry)
  if (client.events.length > 2048) client.events.shift()
  const wire = `id: ${entry.id}\ndata: ${JSON.stringify(sanitizePublicResponse({ channel, args }))}\n\n`
  for (const response of client.connections) {
    if (response.writableLength > 8 * 1024 * 1024) response.destroy()
    else response.write(wire)
  }
}
function errorCode(error, fallback = 'REQUEST_FAILED') {
  return error instanceof HttpError ? error.code : typeof error?.message === 'string' && /^[A-Z][A-Z0-9_]+$/.test(error.message) ? error.message : fallback
}
class DomainWorker {
  pending = new Map()
  constructor(key, init) {
    this.key = key
    this.lastUsed = Date.now()
    this.process = fork(path.join(here, 'worker.mjs'), [], { stdio: ['ignore', 'inherit', 'inherit', 'ipc'], serialization: 'advanced', env: { ...process.env, AI_NOVEL_RUNTIME: 'web' } })
    this.ready = new Promise((resolve, reject) => { this.resolve = resolve; this.reject = reject })
    this.process.on('message', message => {
      if (message.type === 'ready') { this.info = message; this.resolve(message); return }
      if (message.type === 'fatal') { this.reject(new Error(message.code)); return }
      if (message.type === 'result') {
        const pending = this.pending.get(message.id)
        if (pending) {
          clearTimeout(pending.timer); this.pending.delete(message.id)
          if (message.error) pending.reject(new Error(message.error.code))
          else { if (pending.channel.startsWith('dialog:')) bindIssuedGrants(message.result, this, pending.client, message.id); pending.resolve(message.result) }
          departures.check(pending.client)
        }
        return
      }
      const client = clients.get(message.clientId)
      if (!client || !client.workers.has(this)) return
      if (message.type === 'event') {
        if (['llm:stream-done', 'llm:stream-error'].includes(message.channel)) {
          streams.delete(message.args?.[0]?.requestId)
          departures.check(client)
        }
        event(client, message.channel, message.args)
      }
      if (message.type === 'dialog') handleDialog(this, client, message)
    })
    this.process.on('error', error => this.reject(error))
    this.process.on('exit', () => {
      this.reject(new Error('WORKER_EXITED'))
      for (const pending of this.pending.values()) { clearTimeout(pending.timer); pending.reject(new Error('WORKER_EXITED')) }
      this.pending.clear(); if (workers.get(key) === this) workers.delete(key)
      for (const client of clients.values()) client.workers.delete(this)
      for (const map of [grantWorkers, executionLeases, cloudOperations, streams, exports]) for (const [id, owned] of map) if (owned.worker === this) map.delete(id)
      if (key === 'global' && !stopping) { console.error('[native-web] global worker lost; restarting service required'); void shutdown() }
      for (const client of clients.values()) if (client.projectWorker === this) {
        client.projectWorker = undefined; event(client, 'browser:project-lost', [{ code: 'WORKER_EXITED' }])
      }
    })
    this.process.send({ type: 'init', ...init })
  }
  async invoke(channel, args, client) {
    await this.ready
    this.lastUsed = Date.now()
    client.workers.add(this)
    const id = crypto.randomUUID()
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => { this.pending.delete(id); reject(new Error('OPERATION_TIMEOUT_UNKNOWN_RESULT')) }, 15 * 60 * 1000)
      this.pending.set(id, { resolve, reject, timer, client, channel })
      this.process.send({ type: 'invoke', id, channel, args, clientId: client.id })
    })
  }
}
const initialPaths = { userData, migrationUserData, appData: path.join(data, 'config'), canonicalTarget, legacySource, libraryRoot: library }
const globalWorker = new DomainWorker('global', initialPaths)
workers.set('global', globalWorker)
globalInit = await globalWorker.ready
// Eager private credential registration covers generation-first sessions too.
// This trusted initialization client is not a browser session or HTTP operation.
await models({ id: 2, workers: new Set() })

function getClient(req, session, queryId) {
  const value = queryId ?? req.headers['x-client-id']
  if (typeof value !== 'string' || !/^[a-zA-Z0-9_-]{16,128}$/.test(value)) throw new HttpError(400, 'CLIENT_ID_REQUIRED')
  let client = session.clients.get(value)
  if (!client) {
    if (session.clients.size >= 32) throw new HttpError(429, 'TAB_LIMIT')
    client = { id: nextClient++, token: value, session, workers: new Set(), events: [], sequence: 0, connections: new Set(), touched: Date.now(), inflight: 0 }
    clients.set(client.id, client); session.clients.set(value, client)
  }
  client.touched = Date.now()
  return client
}
function releaseProject(client) {
  if (client.projectWorker?.owner === client.id) { client.projectWorker.owner = undefined; client.projectWorker.lastUsed = Date.now() }
  if (client.projectWorker?.process.connected) client.projectWorker.process.send({ type: 'release-client', clientId: client.id })
  client.projectWorker = undefined
}
function projectRoot(value) {
  if (typeof value !== 'string' || !inside(library, value) || path.resolve(value) === library) throw new HttpError(403, 'PROJECT_OUTSIDE_LIBRARY')
  const relative = path.relative(library, path.resolve(value))
  const root = safePath(library, relative)
  const marker = safePath(root, '.ai-novel/project.json')
  if (!fs.statSync(marker).isFile()) throw new HttpError(400, 'PROJECT_MANIFEST_REQUIRED')
  return fs.realpathSync(root)
}
async function openProject(client, target, requestToken) {
  let root
  if (typeof target === 'string') root = projectRoot(target)
  else {
    // A chooser result is an opaque, worker-owned grant; resolve only in the
    // global metadata worker, where the chooser was issued.
    root = await globalWorker.invoke('web:resolve-project-grant', [target?.grantId], client)
    root = projectRoot(root)
  }
  let worker = workers.get(root)
  if (!worker) {
    if (workers.size > 16) {
      const idle = [...workers.values()].filter(item => item.key !== 'global' && !item.owner && !item.pending.size).sort((a, b) => a.lastUsed - b.lastUsed)[0]
      if (!idle) throw new HttpError(429, 'OPEN_PROJECT_LIMIT')
      workers.delete(idle.key); idle.process.kill('SIGTERM')
    }
    worker = new DomainWorker(root, { ...initialPaths, generation: globalInit.generation, dataRoot: globalInit.dataRoot, projectRoot: root })
    workers.set(root, worker)
  }
  await worker.ready
  if (worker.owner && worker.owner !== client.id) {
    const owner = clients.get(worker.owner)
    if (owner && (owner.connections.size || clientBusy(owner) || Date.now() - owner.touched < 2 * 60 * 1000)) throw new HttpError(409, 'PROJECT_OPEN_IN_ANOTHER_TAB', '该作品正在另一个标签页编辑，请先关闭或离开另一个标签页。')
    if (owner) releaseProject(owner)
  }
  // Same-tab refresh/reopen retains the live client sink and its active model
  // attempts. Releasing here would cancel a request merely because UI reloaded.
  if (client.projectWorker !== worker) releaseProject(client)
  worker.owner = client.id; client.projectWorker = worker
  return worker.invoke('project:open', [root, requestToken], client)
}
function requireProject(client) {
  if (!client.projectWorker || client.projectWorker.owner !== client.id) throw new HttpError(409, 'PROJECT_SESSION_NOT_OWNED')
  return client.projectWorker
}
function endpointAllowed(value) {
  let url
  try { url = new URL(value) } catch { throw new HttpError(400, 'MODEL_ENDPOINT_INVALID') }
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.hash || url.search || !allowedEndpoints.has(url.origin)) {
    throw new HttpError(403, 'ENDPOINT_NOT_ALLOWED', '该服务地址尚未加入服务器端允许列表。')
  }
}
function sameEndpoint(a, b) { return a.provider === b.provider && a.protocol === b.protocol && a.baseUrl.replace(/\/+$/, '') === b.baseUrl.replace(/\/+$/, '') }
async function models(client) {
  const privateModels = await globalWorker.invoke('llm:list-models', [], client)
  for (const model of privateModels) registerSecret(model.apiKey)
  return privateModels
}
async function restoreCredential(client, candidate, id = candidate?.id) {
  if (!candidate || typeof candidate !== 'object') throw new HttpError(400, 'MODEL_INVALID')
  endpointAllowed(candidate.baseUrl)
  const stored = (await models(client)).find(item => item.id === id)
  if (!candidate.apiKey && stored?.apiKey) {
    if (!sameEndpoint(candidate, stored)) throw new HttpError(400, 'CREDENTIAL_ENDPOINT_CHANGED')
    candidate = { ...candidate, apiKey: stored.apiKey }
  }
  registerSecret(candidate.apiKey)
  const { hasCredential: _ignored, savedModelId: _saved, ...clean } = candidate
  return clean
}
function handleDialog(worker, client, message) {
  const options = message.options ?? {}
  const directory = options.properties?.includes('openDirectory')
  const defaultName = safeFilename(path.basename(options.defaultPath ?? '作品存档.ainovel'))
  const pending = { ...message, worker, client, directory, defaultName, created: Date.now() }
  dialogs.set(message.id, pending)
  event(client, 'browser:dialog', [{ id: message.id, kind: message.kind, options: {
    directoryKind: options.directoryKind, title: options.title, message: options.message, detail: options.detail, buttons: options.buttons,
    cancelId: options.cancelId, defaultId: options.defaultId, properties: options.properties, filters: options.filters,
    defaultName,
  } }])
}
function dialogDirectory(pending, result) {
  const { directoryKind } = pending.options
  // Saving to the server library is an intentional browser replacement, not a
  // native OS directory browser. No arbitrary server filesystem is exposed.
  if (directoryKind === 'export-directory') {
    const directory = path.join(transfers, 'exports', crypto.randomUUID())
    fs.mkdirSync(directory, { recursive: true, mode: 0o700 })
    exports.set(directory, { client: pending.client, worker: pending.worker, directory, requestId: pending.requestId, created: Date.now() })
    return directory
  }
  if (directoryKind === 'project-create' || directoryKind === 'project-restore-target') return library
  if (directoryKind === 'project-open' && result.projectPath) return projectRoot(result.projectPath)
  if (['legacy-project', 'knowledge-folder', 'writing-skill'].includes(directoryKind) && result.folderUploadId) {
    const upload = uploads.get(result.folderUploadId)
    if (!upload || upload.client !== pending.client || !upload.directory) throw new HttpError(403, 'UPLOAD_NOT_OWNED')
    return upload.path
  }
  throw new HttpError(400, 'DIRECTORY_SELECTION_REQUIRED')
}
async function finishDialog(client, id, result) {
  const pending = dialogs.get(id)
  if (!pending || pending.client !== client || Date.now() - pending.created > 10 * 60 * 1000) throw new HttpError(404, 'DIALOG_EXPIRED')
  let response
  if (pending.kind === 'message') {
    if (!Number.isSafeInteger(result.response) || result.response < 0 || result.response >= (pending.options.buttons?.length ?? 1)) throw new HttpError(400, 'DIALOG_RESPONSE_INVALID')
    response = { response: result.response }
  } else if (result.canceled) response = pending.kind === 'open' ? { canceled: true, filePaths: [] } : { canceled: true }
  else if (pending.kind === 'save') {
    const directory = path.join(transfers, 'exports', crypto.randomUUID())
    fs.mkdirSync(directory, { recursive: true, mode: 0o700 })
    const filename = safeFilename(result.name ?? pending.defaultName)
    const target = safePath(directory, filename, { missing: true })
    exports.set(directory, { client, worker: pending.worker, directory, requestId: pending.requestId, created: Date.now(), file: target })
    response = { canceled: false, filePath: target }
  } else if (pending.directory) response = { canceled: false, filePaths: [dialogDirectory(pending, result)] }
  else {
    if (!Array.isArray(result.uploadIds) || !result.uploadIds.length || result.uploadIds.length > 100) throw new HttpError(400, 'UPLOAD_SELECTION_INVALID')
    const files = result.uploadIds.map(id => {
      const upload = uploads.get(id)
      if (!upload || upload.client !== client || upload.directory) throw new HttpError(403, 'UPLOAD_NOT_OWNED')
      return upload.path
    })
    response = { canceled: false, filePaths: files }
  }
  dialogs.delete(id)
  pending.worker.process.send({ type: 'dialog-result', id, result: response })
}
function checkArgumentsForProject(client, args) {
  const worker = requireProject(client)
  const session = args.at(-1)
  const actual = worker.info.project?.project
  if (session && typeof session === 'object' && 'leaseId' in session) {
    if (session.projectId !== actual?.id || session.leaseId !== actual?.sessionLease || session.projectPath !== actual?.path) throw new HttpError(409, 'PROJECT_SESSION_STALE')
  }
  return worker
}
async function operate(client, operation, args) {
  if (operation === 'browser:prepare-download') {
    const owned = grantWorkers.get(args[0])
    if (!owned || owned.client !== client || !owned.exportRoot) throw new HttpError(403, 'EXPORT_GRANT_NOT_OWNED')
    const file = safePath(owned.exportRoot, args[1])
    const directory = fs.statSync(file).isDirectory(), token = crypto.randomBytes(24).toString('hex')
    const name = path.basename(file) + (directory ? '.zip' : '')
    downloadTokens.set(token, { client, path: file, directory, created: Date.now(), name })
    event(client, 'browser:download-ready', [{ url: `/api/downloads/${token}`, name }])
    return { success: true }
  }
  const scope = operationPolicy.get(operation)
  if (!scope) throw new HttpError(404, 'OPERATION_NOT_EXPOSED')
  const granted = referencedGrants(args).map(id => grantWorkers.get(id))
  if (granted.some(owned => owned.client !== client)) throw new HttpError(403, 'GRANT_NOT_OWNED')
  if (granted.some(owned => Date.now() - owned.created > 10 * 60 * 1000)) throw new HttpError(403, 'GRANT_EXPIRED')
  if (operation === 'project:open') return openProject(client, ...args)
  if (operation === 'project:recent-list') return globalWorker.invoke('web:library-list', [], client)
  if (operation === 'project:get-runtime-context') return client.projectWorker
    ? client.projectWorker.invoke(operation, [], client) : { activeProjectPath: null, dbReady: true }
  if (operation === 'db:close') {
    const active = checkArgumentsForProject(client, args)
    if (args[0] !== active.key) throw new HttpError(409, 'PROJECT_SESSION_STALE')
    releaseProject(client); return { success: true }
  }
  if (operation === 'llm:list-models') return publicModels(await models(client))
  if (operation === 'llm:save-model' || operation === 'llm:test-connection') args[0] = await restoreCredential(client, args[0])
  if (operation === 'llm:discover-models') args[0] = await restoreCredential(client, args[0], args[0]?.savedModelId)
  if (operation === 'config:set') {
    const input = args[0]
    if (!input || typeof input !== 'object') throw new HttpError(400, 'CONFIG_INVALID')
    const supported = new Set(['theme','locale','autoOpenNextChapterAfterFinalize','editorFontSize','editorFontFamily','autoSaveInterval','defaultModelId','defaultEmbeddingModelId','outlineTargetUnits','blueprintTargetUnits','proxy'])
    if (Object.keys(input).some(key => !supported.has(key))) throw new HttpError(400, 'CONFIG_FIELD_DENIED')
    if (input.proxy && JSON.stringify(input.proxy) !== JSON.stringify((await globalWorker.invoke('config:get', [], client)).proxy)) throw new HttpError(403, 'PROXY_SERVER_ADMIN_ONLY')
  }
  if (operation === 'cloud-backup:connect') {
    registerSecret(args[0]?.secret)
    if (args[0]?.endpoint) endpointAllowed(args[0].endpoint)
  }
  let worker
  if (scope === 'global' || operation === 'dialog:select-folder') worker = globalWorker
  else if (scope === 'project') worker = checkArgumentsForProject(client, args)
  else {
    const origins = referencedGrants(args).map(id => grantWorkers.get(id)).filter(Boolean)
    if (origins.some(owned => owned.client !== client)) throw new HttpError(403, 'GRANT_NOT_OWNED')
    if (new Set(origins.map(owned => owned.worker)).size > 1) throw new HttpError(409, 'GRANT_WORKER_MISMATCH')
    worker = origins[0]?.worker ?? (client.projectWorker ? requireProject(client) : globalWorker)
  }
  if (granted.some(owned => owned.worker !== worker)) throw new HttpError(409, 'GRANT_WORKER_MISMATCH')
  if (operation.startsWith('mcp:') && !['mcp:list-tools', 'mcp:get-servers-status'].includes(operation) && !adminUsers.has(client.session.principal)) throw new HttpError(403, 'MCP_SERVER_ADMIN_ONLY')
  const leaseId = operation === 'llm:close-execution-lease' ? args[0]
    : operation === 'llm:generate-stream' ? args[1]?.modelExecutionLeaseId
    : operation === 'llm:generate' ? args[0]?.modelExecutionLeaseId : undefined
  if (leaseId) {
    const lease = executionLeases.get(leaseId)
    if (!lease || lease.client !== client) throw new HttpError(403, 'EXECUTION_LEASE_NOT_OWNED')
    worker = lease.worker
  }
  if (operation.startsWith('cloud-backup:') && operation !== 'cloud-backup:cancel' && args[0]?.operationId) {
    const previous = cloudOperations.get(args[0].operationId)
    if (previous && previous.client !== client) throw new HttpError(403, 'CLOUD_OPERATION_NOT_OWNED')
    cloudOperations.set(args[0].operationId, { client, worker })
  }
  if (operation === 'cloud-backup:cancel') {
    const owned = cloudOperations.get(args[0])
    if (!owned || owned.client !== client) throw new HttpError(403, 'CLOUD_OPERATION_NOT_OWNED')
    worker = owned.worker
  }
  if (operation === 'llm:generate-stream') {
    if (typeof args[0] !== 'string' || streams.has(args[0])) throw new HttpError(409, 'STREAM_ID_CONFLICT')
    if ([...streams.values()].filter(item => item.client === client).length >= 256) throw new HttpError(429, 'STREAM_LIMIT')
    streams.set(args[0], { client, worker, created: Date.now() })
  }
  if (operation === 'llm:cancel') {
    const stream = streams.get(args[0])
    if (stream && stream.client !== client) throw new HttpError(403, 'STREAM_NOT_OWNED')
    worker = stream?.worker ?? worker
  }
  if (operation === 'project:delete') {
    // Only this client's unique active project can ever reach deletion logic.
    projectRoot(args[0])
  }
  let result
  try { result = await worker.invoke(operation, args, client) }
  catch (error) {
    if (operation === 'llm:generate-stream') streams.delete(args[0])
    throw error
  } finally {
    if (operation.startsWith('cloud-backup:') && operation !== 'cloud-backup:cancel' && args[0]?.operationId) cloudOperations.delete(args[0].operationId)
  }
  if (operation === 'llm:begin-execution-lease' && result?.success && result.lease?.leaseId) executionLeases.set(result.lease.leaseId, { client, worker })
  if (operation === 'llm:close-execution-lease' && result?.success) executionLeases.delete(args[0])
  if (operation === 'llm:generate-stream' && !result?.started) streams.delete(args[0])
  if (operation === 'project:archive-export' && result?.success) {
    const target = grantWorkers.get(args[0]?.targetArchiveGrantId)
    if (target?.client === client && target.exportRoot) for (const entry of fs.readdirSync(target.exportRoot)) {
      const file = safePath(target.exportRoot, entry), token = crypto.randomBytes(24).toString('hex')
      if (!fs.statSync(file).isFile()) continue
      downloadTokens.set(token, { client, path: file, name: entry, created: Date.now() })
      event(client, 'browser:download-ready', [{ url: `/api/downloads/${token}`, name: entry }])
    }
  }
  if (operation === 'project:delete' && result?.directoryDeleted) { releaseProject(client); worker.process.kill('SIGTERM') }
  return result
}
const mime = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8', '.json': 'application/json', '.svg': 'image/svg+xml', '.png': 'image/png', '.webp': 'image/webp', '.woff2': 'font/woff2', '.ico': 'image/x-icon' }
const server = http.createServer(async (req, res) => {
  try {
    if (stopping) throw new HttpError(503, 'SHUTTING_DOWN')
    const url = new URL(req.url, 'http://localhost')
    if (url.pathname === '/healthz') return json(res, 200, { ok: true, runtime: 'node', electron: false, workers: workers.size })
    // Header is trusted ONLY because this port is published on loopback, and
    // nginx always overwrites it after authenticating. Never expose this port.
    const principal = req.headers['x-novel-user']
    if (typeof principal !== 'string' || !principal.trim() || principal.length > 256) throw new HttpError(401, 'AUTHENTICATION_REQUIRED')
    const session = sessions.get(req, res, principal)
    if (url.pathname === '/api/bootstrap' && req.method === 'GET') return json(res, 200, { csrfToken: session.csrf, runtime: 'web', generation: globalInit.generation })
    if (url.pathname === '/api/events' && req.method === 'GET') {
      if (req.headers.origin && !origins.has(req.headers.origin)) throw new HttpError(403, 'ORIGIN_DENIED')
      const client = getClient(req, session, url.searchParams.get('client'))
      if (client.connections.size >= 3) throw new HttpError(429, 'EVENT_CONNECTION_LIMIT')
      res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store', 'Connection': 'keep-alive', 'X-Accel-Buffering': 'no' })
      departures.connect(client)
      res.write(': connected\n\n'); client.connections.add(res)
      const last = Number(req.headers['last-event-id'] ?? 0)
      if (last > 0) for (const item of client.events) if (item.id > last) res.write(`id: ${item.id}\ndata: ${JSON.stringify(sanitizePublicResponse({ channel: item.channel, args: item.args }))}\n\n`)
      const heartbeat = setInterval(() => { client.touched = Date.now(); res.write(': heartbeat\n\n') }, 15000)
      req.on('close', () => { clearInterval(heartbeat); client.connections.delete(res); departures.check(client) })
      return
    }
    if (url.pathname.startsWith('/api/')) {
      if (url.pathname.startsWith('/api/downloads/') && req.method === 'GET') {
        const download = downloadTokens.get(url.pathname.split('/').at(-1))
        if (!download || download.client.session !== session || Date.now() - download.created > 60 * 60 * 1000) throw new HttpError(404, 'DOWNLOAD_EXPIRED')
        const bytes = download.directory ? directoryZip(download.path) : fs.readFileSync(download.path)
        res.writeHead(200, { 'Content-Type': download.directory ? 'application/zip' : 'application/octet-stream', 'Content-Length': bytes.length,
          'Content-Disposition': `attachment; filename="download${path.extname(download.name)}"; filename*=UTF-8''${encodeURIComponent(download.name)}`,
          'Cache-Control': 'no-store', 'X-Content-Type-Options': 'nosniff' })
        res.end(bytes); return
      }
      const client = getClient(req, session)
      if (req.method !== 'GET') sessions.verify(req, session, origins)
      if (url.pathname === '/api/library' && req.method === 'GET') return json(res, 200, { projects: await globalWorker.invoke('web:library-list', [], client) })
      if (url.pathname.startsWith('/api/operations/') && req.method === 'POST') {
        const operation = decodeURIComponent(url.pathname.slice('/api/operations/'.length))
        const input = await jsonBody(req); validateArguments(input.args)
        if (client.inflight >= 32) throw new HttpError(429, 'REQUEST_LIMIT')
        client.inflight++
        try { return json(res, 200, { result: await operate(client, operation, input.args) }) }
        finally { client.inflight--; departures.check(client) }
      }
      if (url.pathname === '/api/upload-folders' && req.method === 'POST') {
        if ([...uploads.values()].filter(item => item.client === client && item.directory).length >= 32) throw new HttpError(429, 'UPLOAD_FOLDER_LIMIT')
        await jsonBody(req)
        const id = crypto.randomUUID(), directory = path.join(transfers, 'uploads', crypto.randomUUID())
        fs.mkdirSync(directory, { recursive: true, mode: 0o700 })
        uploads.set(id, { client, path: directory, directory: true, created: Date.now(), size: 0, count: 0 })
        return json(res, 201, { folderUploadId: id })
      }
      if (url.pathname === '/api/uploads' && req.method === 'POST') {
        if ([...uploads.values()].filter(item => item.client === client && !item.directory).length >= 1024) throw new HttpError(429, 'UPLOAD_FILE_LIMIT')
        const bytes = await body(req, 64 * 1024 * 1024)
        const used = [...uploads.values()].filter(item => item.client === client && !item.directory).reduce((sum, item) => sum + (item.size ?? 0), 0)
        if (used + bytes.length > 256 * 1024 * 1024) throw new HttpError(413, 'UPLOAD_SESSION_LIMIT')
        const name = safeFilename(decodeURIComponent(String(req.headers['x-upload-name'] ?? 'document.txt')))
        const folderId = req.headers['x-upload-folder']
        const folder = typeof folderId === 'string' ? uploads.get(folderId) : undefined
        if (folderId && (!folder || folder.client !== client || !folder.directory)) throw new HttpError(403, 'UPLOAD_FOLDER_NOT_OWNED')
        const directory = folder?.path ?? path.join(transfers, 'uploads', crypto.randomUUID())
        fs.mkdirSync(directory, { recursive: true, mode: 0o700 })
        const relative = folder ? decodeURIComponent(String(req.headers['x-upload-relative'] ?? name)) : name
        const filename = safePath(directory, relative, { missing: true })
        fs.mkdirSync(path.dirname(filename), { recursive: true, mode: 0o700 })
        fs.writeFileSync(filename, bytes, { flag: 'wx', mode: 0o600 })
        const id = crypto.randomUUID(); uploads.set(id, { client, path: filename, size: bytes.length, created: Date.now() })
        return json(res, 201, { uploadId: id, name, size: bytes.length })
      }
      if (url.pathname.startsWith('/api/dialogs/') && req.method === 'POST') {
        await finishDialog(client, url.pathname.split('/').at(-1), await jsonBody(req))
        return json(res, 200, { success: true })
      }
      if (url.pathname === '/api/release' && req.method === 'POST') { departures.depart(client); return json(res, 200, { success: true }) }
      throw new HttpError(404, 'API_ROUTE_NOT_FOUND')
    }
    if (req.method !== 'GET' && req.method !== 'HEAD') throw new HttpError(405, 'METHOD_NOT_ALLOWED')
    const relative = decodeURIComponent(url.pathname).replace(/^\/+/, '') || 'index.html'
    const filename = safePath(dist, relative)
    const info = fs.statSync(filename)
    if (!info.isFile()) throw new HttpError(404, 'NOT_FOUND')
    res.writeHead(200, { 'Content-Type': mime[path.extname(filename)] ?? 'application/octet-stream', 'Content-Length': info.size,
      'Cache-Control': relative === 'index.html' ? 'no-store' : 'public, max-age=3600', 'X-Content-Type-Options': 'nosniff',
      'Referrer-Policy': 'same-origin', 'Content-Security-Policy': contentSecurityPolicy })
    if (req.method === 'HEAD') res.end(); else fs.createReadStream(filename).pipe(res)
  } catch (error) {
    const status = error instanceof HttpError ? error.status : 500
    if (status >= 500) console.error('[web-gateway]', errorCode(error), error?.stack)
    if (!res.headersSent) json(res, status, { error: { code: errorCode(error), message: error instanceof HttpError ? error.message : '服务暂时无法完成此操作，请重试或查看服务器日志。' } })
    else res.destroy()
  }
})
server.requestTimeout = 16 * 60 * 1000
server.headersTimeout = 30000
server.listen(port, process.env.HOST ?? '127.0.0.1', () => console.log(`[native-web] listening on ${port}; library=${library}; no Electron/VNC`))
const maintenance = setInterval(() => {
  const now = Date.now()
  for (const [id, client] of clients) if (!client.connections.size && now - client.touched > 2 * 60 * 1000 && !clientBusy(client)) {
    releaseProject(client)
    for (const worker of client.workers) if (worker.process.connected) worker.process.send({ type: 'release-client', clientId: client.id })
    for (const [grantId, owned] of grantWorkers) if (owned.client === client) grantWorkers.delete(grantId)
    for (const [leaseId, owned] of executionLeases) if (owned.client === client) executionLeases.delete(leaseId)
    for (const [operationId, owned] of cloudOperations) if (owned.client === client) cloudOperations.delete(operationId)
    for (const [streamId, owned] of streams) if (owned.client === client) streams.delete(streamId)
    departures.forget(client); clients.delete(id); client.session.clients.delete(client.token)
  }
  for (const [id, download] of downloadTokens) if (now - download.created > 60 * 60 * 1000) downloadTokens.delete(id)
  for (const [id, upload] of uploads) if (now - upload.created > 6 * 60 * 60 * 1000 && !upload.client.inflight) {
    if (upload.directory) fs.rmSync(upload.path, { recursive: true, force: true })
    else fs.rmSync(upload.path, { force: true })
    uploads.delete(id)
  }
  for (const [id, pending] of dialogs) if (now - pending.created > 10 * 60 * 1000) dialogs.delete(id)
  for (const [id, grant] of grantWorkers) if (now - grant.created > 10 * 60 * 1000) grantWorkers.delete(id)
  for (const [id, staging] of exports) if (now - staging.created > 6 * 60 * 60 * 1000 && !staging.client.inflight) {
    fs.rmSync(staging.directory, { recursive: true, force: true }); exports.delete(id)
  }
  for (const [key, worker] of workers) if (key !== 'global' && !worker.owner && !worker.pending.size && now - worker.lastUsed > 10 * 60 * 1000) worker.process.kill('SIGTERM')
  for (const [id, stream] of streams) if (Date.now() - stream.created > 2 * 60 * 60 * 1000) streams.delete(id)
}, 60000)
async function shutdown() {
  if (stopping) return
  stopping = true; clearInterval(maintenance); departures.close(); server.close()
  for (const client of clients.values()) for (const response of client.connections) response.end()
  for (const worker of workers.values()) worker.process.kill('SIGTERM')
  setTimeout(() => process.exit(0), 1000).unref()
}
process.on('SIGTERM', shutdown); process.on('SIGINT', shutdown)
