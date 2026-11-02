// GitHub-only differential oracle. The actual recursively loaded service,
// repository and shared modules are untouched; only DB admission, repository
// clock and crypto.randomUUID are seams. Provider callbacks are controlled input.
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { builtinModules, createRequire, registerHooks, stripTypeScriptTypes } from 'node:module'
import { closeSync, existsSync, mkdirSync, openSync, readFileSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

assert.equal(process.env.GITHUB_ACTIONS, 'true', 'actual generation service oracle is GitHub-only')
const base = process.env.RUNNER_TEMP, target = process.env.GO_GENERATION_SERVICE_CONTRACT
assert(base && target && path.isAbsolute(base) && path.isAbsolute(target))
assert.equal(target, path.join(base, 'novel-go-generation-service', 'contract.json'))
const sqliteContract = process.env.GO_SQLITE_CONTRACT
assert(sqliteContract && path.isAbsolute(sqliteContract))
const baseline = path.join(path.dirname(sqliteContract), 'baseline-v7.db')
const relative = path.relative(base, baseline)
assert(relative && !relative.startsWith('..') && !path.isAbsolute(relative) && existsSync(baseline))
const directory = path.dirname(target)
assert(!existsSync(directory), 'never reuse an oracle directory')
mkdirSync(directory, { recursive: true, mode: 0o700 })
const root = new URL('../../', import.meta.url), databaseURL = new URL('electron/database', root).href
const key = '__GO_GENERATION_SERVICE_ORIGINAL__', databaseSeam = 'generation-service-contract:database', cryptoSeam = 'generation-service-contract:crypto'
assert(!Object.hasOwn(globalThis, key))
const Database = createRequire(import.meta.url)('better-sqlite3')
// Load the native addon before restricting recursive original-module imports.
// better-sqlite3 resolves its lazy native binding on the first constructor call.
const prewarm = new Database(':memory:'); prewarm.close()
const state = { db: null, now: 1000, sequence: 0 }
state.uuid = () => `00000000-0000-4000-8000-${String(++state.sequence).padStart(12, '0')}`
globalThis[key] = state
const builtins = new Set(builtinModules.flatMap(name => [name, `node:${name}`])), originalModules = new Set()
const hooks = registerHooks({
  resolve(specifier, context, next) {
    if ((specifier === 'node:crypto' || specifier === 'crypto') && context.parentURL?.startsWith(root.href)) return { url: cryptoSeam, shortCircuit: true }
    if (builtins.has(specifier)) return next(specifier, context)
    assert(specifier.startsWith('.') || specifier.startsWith('file:'), `unexpected original dependency ${specifier}`)
    const url = new URL(specifier, context.parentURL ?? root).href
    if (url === databaseURL || url === databaseURL + '.ts') return { url: databaseSeam, shortCircuit: true }
    const candidate = url.endsWith('.ts') ? url : url + '.ts', filename = fileURLToPath(candidate)
    const relative = path.relative(fileURLToPath(root), filename)
    assert(relative && !relative.startsWith('..') && !path.isAbsolute(relative))
    assert(/^(electron|src\/(shared|services|utils))\//u.test(relative), relative)
    assert(existsSync(filename), `missing original module ${relative}`)
    originalModules.add(relative)
    return { url: candidate, shortCircuit: true }
  },
  load(url, context, next) {
    if (url === databaseSeam) return { format: 'module', source: `export function getProjectDb(){return globalThis[${JSON.stringify(key)}].db}`, shortCircuit: true }
    if (url === cryptoSeam) return { format: 'module', source: `export * from 'node:crypto'; export const randomUUID=()=>globalThis[${JSON.stringify(key)}].uuid()`, shortCircuit: true }
    if (!url.startsWith(root.href) || !url.endsWith('.ts')) return next(url, context)
    return { format: 'module', source: stripTypeScriptTypes(readFileSync(new URL(url), 'utf8'), { mode: 'transform', sourceUrl: url }), shortCircuit: true }
  },
})
const clone = value => JSON.parse(JSON.stringify(value))
const H = value => createHash('sha256').update(value, 'utf8').digest('hex')
const UNICODE = '中文 café e\u0301 🙂 <&>\u2028\u2029 " \\ \n'
const fingerprint = Object.fromEntries(['chapterBriefHash', 'authorGuidanceHash', 'dependencyHash', 'contextSnapshotHash', 'templateHash', 'skillSnapshotHash', 'modelLeaseRevision', 'policyHash', 'outputContractHash'].map((name, i) => [name, H(`${i}:${UNICODE}`)]))
const source = { projectId: 'synthetic-project', epoch: 'synthetic-epoch', sourceId: 'synthetic-source', revision: 0, contentHash: H(UNICODE), span: { start: 0, end: 2, unit: 'utf16-code-unit' } }
const openRequest = { projectId: source.projectId, epoch: source.epoch, fingerprint, contextSnapshotId: 'synthetic-context', sourceManifest: { operation: 'synthetic-contract', outputContract: 'visible-text', authorText: UNICODE, nested: { created_at: 'author-clock', elapsedMs: 123, hash: H(UNICODE) } }, sourceRefs: [source], operation: 'synthetic-contract', uiActionNonce: 'synthetic-author-action', frozenInputHash: H('frozen input'), budget: { maxPhysicalRequests: 3, maxTokenLiability: 10, maxOutputPerRequest: 20, maxActiveElapsedMs: 60000 } }
const policy = { estimatorVersion: 'synthetic-estimator-v1', safetyMarginTokens: 2, reasoning: 'included-in-completion', canBoundTotalLiability: true }
const executeRequest = { invocationNonce: 'synthetic-invoke', requestHash: H('synthetic provider request'), providerRequest: { synthetic: true, authorText: UNICODE }, reservedTokens: 30, requestedOutputTokens: 20, inputUpperBoundTokens: 5, reasoningUpperBoundTokens: 3, usagePolicy: policy, purpose: 'synthetic-purpose' }
const trustedUsage = { promptTokens: 2, completionTokens: 5, totalTokens: 7, reasoningTokens: 1, accounting: 'included-in-completion', totalIncludesReasoning: true, trusted: true }
const definitions = [], add = (name, events = [], options = {}) => definitions.push({ name, request: clone(openRequest), execute: clone(executeRequest), events, providerResult: { usage: clone(trustedUsage), finishReason: 'stop' }, ...options })
const V = (text, kind = 'delta', eventId) => ({ type: 'visible', kind, text, ...(eventId === undefined ? {} : { eventId }) })
const R = text => ({ type: 'reasoning', text })
const C = action => ({ type: 'control', action })
add('execute-empty')
add('execute-unicode-delta', [V(UNICODE), V('尾声')])
add('execute-cumulative-prefix', [V('中', 'cumulative'), V('中文', 'cumulative'), V('中文🙂', 'cumulative')])
add('execute-mixed-prefix', [V('中'), V('中文', 'cumulative'), V('🙂')])
add('execute-cumulative-repeat', [V(UNICODE, 'cumulative'), V(UNICODE, 'cumulative')])
add('execute-empty-events', [V(''), V('', 'cumulative'), V('tail')])
add('execute-duplicate-id-exact', [V('A', 'delta', 'same'), V('A', 'delta', 'same'), V('B', 'delta', 'next')])
add('execute-duplicate-id-text-conflict', [V('A', 'delta', 'same'), V('B', 'delta', 'same'), V('late')])
add('execute-duplicate-id-kind-conflict', [V('A', 'delta', 'same'), V('A', 'cumulative', 'same'), V('late')])
add('execute-empty-event-id-is-not-deduped', [V('A', 'delta', ''), V('A', 'delta', '')])
add('execute-cumulative-regression', [V('AB'), V('A', 'cumulative'), V('late')])
add('execute-cumulative-replacement', [V('AB'), V('XY', 'cumulative')])
for (const [name, parts] of [ ['open-think', ['visible', '<think>secret']], ['close-think', ['visible', '</THINK>']], ['split-open-think', ['visible <thi', 'nk>secret']], ['split-close-think', ['visible </thi', 'nk>']], ['think-word-boundary-allowed', ['<thinking>visible']], ['split-think-word-boundary-allowed', ['<thi', 'nking>visible']] ]) add(`execute-${name}`, parts.map(text => V(text)))
add('execute-reasoning-never-artifact', [R('secret internal reasoning'), V(UNICODE), R('second secret'), R('')])
add('execute-reasoning-after-invalid-ignored', [V('safe'), V('<think>'), R('late'), V('late')])
add('execute-ui-snapshot-throws-isolated', [V(UNICODE)], { throwSnapshot: true })
add('execute-ui-reasoning-throws-isolated', [R('secret'), V(UNICODE)], { throwReasoning: true })
for (const size of [4095, 4096, 4097]) add(`execute-byte-flush-${size}`, [V('x'.repeat(size)), V('尾')], { checkpoints: true })
add('execute-byte-flush-unicode-boundary', [V('中'.repeat(1365)), V('🙂'), V('尾')], { checkpoints: true })
add('execute-byte-flush-two-revisions', [V('x'.repeat(4096)), V('y'.repeat(4096)), V('tail')], { checkpoints: true })
for (const [name, usage] of [['missing', null], ['untrusted', { ...trustedUsage, trusted: false }], ['inconsistent-total', { ...trustedUsage, totalTokens: 99 }], ['over-reservation', { ...trustedUsage, promptTokens: 400, completionTokens: 500, totalTokens: 900 }], ['zero', { ...trustedUsage, promptTokens: 0, completionTokens: 0, reasoningTokens: 0, totalTokens: 0 }]]) add(`execute-usage-${name}`, [V(UNICODE)], { providerResult: { usage, finishReason: 'stop' } })
for (const finishReason of ['length', 'error', 'content_filter']) add(`execute-finish-${finishReason}`, [V(UNICODE)], { providerResult: { usage: clone(trustedUsage), finishReason } })
for (const message of ['NETWORK_ERROR', 'ECONNRESET', 'ETIMEDOUT', 'FETCH_FAILED', 'GENERATION_CONTEXT_LENGTH_EXCEEDED', 'LLM_CONTEXT_LENGTH_EXCEEDED', 'LLM_STREAM_NETWORK_FAILED', 'NETWORK_ERROR secret suffix', 'provider secret api_key=must-not-persist', 'context length exceeded contains secret']) add(`execute-provider-error-${definitions.length}`, [V(UNICODE)], { providerError: message })
const completeDiagnostics = { startedAt: 1000, elapsedMs: 17, phase: 'complete', endReason: 'failed', visibleEvents: 1, reasoningEvents: 0, causeCode: 'UND_ERR_SOCKET' }
add('execute-diagnostics-stable-network-category', [{ type: 'diagnostics', value: completeDiagnostics }, V(UNICODE)], { providerError: 'provider credentials must not persist' })
add('execute-diagnostics-success-exact', [{ type: 'diagnostics', value: { ...completeDiagnostics, endReason: 'completed', causeCode: undefined } }, V(UNICODE)])
for (const action of ['pause', 'cancel', 'suspend']) add(`execute-${action}-flush-and-ignore-late`, [V(UNICODE), C(action), V('late MUST NOT persist'), R('late secret')])
add('execute-replay-does-not-redispatch', [V(UNICODE)], { replay: true })
add('execute-replay-hash-conflict', [V(UNICODE)], { replayHash: H('different invocation') })
for (const [name, patch] of [['invalid-hash', { requestHash: 'bad' }], ['insufficient-reservation', { reservedTokens: 29 }], ['negative-input-bound', { inputUpperBoundTokens: -1 }], ['negative-reasoning-bound', { reasoningUpperBoundTokens: -1 }], ['invalid-policy-margin', { usagePolicy: { ...policy, safetyMarginTokens: -1 } }], ['invalid-policy-estimator', { usagePolicy: { ...policy, estimatorVersion: '' } }], ['invalid-policy-bound-flag', { usagePolicy: { ...policy, canBoundTotalLiability: 'yes' } }], ['output-exceeds-budget', { requestedOutputTokens: 21, reservedTokens: 31 }]]) add(`execute-${name}`, [], { execute: { ...clone(executeRequest), ...patch } })

function writeNew(filename, bytes) { const fd = openSync(filename, 'wx', 0o600); try { writeFileSync(fd, bytes) } finally { closeSync(fd) } }
const quote = name => '"' + name.replaceAll('"', '""') + '"'
const jsonColumns = new Set(['action_json', 'budget_json', 'binding_json', 'attempt_json', 'usage_receipt_json', 'artifact_json', 'idempotency_key', 'open_key'])
function normalize(value) {
  if (typeof value === 'string' && /^00000000-0000-4000-8000-\d{12}$/u.test(value)) {
    const n = Number(value.slice(-12)); if (n > 0 && n <= state.sequence) return `generated-id:${n}`
  }
  if (Array.isArray(value)) return value.map(normalize)
  if (value && typeof value === 'object') return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, normalize(item)]))
  return value
}
function wholeSql(db) {
  return Object.fromEntries(db.prepare("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name").all().map(({ name }) => [name, db.prepare(`SELECT * FROM ${quote(name)} ORDER BY rowid`).all().map(row => Object.fromEntries(Object.entries(row).map(([column, value]) => [column, normalize(name.startsWith('generation_') && jsonColumns.has(column) && typeof value === 'string' ? JSON.parse(value) : value)])))]))
}
function ordinaryError(error, label) {
  assert(error instanceof Error, label)
  assert(!(error instanceof TypeError) && !(error instanceof SyntaxError), `unsupported native exception must not masquerade as parity ${label}: ${error}`)
  return error.message
}
try {
  const { createGenerationRunService, settleProviderUsage } = await import(new URL('electron/services/generation-run-service.ts', root).href)
  const { GenerationRunRepository } = await import(new URL('electron/repositories/generation-run-repository.ts', root).href)
  const usageCases = [], unsupportedBoundaries = []
  const addUsage = (name, usage, usagePolicy) => {
    const before = JSON.stringify([usage, usagePolicy])
    const expected = settleProviderUsage(usage, usagePolicy)
    assert.equal(JSON.stringify([usage, usagePolicy]), before, `usage mutated input: ${name}`)
    usageCases.push({ name, usage: clone(usage), policy: clone(usagePolicy), expected: clone(expected) })
  }
  // A semantic matrix, not repeated fixtures: accounting/policy agreement,
  // zero, absent totals/reasoning, arithmetic inconsistency and JS safe overflow.
  const tuples = [[0, 0, 0], [2, 5, 1], [2, 5, 5], [2, 5, 6], [2, 5, null], [9007199254740990, 1, 0], [9007199254740991, 1, 1]]
  for (const reasoning of ['included-in-completion', 'separately-billed', 'unknown']) {
    const usagePolicy = { ...policy, reasoning }
    addUsage(`${reasoning}-null-usage`, null, usagePolicy)
    addUsage(`${reasoning}-untrusted`, { ...trustedUsage, trusted: false }, usagePolicy)
    for (const accounting of ['included-in-completion', 'separately-billed', 'unknown']) for (const totalIncludesReasoning of [false, true]) for (let i = 0; i < tuples.length; i++) {
      const [promptTokens, completionTokens, reasoningTokens] = tuples[i]
      const total = promptTokens + completionTokens + (accounting === 'separately-billed' && totalIncludesReasoning ? reasoningTokens ?? 0 : 0)
      for (const variant of ['consistent', 'missing', 'inconsistent']) {
        addUsage(`${reasoning}-${accounting}-${totalIncludesReasoning}-${i}-${variant}`, { promptTokens, completionTokens, reasoningTokens, totalTokens: variant === 'missing' ? null : total + (variant === 'inconsistent' ? 1 : 0), accounting, totalIncludesReasoning, trusted: true }, usagePolicy)
      }
    }
    for (const field of ['promptTokens', 'completionTokens', 'reasoningTokens', 'totalTokens']) for (const invalid of [null, -1, 0.5, 9007199254740992, '7']) addUsage(`${reasoning}-invalid-${field}-${JSON.stringify(invalid)}`, { ...trustedUsage, [field]: invalid }, usagePolicy)
  }
  // Native language exceptions are evidence, never fabricated business parity.
  try { settleProviderUsage(trustedUsage, null); assert.fail('expected null-policy TypeError') } catch (error) {
    assert(error instanceof TypeError)
    unsupportedBoundaries.push({ name: 'trusted-null-policy', method: 'settleProviderUsage', usage: trustedUsage, policy: null, originalErrorName: error.name, originalError: error.message, classification: 'Unsupported native JavaScript null policy, not ordinary typed provider input or business parity.' })
  }
  const bytes = readFileSync(baseline), cases = [], names = new Set()
  for (const definition of definitions) {
    assert(!names.has(definition.name)); names.add(definition.name)
    writeNew(path.join(directory, definition.name + '-before.db'), bytes)
    const filename = path.join(directory, definition.name + '.db'); writeNew(filename, bytes)
    const db = new Database(filename)
    try {
      db.pragma('foreign_keys=ON')
      for (const table of ['generation_roots', 'generation_runs', 'generation_attempts', 'generation_artifacts']) assert.equal(db.prepare(`SELECT COUNT(*) AS n FROM ${table}`).get().n, 0, `actual centrally migrated baseline must be synthetic and empty: ${table}`)
      state.db = db; state.now = 1000; state.sequence = 0
      const repository = new GenerationRunRepository(() => state.db, () => state.now)
      const snapshots = [], reasoning = [], checkpoints = [], controls = []
      let dispatches = 0, service, run, providerAborted = false
      service = createGenerationRunService({ repository,
        onSnapshot: value => { snapshots.push(clone(value)); if (definition.throwSnapshot) throw new Error('synthetic UI-only failure') },
        onReasoning: value => { reasoning.push(clone(value)); if (definition.throwReasoning) throw new Error('synthetic UI-only failure') },
        dispatch: async (request, callbacks) => {
          dispatches++
          assert.deepEqual(request, definition.execute.providerRequest)
          for (let index = 0; index < definition.events.length; index++) {
            const event = definition.events[index]
            if (event.type === 'visible') { const { type, ...visible } = event; callbacks.onVisible(visible) }
            else if (event.type === 'reasoning') callbacks.onReasoning(event.text)
            else if (event.type === 'diagnostics') callbacks.onDiagnostics(clone(event.value))
            else if (event.type === 'control') {
              let error = ''
              try {
                if (event.action === 'suspend') service.suspendForProjectClose()
                else service[event.action](run.rootActionId)
              } catch (caught) { error = ordinaryError(caught, `${definition.name}/${event.action}`) }
              controls.push({ action: event.action, error })
            } else assert.fail('unknown fixture provider event')
            if (definition.checkpoints) checkpoints.push({ index, wholeSql: wholeSql(db), generatedIDs: state.sequence })
          }
          providerAborted = callbacks.signal.aborted
          if (definition.providerError) throw new Error(definition.providerError)
          return clone(definition.providerResult)
        },
      })
      run = service.open(clone(definition.request))
      const request = { ...clone(definition.execute), runId: run.runId }, before = JSON.stringify(request)
      let receipt = null, error = ''
      try { receipt = await service.execute(request) } catch (caught) { error = ordinaryError(caught, definition.name) }
      assert.equal(JSON.stringify(request), before, `${definition.name}: mutated execute input`)
      let replayReceipt = null, replayError = ''
      if (definition.replay || definition.replayHash) {
        try { replayReceipt = await service.execute({ ...request, requestHash: definition.replayHash ?? request.requestHash }) } catch (caught) { replayError = ordinaryError(caught, definition.name + '/replay') }
      }
      const attempts = db.prepare('SELECT attempt_id FROM generation_attempts ORDER BY rowid').all()
      const readAttempt = attempts.length ? service.readAttempt(attempts[0].attempt_id) : null
      cases.push({ ...definition, expected: normalize({ receipt, error, replayReceipt, replayError, readAttempt, run: service.get(run.runId), snapshots, reasoning, checkpoints, controls, dispatches, providerAborted, unsavedTails: service.readUnsavedTails(), wholeSql: wholeSql(db), generatedIDs: state.sequence, argsUnchanged: true }) })
    } finally { state.db = null; db.close() }
  }
  for (const required of ['electron/services/generation-run-service.ts', 'electron/repositories/generation-run-repository.ts', 'src/shared/generation-contract.ts', 'src/shared/source-ref.ts', 'src/shared/agent-response-protocol.ts']) assert(originalModules.has(required), required)
  assert(usageCases.length >= 100 && cases.length >= 40)
  writeNew(target, JSON.stringify({ format: 1, usageCases, cases, unsupportedBoundaries, originalModules: [...originalModules].sort(), normalization: 'Only deterministic generated UUIDs are labeled generated-id:N; only declared generation ledger JSON columns are parsed. Every raw SQL table/column, author byte, hash, author timestamp and nested elapsedMs is exact. No diagnostics field is stripped: catch scenarios either omit diagnostics or provide explicit endReason so performance.now cannot introduce elapsed nondeterminism.', authority: 'Untouched recursive actual generation-run-service.ts, actual repository and shared modules on per-case copies of centrally migrated baseline-v7.db. Only database admission, repository now and randomUUID seams. Native Date, performance and timers remain original; immediate controlled provider events plus terminal flush avoid one-second sleeps. No fake business repository or substituted business method.', excludedNativeDomain: 'Actual null-policy TypeError recorded separately and excluded from parity cases.' }))
  console.error(`Actual generation service emitted ${usageCases.length} usage cases, ${cases.length} lifecycle traces, ${originalModules.size} recursive original modules`)
} finally { hooks.deregister(); delete globalThis[key] }
