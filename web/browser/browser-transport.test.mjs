import { afterEach, test } from 'node:test'
import assert from 'node:assert/strict'
import { setImmediate } from 'node:timers/promises'
import { BrowserTransportError, createBrowserTransport } from '../../src/services/browser-transport.ts'
import { getAPI, ipc } from '../../src/services/ipc-client.ts'
import { setActiveProjectSessionContext } from '../../src/shared/project-session-context.ts'

const clientId = 'test-browser-tab-identity-123'
const transports = []
const session = { projectId: 'book-a', leaseId: 'lease-a', projectPath: '/books/a' }
const handle = { projectId: 'book-a', epoch: 'lease-a', rootActionId: 'action-a', runId: 'run-a' }
const snapshot = { ...handle, artifactId: 'artifact-a', attemptId: 'attempt-a', revision: 4, durableRevision: 4, text: '恢复内容', textHash: 'hash', status: 'completed' }
const json = (body, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

function fixture(options = {}) {
  const calls = []
  const streams = []
  const storage = options.storage ?? new Map()
  const transport = createBrowserTransport({
    randomId: () => clientId,
    storage: { getItem: key => storage.get(key) ?? null, setItem: (key, value) => storage.set(key, value) },
    captureSession: options.captureSession ?? (() => null),
    connectionTimeoutMs: options.timeout ?? 100,
    fetch: async (path, init) => {
      calls.push({ path, init })
      if (options.fetch) return options.fetch(path, init)
      if (path === '/api/bootstrap') return json({ csrfToken: 'csrf-only-in-header' })
      return json({ result: { success: true } })
    },
    createEventSource(url, init) {
      const stream = {
        url, init, onopen: null, onerror: null, onmessage: null, closed: false,
        close() { this.closed = true },
        open() { this.onopen?.({}) },
        error() { this.onerror?.({}) },
        message(value) { this.onmessage?.({ data: typeof value === 'string' ? value : JSON.stringify(value) }) },
      }
      streams.push(stream)
      if (options.autoOpen !== false) queueMicrotask(() => stream.open())
      return stream
    },
  })
  transports.push(transport)
  return { transport, calls, streams, storage }
}

async function until(predicate) {
  for (let index = 0; index < 30; index += 1) {
    if (predicate()) return
    await setImmediate()
  }
  assert.fail('Expected asynchronous transport work did not finish')
}

afterEach(() => {
  for (const transport of transports.splice(0)) transport.close()
  delete globalThis.__BROWSER_TEST_ENV__
  delete globalThis.window
  setActiveProjectSessionContext(null)
})

test('bootstraps once, waits for SSE, and posts encoded operations with cookie/CSRF/tab headers', async () => {
  const { transport, calls, streams } = fixture({ autoOpen: false })
  const response = transport.invoke('config:get')
  await until(() => streams.length === 1)
  assert.equal(calls.length, 1, 'operation must wait for the stream so dialog events cannot be lost')
  streams[0].open()
  assert.deepEqual(await response, { success: true })
  assert.equal(calls[0].path, '/api/bootstrap')
  assert.equal(streams[0].url, `/api/events?client=${clientId}`)
  assert.deepEqual(streams[0].init, { withCredentials: true })
  assert.ok(!streams[0].url.includes('csrf'))
  const operation = calls[1]
  assert.equal(operation.path, '/api/operations/config%3Aget')
  assert.equal(operation.init.method, 'POST')
  assert.deepEqual(JSON.parse(operation.init.body), { args: [] })
  assert.equal(operation.init.credentials, 'same-origin')
  assert.equal(operation.init.mode, 'same-origin')
  assert.equal(operation.init.redirect, 'error')
  assert.equal(operation.init.headers.get('X-CSRF-Token'), 'csrf-only-in-header')
  assert.equal(operation.init.headers.get('X-Client-Id'), clientId)
  await Promise.all([transport.bootstrap(), transport.invoke('config:get')])
  assert.equal(calls.filter(call => call.path === '/api/bootstrap').length, 1)
})

test('page departure is an authenticated keepalive hint, never bootstraps or retries writes', async () => {
  const f = fixture()
  f.transport.depart(); assert.equal(f.calls.length, 0)
  await f.transport.bootstrap()
  f.transport.depart()
  const call = f.calls.at(-1)
  assert.equal(call.path, '/api/release'); assert.equal(call.init.method, 'POST')
  assert.equal(call.init.keepalive, true); assert.equal(call.init.credentials, 'same-origin')
  assert.equal(call.init.headers['X-CSRF-Token'], 'csrf-only-in-header')
  assert.equal(call.init.headers['X-Client-Id'], clientId)
  f.transport.close(); f.transport.depart()
  assert.equal(f.calls.filter(call => call.path === '/api/release').length, 1)
})

test('keeps a random ID in this tab sessionStorage and works when storage is disabled', () => {
  const first = fixture()
  const second = fixture({ storage: first.storage })
  assert.equal(first.transport.clientId, second.transport.clientId)
  const isolated = createBrowserTransport({
    randomId: () => 'another-tab-identity-1234',
    storage: { getItem() { throw Error('disabled') }, setItem() { throw Error('disabled') } },
  })
  transports.push(isolated)
  assert.equal(isolated.clientId, 'another-tab-identity-1234')
})

test('gives a duplicated tab or window.open copy its own id while a reload keeps its id', () => {
  const storage = new Map()
  const shared = { getItem: key => storage.get(key) ?? null, setItem: (key, value) => storage.set(key, value) }
  const issued = ['original-tab-identity-1234', 'duplicate-tab-identity-1234', 'opener-copy-identity-1234', 'storage-free-identity-1234']
  let next = 0
  const nameStore = () => { let value = ''; return { get: () => value, set: updated => { value = updated } } }
  const build = (tabName, store = shared) => {
    const transport = createBrowserTransport({
      randomId: () => issued[next++],
      storage: store,
      tabName,
      fetch: async path => json(path === '/api/bootstrap' ? { csrfToken: 'token' } : { result: null }),
      createEventSource: () => ({ onopen: null, onerror: null, onmessage: null, close() {} }),
    })
    transports.push(transport)
    return transport
  }
  const tab = nameStore()
  const original = build(tab)
  assert.equal(original.clientId, issued[0])
  // Same browsing context (reload): the surviving name and the inherited witness agree.
  assert.equal(build(tab).clientId, issued[0])
  assert.equal(next, 1, 'a reload must not consume a fresh identity')
  // Chrome/Edge "Duplicate tab" and window.open inherit sessionStorage but not window.name.
  const duplicate = build(nameStore())
  const openerCopy = build(nameStore())
  assert.equal(duplicate.clientId, issued[1])
  assert.equal(openerCopy.clientId, issued[2])
  assert.notEqual(duplicate.clientId, original.clientId)
  assert.notEqual(openerCopy.clientId, original.clientId)
  assert.notEqual(openerCopy.clientId, duplicate.clientId)
  // Storage being unavailable must not churn the identity on every reload either.
  const throwing = { getItem() { throw Error('disabled') }, setItem() { throw Error('disabled') } }
  const noStorageTab = nameStore()
  assert.equal(build(noStorageTab, throwing).clientId, issued[3])
  assert.equal(build(noStorageTab, throwing).clientId, issued[3])
  assert.equal(next, 4)
})

test('subscriptions and once receive envelope args, reject malformed events, and cannot spoof lifecycle', async () => {
  const { transport, streams } = fixture()
  const events = [], once = [], errors = [], reconnected = []
  const detach = transport.on('generation:snapshot', (...args) => events.push(args))
  transport.once('generation:snapshot', (...args) => once.push(args))
  transport.on('browser:transport-error', error => errors.push(error))
  transport.on('browser:reconnected', event => reconnected.push(event))
  await transport.bootstrap()
  const stream = streams[0]
  stream.message({ channel: 'generation:snapshot', args: [snapshot, 'second'] })
  stream.message({ channel: 'generation:snapshot', args: [snapshot] })
  assert.equal(events.length, 2)
  assert.deepEqual(events[0], [snapshot, 'second'])
  assert.equal(once.length, 1)
  detach()
  stream.message({ channel: 'generation:snapshot', args: [snapshot] })
  stream.message('not json')
  stream.message({ channel: 'generation:snapshot', args: {} })
  stream.message({ channel: 'browser:reconnected', args: [{}] })
  assert.equal(events.length, 2)
  assert.equal(errors.length, 2)
  assert.equal(reconnected.length, 0)
  const dialogs = []
  transport.on('browser:dialog', event => dialogs.push(event))
  stream.message({ channel: 'browser:dialog', args: [{ id: 'dialog-1', kind: 'open', options: {} }] })
  assert.equal(dialogs[0].id, 'dialog-1')
})

test('explicit server download, external-link and project-loss events do not permit lifecycle injection', async () => {
  const { transport, streams } = fixture()
  const delivered = new Map()
  const allowed = ['browser:download-ready', 'browser:open-external', 'browser:project-lost']
  const localOnly = ['browser:reconnected', 'browser:connection', 'browser:transport-error', 'browser:unknown']
  for (const channel of [...allowed, ...localOnly]) transport.on(channel, event => {
    const events = delivered.get(channel) ?? []
    events.push(event)
    delivered.set(channel, events)
  })
  await transport.bootstrap()
  delivered.clear() // Ignore the genuine local connection notification.
  for (const channel of [...allowed, ...localOnly]) streams[0].message({ channel, args: [{ fromServer: channel }] })
  for (const channel of allowed) assert.deepEqual(delivered.get(channel), [{ fromServer: channel }])
  for (const channel of localOnly) assert.equal(delivered.has(channel), false, `${channel} must remain local-only`)
})

test('reconnect uses only existing read operations and re-emits durable snapshots with captured session', async () => {
  const { transport, calls, streams } = fixture({ captureSession: () => session, fetch(path) {
    if (path === '/api/bootstrap') return json({ csrfToken: 'token' })
    if (path.endsWith('generation%3Alist')) return json({ result: [{ handle, status: 'completed' }] })
    if (path.endsWith('generation%3Aread')) return json({ result: { handle, artifacts: [snapshot] } })
    if (path.endsWith('update%3Aget-state')) return json({ result: { state: 'idle' } })
    assert.fail(`Unexpected recovery operation ${path}`)
  } })
  const received = [], reconnects = [], updates = []
  transport.on('generation:snapshot', value => received.push(value))
  transport.on('browser:reconnected', value => reconnects.push(value))
  transport.on('update:state', value => updates.push(value))
  await transport.bootstrap()
  streams[0].error()
  streams[0].open()
  await until(() => received.length === 1 && updates.length === 1)
  assert.equal(reconnects.length, 1)
  assert.deepEqual(received, [snapshot])
  assert.deepEqual(JSON.parse(calls.find(call => call.path.endsWith('generation%3Alist')).init.body), { args: [session] })
  assert.deepEqual(JSON.parse(calls.find(call => call.path.endsWith('generation%3Aread')).init.body), { args: [handle, session] })
  assert.ok(calls.every(call => call.path === '/api/bootstrap' || /(?:generation%3A(?:list|read)|update%3Aget-state)$/u.test(call.path)))
})

test('does not project a recovered snapshot into a reopened project lease', async () => {
  let current = session
  let resolveRead
  const { transport, streams } = fixture({ captureSession: () => current, fetch(path) {
    if (path === '/api/bootstrap') return json({ csrfToken: 'token' })
    if (path.endsWith('generation%3Alist')) return json({ result: [{ handle }] })
    return new Promise(resolve => { resolveRead = resolve })
  } })
  const received = []
  transport.on('generation:snapshot', value => received.push(value))
  await transport.bootstrap()
  streams[0].open()
  await until(() => resolveRead)
  current = { ...session, leaseId: 'lease-reopened' }
  resolveRead(json({ result: { handle, artifacts: [snapshot] } }))
  await setImmediate()
  assert.deepEqual(received, [])
})

test('auth errors invalidate bootstrap but never replay the rejected operation', async () => {
  let bootstrapCount = 0, writes = 0
  const { transport } = fixture({ fetch(path) {
    if (path === '/api/bootstrap') return json({ csrfToken: `token-${++bootstrapCount}` })
    writes += 1
    return writes === 1 ? json({ error: { code: 'CSRF_INVALID', message: 'Session expired' } }, 403) : json({ result: 'ok' })
  } })
  await assert.rejects(transport.invoke('config:set', { theme: 'dark' }), error => error instanceof BrowserTransportError && error.code === 'CSRF_INVALID')
  assert.equal(writes, 1)
  assert.equal(bootstrapCount, 1)
  assert.equal(await transport.invoke('config:get'), 'ok')
  assert.equal(bootstrapCount, 2)
})

test('upload/dialog helper forwards raw bodies and cannot leak credentials to external routes', async () => {
  const { transport, calls } = fixture()
  const file = new Blob(['中文文件'], { type: 'text/plain' })
  await transport.request('/api/uploads', { method: 'POST', body: file, headers: { 'X-Upload-Name': encodeURIComponent('小说.txt') } })
  const upload = calls[1]
  assert.equal(upload.init.body, file)
  assert.equal(upload.init.headers.get('X-Upload-Name'), encodeURIComponent('小说.txt'))
  assert.equal(upload.init.headers.get('X-Client-Id'), clientId)
  assert.equal(upload.init.headers.get('X-CSRF-Token'), 'csrf-only-in-header')
  for (const path of ['https://example.com/api/', '//example.com/api/', '/api/../secret', '/api/\\evil']) {
    await assert.rejects(transport.request(path), error => error.code === 'BROWSER_REQUEST_PATH_INVALID')
  }
  assert.equal(calls.length, 2)
})

test('one-way send is a closed set of explicit POST operations; browser zoom is inert', async () => {
  const { transport, calls } = fixture()
  assert.throws(() => transport.send('arbitrary:execute'), error => error.code === 'BROWSER_SEND_UNSUPPORTED')
  await assert.rejects(transport.invoke('../arbitrary'), error => error.code === 'BROWSER_OPERATION_INVALID')
  transport.send('window:close')
  await until(() => calls.length === 2)
  assert.equal(calls[1].path, '/api/operations/window%3Aclose')
  assert.equal(transport.getZoomLevel(), 0)
  transport.setZoomLevel(4)
  transport.setZoomFactor(2)
  assert.equal(transport.getZoomLevel(), 0)
})

test('unreachable/malformed backends reject without leaking request data or hanging', async () => {
  const invalid = fixture({ fetch: () => json({ unrelated: 'response' }) })
  await assert.rejects(invalid.transport.bootstrap(), error => error.code === 'BROWSER_BOOTSTRAP_INVALID')
  const network = fixture({ fetch() { throw Error('request may contain api-key') } })
  await assert.rejects(network.transport.bootstrap(), error => error.code === 'BROWSER_NETWORK_ERROR' && !error.message.includes('api-key'))
  const noEvents = fixture({ autoOpen: false, timeout: 5 })
  await assert.rejects(noEvents.transport.bootstrap(), error => error.code === 'BROWSER_EVENTS_TIMEOUT')
  const badResult = fixture({ fetch: path => json(path === '/api/bootstrap' ? { csrfToken: 'token' } : {}) })
  await assert.rejects(badResult.transport.invoke('config:get'), error => error.code === 'BROWSER_INVALID_RESPONSE')
})

test('runtime selection is explicit and keeps desktop identity separate from browser backend availability', () => {
  globalThis.__BROWSER_TEST_ENV__ = { MODE: 'test' }
  assert.equal(ipc.hasBackend, false)
  const desktop = { invoke() {} }
  globalThis.window = { aiNovelAPI: desktop }
  assert.equal(ipc.isElectron, true)
  assert.equal(ipc.hasBackend, true)
  assert.equal(ipc.runtimeKind, 'electron')
  assert.equal(getAPI(), desktop)
  for (const env of [{ MODE: 'web' }, { MODE: 'production', VITE_RUNTIME: 'web' }]) {
    globalThis.__BROWSER_TEST_ENV__ = env
    assert.equal(ipc.isElectron, false)
    assert.equal(ipc.hasBackend, true)
    assert.equal(ipc.runtimeKind, 'web')
  }
  globalThis.__BROWSER_TEST_ENV__ = { MODE: 'test' }
  globalThis.window = { velaAPI: desktop }
  assert.equal(ipc.runtimeKind, 'unavailable')
  assert.throws(() => getAPI(), /aiNovelAPI 未就绪/u)
})

test('typed ipc preserves explicit/active project sessions and grant channels on the desktop path', async () => {
  const calls = []
  globalThis.__BROWSER_TEST_ENV__ = { MODE: 'test' }
  globalThis.window = { aiNovelAPI: { invoke: (...args) => { calls.push(args); return Promise.resolve('ok') } } }
  setActiveProjectSessionContext(session)
  await ipc.invoke('db:blueprint-get-all', session.projectPath)
  assert.deepEqual(calls[0], ['db:blueprint-get-all', session.projectPath, session])
  const frozen = { ...session, leaseId: 'frozen-lease' }
  await ipc.invokeWithProjectSession(frozen, 'generation:read', handle)
  assert.deepEqual(calls[1], ['generation:read', handle, frozen])
  setActiveProjectSessionContext(null)
  await ipc.invoke('fs:grant-write-file', 'grant-1', '正文.txt', '正文')
  assert.deepEqual(calls[2], ['fs:grant-write-file', 'grant-1', '正文.txt', '正文'])
  await assert.rejects(ipc.invoke('db:blueprint-get-all', session.projectPath), /GENERATION_PROJECT_SESSION_REQUIRED/u)
})
