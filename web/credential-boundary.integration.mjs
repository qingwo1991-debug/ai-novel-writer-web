/**
 * Real native-image HTTP/SSE credential-boundary canary; no sanitizer imports.
 * Run ONLY after the image owner declares the immutable image ready:
 *   NOVEL_CANARY_READY_IMAGE=sha256:<ready-image-id> node web/credential-boundary.integration.mjs
 *
 * Creates only novel-native-canary-* containers/network and a fresh, ignored
 * .native-test-data fixture. The internal Docker network has no external route;
 * HTTP runs directly over the fresh private bridge IP (no published ports).
 * The fake provider is the sole explicitly allowlisted origin. No production data/keys,
 * existing containers, external providers, dependency installation, or paid API.
 * All owned containers/network/fixtures are removed, including on failure.
 *
 * Recorded acceptance: 2026-11-02, run mvyfvnni-0ff932e6, exit 0, image
 * sha256:c0234f4769ea7d1b361933aa81eb35886a274055179db3bd6ddd330b3d759ed5.
 * 33 audited HTTP JSON responses; 7 audited SSE events (one dialog plus three
 * live and three replayed generation events); 6 genuine fake-provider calls.
 * Every call received the original Authorization; model JSON stayed byte-identical
 * (0600), private provider logs and SQLite history retained the original canary.
 * All owned resources removed; original official/test/relay remained unchanged.
 * Setup-only failures before this successful run: Docker29 internal networks do
 * not publish ports; canonical model JSON lives below bootstrap.generation.
 * Neither was a product sanitizer failure; both are handled by this runner.
 *
 * Scope: raw + decoded JSON, live/replayed SSE, saved/blank-key credentials,
 * connection errors, generation content/errors, execution leases, history data,
 * startup registration, and private-storage/Authorization nonmutation evidence.
 * Excludes browser DOM/authenticated reverse proxy/TLS, production volumes,
 * arbitrary secret detection, short/encoded/split-across-event credentials,
 * other provider protocols, cloud credentials, and concurrent-writer behavior.
 */
import crypto from 'node:crypto'
import fs from 'node:fs'
import http from 'node:http'
import path from 'node:path'
import { execFileSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { setTimeout as delay } from 'node:timers/promises'

const script = fileURLToPath(import.meta.url)
const root = path.resolve(path.dirname(script), '..')

// This mode runs in the isolated fake-provider container, not the gateway.
async function fakeProvider() {
  const { key, control } = JSON.parse(fs.readFileSync(process.env.NOVEL_CANARY_PRIVATE_FIXTURE, 'utf8'))
  const calls = []
  const server = http.createServer(async (req, res) => {
    if (req.url === '/__proof' && req.headers['x-canary-control'] === control) {
      res.writeHead(200, { 'Content-Type': 'application/json' })
      res.end(JSON.stringify({ calls, allAuthorizationExact: calls.length > 0 && calls.every(call => call.authorizationExact) }))
      return
    }
    if (req.method !== 'POST' || req.url !== '/v1/chat/completions') {
      res.writeHead(404); res.end('unexpected fixture route'); return
    }
    let bytes = ''
    for await (const chunk of req) bytes += chunk
    let input
    try { input = JSON.parse(bytes) } catch { res.writeHead(400); res.end('invalid fixture request'); return }
    const authorizationExact = req.headers.authorization === `Bearer ${key}`
    const mode = input.model
    const call = { mode, stream: input.stream === true, authorizationExact, sentCanary: false }
    calls.push(call)
    if (!authorizationExact) { res.writeHead(401); res.end('fixture credential was mutated'); return }
    // Deliberately raw trusted-provider logs must stay private, not be rewritten.
    console.log(`private-provider-log:${key};mode=${mode}`)
    if (mode === 'canary-error' || mode === 'canary-stream-error') {
      call.sentCanary = true
      res.writeHead(400, { 'Content-Type': 'application/json' })
      res.end(JSON.stringify({ error: { message: `provider-error:${key}`, detail: `payload-value:${key}`, apiKey: key } }))
      return
    }
    if (mode !== 'canary-content' && mode !== 'canary-stream') {
      res.writeHead(400); res.end('unexpected fixture model'); return
    }
    call.sentCanary = true
    if (input.stream) {
      res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store' })
      res.write(`data: ${JSON.stringify({ choices: [{ delta: { content: `stream-content:${key}` }, finish_reason: null }] })}\n\n`)
      res.write(`data: ${JSON.stringify({ choices: [{ delta: {}, finish_reason: 'stop' }], usage: { prompt_tokens: 3, completion_tokens: 5, total_tokens: 8 } })}\n\n`)
      res.end('data: [DONE]\n\n')
    } else {
      res.writeHead(200, { 'Content-Type': 'application/json' })
      res.end(JSON.stringify({ choices: [{ message: { content: `json-content:${key}` }, finish_reason: 'stop' }], usage: { prompt_tokens: 3, completion_tokens: 5, total_tokens: 8 } }))
    }
  })
  server.listen(7000, '0.0.0.0')
  process.on('SIGTERM', () => server.close(() => process.exit(0)))
}

async function main() {
  const image = process.env.NOVEL_CANARY_READY_IMAGE
  if (!/^sha256:[a-f0-9]{64}$/.test(image ?? '')) throw new Error('Set NOVEL_CANARY_READY_IMAGE to the explicitly READY immutable image ID; tags are intentionally refused')
  const runId = `${Date.now().toString(36)}-${crypto.randomBytes(4).toString('hex')}`
  const prefix = `novel-native-canary-${runId}`
  const network = `${prefix}-net`, gateway = `${prefix}-gateway`, provider = `${prefix}-provider`
  const fakeOrigin = `http://${provider}:7000`
  const fixture = path.join(root, '.native-test-data', prefix)
  const data = path.join(fixture, 'data'), privateFile = path.join(fixture, 'private.json')
  const key = `sk-native-canary-${crypto.randomBytes(24).toString('hex')}`
  const control = crypto.randomBytes(32).toString('hex')
  const privateValues = [key, control]
  const checks = [], containers = [], collectors = [], cleanupErrors = []
  let createdNetwork = false, createdFixture = false, failure, base, origin, privateModelBytes
  let browserResponses = 0, auditedEvents = 0, liveEvents = 0, replayEvents = 0, providerCalls = 0
  const safe = value => privateValues.reduce((output, secret) => output.split(secret).join('[PRIVATE]'), String(value))
  function require(value, label) { if (!value) throw new Error(label) }
  function passed(label) { checks.push(label) }
  function docker(args) {
    try { return execFileSync('docker', args, { encoding: 'utf8', timeout: 45000, maxBuffer: 4 * 1024 * 1024, stdio: ['ignore', 'pipe', 'pipe'] }).trim() }
    catch (error) { throw new Error(`docker ${args[0]} failed: ${safe(error.stderr?.toString() || error.message)}`) }
  }
  function audit(label, wire, value) {
    require(!wire.includes(key), `${label}: literal credential in public wire`)
    const visit = item => {
      if (typeof item === 'string') require(!item.includes(key), `${label}: literal credential in decoded public payload`)
      else if (item && typeof item === 'object') for (const [name, child] of Object.entries(item)) {
        require(!name.includes(key), `${label}: literal credential in public property name`)
        if (['apikey', 'password', 'accesstoken', 'secret'].includes(name.toLowerCase())) require(child === '', `${label}: nonblank public credential field`)
        visit(child)
      }
    }
    visit(value)
  }
  async function wireRequest(route, options = {}) {
    const response = await fetch(base + route, { ...options, redirect: 'error', signal: options.signal ?? AbortSignal.timeout(20000) })
    const text = await response.text()
    let value
    try { value = JSON.parse(text) } catch { throw new Error(`${route}: expected JSON response`) }
    audit(route, `${JSON.stringify([...response.headers])}\n${text}`, value)
    browserResponses++
    return { response, value, text }
  }
  function client() {
    let cookie = '', csrf = ''
    const id = `native-canary-${crypto.randomBytes(12).toString('hex')}`
    const headers = extra => ({ 'X-Novel-User': 'canary-admin', 'X-Client-Id': id, ...(cookie ? { Cookie: cookie } : {}), ...extra })
    return {
      id,
      headers,
      generation: undefined,
      csrfHeaders(extra = {}) { return headers({ Origin: origin, 'Content-Type': 'application/json', 'X-CSRF-Token': csrf, ...extra }) },
      async boot() {
        const result = await wireRequest('/api/bootstrap', { headers: headers({ Origin: origin }) })
        require(result.response.status === 200, 'bootstrap HTTP status')
        require(/HttpOnly/i.test(result.response.headers.get('set-cookie') ?? ''), 'session cookie HttpOnly')
        cookie = result.response.headers.get('set-cookie')?.split(';')[0] ?? ''
        csrf = result.value.csrfToken
        this.generation = result.value.generation
        require(cookie && typeof csrf === 'string' && csrf.length > 20, 'bootstrap session cookie and CSRF')
      },
      async invoke(operation, args = [], status = 200, overrides = {}) {
        const result = await wireRequest('/api/operations/' + encodeURIComponent(operation), {
          method: 'POST', headers: headers({ Origin: origin, 'Sec-Fetch-Site': 'same-origin', 'Content-Type': 'application/json', 'X-CSRF-Token': csrf, ...overrides }), body: JSON.stringify({ args }),
        })
        require(result.response.status === status, `${operation}: unexpected HTTP status ${result.response.status}`)
        return status === 200 ? result.value.result : result.value
      },
    }
  }
  async function openEvents(caller, lastEventId) {
    const controller = new AbortController()
    const response = await fetch(`${base}/api/events?client=${caller.id}`, { headers: caller.headers({ Origin: origin, ...(lastEventId ? { 'Last-Event-ID': String(lastEventId) } : {}) }), signal: controller.signal, redirect: 'error' })
    require(response.status === 200 && response.headers.get('content-type')?.startsWith('text/event-stream'), 'SSE HTTP response')
    const entries = [], waiters = new Set(), reader = response.body.getReader()
    let buffer = '', completeWire = '', stopped = false, pumpError
    const decoder = new TextDecoder()
    function notify(error) {
      if (error) pumpError = error
      for (const waiter of [...waiters]) {
        const found = entries.find(waiter.predicate)
        if (found || pumpError) { clearTimeout(waiter.timer); waiters.delete(waiter); found ? waiter.resolve(found) : waiter.reject(pumpError) }
      }
    }
    const pump = (async () => {
      try {
        while (true) {
          const { value, done } = await reader.read()
          if (done) break
          const text = decoder.decode(value, { stream: true })
          completeWire += text; buffer += text
          require(!completeWire.includes(key), 'SSE literal credential across transport chunks')
          let end
          while ((end = buffer.indexOf('\n\n')) !== -1) {
            const frame = buffer.slice(0, end); buffer = buffer.slice(end + 2)
            const lines = frame.split('\n'), dataLines = lines.filter(line => line.startsWith('data:')).map(line => line.slice(5).trimStart())
            if (!dataLines.length) continue
            const event = JSON.parse(dataLines.join('\n'))
            audit(lastEventId ? 'replayed SSE' : 'live SSE', frame, event)
            auditedEvents++
            const sequence = Number(lines.find(line => line.startsWith('id:'))?.slice(3))
            require(Number.isSafeInteger(sequence) && sequence > 0, 'SSE event sequence')
            entries.push({ id: sequence, ...event }); notify()
          }
        }
        if (!stopped) notify(new Error('SSE closed before requested evidence'))
      } catch (error) { if (!stopped) notify(error) }
    })()
    const collector = {
      entries,
      waitFor(predicate, label) {
        const existing = entries.find(predicate)
        if (existing) return Promise.resolve(existing)
        if (pumpError) return Promise.reject(pumpError)
        return new Promise((resolve, reject) => {
          const waiter = { predicate, resolve, reject, timer: setTimeout(() => { waiters.delete(waiter); reject(new Error(`${label}: SSE evidence timeout`)) }, 20000) }
          waiters.add(waiter)
        })
      },
      async close() { stopped = true; controller.abort(); await pump; for (const waiter of waiters) { clearTimeout(waiter.timer); waiter.reject(new Error('SSE collector closed')) }; waiters.clear() },
    }
    collectors.push(collector)
    return collector
  }
  async function ready() {
    const deadline = Date.now() + 45000
    let lastError
    while (Date.now() < deadline) {
      try {
        const result = await wireRequest('/healthz')
        if (result.response.status === 200 && result.value.runtime === 'node' && result.value.electron === false) return
      } catch (error) { lastError = error }
      // This is bounded readiness retry, never background-job polling.
      await delay(200)
    }
    throw new Error(`isolated native gateway readiness timeout: ${safe(lastError?.message ?? 'not healthy')}`)
  }
  const signalAbort = () => { failure ??= new Error('Interrupted; cleaning only owned canary resources'); void cleanup().finally(() => process.exit(130)) }
  let cleaning
  async function cleanup() {
    if (cleaning) return cleaning
    cleaning = (async () => {
      for (const collector of collectors) { try { await collector.close() } catch (error) { cleanupErrors.push(safe(error.message)) } }
      for (const name of containers.slice().reverse()) { try { docker(['rm', '--force', name]) } catch (error) { cleanupErrors.push(safe(error.message)) } }
      if (createdNetwork) { try { docker(['network', 'rm', network]) } catch (error) { cleanupErrors.push(safe(error.message)) } }
      if (createdFixture) { try { fs.rmSync(fixture, { recursive: true, force: true }) } catch (error) { cleanupErrors.push(safe(error.message)) } }
    })()
    return cleaning
  }
  process.once('SIGINT', signalAbort); process.once('SIGTERM', signalAbort)
  try {
    require(docker(['image', 'inspect', image, '--format', '{{.Id}}']) === image, 'READY immutable image exists locally')
    const protectedNames = new Set(['ai-novel-writer', 'ai-novel-native-test', 'ai-novel-native-relay'])
    const baseline = new Map(docker(['ps', '--all', '--format', '{{json .}}']).split('\n').filter(Boolean).map(line => JSON.parse(line)).filter(item => protectedNames.has(item.Names)).map(item => [item.Names, { id: item.ID, status: item.State }]))
    fs.mkdirSync(data, { recursive: true, mode: 0o700 }); createdFixture = true
    fs.writeFileSync(privateFile, JSON.stringify({ key, control }), { mode: 0o600, flag: 'wx' })
    docker(['network', 'create', '--internal', '--label', `novel-native-canary=${runId}`, network]); createdNetwork = true
    const hardening = ['--read-only', '--cap-drop=ALL', '--security-opt=no-new-privileges:true', '--pids-limit=128', '--tmpfs', '/tmp:rw,nosuid,nodev,size=64m']
    docker(['run', '--detach', '--pull=never', '--name', provider, '--label', `novel-native-canary=${runId}`, '--network', network, ...hardening, '--memory=192m', '--entrypoint', 'node', '--mount', `type=bind,src=${script},dst=/canary.mjs,readonly`, '--mount', `type=bind,src=${privateFile},dst=/private.json,readonly`, '--env', 'NOVEL_CANARY_PRIVATE_FIXTURE=/private.json', image, '/canary.mjs', '--fake-provider']); containers.push(provider)
    // The provider socket is local to the internal network; never published.
    docker(['exec', provider, 'node', '--input-type=module', '-e', `for(let n=0;n<100;n++){try{const r=await fetch('http://127.0.0.1:7000/');if(r.status===404)process.exit(0)}catch{}await new Promise(r=>setTimeout(r,50))}process.exit(1)`])
    // Docker29 suppresses published ports on --internal networks. Reach our own
    // private bridge IP from the local host instead, with no publication at all.
    const subnet = JSON.parse(docker(['network', 'inspect', network]))[0].IPAM.Config[0].Subnet
    require(/^\d+\.\d+\.\d+\.\d+\/16$/.test(subnet), 'fresh internal bridge has expected /16 IPv4 allocation')
    const gatewayIp = subnet.split('/')[0].split('.').slice(0, 2).join('.') + '.0.10'
    base = origin = `http://${gatewayIp}:6081`
    docker(['run', '--detach', '--pull=never', '--name', gateway, '--label', `novel-native-canary=${runId}`, '--network', network, '--ip', gatewayIp, ...hardening, '--memory=512m', '--mount', `type=bind,src=${data},dst=/data`, '--env', `NOVEL_ALLOWED_ORIGINS=${origin}`, '--env', `NOVEL_ALLOWED_ENDPOINTS=${fakeOrigin}`, '--env', 'NOVEL_ADMIN_USERS=canary-admin', '--env', 'NODE_ENV=production', '--env', 'HTTP_PROXY=', '--env', 'HTTPS_PROXY=', '--env', 'ALL_PROXY=', '--env', 'http_proxy=', '--env', 'https_proxy=', '--env', 'all_proxy=', '--env', 'NO_PROXY=*', '--env', 'no_proxy=*', image]); containers.push(gateway)
    await ready()
    const deployment = JSON.parse(docker(['inspect', gateway]))[0]
    const providerDeployment = JSON.parse(docker(['inspect', provider]))[0]
    const deployedEnv = Object.fromEntries(deployment.Config.Env.map(value => { const index = value.indexOf('='); return [value.slice(0, index), value.slice(index + 1)] }))
    require(deployment.Image === image && providerDeployment.Image === image, 'both canary containers use READY immutable native image')
    require(deployment.HostConfig.ReadonlyRootfs && deployment.HostConfig.CapDrop.includes('ALL') && deployment.HostConfig.SecurityOpt.some(value => value.startsWith('no-new-privileges')), 'production container hardening')
    require(deployedEnv.NODE_ENV === 'production' && deployedEnv.NOVEL_ALLOWED_ENDPOINTS === fakeOrigin && deployedEnv.NOVEL_ALLOWED_ORIGINS === origin, 'production flags and exact allowlisted fake origin')
    require(JSON.parse(docker(['network', 'inspect', network]))[0].Internal === true && Object.keys(deployment.NetworkSettings.Networks).length === 1, 'internal-only network, no external provider route')
    require([deployment, providerDeployment].every(item => Object.keys(item.NetworkSettings.Ports ?? {}).every(port => !item.NetworkSettings.Ports[port])), 'gateway and fake provider have no published ports')
    passed('immutable native image, fresh data, production hardening, exact fake-origin allowlist, internal-only network')
    let caller = client(); await caller.boot()
    const models = ['canary-content', 'canary-error', 'canary-stream', 'canary-stream-error'].map(modelName => ({ id: modelName, name: `private-display:${key}`, provider: 'custom', protocol: 'openai', modelName, apiKey: key, baseUrl: `${fakeOrigin}/v1`, temperature: 0.2, maxTokens: 1024, purposes: ['generation'] }))
    require((await caller.invoke('llm:list-models')).length === 0, 'truly fresh model configuration')
    for (const model of models) require((await caller.invoke('llm:save-model', [model])).success === true, 'HTTP model save')
    require((await caller.invoke('llm:save-model', [{ ...models[0], apiKey: '', hasCredential: true }])).success === true, 'blank-key model save restores private credential')
    require(/^[a-f0-9-]{36}$/.test(caller.generation), 'native canonical generation from HTTP bootstrap')
    const modelFile = path.join(data, 'config', 'ai-novel-writer', 'generations', caller.generation, 'models.json')
    privateModelBytes = fs.readFileSync(modelFile, 'utf8')
    const stored = JSON.parse(privateModelBytes)
    require(stored.length === models.length && stored.every(model => model.apiKey === key && model.name.includes(key)), 'private config retains exact key and unredacted ordinary payload')
    require((fs.statSync(modelFile).mode & 0o777) === 0o600, 'private model JSON mode 0600')
    passed('HTTP fresh-model save and blank-key save; private credential/payload unchanged, JSON mode 0600')
    // Restart only our gateway, then generate before public list/test operations:
    // protection must come from eager private startup registration, not a UI read.
    docker(['restart', gateway]); await ready(); caller = client(); await caller.boot()
    const makeRequest = (modelId, leaseId) => ({ modelId, modelExecutionLeaseId: leaseId, messages: [{ role: 'user', content: 'local fixture only' }], maxTokens: 64, purpose: `private-purpose:${key}` })
    const lease = await caller.invoke('llm:begin-execution-lease', ['canary-content'])
    require(lease.success && lease.lease.leaseId, 'generation execution lease')
    const firstGeneration = await caller.invoke('llm:generate', [makeRequest('canary-content', lease.lease.leaseId)])
    require(firstGeneration.success && firstGeneration.content === 'json-content:[REDACTED]' && firstGeneration.finishReason === 'stop', 'generation-first startup secret registration and content-value redaction')
    require((await caller.invoke('llm:close-execution-lease', [lease.lease.leaseId])).success, 'close generation lease')
    passed('own-gateway restart, generation-first startup registration, execution lease, nonstream content-value redaction')
    const publicModels = await caller.invoke('llm:list-models')
    require(publicModels.length === models.length && publicModels.every(model => model.apiKey === '' && model.hasCredential === true && model.name === 'private-display:[REDACTED]'), 'public models redact both apiKey and ordinary name values')
    const publicModel = id => publicModels.find(model => model.id === id)
    require((await caller.invoke('llm:test-connection', [publicModel('canary-content')])).success === true, 'blank-key connection uses restored credential')
    const connectionError = await caller.invoke('llm:test-connection', [publicModel('canary-error')])
    require(connectionError.success === false && connectionError.error.includes('400') && connectionError.error.includes('provider-error:[REDACTED]') && connectionError.error.includes('payload-value:[REDACTED]'), 'connection provider-error value is exercised and redacted')
    const deniedEndpoint = await caller.invoke('llm:test-connection', [{ ...publicModel('canary-content'), baseUrl: 'http://not-allowlisted.invalid/v1' }], 403)
    require(deniedEndpoint.error.code === 'ENDPOINT_NOT_ALLOWED', 'non-allowlisted endpoint rejected before dispatch')
    require((await caller.invoke('config:get', [], 403, { 'X-CSRF-Token': '' })).error.code === 'CSRF_DENIED', 'cookie alone cannot mutate')
    require((await caller.invoke('config:get', [], 403, { Origin: 'http://not-same-origin.invalid' })).error.code === 'ORIGIN_DENIED', 'wrong Origin rejected')
    passed('public list apiKey/readiness/name projection, restored connection, malicious HTTP400 error-body redaction, egress/CSRF/Origin negatives')
    const live = await openEvents(caller)
    const pendingGrant = caller.invoke('dialog:select-folder', ['project-create'])
    const dialog = await live.waitFor(entry => entry.channel === 'browser:dialog', 'fresh project chooser')
    const dialogResult = await wireRequest(`/api/dialogs/${dialog.args[0].id}`, { method: 'POST', headers: caller.csrfHeaders(), body: JSON.stringify({ canceled: false }) })
    require(dialogResult.response.status === 200 && dialogResult.value.success, 'same-session browser dialog completion')
    const grant = await pendingGrant
    require(grant?.grantId, 'fresh library directory grant')
    const created = await caller.invoke('project:create', [{ name: 'Credential Boundary Canary', parentGrantId: grant.grantId, genre: 'fixture', targetAudience: 'fixture' }, crypto.randomUUID()])
    require(created.success && created.projectPath.startsWith('/data/workspaces/'), 'fresh project created by native domain')
    const opened = await caller.invoke('project:open', [created.projectPath, crypto.randomUUID()])
    require(opened.success && opened.project.sessionLease, 'fresh isolated project open')
    const projectSession = { projectId: opened.project.id, leaseId: opened.project.sessionLease, projectPath: created.projectPath }
    const request = (modelId, leaseId) => ({ ...makeRequest(modelId, leaseId), projectSession })
    const projectLease = await caller.invoke('llm:begin-execution-lease', ['canary-stream'])
    require(projectLease.success, 'project worker execution lease')
    const streamId = `canary-stream-${crypto.randomUUID()}`
    require((await caller.invoke('llm:generate-stream', [streamId, request('canary-stream', projectLease.lease.leaseId)])).started === true, 'raw generate-stream request/lease shape accepted')
    const terminal = await live.waitFor(entry => entry.channel === 'llm:stream-done' && entry.args[0].requestId === streamId, 'live successful generation')
    require(terminal.args[0].fullText === 'stream-content:[REDACTED]' && terminal.args[0].finishReason === 'stop', 'live SSE terminal content-value redaction')
    const chunk = live.entries.find(entry => entry.channel === 'llm:stream-chunk' && entry.args[0].requestId === streamId)
    require(chunk?.args[0].chunk === 'stream-content:[REDACTED]', 'live SSE chunk content-value redaction')
    const errorLease = await caller.invoke('llm:begin-execution-lease', ['canary-error'])
    require(errorLease.success, 'provider-error generation lease')
    const generationError = await caller.invoke('llm:generate', [request('canary-error', errorLease.lease.leaseId)])
    require(generationError.success === false && generationError.httpFailure.status === 400 && generationError.httpFailure.body.includes('provider-error:[REDACTED]'), 'JSON generation httpFailure body-value redaction')
    const errorStreamLease = await caller.invoke('llm:begin-execution-lease', ['canary-stream-error'])
    require(errorStreamLease.success, 'error stream lease')
    const errorStreamId = `canary-stream-error-${crypto.randomUUID()}`
    require((await caller.invoke('llm:generate-stream', [errorStreamId, request('canary-stream-error', errorStreamLease.lease.leaseId)])).started === true, 'provider-error stream started')
    const streamError = await live.waitFor(entry => entry.channel === 'llm:stream-error' && entry.args[0].requestId === errorStreamId, 'live failed generation')
    require(streamError.args[0].error === 'LLM_REQUEST_FAILED', 'provider stream HTTP error normalized to fixed code')
    const history = await caller.invoke('db:get-llm-history', [20, created.projectPath, projectSession])
    require(history.length >= 3 && history.every(row => row.modelName === 'private-display:[REDACTED]' && row.purpose === 'private-purpose:[REDACTED]'), 'HTTP returned history log payload values redacted')
    passed('native dialog/project/session, live SSE chunk/done redaction, JSON httpFailure redaction, SSE provider-error path, history payload-value redaction')
    await live.close()
    const replay = await openEvents(caller, dialog.id)
    await replay.waitFor(entry => entry.id === streamError.id, 'replayed generation terminal')
    const liveGenerationEvents = live.entries.filter(entry => entry.id > dialog.id && entry.channel.startsWith('llm:stream-'))
    const replayGenerationEvents = replay.entries.filter(entry => entry.channel.startsWith('llm:stream-'))
    require(liveGenerationEvents.length >= 3 && JSON.stringify(replayGenerationEvents) === JSON.stringify(liveGenerationEvents), 'Last-Event-ID replay exactly matches sanitized live generation events')
    liveEvents = liveGenerationEvents.length; replayEvents = replayGenerationEvents.length
    await replay.close()
    for (const receipt of [projectLease, errorLease, errorStreamLease]) require((await caller.invoke('llm:close-execution-lease', [receipt.lease.leaseId])).success, 'close project execution lease')
    passed('Last-Event-ID replay re-sanitizes chunk/done/error and exactly matches live public events')
    require(fs.readFileSync(modelFile, 'utf8') === privateModelBytes, 'private model config byte-for-byte unchanged by public projections/operations')
    const proof = JSON.parse(docker(['exec', provider, 'node', '--input-type=module', '-e', `import fs from 'node:fs';const p=JSON.parse(fs.readFileSync('/private.json','utf8'));const r=await fetch('http://127.0.0.1:7000/__proof',{headers:{'X-Canary-Control':p.control}});if(!r.ok)process.exit(1);console.log(JSON.stringify(await r.json()))`]))
    providerCalls = proof.calls.length
    require(proof.allAuthorizationExact && proof.calls.every(call => call.sentCanary), 'trusted fake provider received exact private credential on every real dispatch')
    require(proof.calls.some(call => call.mode === 'canary-content' && !call.stream) && proof.calls.some(call => call.mode === 'canary-error' && !call.stream) && proof.calls.some(call => call.mode === 'canary-stream' && call.stream) && proof.calls.some(call => call.mode === 'canary-stream-error' && call.stream), 'all intended provider paths actually called, not vacuous sanitization')
    require(docker(['logs', provider]).includes(key), 'trusted provider logs retain original canary')
    const dbProof = JSON.parse(docker(['exec', gateway, 'node', '--input-type=module', '-e', `import fs from 'node:fs';import {createRequire} from 'node:module';const require=createRequire(import.meta.url);const models=JSON.parse(fs.readFileSync(${JSON.stringify(path.posix.join('/data/config/ai-novel-writer/generations', caller.generation, 'models.json'))},'utf8'));const key=models[0].apiKey;const Database=require('better-sqlite3');const db=new Database(${JSON.stringify(path.posix.join(created.projectPath, '.ai-novel', 'project.db'))},{readonly:true});const rows=db.prepare('SELECT model_name,purpose FROM llm_calls').all();console.log(JSON.stringify({rows:rows.length,allOriginal:rows.length>=3&&rows.every(row=>row.model_name.includes(key)&&row.purpose.includes(key))}));db.close()`]))
    require(dbProof.allOriginal, 'private database history log values remain original, not rewritten for passing test')
    passed('private config byte identity, exact provider Authorization on every dispatch, raw private provider log, unmodified private database history')
    for (const [name, before] of baseline) {
      if (name.startsWith('novel-native-canary-')) continue
      const now = JSON.parse(docker(['inspect', '--format', '{{json .}}', name]))
      require(now.Id.startsWith(before.id), `existing container ${name} identity unchanged`)
      // Other agents can legitimately create/stop their own fixtures; require the
      // protected original service and parent fixtures to stay running only.
      if (['ai-novel-writer', 'ai-novel-native-test', 'ai-novel-native-relay'].includes(name) && before.status === 'running') require(now.State.Running, `protected existing container ${name} remains running`)
    }
    passed('pre-existing container identities unchanged; protected official/test/relay containers remain running')
  } catch (error) { failure = error }
  finally { await cleanup(); process.removeListener('SIGINT', signalAbort); process.removeListener('SIGTERM', signalAbort) }
  if (cleanupErrors.length) failure ??= new Error('owned-resource cleanup failed')
  const report = { status: failure ? 'failed' : 'passed', image, runId, checks, browserResponses, auditedEvents, liveGenerationEvents: liveEvents, replayedGenerationEvents: replayEvents, providerCalls, failure: failure ? safe(failure.message) : undefined, cleanup: cleanupErrors.length ? cleanupErrors : 'all owned containers, network, fixture removed', exclusions: ['DOM/reverse-proxy/TLS', 'production volumes or credentials', 'unregistered/short/encoded/split-event secret detection', 'other provider protocols or cloud credentials', 'concurrent writer behavior'] }
  console.log(JSON.stringify(report, null, 2))
  if (failure) process.exitCode = 1
}

if (process.argv.includes('--fake-provider')) await fakeProvider()
else await main().catch(error => { console.error(error.message); process.exitCode = 1 })
