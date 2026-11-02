// GitHub-only differential oracle: recursively load the ACTUAL repository and
// shared contracts. Only database admission and crypto.randomUUID are seams.
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { builtinModules, createRequire, registerHooks, stripTypeScriptTypes } from 'node:module'
import { closeSync, existsSync, mkdirSync, openSync, readFileSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

assert.equal(process.env.GITHUB_ACTIONS, 'true', 'original generation ledger oracle is GitHub-only')
const base = process.env.RUNNER_TEMP, target = process.env.GO_GENERATION_LEDGER_CONTRACT
assert(base && target && path.isAbsolute(base) && path.isAbsolute(target))
assert.equal(target, path.join(base, 'novel-go-generation-ledger', 'contract.json'))
const sqliteContract = process.env.GO_SQLITE_CONTRACT
assert(sqliteContract && path.isAbsolute(sqliteContract))
const baseline = path.join(path.dirname(sqliteContract), 'baseline-v7.db')
const relative = path.relative(base, baseline)
assert(relative && !relative.startsWith('..') && !path.isAbsolute(relative) && existsSync(baseline))
const directory = path.dirname(target)
assert(!existsSync(directory), 'never reuse an oracle directory')
mkdirSync(directory, { recursive: true, mode: 0o700 })
const root = new URL('../../', import.meta.url), databaseURL = new URL('electron/database', root).href
const key = '__GO_GENERATION_LEDGER_ORIGINAL__', databaseSeam = 'generation-ledger-contract:database', cryptoSeam = 'generation-ledger-contract:crypto'
assert(!Object.hasOwn(globalThis, key))
const Database = createRequire(import.meta.url)('better-sqlite3')
const probe = new Database(':memory:'); probe.close()
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
const { GenerationRunRepository } = await import(new URL('electron/repositories/generation-run-repository.ts', root).href)
const shared = await import(new URL('src/shared/generation-contract.ts', root).href)
const H = value => createHash('sha256').update(value, 'utf8').digest('hex')
const UNICODE = '中文 café e\u0301 🙂 <&>\u2028\u2029 " \\ \n'
const fingerprint = Object.fromEntries(['chapterBriefHash', 'authorGuidanceHash', 'dependencyHash', 'contextSnapshotHash', 'templateHash', 'skillSnapshotHash', 'modelLeaseRevision', 'policyHash', 'outputContractHash'].map((name, index) => [name, H(`${index}:${UNICODE}`)]))
const policy = { maxPhysicalRequests: 3, maxTokenLiability: 10, maxOutputPerRequest: 20, maxActiveElapsedMs: 100 }
const source = { projectId: 'synthetic-project', epoch: 'synthetic-epoch', sourceId: 'synthetic-source', revision: 0, contentHash: H(UNICODE), span: { start: 0, end: 2, unit: 'utf16-code-unit' } }
const binding = { projectId: source.projectId, epoch: source.epoch, fingerprint, contextSnapshotId: 'synthetic-context', sourceManifest: { operation: 'synthetic-contract', outputContract: 'visible-text', authorText: UNICODE, nested: { created_at: 'author-clock', hash: H(UNICODE) } }, sourceRefs: [source] }
const request = { ...binding, operation: 'synthetic-contract', uiActionNonce: 'synthetic-author-action', frozenInputHash: H('synthetic frozen input'), budget: policy }
const action = { projectId: source.projectId, epoch: source.epoch, operation: request.operation, uiActionNonce: request.uiActionNonce, frozenInputHash: request.frozenInputHash, rootActionId: 'synthetic-root', status: 'active' }
const attempt = { attemptId: 'synthetic-attempt-next', reservationId: 'synthetic-reservation-next', rootActionId: action.rootActionId, status: 'reserved', reservedTokens: 30, requestedOutputTokens: 20 }
const usagePolicy = { estimatorVersion: 'synthetic-estimator-v1', safetyMarginTokens: 2, reasoning: 'included-in-completion', canBoundTotalLiability: true }
const clone = value => JSON.parse(JSON.stringify(value))
const definitions = [], add = (name, operations, options = {}) => definitions.push({ name, operations, ...options })
const op = (method, args = [], options = {}) => ({ method, args, ...options })
const pure = (name, method, args) => add(`pure-${name}`, [op(method, args, { pure: true })])
const ref = value => `@ref:${value}`
const OPEN = () => op('open', [clone(request)], { save: 'run' })
const RESERVE = (nonce = 'invoke-1', options = {}) => op('reserve', [ref('run.runId'), nonce, H(nonce), 30, 20, usagePolicy, 'synthetic-purpose'], { save: 'receipt', ...options })
const DISPATCH = () => op('markDispatched', [ref('receipt.attempt.attemptId')])
const SNAPSHOT = (revision = 0, text = UNICODE) => op('snapshot', [ref('receipt.attempt.attemptId'), revision, text])
const SETTLE = (usage = null, finish = 'stop', options = {}) => op('settle', [ref('receipt.attempt.attemptId'), usage, finish], options)
const BUDGET = (now = 1000) => op('budget', [ref('run.rootActionId')], { now })
const ROOT = ref('run.rootActionId'), RUN = ref('run.runId'), ATTEMPT = ref('receipt.attempt.attemptId'), ARTIFACT = ref('receipt.artifact.artifactId')

pure('root-key-valid', 'rootKey', [action])
pure('root-key-epoch-excluded', 'rootKey', [{ ...action, epoch: 'new-epoch', rootActionId: 'new-root' }])
pure('root-key-unicode-exact', 'rootKey', [{ ...action, projectId: UNICODE, operation: UNICODE, uiActionNonce: UNICODE }])
for (const field of ['projectId', 'epoch', 'operation', 'uiActionNonce', 'rootActionId']) for (const value of ['', ' \uFEFF\n']) pure(`root-invalid-${field}-${value ? 'whitespace' : 'empty'}`, 'rootKey', [{ ...action, [field]: value }])
for (const value of ['A'.repeat(64), 'f'.repeat(63), 'g'.repeat(64), ' ' + H('x')]) pure(`root-hash-${definitions.length}`, 'rootKey', [{ ...action, frozenInputHash: value }])
const statuses = ['reserved', 'dispatch-marked', 'settled', 'unknown', 'cancelled-before-dispatch', 'invalid']
for (const before of statuses) for (const after of statuses) pure(`transition-${before}-${after}`, 'assertTransition', [before, after])
for (const status of statuses.slice(0, 5)) for (const actual of [undefined, 0, 47]) pure(`liability-${status}-${actual ?? 'absent'}`, 'tokenLiability', [{ ...attempt, status, ...(actual === undefined ? {} : { actualTokens: actual }) }])
const reservePure = (name, edit) => { const args = clone([action, policy, [], attempt, 0]); edit?.(args); pure(`reservation-${name}`, 'assertReservation', args) }
reservePure('valid-token-cap-is-accounting-only')
reservePure('exact-output-limit', args => { args[3].reservedTokens = args[3].requestedOutputTokens })
for (const field of Object.keys(policy)) for (const value of [0, -1, 1.5, Number.MAX_SAFE_INTEGER + 1]) reservePure(`policy-${field}-${value}`, args => { args[1][field] = value })
for (const field of ['reservedTokens', 'requestedOutputTokens']) for (const value of [0, -1, 1.5]) reservePure(`next-${field}-${value}`, args => { args[3][field] = value })
for (const elapsed of [-1, 0.5, 99, 100, 101]) reservePure(`elapsed-${elapsed}`, args => { args[4] = elapsed })
for (const status of ['paused', 'cancelled', 'sealed']) reservePure(`root-${status}`, args => { args[0].status = status })
for (const [name, change] of [
  ['wrong-root', a => { a[3].rootActionId = 'other' }], ['blank-attempt', a => { a[3].attemptId = '' }], ['blank-reservation', a => { a[3].reservationId = '' }], ['not-reserved', a => { a[3].status = 'settled' }],
  ['output-too-large', a => { a[3].requestedOutputTokens = 21 }], ['under-reserved', a => { a[3].reservedTokens = 19 }],
  ['attempt-id-conflict', a => { a[2] = [{ ...attempt, reservationId: 'other' }] }], ['reservation-id-conflict', a => { a[2] = [{ ...attempt, attemptId: 'other' }] }],
  ['foreign-ledger-root', a => { a[2] = [{ ...attempt, attemptId: 'old', reservationId: 'old', rootActionId: 'other' }] }],
  ['invalid-old-status', a => { a[2] = [{ ...attempt, attemptId: 'old', reservationId: 'old', status: 'bad' }] }],
  ['invalid-old-actual', a => { a[2] = [{ ...attempt, attemptId: 'old', reservationId: 'old', actualTokens: -1 }] }],
  ['invalid-old-reserved', a => { a[2] = [{ ...attempt, attemptId: 'old', reservationId: 'old', reservedTokens: 0 }] }],
  ['physical-limit', a => { a[1].maxPhysicalRequests = 1; a[2] = [{ ...attempt, attemptId: 'old', reservationId: 'old', status: 'unknown' }] }],
  ['cancelled-does-not-consume-request', a => { a[1].maxPhysicalRequests = 1; a[2] = [{ ...attempt, attemptId: 'old', reservationId: 'old', status: 'cancelled-before-dispatch' }] }],
  ['actual-over-estimate-not-clipped', a => { a[2] = [{ ...attempt, attemptId: 'old', reservationId: 'old', status: 'settled', actualTokens: 900 }] }],
  ['duplicate-old-attempts', a => { a[2] = [1, 2].map(i => ({ ...attempt, attemptId: 'old', reservationId: `old-${i}` })) }],
  ['duplicate-old-reservations', a => { a[2] = [1, 2].map(i => ({ ...attempt, attemptId: `old-${i}`, reservationId: 'old' })) }],
]) reservePure(name, change)
pure('binding-valid-deep-clone', 'binding', [binding])
pure('binding-empty-refs-valid', 'binding', [{ ...binding, sourceRefs: [] }])
pure('binding-manifest-array-invalid', 'binding', [{ ...binding, sourceManifest: [] }])
pure('binding-manifest-empty-invalid', 'binding', [{ ...binding, sourceManifest: {} }])
for (const field of ['projectId', 'epoch', 'contextSnapshotId']) pure(`binding-${field}-empty`, 'binding', [{ ...binding, [field]: '' }])
for (const field of Object.keys(fingerprint)) pure(`binding-fingerprint-${field}`, 'binding', [{ ...binding, fingerprint: { ...fingerprint, [field]: 'A'.repeat(64) } }])
pure('binding-fingerprint-extra-field', 'binding', [{ ...binding, fingerprint: { ...fingerprint, extra: H('extra') } }])
for (const [name, changes] of [['foreign-project', { projectId: 'other' }], ['foreign-epoch', { epoch: 'other' }], ['negative-revision', { revision: -1 }], ['fractional-revision', { revision: 0.5 }], ['unsafe-revision', { revision: Number.MAX_SAFE_INTEGER + 1 }], ['empty-source-id', { sourceId: '' }], ['uppercase-hash', { contentHash: H('x').toUpperCase() }], ['extra-source-metadata-retained', { extra: { created_at: 'keep-clock', textHash: H(UNICODE) } }], ['span-not-validated-by-binding', { span: { unit: 'not-utf16', start: -2, end: -1 } }]]) pure(`binding-source-${name}`, 'binding', [{ ...binding, sourceRefs: [{ ...source, ...changes }] }])

add('open-get-budget', [OPEN(), op('get', [RUN]), BUDGET()])
add('open-idempotent-replay', [OPEN(), OPEN(), BUDGET()])
for (const field of ['contextSnapshotId', 'sourceManifest', 'sourceRefs', 'fingerprint', 'budget']) {
  const changed = clone(request)
  if (field === 'contextSnapshotId') changed[field] += '-changed'
  if (field === 'sourceManifest') changed[field].authorText += ' changed'
  if (field === 'sourceRefs') changed[field][0].revision++
  if (field === 'fingerprint') changed[field].chapterBriefHash = H('changed')
  if (field === 'budget') changed[field].maxPhysicalRequests++
  add(`open-nonce-conflict-${field}`, [OPEN(), op('open', [changed])])
}
for (const field of ['fingerprint', 'budget', 'sourceManifest']) { const changed = clone(request); changed[field] = Object.fromEntries(Object.entries(changed[field]).reverse()); add(`open-reordered-${field}`, [OPEN(), op('open', [changed])]) }
add('open-new-epoch-reuses-original', [OPEN(), op('open', [{ ...request, epoch: 'new-epoch', sourceRefs: [{ ...source, epoch: 'new-epoch' }] }])])
add('open-child-shares-budget', [OPEN(), op('open', [{ ...request, uiActionNonce: 'child', parentRootActionId: ROOT }], { save: 'child' }), op('budget', [ref('child.rootActionId')])])
add('siblings-share-active-clock-and-idle-stop', [
  OPEN(), op('open', [{ ...request, uiActionNonce: 'child', parentRootActionId: ROOT }], { save: 'child' }),
  RESERVE(), op('reserve', [ref('child.runId'), 'child-invoke', H('child-invoke'), 30, 20, usagePolicy], { save: 'childReceipt' }),
  DISPATCH(), op('markDispatched', [ref('childReceipt.attempt.attemptId')], { now: 1020 }),
  BUDGET(1025), SETTLE(null, 'stop', { now: 1030 }), BUDGET(1040),
  op('settle', [ref('childReceipt.attempt.attemptId'), { policy: usagePolicy, trusted: true, actualTokens: 500 }, 'stop'], { now: 1050 }), BUDGET(1060),
])
add('siblings-share-physical-limit-not-inflight-lock', [
  OPEN(), op('open', [{ ...request, uiActionNonce: 'child', parentRootActionId: ROOT }], { save: 'child' }),
  RESERVE(), op('reserve', [ref('child.runId'), 'child-invoke', H('child-invoke'), 30, 20], { save: 'childReceipt' }),
  DISPATCH(), SETTLE(), op('markDispatched', [ref('childReceipt.attempt.attemptId')]), op('settle', [ref('childReceipt.attempt.attemptId'), null]),
  RESERVE('invoke-3'), DISPATCH(), SETTLE(), op('reserve', [ref('child.runId'), 'child-invoke-4', H('child-invoke-4'), 30, 20]), BUDGET(),
])
add('open-invalid-parent', [OPEN(), op('pause', [ROOT]), op('open', [{ ...request, uiActionNonce: 'child', parentRootActionId: ROOT }])])
add('open-foreign-parent', [OPEN(), op('open', [{ ...request, projectId: 'other-project', sourceRefs: [], uiActionNonce: 'child', parentRootActionId: ROOT }])])
add('open-missing-parent', [op('open', [{ ...request, parentRootActionId: 'missing-root' }])])
for (const field of Object.keys(policy)) add(`open-invalid-budget-${field}`, [op('open', [{ ...request, budget: { ...policy, [field]: 0 } }])])
for (const method of ['get', 'budget', 'receipt', 'markDispatched', 'snapshot', 'settle', 'pause']) { const args = method === 'snapshot' ? ['missing', 0, ''] : method === 'settle' ? ['missing', null] : ['missing']; add(`missing-${method}`, [op(method, args)]) }
add('no-database-get', [op('get', ['missing'])], { nilDB: true })
add('reserve-and-receipt', [OPEN(), RESERVE(), op('receipt', [ATTEMPT]), BUDGET()])
add('reserve-exact-replay', [OPEN(), RESERVE(), RESERVE()])
add('reserve-invocation-conflict', [OPEN(), RESERVE(), op('reserve', [RUN, 'invoke-1', H('different'), 30, 20])])
add('find-invocation-missing', [OPEN(), op('findInvocation', [RUN, 'not-found', H('not-found')])])
add('find-invocation-exact', [OPEN(), RESERVE(), op('findInvocation', [RUN, 'invoke-1', H('invoke-1')])])
add('find-invocation-conflict', [OPEN(), RESERVE(), op('findInvocation', [RUN, 'invoke-1', H('different')])])
add('reserve-inflight-rejected', [OPEN(), RESERVE(), RESERVE('invoke-2')])
for (const [name, nonce, hash, reserved, output] of [['blank-nonce', '', H('x'), 30, 20], ['bad-hash', 'x', 'bad', 30, 20], ['zero-reserved', 'x', H('x'), 0, 20], ['negative-output', 'x', H('x'), 30, -1], ['fractional-output', 'x', H('x'), 30, 1.5], ['output-over-cap', 'x', H('x'), 30, 21], ['under-reserved', 'x', H('x'), 10, 20]]) add(`reserve-${name}`, [OPEN(), op('reserve', [RUN, nonce, hash, reserved, output])])
add('dispatch-clock-exact', [OPEN(), RESERVE(), DISPATCH(), BUDGET(1040), BUDGET(990)])
add('dispatch-twice-rejected', [OPEN(), RESERVE(), DISPATCH(), DISPATCH()])
add('dispatch-after-pause-blocked', [OPEN(), RESERVE(), op('pause', [ROOT]), DISPATCH()])
add('dispatch-after-block-blocked', [OPEN(), RESERVE(), op('block', [ROOT, 'synthetic-block']), DISPATCH()])
add('snapshot-reserved-allowed', [OPEN(), RESERVE(), SNAPSHOT()])
add('snapshot-prefix-two-revisions', [OPEN(), RESERVE(), DISPATCH(), SNAPSHOT(), SNAPSHOT(1, UNICODE + '尾声')])
add('snapshot-exact-replay', [OPEN(), RESERVE(), SNAPSHOT(), SNAPSHOT()])
add('snapshot-wrong-revision', [OPEN(), RESERVE(), SNAPSHOT(4)])
add('snapshot-prefix-regression', [OPEN(), RESERVE(), SNAPSHOT(), SNAPSHOT(1, 'replacement')])
for (const text of ['<think>hidden</think>', '<THINK hidden>', 'visible </think>']) add(`snapshot-think-${definitions.length}`, [OPEN(), RESERVE(), SNAPSHOT(0, text)])
add('snapshot-empty-exact-hash', [OPEN(), RESERVE(), SNAPSHOT(0, '')])
add('snapshot-long-unicode-hash', [OPEN(), RESERVE(), SNAPSHOT(0, UNICODE.repeat(500) + 'final-tail')])
add('settle-reserved-rejected', [OPEN(), RESERVE(), SETTLE()])
add('settle-unknown-preserves-reservation', [OPEN(), RESERVE(), DISPATCH(), SNAPSHOT(), SETTLE(null), BUDGET(1040)])
for (const actualTokens of [0, 7, 900]) add(`settle-trusted-${actualTokens}`, [OPEN(), RESERVE(), DISPATCH(), SNAPSHOT(), SETTLE({ policy: usagePolicy, trusted: true, inputTokens: 2, completionTokens: 5, actualTokens }), BUDGET(1050)])
add('settle-untrusted-actual-not-authority', [OPEN(), RESERVE(), DISPATCH(), SETTLE({ policy: usagePolicy, trusted: false, actualTokens: 0 }), BUDGET()])
add('settle-diagnostics-failure-retained', [OPEN(), RESERVE(), DISPATCH(), op('settle', [ATTEMPT, null, 'length', 'GENERATION_CONTEXT_LENGTH_EXCEEDED', { startedAt: 1000, elapsedMs: 30, firstResponseMs: 3, lastResponseMs: 25, lastOutputMs: 24, phase: 'complete', endReason: 'failed', visibleEvents: 2, reasoningEvents: 1 }], { now: 1030 }), op('receipt', [ATTEMPT])])
add('settle-twice-rejected', [OPEN(), RESERVE(), DISPATCH(), SETTLE(), SETTLE()])
add('snapshot-terminal-rejected', [OPEN(), RESERVE(), DISPATCH(), SETTLE(), SNAPSHOT()])
add('snapshot-terminal-exact-replay', [OPEN(), RESERVE(), SNAPSHOT(), DISPATCH(), SETTLE(), SNAPSHOT()])
add('block-and-reserve-rejected', [OPEN(), op('block', [ROOT, UNICODE]), BUDGET(), RESERVE()])
add('block-missing-is-noop', [op('block', ['missing', 'synthetic'])])
add('pause-empty-root', [OPEN(), op('pause', [ROOT]), BUDGET()])
add('pause-reservation-cancels-liability', [OPEN(), RESERVE(), op('pause', [ROOT]), op('receipt', [ATTEMPT])])
add('pause-dispatched-remains-inflight', [OPEN(), RESERVE(), DISPATCH(), op('pause', [ROOT], { now: 1042 }), op('receipt', [ATTEMPT])])
add('cancel-reserved-root', [OPEN(), RESERVE(), op('pause', [ROOT, true]), BUDGET()])
add('cancel-idempotent', [OPEN(), op('pause', [ROOT, true]), op('pause', [ROOT]), op('pause', [ROOT, true])])
add('recover-empty-database', [op('recoverInterrupted')])
add('recover-reserved-cancels', [OPEN(), RESERVE(), op('recoverInterrupted'), op('receipt', [ATTEMPT])])
add('recover-dispatched-unknown', [OPEN(), RESERVE(), DISPATCH(), SNAPSHOT(), op('recoverInterrupted', [], { now: 1049 }), op('receipt', [ATTEMPT]), op('recoverInterrupted')])
add('recover-settled-preserves-usage', [OPEN(), RESERVE(), DISPATCH(), SETTLE({ policy: usagePolicy, trusted: true, actualTokens: 900 }), op('recoverInterrupted'), BUDGET()])
add('discard-missing-is-noop', [op('discardCandidate', ['missing'])])
add('discard-keeps-receipt-and-text', [OPEN(), RESERVE(), SNAPSHOT(), op('discardCandidate', [ARTIFACT]), op('receipt', [ATTEMPT]), op('discardCandidate', [ARTIFACT])])
add('physical-limit-after-terminal-attempts', [OPEN(), RESERVE(), DISPATCH(), SETTLE(), RESERVE('invoke-2'), DISPATCH(), SETTLE(), RESERVE('invoke-3'), DISPATCH(), SETTLE(), RESERVE('invoke-4')])
add('elapsed-limit-after-settle', [OPEN(), RESERVE(), DISPATCH(), SETTLE(null, 'stop', { now: 1100 }), RESERVE('invoke-2')])

function writeNew(filename, bytes) { const fd = openSync(filename, 'wx', 0o600); try { writeFileSync(fd, bytes) } finally { closeSync(fd) } }
const quote = name => '"' + name.replaceAll('"', '""') + '"'
const jsonColumns = new Set(['action_json', 'budget_json', 'binding_json', 'attempt_json', 'usage_receipt_json', 'artifact_json', 'idempotency_key', 'open_key'])
function normalize(value) {
  if (typeof value === 'string' && /^00000000-0000-4000-8000-\d{12}$/u.test(value)) {
    const sequence = Number(value.slice(-12)); if (sequence > 0 && sequence <= state.sequence) return `generated-id:${sequence}`
  }
  if (Array.isArray(value)) return value.map(normalize)
  if (value && typeof value === 'object') return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, normalize(item)]))
  return value
}
function wholeSql(db) {
  return Object.fromEntries(db.prepare("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name").all().map(({ name }) => [name, db.prepare(`SELECT * FROM ${quote(name)} ORDER BY rowid`).all().map(row => Object.fromEntries(Object.entries(row).map(([column, value]) => [column, normalize(name.startsWith('generation_') && jsonColumns.has(column) && typeof value === 'string' ? JSON.parse(value) : value)])))]))
}
function resolveRefs(value, saved) {
  if (typeof value === 'string' && value.startsWith('@ref:')) {
    const parts = value.slice(5).split('.'); let current = saved
    for (const part of parts) { assert(current && Object.hasOwn(current, part), `unresolved operation reference ${value}`); current = current[part] }
    return clone(current)
  }
  if (Array.isArray(value)) return value.map(item => resolveRefs(item, saved))
  if (value && typeof value === 'object') return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, resolveRefs(item, saved)]))
  return value
}
function invokePure(repository, method, args, db) {
  switch (method) {
    case 'rootKey': return shared.rootActionIdempotencyKey(...args)
    case 'assertReservation': return shared.assertReservation(...args)
    case 'assertTransition': return shared.assertAttemptTransition(...args)
    case 'tokenLiability': return shared.tokenLiability(...args)
    case 'binding': {
      // Binding is intentionally private: exercise it through the ORIGINAL open
      // constructor/method, rollback only the surrounding fixture transaction.
      const rollback = new Error('fixture-rollback-only'), sequence = state.sequence
      let result
      try { db.transaction(() => { result = repository.open({ ...clone(request), ...args[0] }).binding; throw rollback })() }
      catch (error) { if (error !== rollback) throw error }
      finally { state.sequence = sequence }
      return result
    }
    default: assert.fail(`unknown actual pure method ${method}`)
  }
}
const cases = [], unsupportedBoundaries = []
try {
  const bytes = readFileSync(baseline), names = new Set()
  for (const definition of definitions) {
    assert(!names.has(definition.name)); names.add(definition.name)
    writeNew(path.join(directory, definition.name + '-before.db'), bytes)
    const filename = path.join(directory, definition.name + '.db'); writeNew(filename, bytes)
    const db = new Database(filename)
    try {
      db.pragma('foreign_keys=ON')
      for (const table of ['generation_roots', 'generation_runs', 'generation_attempts', 'generation_artifacts']) assert.equal(db.prepare(`SELECT COUNT(*) AS n FROM ${table}`).get().n, 0, `actual migrated baseline must be synthetic and empty: ${table}`)
      state.db = definition.nilDB ? null : db; state.now = 1000; state.sequence = 0
      const repository = new GenerationRunRepository(() => state.db, () => state.now)
      const saved = {}, operations = []
      for (const operation of definition.operations) {
        state.now = operation.now ?? 1000
        const args = resolveRefs(clone(operation.args), saved), input = JSON.stringify(args)
        const before = wholeSql(db), beforeBytes = db.serialize()
        let result = null, error = ''
        try { const actual = operation.pure ? invokePure(repository, operation.method, args, db) : repository[operation.method](...args); result = actual === undefined ? null : actual; if (operation.save) saved[operation.save] = clone(result) }
        catch (caught) { assert(caught instanceof Error); assert(!(caught instanceof TypeError) && !(caught instanceof SyntaxError), `unsupported native failure must not masquerade as parity: ${definition.name}/${operation.method}: ${caught}`); error = caught.message }
        assert.equal(JSON.stringify(args), input, `actual method mutated caller args: ${definition.name}/${operation.method}`)
        const after = wholeSql(db), unchanged = JSON.stringify(before) === JSON.stringify(after), byteUnchanged = beforeBytes.equals(db.serialize())
        if (error || operation.pure) assert(unchanged, `failed/pure operation mutated whole SQL: ${definition.name}/${operation.method}`)
        operations.push({ ...operation, now: state.now, result: normalize(result), error, wholeSql: after, unchanged, byteUnchanged, argsUnchanged: true, generatedIDs: state.sequence })
      }
      cases.push({ name: definition.name, nilDB: definition.nilDB ?? false, operations })
    } finally { state.db = null; db.close() }
  }
  // Actual TypeError/SyntaxError evidence is explicitly OUTSIDE differential
  // parity; these invalid JavaScript object shapes are not ordinary typed input.
  for (const [name, malformed] of [['null-fingerprint', { ...binding, fingerprint: null }], ['null-source-ref', { ...binding, sourceRefs: [null] }], ['null-source-refs', { ...binding, sourceRefs: null }]]) {
    const db = new Database(':memory:')
    try {
      state.sequence = 0
      const repository = new GenerationRunRepository(() => db, () => 1000)
      let originalError = '', originalErrorName = ''
      try { repository.open({ ...request, ...malformed }); assert.fail('expected actual unsupported native failure') } catch (error) { assert(error instanceof TypeError); originalError = error.message; originalErrorName = error.name }
      unsupportedBoundaries.push({ name, method: 'binding', args: [malformed], originalError, originalErrorName, classification: 'Outside original parity: malformed native JavaScript input; no business method substitution.' })
    } finally { db.close() }
  }
  assert(cases.length >= 100)
  for (const required of ['electron/repositories/generation-run-repository.ts', 'src/shared/generation-contract.ts', 'src/shared/source-ref.ts', 'src/shared/plot-outline-contract.ts']) assert(originalModules.has(required), required)
  const counts = {}
  for (const fixture of cases) for (const operation of fixture.operations) counts[`${operation.pure ? 'pure' : 'ledger'}:${operation.method}`] = (counts[`${operation.pure ? 'pure' : 'ledger'}:${operation.method}`] ?? 0) + 1
  writeNew(target, JSON.stringify({ format: 1, cases, unsupportedBoundaries, originalModules: [...originalModules].sort(), counts, normalization: 'Only generated UUIDs receive generated-id:N labels; only declared generation ledger JSON SQL columns are parsed structurally. Every text/source hash, source body, author metadata timestamp, SQL clock, and other raw SQL value remains exact. Open replay still validates original ordered encoded fingerprint/budget inside actual business methods.', authority: 'Actual centrally migrated baseline; actual recursive original business modules; actual GenerationRunRepository constructor with admitted database and now injections. crypto wrapper changes only randomUUID and reexports actual createHash. No author data, secrets, fake class or business-method monkeypatch. Binding invokes actual open under an outer rollback-only fixture transaction.', excludedNativeDomain: 'Malformed native JavaScript null fingerprint/ref shapes are recorded separately as unsupported evidence, never counted as original domain parity.' }))
  console.error(`Original generation ledger emitted ${cases.length} cases, ${JSON.stringify(counts)}, ${originalModules.size} actual recursive modules`)
} finally { hooks.deregister(); delete globalThis[key] }
