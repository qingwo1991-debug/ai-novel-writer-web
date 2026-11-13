import type { RendererAPI } from '../shared/desktop-api'
import type { ProjectSessionContext } from '../shared/ipc-channels'
import { getActiveProjectSessionContext, sameProjectSessionContext } from '../shared/project-session-context'
import type { MainGenerationRunSummary, MainGenerationRunView } from './generation/generation-runtime'

// A duplicated tab or a window.open copy inherits sessionStorage, so this key is
// only a copy witness: it proves what the document inherited, never who this tab is.
const CLIENT_SESSION_WITNESS_KEY = 'ai-novel-writer-browser-client-id'
// The browsing-context name is the authoritative per-tab slot: a reload keeps it,
// while Browser "duplicate tab" and window.open start a fresh context without it.
const CLIENT_NAME_PREFIX = 'ai-novel-writer-browser-client:'
const CLIENT_ID_PATTERN = /^[a-zA-Z0-9_-]{16,128}$/u
const OPERATION_PATTERN = /^[a-z][a-z0-9:-]{0,127}$/u
const SEND_OPERATIONS = new Set([
  'window:minimize', 'window:toggle-maximize', 'window:close', 'window:resolve-close', 'llm:cancel',
])

type Listener = (...args: unknown[]) => void
type BrowserEventSource = Pick<EventSource, 'onopen' | 'onmessage' | 'onerror' | 'close'>
/** The per-browsing-context `window.name` slot, injectable for non-document hosts. */
export interface BrowserTabName {
  get(): string
  set(value: string): void
}
export interface BrowserTransportDependencies {
  fetch?: typeof fetch
  createEventSource?: (url: string, options: EventSourceInit) => BrowserEventSource
  storage?: Pick<Storage, 'getItem' | 'setItem'>
  randomId?: () => string
  captureSession?: () => ProjectSessionContext | null
  connectionTimeoutMs?: number
  tabName?: BrowserTabName
}

export class BrowserTransportError extends Error {
  constructor(readonly code: string, message: string, readonly status?: number) {
    super(message)
    this.name = 'BrowserTransportError'
  }
}

export interface BrowserTransport extends RendererAPI {
  readonly clientId: string
  bootstrap(): Promise<void>
  /** Same-origin JSON request; a Blob/File body is forwarded without JSON conversion. */
  request<T = unknown>(path: string, init?: RequestInit): Promise<T>
  /** Best-effort idle project release hint; reconnects and active requests remain protected. */
  depart(): void
  /** Stop the EventSource and remove subscriptions, for page teardown and isolated tests. */
  close(): void
}

function randomClientId(): string {
  const bytes = new Uint8Array(24)
  globalThis.crypto.getRandomValues(bytes)
  return Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join('')
}

/**
 * The real `window.name` slot when a document exists. Reading it is best-effort:
 * a sandboxed or restricted document may reject both access and assignment, and
 * that must only cost duplicate detection, never this page's identity.
 */
function defaultTabName(): BrowserTabName | undefined {
  if (typeof window === 'undefined') return undefined
  const view = window
  return {
    get() { try { return view.name } catch { return '' } },
    set(value) { try { view.name = value } catch { /* Keep this identity for this page lifetime. */ } },
  }
}

// Only OUR name is an identity. An unrelated window name (a legacy popup target,
// another same-origin page) is never trusted or reused as a client id.
function namedClientId(value: string): string | undefined {
  if (!value.startsWith(CLIENT_NAME_PREFIX)) return undefined
  const clientId = value.slice(CLIENT_NAME_PREFIX.length)
  return CLIENT_ID_PATTERN.test(clientId) ? clientId : undefined
}

function issueClientId(
  dependencies: BrowserTransportDependencies,
  storage: Pick<Storage, 'getItem' | 'setItem'> | undefined,
  tabName: BrowserTabName | undefined,
): string {
  const clientId = (dependencies.randomId ?? randomClientId)()
  if (!CLIENT_ID_PATTERN.test(clientId)) throw new BrowserTransportError('BROWSER_CLIENT_ID_INVALID', 'Invalid browser client identity.')
  // The witness is written first so a copy made between the two writes still sees
  // a name that does not match it, and therefore still takes a fresh identity.
  try { storage?.setItem(CLIENT_SESSION_WITNESS_KEY, clientId) } catch { /* Keep this identity for this page lifetime. */ }
  try { tabName?.set(CLIENT_NAME_PREFIX + clientId) } catch { /* Keep this identity for this page lifetime. */ }
  return clientId
}

/**
 * One client id per browsing context. A same-tab reload keeps the name AND the
 * inherited session witness, so it reuses its id (the server owner and project
 * lease survive). A duplicated tab or window.open copy inherits the session
 * witness but not the browsing-context name, so it MUST take a fresh id rather
 * than silently sharing another tab's owner and project lease.
 */
function readClientId(dependencies: BrowserTransportDependencies): string {
  const tabName = dependencies.tabName ?? defaultTabName()
  let storage = dependencies.storage
  let witness: string | undefined
  try {
    storage ??= window.sessionStorage
    const saved = storage.getItem(CLIENT_SESSION_WITNESS_KEY)
    if (saved && CLIENT_ID_PATTERN.test(saved)) witness = saved
  } catch { /* Private browsing/storage restrictions must not prevent a per-tab in-memory identity. */ }
  if (!tabName) {
    // No browsing context to name (non-document host): keep the historical
    // per-session identity so one tab still reuses exactly one client id.
    if (witness) return witness
    return issueClientId(dependencies, storage, tabName)
  }
  const named = namedClientId(tabName.get())
  // Only an inherited witness that DIFFERS from the name proves this document is a
  // copy. An absent witness is never copy evidence: storage being unavailable must
  // not churn the id (and the server lease) on every reload of the same tab.
  if (named && (witness === undefined || witness === named)) return named
  return issueClientId(dependencies, storage, tabName)
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

/**
 * Cookie-authenticated browser backend, never installed on window.aiNovelAPI.
 * Requests are not retried: a lost response must not duplicate a write or a model execution.
 * EventSource owns Last-Event-ID/reconnection; durable read projections fill any replay gaps.
 */
export function createBrowserTransport(dependencies: BrowserTransportDependencies = {}): BrowserTransport {
  const clientId = readClientId(dependencies)
  const requestFetch = dependencies.fetch ?? ((...args: Parameters<typeof fetch>) => globalThis.fetch(...args))
  const createEvents = dependencies.createEventSource ?? ((url, options) => new EventSource(url, options))
  const captureSession = dependencies.captureSession ?? getActiveProjectSessionContext
  const listeners = new Map<string, Set<Listener>>()
  let csrfToken: string | undefined
  let bootstrapPromise: Promise<void> | undefined
  let source: BrowserEventSource | undefined
  let rejectConnection: ((error: BrowserTransportError) => void) | undefined
  let everConnected = false
  let closed = false
  let recoveryPromise: Promise<void> | undefined
  let recoveryRequested = false

  function emit(channel: string, ...args: unknown[]) {
    for (const listener of [...(listeners.get(channel) ?? [])]) {
      try { listener(...args) } catch {
        // One renderer subscriber must not prevent other subscribers from receiving the stream.
        if (channel !== 'browser:transport-error') emit('browser:transport-error', { code: 'BROWSER_EVENT_LISTENER_FAILED', channel })
      }
    }
  }

  function report(error: unknown, operation?: string) {
    // Deliberately omit request bodies, server messages, URLs and credentials from diagnostics.
    emit('browser:transport-error', {
      code: error instanceof BrowserTransportError ? error.code : 'BROWSER_TRANSPORT_FAILED',
      ...(operation ? { operation } : {}),
    })
  }

  async function fetchJSON<T>(path: string, init: RequestInit): Promise<T> {
    let response: Response
    try {
      response = await requestFetch(path, { ...init, credentials: 'same-origin', mode: 'same-origin', redirect: 'error', cache: 'no-store' })
    } catch {
      throw new BrowserTransportError('BROWSER_NETWORK_ERROR', 'The browser backend could not be reached. Check your connection and try again.')
    }
    let body: unknown
    try { body = await response.json() } catch {
      throw new BrowserTransportError('BROWSER_INVALID_RESPONSE', 'The browser backend returned an invalid response.', response.status)
    }
    if (!response.ok) {
      const error = isRecord(body) && isRecord(body.error) ? body.error : undefined
      throw new BrowserTransportError(
        error && typeof error.code === 'string' ? error.code : 'BROWSER_REQUEST_FAILED',
        error && typeof error.message === 'string' ? error.message : `The browser backend rejected the request (${response.status}).`,
        response.status,
      )
    }
    return body as T
  }

  function invalidateBootstrap() {
    csrfToken = undefined
    bootstrapPromise = undefined
    source?.close()
    source = undefined
  }

  async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
    // Do not allow helpers to forward session cookies or CSRF headers to an arbitrary origin.
    if (!path.startsWith('/api/') || path.includes('\\') || /[\r\n]/u.test(path)
      || /(?:^|\/)\.{1,2}(?:\/|\?|$)/u.test(path)) {
      throw new BrowserTransportError('BROWSER_REQUEST_PATH_INVALID', 'Browser requests must use a same-origin API route.')
    }
    await bootstrap()
    const headers = new Headers(init.headers)
    headers.set('X-CSRF-Token', csrfToken!)
    headers.set('X-Client-Id', clientId)
    try {
      return await fetchJSON<T>(path, { ...init, headers })
    } catch (error) {
      if (error instanceof BrowserTransportError && (error.status === 401 || error.status === 403)) invalidateBootstrap()
      throw error
    }
  }

  async function invoke(channel: string, ...args: unknown[]): Promise<unknown> {
    if (!OPERATION_PATTERN.test(channel)) throw new BrowserTransportError('BROWSER_OPERATION_INVALID', 'Invalid backend operation.')
    let body: string
    try { body = JSON.stringify({ args }) } catch {
      throw new BrowserTransportError('BROWSER_ARGUMENTS_INVALID', 'Backend operation arguments are not JSON serializable.')
    }
    const envelope = await request<unknown>(`/api/operations/${encodeURIComponent(channel)}`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body,
    })
    if (!isRecord(envelope) || !Object.hasOwn(envelope, 'result')) {
      // A business refusal arrives as {error:{code,message}}; keep the stable
      // code so callers (and the workflow's resume retry) can see the real
      // refusal instead of a masked BROWSER_INVALID_RESPONSE.
      if (isRecord(envelope) && isRecord(envelope.error)
        && typeof envelope.error.code === 'string' && envelope.error.code.trim()) {
        const message = typeof envelope.error.message === 'string' ? envelope.error.message : envelope.error.code
        throw new BrowserTransportError(envelope.error.code, message)
      }
      throw new BrowserTransportError('BROWSER_INVALID_RESPONSE', 'The browser backend returned an invalid operation result.')
    }
    return envelope.result
  }

  async function refreshSnapshots() {
    const session = captureSession()
    const sameSession = () => session && sameProjectSessionContext(session, captureSession())
    const tasks: Promise<unknown>[] = []
    if (session && listeners.get('generation:snapshot')?.size) {
      tasks.push((async () => {
        // Use the existing server-owned run ledger, with the captured project lease on every read.
        const summaries = await invoke('generation:list', session) as MainGenerationRunSummary[]
        if (!sameSession() || !Array.isArray(summaries)) return
        for (const summary of summaries) {
          if (!sameSession() || closed) return
          if (!isRecord(summary) || !isRecord(summary.handle) || summary.handle.projectId !== session.projectId) continue
          const expectedHandle = summary.handle
          const view = await invoke('generation:read', expectedHandle, session) as MainGenerationRunView
          if (!sameSession() || closed) return
          if (!isRecord(view) || !isRecord(view.handle) || !Array.isArray(view.artifacts)) {
            throw new BrowserTransportError('BROWSER_SNAPSHOT_IDENTITY_MISMATCH', 'The recovered run identity did not match.')
          }
          const recoveredHandle = view.handle
          if (['projectId', 'epoch', 'rootActionId', 'runId'].some(key => recoveredHandle[key as keyof typeof recoveredHandle] !== expectedHandle[key])) {
            throw new BrowserTransportError('BROWSER_SNAPSHOT_IDENTITY_MISMATCH', 'The recovered run identity did not match.')
          }
          for (const snapshot of view.artifacts) emit('generation:snapshot', snapshot)
        }
      })().catch(error => report(error, 'generation:read')))
    }
    if (listeners.get('update:state')?.size) {
      tasks.push(invoke('update:get-state').then(state => { if (!closed) emit('update:state', state) })
        .catch(error => report(error, 'update:get-state')))
    }
    await Promise.all(tasks)
  }

  function recover() {
    recoveryRequested = true
    if (recoveryPromise) return
    recoveryPromise = (async () => {
      while (recoveryRequested && !closed) {
        recoveryRequested = false
        await refreshSnapshots()
      }
    })().catch(error => report(error)).finally(() => { recoveryPromise = undefined })
  }

  function connectEvents(): Promise<void> {
    return new Promise((resolve, reject) => {
      let initialOpen = false
      let timer: ReturnType<typeof setTimeout> | undefined
      const fail = (error: BrowserTransportError) => {
        if (initialOpen) return
        if (timer !== undefined) clearTimeout(timer)
        stream?.close()
        if (source === stream) source = undefined
        rejectConnection = undefined
        reject(error)
      }
      let stream: BrowserEventSource
      try {
        // Authentication is exclusively the HttpOnly session cookie; only a non-secret tab ID is in this URL.
        stream = createEvents(`/api/events?client=${encodeURIComponent(clientId)}`, { withCredentials: true })
      } catch {
        reject(new BrowserTransportError('BROWSER_EVENTS_UNAVAILABLE', 'The browser event stream could not be opened.'))
        return
      }
      source?.close()
      source = stream
      rejectConnection = fail
      timer = setTimeout(() => fail(new BrowserTransportError('BROWSER_EVENTS_TIMEOUT', 'The browser event stream did not connect. Try again.')), dependencies.connectionTimeoutMs ?? 15_000)
      stream.onopen = () => {
        if (closed || source !== stream) return
        const reconnect = everConnected
        everConnected = true
        initialOpen = true
        if (timer !== undefined) clearTimeout(timer)
        rejectConnection = undefined
        resolve()
        emit('browser:connection', { state: 'connected', clientId })
        if (reconnect) {
          emit('browser:reconnected', { clientId })
          // Queue after bootstrap settles, so recovered reads never wait on their own connection promise.
          void Promise.resolve().then(recover)
        }
      }
      stream.onerror = () => {
        if (closed || source !== stream) return
        emit('browser:connection', { state: 'reconnecting', clientId })
        if (!initialOpen) fail(new BrowserTransportError('BROWSER_EVENTS_UNAVAILABLE', 'The browser event stream could not be reached. Try again.'))
        // Once connected, native EventSource retries with its Last-Event-ID; never replay writes here.
      }
      stream.onmessage = event => {
        if (closed || source !== stream) return
        let envelope: unknown
        try { envelope = JSON.parse(event.data) } catch {
          report(new BrowserTransportError('BROWSER_EVENT_INVALID', 'Invalid browser event.'))
          return
        }
        if (!isRecord(envelope) || typeof envelope.channel !== 'string' || !Array.isArray(envelope.args)) {
          report(new BrowserTransportError('BROWSER_EVENT_INVALID', 'Invalid browser event.'))
          return
        }
        // browser:* lifecycle notifications are local-only, except for the explicit server dialog protocol.
        if (envelope.channel.startsWith('browser:') && !new Set(['browser:dialog', 'browser:download-ready', 'browser:open-external', 'browser:project-lost']).has(envelope.channel)) return
        emit(envelope.channel, ...envelope.args)
      }
    })
  }

  function bootstrap(): Promise<void> {
    if (closed) return Promise.reject(new BrowserTransportError('BROWSER_TRANSPORT_CLOSED', 'The browser backend connection has been closed.'))
    if (!bootstrapPromise) {
      bootstrapPromise = (async () => {
        const body = await fetchJSON<unknown>('/api/bootstrap', { method: 'GET', headers: { 'X-Client-Id': clientId } })
        if (!isRecord(body) || typeof body.csrfToken !== 'string' || !body.csrfToken.trim()) {
          throw new BrowserTransportError('BROWSER_BOOTSTRAP_INVALID', 'The browser backend did not provide a session token.')
        }
        if (closed) throw new BrowserTransportError('BROWSER_TRANSPORT_CLOSED', 'The browser backend connection has been closed.')
        csrfToken = body.csrfToken
        await connectEvents()
      })().catch(error => {
        csrfToken = undefined
        bootstrapPromise = undefined
        throw error
      })
    }
    return bootstrapPromise
  }

  return {
    clientId,
    bootstrap,
    request,
    invoke,
    depart() {
      if (!csrfToken || closed) return
      // No bootstrap or retry during navigation. Header-bound CSRF remains in
      // force; sendBeacon cannot supply these headers and is not a substitute.
      void requestFetch('/api/release', { method: 'POST', credentials: 'same-origin',
        mode: 'same-origin', redirect: 'error', cache: 'no-store', keepalive: true,
        headers: { 'X-CSRF-Token': csrfToken, 'X-Client-Id': clientId } }).catch(() => {})
    },
    on(channel, listener) {
      if (closed) throw new BrowserTransportError('BROWSER_TRANSPORT_CLOSED', 'The browser backend connection has been closed.')
      let subscriptions = listeners.get(channel)
      if (!subscriptions) { subscriptions = new Set(); listeners.set(channel, subscriptions) }
      subscriptions.add(listener)
      void bootstrap().catch(error => report(error))
      return () => {
        subscriptions.delete(listener)
        if (!subscriptions.size) listeners.delete(channel)
      }
    },
    once(channel, listener) {
      const unsubscribe = this.on(channel, (...args) => { unsubscribe(); listener(...args) })
    },
    send(channel, ...args) {
      if (!SEND_OPERATIONS.has(channel)) throw new BrowserTransportError('BROWSER_SEND_UNSUPPORTED', 'This operation is not an allowed one-way browser request.')
      void invoke(channel, ...args).catch(error => report(error, channel))
    },
    // Native browser scaling belongs to browser/UI preferences, never Electron webFrame.
    setZoomLevel: () => {},
    setZoomFactor: () => {},
    getZoomLevel: () => 0,
    close() {
      if (closed) return
      closed = true
      rejectConnection?.(new BrowserTransportError('BROWSER_TRANSPORT_CLOSED', 'The browser backend connection has been closed.'))
      source?.close()
      source = undefined
      listeners.clear()
    },
  }
}

let browserTransport: BrowserTransport | undefined
export function getBrowserTransport(): BrowserTransport {
  return browserTransport ??= createBrowserTransport()
}
export function getBrowserClientId(): string { return getBrowserTransport().clientId }
export function bootstrapBrowserTransport(): Promise<void> { return getBrowserTransport().bootstrap() }
export function browserRequest<T = unknown>(path: string, init?: RequestInit): Promise<T> {
  return getBrowserTransport().request<T>(path, init)
}
