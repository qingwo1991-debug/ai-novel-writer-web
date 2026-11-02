import assert from 'node:assert/strict'
import { builtinModules, createRequire, registerHooks, stripTypeScriptTypes } from 'node:module'
import { chmodSync, constants, copyFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { createHash } from 'node:crypto'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

// GitHub-only synthetic oracle, NOT an application runtime bridge. The actual
// TypeScript repositories execute unchanged; only their private getProjectDb
// dependency is redirected to this case's private synthetic baseline clone.
assert.equal(process.env.GITHUB_ACTIONS, 'true', 'contract generation is GitHub-only')
const base = process.env.RUNNER_TEMP
const target = process.env.GO_POSTHISTORY_CONTRACT
assert(base && target && path.isAbsolute(base) && path.isAbsolute(target))
assert.equal(target, path.join(base, 'novel-go-posthistory', 'contract.json'), 'use the exact disposable post/history output path')
const sqliteContract = process.env.GO_SQLITE_CONTRACT
assert(sqliteContract && path.isAbsolute(sqliteContract))
const source = path.join(path.dirname(sqliteContract), 'baseline-v7.db')
const relativeSource = path.relative(base, source)
assert(relativeSource && !relativeSource.startsWith('..') && !path.isAbsolute(relativeSource), 'baseline must be disposable and synthetic')
assert(existsSync(source), 'generate the synthetic SQLite contract first')
const directory = path.dirname(target)
assert(!existsSync(directory), 'never reuse a preexisting fixture directory')
mkdirSync(directory, { recursive: true, mode: 0o700 })

const root = new URL('../../', import.meta.url)
const databaseURL = new URL('electron/database', root).href
const databaseStub = 'posthistory-contract:database'
const globalKey = '__GO_POSTHISTORY_CONTRACT_DB__'
assert(!Object.hasOwn(globalThis, globalKey))
globalThis[globalKey] = null
const Database = createRequire(import.meta.url)('better-sqlite3')
// NODE_PATH may supply the native driver. Load its lazy native binding BEFORE
// installing hooks so its CommonJS/native dependencies cannot be intercepted.
const driverProbe = new Database(':memory:')
driverProbe.close()
const builtins = new Set(builtinModules.flatMap(name => [name, `node:${name}`]))
const hooks = registerHooks({
  resolve(specifier, context, nextResolve) {
    if (builtins.has(specifier)) return nextResolve(specifier, context)
    assert(specifier.startsWith('.') || specifier.startsWith('file:'), `unexpected oracle dependency ${specifier}`)
    const url = new URL(specifier, context.parentURL ?? root).href
    if (url === databaseURL || url === `${databaseURL}.ts`) return { url: databaseStub, shortCircuit: true }
    const candidate = url.endsWith('.ts') ? url : `${url}.ts`
    const filename = fileURLToPath(candidate)
    const relative = path.relative(fileURLToPath(root), filename)
    assert(relative && !relative.startsWith('..') && !path.isAbsolute(relative), 'dependency escaped original checkout')
    assert(relative.startsWith('electron/repositories/') || relative.startsWith('electron/migrations/') || relative.startsWith('src/shared/') || relative.startsWith('src/utils/'), `unexpected original module ${relative}`)
    assert(existsSync(filename), `missing original module ${relative}`)
    return { url: candidate, shortCircuit: true }
  },
  load(url, context, nextLoad) {
    if (url === databaseStub) return { format: 'module', source: `export function getProjectDb(){return globalThis[${JSON.stringify(globalKey)}]}`, shortCircuit: true }
    if (!url.startsWith(root.href) || !url.endsWith('.ts')) return nextLoad(url, context)
    return { format: 'module', source: stripTypeScriptTypes(readFileSync(new URL(url), 'utf8'), { mode: 'transform', sourceUrl: url }), shortCircuit: true }
  },
})
const { PostProcessRepository } = await import(new URL('electron/repositories/post-process-repository.ts', root).href)
const { LLMHistoryRepository } = await import(new URL('electron/repositories/llm-repository.ts', root).href)

const FIXED_RUN_ID = '11111111-1111-4111-8111-111111111111'
const droppedTimestampKeys = new Set(['created_at', 'updated_at', 'createdAt', 'updatedAt'])
const receiptTimestampKeys = new Set(['completed_at', 'last_attempt_at', 'completedAt', 'lastAttemptAt'])
// Transparent native-UUID canonicalization: do NOT patch crypto/randomUUID or
// the original repository. After a successful CreateRun ONLY, replace its actual
// returned UUID consistently in result and raw after rows. The local Go test
// wrapper overrides repository newRunID with FIXED_RUN_ID for the same cases.
// Seeded ordering times remain untouched in SQLite and the closed before image.
function normalize(value, createdRunId, field) {
  if (Buffer.isBuffer(value)) return value.toJSON()
  if (Array.isArray(value)) return value.map(child => normalize(child, createdRunId))
  if (value !== null && typeof value === 'object') return Object.fromEntries(Object.entries(value)
    .filter(([key]) => !droppedTimestampKeys.has(key))
    .map(([key, child]) => [key, normalize(child, createdRunId, key)]))
  if (typeof value === 'string' && receiptTimestampKeys.has(field) && value !== '') return '<timestamp>'
  if (createdRunId !== undefined && value === createdRunId) return FIXED_RUN_ID
  return value
}
function snapshot(db) {
  // These are SELECT * raw storage rows: no omitted business fields, projections,
  // boolean coercions, or omission of error_message. Result projections differ.
  return {
    post_process_runs: db.prepare('SELECT * FROM post_process_runs ORDER BY rowid').all(),
    post_process_steps: db.prepare('SELECT * FROM post_process_steps ORDER BY id').all(),
    llm_calls: db.prepare('SELECT * FROM llm_calls ORDER BY id').all(),
  }
}
function insert(db, table, row) {
  const columns = Object.keys(row)
  return db.prepare(`INSERT INTO ${table}(${columns.join(',')}) VALUES(${columns.map(() => '?').join(',')})`).run(...Object.values(row)).lastInsertRowid
}
function update(db, table, where, values) {
  const columns = Object.keys(values)
  assert(columns.every(key => /^[a-z_]+$/u.test(key)))
  db.prepare(`UPDATE ${table} SET ${columns.map(key => `${key}=?`).join(',')} WHERE ${where}`).run(...Object.values(values))
}
const BODY = 'Synthetic 定稿 😀\nsecond line <&>\u2028\u2029'
const BODY_HASH = createHash('sha256').update(BODY).digest('hex')
const FID = 'synthetic-finalization-7-v3'
const RUN = '22222222-2222-4222-8222-222222222222'
const OTHER_RUN = '33333333-3333-4333-8333-333333333333'
const EARLY = '2026-01-01 00:00:00'
const LATE = '2026-02-01 00:00:00'
const identity = () => ({ draftId: 11, finalizationId: FID, chapterNumber: 7, contentHash: BODY_HASH })
const criticalSteps = () => [{ key: 'facts', label: '关键事实 中文 😀', critical: true }, { key: 'index', label: 'Optional index', critical: false }]
const genericInput = () => ({ triggerSourceType: 'arch_extract', triggerSourceId: '11', sourceLabel: 'Synthetic source 中文 😀', steps: criticalSteps() })
const finalizedInput = () => ({ ...genericInput(), triggerSourceType: 'chapter_finalize', triggerSourceId: '7', finalizedSource: identity() })
function seedDraft(db, { id = 11, chapter = 7, version = 3, status = 'finalized', body = BODY } = {}) {
  const contentId = insert(db, 'contents', { body, created_at: EARLY })
  insert(db, 'drafts', { id, chapter_number: chapter, version, status, content_id: contentId, word_count: 19, created_at: EARLY, updated_at: EARLY })
}
function seedOutbox(db, { draftId = 11, chapter = 7, fid = FID, body = BODY } = {}) {
  // All NOT NULL fields are explicit, including content_revision/target_file_name.
  insert(db, 'finalization_outbox', { finalization_id: fid, draft_id: draftId, chapter_number: chapter,
    chapter_title: 'Synthetic chapter', content_hash: createHash('sha256').update(body).digest('hex'), content_revision: 3,
    content_snapshot: body, target_file_name: '007-synthetic.txt', knowledge_document_id: '',
    publication_status: 'pending', last_error: '', published_at: null, created_at: EARLY, updated_at: EARLY })
}
function seedRun(db, { id = RUN, type = 'arch_extract', sourceId = '11', label = 'Seeded run', passed = 0, created = EARLY, updated = EARLY } = {}) {
  insert(db, 'post_process_runs', { id, trigger_source_type: type, trigger_source_id: sourceId, source_label: label,
    all_critical_passed: passed, created_at: created, updated_at: updated })
}
function seedStep(db, { id = 101, run = RUN, key = 'facts', label = 'Critical 中文 😀', critical = 1, ok = 0, error = '', attempts = 0, completed = '', lastAttempt = '' } = {}) {
  insert(db, 'post_process_steps', { id, run_id: run, step_key: key, label, critical, ok, error_msg: error,
    attempt_count: attempts, completed_at: completed, last_attempt_at: lastAttempt })
}
const callInput = () => ({ modelId: 'synthetic-model', modelName: 'Synthetic 模型 😀', purpose: 'write', promptTokens: 7, completionTokens: 11, totalTokens: 18, durationMs: 123, success: true })
function seedCall(db, { id = 1, model = 'seed-model', name = 'Seed model', purpose = 'review', prompt = 7, completion = 11, total = 18, duration = 123, success = 1, error = '', created = EARLY } = {}) {
  insert(db, 'llm_calls', { id, model_id: model, model_name: name, purpose, prompt_tokens: prompt, completion_tokens: completion,
    total_tokens: total, duration_ms: duration, success, error_message: error, created_at: created })
}
const definitions = []
const cases = []
const add = (name, method, options = {}) => definitions.push({ name, method, ...options })
const expectCount = count => assert.equal(definitions.length, count, 'reviewed fixture section count')
function dispatch(definition, input) {
  const args = definition.args ?? []
  switch (definition.method) {
    case 'CreateRun': return PostProcessRepository.createRun(input)
    case 'LatestRun': return PostProcessRepository.getLatestRun(...args)
    case 'Steps': return PostProcessRepository.getSteps(...args)
    case 'MarkStepOK': return PostProcessRepository.markStepOk(...args)
    case 'MarkStepFailed': return PostProcessRepository.markStepFailed(...args)
    case 'AllPassed': return PostProcessRepository.isAllCriticalPassed(...args)
    case 'FailedStepLabels': return PostProcessRepository.getFailedStepLabels(...args)
    case 'LogCall': return LLMHistoryRepository.logCall(input)
    case 'Stats': return LLMHistoryRepository.getStats()
    // Empty args invokes the original default parameter (50), not a local limit.
    case 'History': return LLMHistoryRepository.getHistory(...args)
    default: throw new Error(`unknown method ${definition.method}`)
  }
}

// 35 CreateRun cases: both legacy/generic calls and the complete source gate.
const create = (suffix, options = {}) => add(`create-${suffix}`, 'CreateRun', { makeInput: genericInput, ...options })
create('generic-critical-and-noncritical')
create('generic-empty-steps', { changeInput: input => { input.steps = [] } })
create('generic-noncritical-only', { changeInput: input => { input.steps = [{ key: 'index', label: 'Index', critical: false }] } })
// Baseline has NO UNIQUE(run_id,step_key): duplicate keys are valid creation.
create('generic-duplicate-keys', { changeInput: input => { input.steps = [{ key: 'same', label: 'First', critical: true }, { key: 'same', label: 'Second', critical: false }] } })
create('generic-empty-key-label', { changeInput: input => { input.steps = [{ key: '', label: '', critical: true }] } })
create('generic-null-source-label', { changeInput: input => { input.sourceLabel = null } })
create('generic-null-step-label', { changeInput: input => { input.steps[0].label = null } })
create('legacy-finalize-missing-source', { changeInput: input => { input.triggerSourceType = 'chapter_finalize'; input.triggerSourceId = '7' } })
create('legacy-finalize-null-source', { changeInput: input => { input.triggerSourceType = 'chapter_finalize'; input.triggerSourceId = '7'; input.finalizedSource = null } })
create('prefix-requires-source', { changeInput: input => { input.triggerSourceId = `finalization:${FID}` } })
create('prefix-null-source-rejected', { changeInput: input => { input.triggerSourceId = `finalization:${FID}`; input.finalizedSource = null } })
const finalized = (suffix, options = {}) => create(`finalized-${suffix}`, { makeInput: finalizedInput, ...options })
finalized('matching-full-snapshot')
finalized('wrong-trigger-type', { changeInput: input => { input.triggerSourceType = 'arch_extract' } })
finalized('wrong-trigger-id', { changeInput: input => { input.triggerSourceId = '8' } })
for (const [field, value] of [['draftId', 999], ['finalizationId', 'stale-finalization'], ['chapterNumber', 8], ['contentHash', '0'.repeat(64)]]) {
  finalized(`identity-drift-${field}`, { changeInput: input => { input.finalizedSource[field] = value } })
}
finalized('live-status-drift', { seed: db => update(db, 'drafts', 'id=11', { status: 'reviewed' }) })
finalized('live-chapter-drift', { seed: db => update(db, 'drafts', 'id=11', { chapter_number: 9 }) })
finalized('live-body-drift', { seed: db => db.prepare('UPDATE contents SET body=? WHERE id=(SELECT content_id FROM drafts WHERE id=11)').run('Author edit') })
for (const [field, value] of [['finalization_id', 'different-receipt'], ['content_hash', 'f'.repeat(64)], ['content_snapshot', 'Different frozen body']]) {
  finalized(`outbox-drift-${field}`, { seed: db => update(db, 'finalization_outbox', 'draft_id=11', { [field]: value }) })
}
finalized('missing-draft', { seed: db => db.prepare('DELETE FROM drafts WHERE id=11').run() })
finalized('missing-outbox', { seed: db => db.prepare('DELETE FROM finalization_outbox WHERE draft_id=11').run() })
finalized('new-highest-finalized-version', { seed: db => seedDraft(db, { id: 31, version: 4 }) })
finalized('new-highest-finalized-version-beats-smaller-id', { seed: db => seedDraft(db, { id: 10, version: 4 }) })
finalized('higher-nonfinal-version-ignored', { seed: db => seedDraft(db, { id: 31, version: 4, status: 'draft' }) })
finalized('empty-body-valid-hash-and-snapshot', { seed: db => { db.prepare('UPDATE contents SET body=? WHERE id=(SELECT content_id FROM drafts WHERE id=11)').run(''); update(db, 'finalization_outbox', 'draft_id=11', { content_snapshot: '', content_hash: createHash('sha256').update('').digest('hex') }) }, changeInput: input => { input.finalizedSource.contentHash = createHash('sha256').update('').digest('hex') } })
finalized('failed-publication-not-source-drift', { seed: db => update(db, 'finalization_outbox', 'draft_id=11', { publication_status: 'failed', last_error: 'Synthetic publication failure' }) })
finalized('uncompared-outbox-metadata-ignored', { seed: db => update(db, 'finalization_outbox', 'draft_id=11', { chapter_number: 70, chapter_title: 'Changed title', content_revision: 99, target_file_name: 'other.txt', knowledge_document_id: 'other-document' }) })
create('insert-step-failure-rolls-back-run-and-earlier-step', { seed: db => db.exec("CREATE TRIGGER contract_reject_second_step BEFORE INSERT ON post_process_steps WHEN NEW.step_key='index' BEGIN SELECT RAISE(ABORT,'SYNTHETIC_STEP_INSERT_ABORT'); END") })
finalized('prefix-trigger-id-still-must-be-chapter', { changeInput: input => { input.triggerSourceId = `finalization:${FID}` } })
finalized('uppercase-hash-is-not-identical', { changeInput: input => { input.finalizedSource.contentHash = BODY_HASH.toUpperCase() } })
expectCount(35)

// 14 latest-run cases. IDs intentionally disagree with insertion/time order.
const latest = (suffix, options = {}) => add(`latest-${suffix}`, 'LatestRun', { args: ['arch_extract', '11'], ...options })
latest('empty')
latest('source-type-and-id-isolation', { seed: db => { seedRun(db); seedRun(db, { id: OTHER_RUN, type: 'other', created: LATE }); seedRun(db, { id: 'different-source', sourceId: '22', created: LATE }) } })
latest('rowid-breaks-created-time-tie', { seed: db => { seedRun(db, { id: 'z-first' }); seedRun(db, { id: 'a-second', label: 'Last inserted' }) } })
latest('created-time-precedes-rowid', { seed: db => { seedRun(db, { id: 'new-time-first', created: LATE }); seedRun(db, { id: 'old-time-last', created: EARLY, updated: LATE }) } })
latest('aggregate-flag-is-exactly-one', { seed: db => seedRun(db, { passed: 2 }) })
latest('nullable-label-preserved', { seed: db => seedRun(db, { label: null }) })
function finalizedRuns(db) {
  seedRun(db, { type: 'chapter_finalize', sourceId: `finalization:${FID}`, passed: 1 })
  seedRun(db, { id: OTHER_RUN, type: 'chapter_finalize', sourceId: '7', created: LATE, label: 'Legacy numeric is not current' })
}
latest('decimal-finalization-source-lookup', { args: ['chapter_finalize', '7'], seed: finalizedRuns })
latest('leading-zero-finalization-source-lookup', { args: ['chapter_finalize', '007'], seed: finalizedRuns })
latest('literal-finalization-source-id', { args: ['chapter_finalize', `finalization:${FID}`], seed: finalizedRuns })
latest('nondecimal-id-not-rewritten', { args: ['chapter_finalize', '7.0'], seed: db => { finalizedRuns(db); seedRun(db, { id: 'decimal-text', type: 'chapter_finalize', sourceId: '7.0' }) } })
latest('other-source-type-digits-not-rewritten', { args: ['arch_extract', '7'], seed: db => { seedRun(db, { id: 'other-type-run', sourceId: '7' }); finalizedRuns(db) } })
latest('no-finalized-draft-falls-back-to-numeric', { args: ['chapter_finalize', '7'], seed: db => { finalizedRuns(db); update(db, 'drafts', 'id=11', { status: 'draft' }) } })
latest('new-current-finalization-has-no-run', { args: ['chapter_finalize', '7'], seed: db => { finalizedRuns(db); seedDraft(db, { id: 31, version: 4 }); seedOutbox(db, { draftId: 31, fid: 'synthetic-new-finalization' }) } })
latest('newest-draft-without-outbox-join-selects-earlier', { args: ['chapter_finalize', '7'], seed: db => { finalizedRuns(db); seedDraft(db, { id: 31, version: 4 }) } })
expectCount(49)

// 8 aggregate reads use the latest stored flag, not a fresh steps calculation.
const all = (suffix, options = {}) => add(`all-passed-${suffix}`, 'AllPassed', { args: ['arch_extract', '11'], ...options })
all('empty')
all('stored-false', { seed: db => seedRun(db) })
all('stored-true', { seed: db => seedRun(db, { passed: 1 }) })
all('latest-false-supersedes-true', { seed: db => { seedRun(db, { passed: 1 }); seedRun(db, { id: OTHER_RUN, created: LATE }) } })
all('stored-true-despite-pending-steps', { seed: db => { seedRun(db, { passed: 1 }); seedStep(db) } })
all('decimal-finalization-lookup', { args: ['chapter_finalize', '7'], seed: finalizedRuns })
all('leading-zero-finalization-lookup', { args: ['chapter_finalize', '0007'], seed: finalizedRuns })
all('source-isolation', { args: ['arch_extract', '999'], seed: db => seedRun(db, { passed: 1 }) })
expectCount(57)

// 6 step projections + 5 failed-label queries, including raw Buffer JSON shape.
const steps = (suffix, options = {}) => add(`steps-${suffix}`, 'Steps', { args: [RUN], ...options })
steps('empty')
steps('id-order-and-run-isolation', { seed: db => { seedRun(db); seedRun(db, { id: OTHER_RUN }); seedStep(db, { id: 109, key: 'z' }); seedStep(db, { id: 101, key: 'a' }); seedStep(db, { id: 105, run: OTHER_RUN }) } })
steps('nullable-optional-fields-default-to-empty', { seed: db => { seedRun(db); seedStep(db, { label: null, error: null, completed: null, lastAttempt: null, attempts: null }) } })
steps('flags-require-integer-one', { seed: db => { seedRun(db); seedStep(db, { critical: 2, ok: 2 }) } })
steps('blob-fields-preserve-storage-class', { seed: db => { seedRun(db); seedStep(db, { label: Buffer.from('Blob label 中文'), error: Buffer.from('Blob error') }) } })
steps('receipt-null-versus-empty-and-nonempty', { seed: db => { seedRun(db); seedStep(db, { id: 101, completed: null, lastAttempt: '' }); seedStep(db, { id: 102, key: 'other', completed: EARLY, lastAttempt: LATE }) } })
const failed = (suffix, options = {}) => add(`failed-labels-${suffix}`, 'FailedStepLabels', { args: [RUN], ...options })
failed('empty')
failed('only-critical-not-ok', { seed: db => { seedRun(db); seedStep(db); seedStep(db, { id: 102, key: 'done', ok: 1 }); seedStep(db, { id: 103, key: 'optional', critical: 0 }); seedStep(db, { id: 104, key: 'noncanonical', critical: 2 }) } })
failed('duplicates-and-id-order', { seed: db => { seedRun(db); seedStep(db, { id: 109, key: 'same', label: 'Second' }); seedStep(db, { id: 101, key: 'same', label: 'First' }) } })
failed('null-label-not-coalesced', { seed: db => { seedRun(db); seedStep(db, { label: null }) } })
failed('blob-label-preserved', { seed: db => { seedRun(db); seedStep(db, { label: Buffer.from('Blob label') }) } })
expectCount(68)

// 13 cases per marker: success/failure receipt + aggregate update is atomic.
// Duplicate matches are legal stored data but changes!==1 rejects and rolls back.
for (const method of ['MarkStepOK', 'MarkStepFailed']) {
  const isOK = method === 'MarkStepOK'
  const args = key => isOK ? [RUN, key] : [RUN, key, 'Synthetic failure 中文 😀\n<&>']
  const mark = (suffix, options = {}) => add(`${method}-${suffix}`, method, { args: args('facts'), seed: db => { seedRun(db); seedStep(db) }, ...options })
  mark('critical-with-another-pending', { seed: db => { seedRun(db); seedStep(db); seedStep(db, { id: 102, key: 'other-critical' }) } })
  mark(isOK ? 'last-critical-completes-aggregate' : 'success-to-failure-clears-aggregate-and-completion', { seed: db => { seedRun(db, { passed: isOK ? 0 : 1 }); seedStep(db, { ok: isOK ? 0 : 1, completed: isOK ? '' : EARLY, attempts: 2, lastAttempt: EARLY }) } })
  mark('noncritical-does-not-pass-pending-critical', { args: args('optional'), seed: db => { seedRun(db); seedStep(db); seedStep(db, { id: 102, key: 'optional', critical: 0 }) } })
  mark('noncritical-only-makes-aggregate-true', { seed: db => { seedRun(db); seedStep(db, { critical: 0 }) } })
  mark(isOK ? 'repeated-success-increments-attempts' : 'repeated-failure-increments-attempts', { seed: db => { seedRun(db, { passed: isOK ? 1 : 0 }); seedStep(db, { ok: isOK ? 1 : 0, attempts: 4, completed: isOK ? EARLY : '', lastAttempt: EARLY, error: isOK ? '' : 'Old failure' }) } })
  mark(isOK ? 'prior-failure-clears-error' : 'empty-error-still-records-failure', { ...(isOK ? {} : { args: [RUN, 'facts', ''] }), seed: db => { seedRun(db, { passed: 1 }); seedStep(db, { ok: isOK ? 0 : 1, attempts: 3, error: 'Old error', completed: EARLY, lastAttempt: EARLY }) } })
  mark('missing-run', { args: isOK ? ['missing-run', 'facts'] : ['missing-run', 'facts', 'missing'] })
  mark('missing-key', { args: args('missing-key') })
  mark('duplicate-key-rolls-back-both-receipts', { seed: db => { seedRun(db, { passed: 1 }); seedStep(db, { ok: 1, completed: EARLY, attempts: 2 }); seedStep(db, { id: 102, ok: 0, attempts: 5, error: 'Unchanged' }) } })
  mark('aggregate-error-rolls-back-receipt', { seed: db => { seedRun(db); seedStep(db, { attempts: 2, error: 'Keep this error' }); db.exec("CREATE TRIGGER contract_reject_aggregate BEFORE UPDATE ON post_process_runs BEGIN SELECT RAISE(ABORT,'SYNTHETIC_AGGREGATE_ABORT'); END") } })
  mark('null-attempt-count-remains-null', { seed: db => { seedRun(db); seedStep(db, { attempts: null, completed: null, lastAttempt: null }) } })
  mark('empty-key-is-valid', { args: args(''), seed: db => { seedRun(db); seedStep(db, { key: '', label: '' }) } })
  mark('other-run-isolation', { seed: db => { seedRun(db); seedStep(db); seedRun(db, { id: OTHER_RUN, passed: 1 }); seedStep(db, { id: 102, run: OTHER_RUN, ok: 1, completed: EARLY, lastAttempt: EARLY, attempts: 9 }) } })
}
expectCount(94)

// 11 log cases. Optional errorMessage defaults through the original ?? ''.
const log = (suffix, options = {}) => add(`log-${suffix}`, 'LogCall', { makeInput: callInput, ...options })
log('known-success')
log('known-failure', { changeInput: input => { input.success = false; input.errorMessage = 'Provider failed' } })
log('unknown-usage', { changeInput: input => { input.promptTokens = null; input.completionTokens = null; input.totalTokens = null } })
log('known-zero-usage', { changeInput: input => { input.promptTokens = 0; input.completionTokens = 0; input.totalTokens = 0 } })
log('mixed-null-usage', { changeInput: input => { input.promptTokens = null; input.totalTokens = null } })
log('omitted-error-on-failure', { changeInput: input => { input.success = false } })
log('null-error-default', { changeInput: input => { input.success = false; input.errorMessage = null } })
log('empty-error', { changeInput: input => { input.errorMessage = '' } })
log('unicode-error-and-metadata', { changeInput: input => { input.success = false; input.purpose = 'review 中文\n<&>'; input.errorMessage = '失败 😀\n<&>\u2028\u2029' } })
log('success-retains-raw-finish-error', { changeInput: input => { input.errorMessage = 'finish:length' } })
log('zero-duration-appends-history', { seed: db => seedCall(db, { id: 17 }), changeInput: input => { input.durationMs = 0 } })
expectCount(105)

// 9 SQL statistics cases: knownUsageCalls depends ONLY on total_tokens IS NOT NULL.
const stats = (suffix, seed) => add(`stats-${suffix}`, 'Stats', { seed })
stats('empty')
stats('all-unknown-usage', db => { seedCall(db, { prompt: null, completion: null, total: null }); seedCall(db, { id: 2, prompt: null, completion: null, total: null, success: 0 }) })
stats('known-zero-not-null', db => seedCall(db, { prompt: 0, completion: 0, total: 0 }))
stats('known-positive', db => seedCall(db))
stats('mixed-known-and-unknown', db => { seedCall(db); seedCall(db, { id: 2, prompt: null, completion: null, total: null, success: 0 }); seedCall(db, { id: 3, prompt: 0, completion: 0, total: 0 }) })
stats('known-total-null-components', db => seedCall(db, { prompt: null, completion: null, total: 18 }))
stats('null-total-known-components', db => seedCall(db, { prompt: 7, completion: 11, total: null }))
stats('success-counts-exact-zero-and-one', db => { seedCall(db, { success: 2 }); seedCall(db, { id: 2, success: null }); seedCall(db, { id: 3, success: 0 }); seedCall(db, { id: 4, success: 1 }) })
stats('real-valued-usage-preserved', db => { seedCall(db, { prompt: 1.5, completion: 2.25, total: 3.75 }); seedCall(db, { id: 2, prompt: null, completion: 0, total: 0 }) })
expectCount(114)

// 17 history cases. Raw after keeps error_message; public projection does NOT.
const history = (suffix, options = {}) => add(`history-${suffix}`, 'History', { ...options })
const manyCalls = db => { for (let id = 1; id <= 55; id++) seedCall(db, { id, success: id % 2, error: id % 2 ? '' : 'finish:length' }) }
history('empty')
history('undefined-limit-original-default-fifty', { seed: manyCalls })
history('limit-one', { args: [1], seed: manyCalls })
history('limit-zero', { args: [0], seed: manyCalls })
history('negative-limit-unbounded', { args: [-1], seed: manyCalls })
history('large-limit-all-rows', { args: [100], seed: manyCalls })
for (const [suffix, error] of [['length', 'finish:length'], ['content-filter', 'finish:content_filter'], ['finish-cancelled', 'finish:cancelled'], ['cancelled', 'cancelled'], ['error', 'finish:error'], ['unknown', 'finish:unknown'], ['unrecognized', 'Provider failure 中文 😀'], ['empty', '']]) {
  history(`failed-finish-${suffix}`, { seed: db => seedCall(db, { success: 0, error }) })
}
history('success-stop-precedes-error-case', { seed: db => seedCall(db, { success: 1, error: 'finish:content_filter' }) })
history('nullable-and-zero-usage-preserved', { seed: db => { seedCall(db, { prompt: null, completion: null, total: null, success: 0, error: null }); seedCall(db, { id: 2, prompt: 0, completion: 0, total: 0, duration: 0 }) } })
history('id-desc-not-created-time-order', { seed: db => { seedCall(db, { id: 9, created: EARLY }); seedCall(db, { id: 1, created: LATE }) } })
expectCount(131)
// SQLite INTEGER affinity does not reject REAL chapter numbers. The original
// uses strict numeric equality here, so 1.5 remains valid when all source facts
// agree; it is not a digit-only latest-run lookup and must not be truncated.
finalized('real-chapter-number', {
  seed: db => {
    update(db, 'drafts', 'id=11', { chapter_number: 1.5 })
    update(db, 'finalization_outbox', 'draft_id=11', { chapter_number: 1.5 })
  },
  changeInput: input => { input.triggerSourceId = '1.5'; input.finalizedSource.chapterNumber = 1.5 },
})
expectCount(132)
assert.equal(new Set(definitions.map(definition => definition.name)).size, definitions.length)

try {
  for (const definition of definitions) {
    assert(/^[A-Za-z0-9_-]+$/u.test(definition.name))
    const filename = path.join(directory, `${definition.name}.db`)
    copyFileSync(source, filename, constants.COPYFILE_EXCL)
    chmodSync(filename, 0o600)
    const db = new Database(filename)
    let before
    try {
      db.pragma('foreign_keys=ON')
      db.pragma('journal_mode=DELETE')
      globalThis[globalKey] = db
      for (const table of ['drafts', 'finalization_outbox', 'post_process_runs', 'post_process_steps', 'llm_calls']) {
        assert.equal(db.prepare(`SELECT COUNT(*) AS n FROM ${table}`).get().n, 0, `${table} baseline must be synthetic and empty`)
      }
      seedDraft(db)
      seedOutbox(db)
      seedDraft(db, { id: 22, chapter: 8, version: 2, status: 'reviewed', body: 'Unrelated synthetic source' })
      definition.seed?.(db)
      const input = definition.makeInput?.()
      definition.changeInput?.(input)
      assert.deepEqual(db.pragma('foreign_key_check'), [], 'fixtures must have valid foreign keys')
      // Preserve exact preoperation schema, triggers, sequences, storage classes,
      // source fixtures and seeded ordering timestamps for the Go local wrapper.
      before = db.serialize()
      let result = null
      let error
      try { result = dispatch(definition, input) ?? null } catch (failure) {
        assert(failure instanceof Error)
        error = failure.message
      }
      let createdRunId
      if (definition.method === 'CreateRun' && error === undefined) {
        assert.equal(typeof result, 'string')
        assert.match(result, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/u)
        createdRunId = result
      }
      const after = normalize(snapshot(db), createdRunId)
      assert.deepEqual(db.pragma('foreign_key_check'), [])
      cases.push({ name: definition.name, database: filename, method: definition.method,
        ...(input === undefined ? {} : { input }), ...(definition.args === undefined ? {} : { args: definition.args }),
        result: normalize(result, createdRunId), ...(error === undefined ? {} : { error }), after })
    } finally {
      globalThis[globalKey] = null
      db.close()
      // Restore only AFTER close. Published files are closed, exact BEFORE
      // images, never postoperation state used to shortcut native execution.
      if (before) writeFileSync(filename, before, { mode: 0o600 })
    }
  }
  assert.equal(cases.length, 132)
  writeFileSync(target, JSON.stringify({ formatVersion: 1, cases }) + '\n', { flag: 'wx', mode: 0o600 })
} finally {
  delete globalThis[globalKey]
  hooks.deregister()
}
console.log(`Generated ${cases.length} original post-process/LLM-history contracts from private synthetic baseline-v7 clones`)
