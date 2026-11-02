import assert from 'node:assert/strict'
import { builtinModules, createRequire, registerHooks, stripTypeScriptTypes } from 'node:module'
import { chmodSync, constants, copyFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { createHash } from 'node:crypto'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

// GitHub-only synthetic oracle, NOT an application bridge. Execute the actual
// original repositories; the ONLY substituted dependency is private getProjectDb.
assert.equal(process.env.GITHUB_ACTIONS, 'true', 'contract generation is GitHub-only')
const base = process.env.RUNNER_TEMP
const target = process.env.GO_NARRATIVE_CONTRACT
assert(base && target && path.isAbsolute(base) && path.isAbsolute(target))
assert.equal(target, path.join(base, 'novel-go-narrative', 'contract.json'), 'use the exact disposable narrative output path')
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
const databaseStub = 'narrative-contract:database'
const globalKey = '__GO_NARRATIVE_CONTRACT_DB__'
assert(!Object.hasOwn(globalThis, globalKey))
const Database = createRequire(import.meta.url)('better-sqlite3')
// Native bindings load lazily. Probe BEFORE installing hooks, including when
// NODE_PATH supplies the driver, so no CJS/native driver dependencies are hooked.
const driverProbe = new Database(':memory:')
driverProbe.close()
globalThis[globalKey] = null
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
    assert(relative.startsWith('electron/repositories/') || relative.startsWith('src/shared/'), `unexpected original module ${relative}`)
    assert(existsSync(filename), `missing original module ${relative}`)
    return { url: candidate, shortCircuit: true }
  },
  load(url, context, nextLoad) {
    if (url === databaseStub) return { format: 'module', source: `export function getProjectDb(){return globalThis[${JSON.stringify(globalKey)}]}`, shortCircuit: true }
    if (!url.startsWith(root.href) || !url.endsWith('.ts')) return nextLoad(url, context)
    return { format: 'module', source: stripTypeScriptTypes(readFileSync(new URL(url), 'utf8'), { mode: 'transform', sourceUrl: url }), shortCircuit: true }
  },
})
const { NarrativeThreadRepository } = await import(new URL('electron/repositories/narrative-thread-repository.ts', root).href)
const { ConsistencyExemptionRepository } = await import(new URL('electron/repositories/consistency-exemption-repository.ts', root).href)

const droppedTimestampKeys = new Set(['created_at', 'updated_at', 'createdAt', 'updatedAt'])
function normalize(value) {
  if (Buffer.isBuffer(value)) return value.toJSON()
  if (Array.isArray(value)) return value.map(normalize)
  if (value !== null && typeof value === 'object') return Object.fromEntries(Object.entries(value)
    .filter(([key]) => !droppedTimestampKeys.has(key))
    .map(([key, child]) => [key, normalize(child)]))
  return value
}
const tables = ['narrative_thread_plans', 'narrative_thread_confirmations', 'consistency_exemptions']
function snapshot(db) {
  // No projections/coalescing/boolean conversion: ALL raw columns, rowid order.
  return Object.fromEntries(tables.map(table => [table, db.prepare(`SELECT * FROM ${table} ORDER BY rowid`).all()]))
}
function insert(db, table, row) {
  const columns = Object.keys(row)
  return db.prepare(`INSERT INTO ${table}(${columns.join(',')}) VALUES(${columns.map(() => '?').join(',')})`).run(...Object.values(row)).lastInsertRowid
}
const EARLY = '2026-01-01 00:00:00'
const LATE = '2026-02-01 00:00:00'
const BODY = 'Synthetic 定稿 😀\n旧门 缓缓开启，林舟取出星钥。\nAlpha beta gamma\n' + '证'.repeat(260) + '\n' + '😀'.repeat(130)
const planInput = () => ({ title: '星钥之谜', type: '伏笔', targetStartChapter: 5, targetEndChapter: 9, authorIntent: '林舟调查旧门，逐步揭开星钥的来历。' })
const eventInput = () => ({ planId: 1, draftId: 11, type: 'planted', evidence: '旧门 缓缓开启', reason: '林舟发现线索 中文 😀' })
const relevantInput = () => ({ chapterNumber: 20, title: '无关标题', keyEvents: '无关事件', characters: [] })
function seedPlan(db, { id = 1, title = '星钥之谜', type = '伏笔', start = 5, end = 9, intent = '林舟调查旧门，逐步揭开星钥的来历。' } = {}) {
  insert(db, 'narrative_thread_plans', { id, title, type, target_start_chapter: start, target_end_chapter: end,
    author_intent: intent, created_at: EARLY, updated_at: LATE })
}
function seedDraft(db, { id = 11, chapter = 7, version = 3, status = 'finalized', body = BODY } = {}) {
  const contentId = insert(db, 'contents', { body, created_at: EARLY })
  insert(db, 'drafts', { id, chapter_number: chapter, version, status, source: 'write', content_id: contentId,
    word_count: 19, source_dependencies: '[]', created_at: EARLY, updated_at: EARLY })
}
function seedOutbox(db, { draftId = 11, chapter = 7, body = BODY, title = '第七章 星钥 中文 😀', publication = 'pending' } = {}) {
  // Every NOT NULL outbox field is explicit, especially revision and filename.
  insert(db, 'finalization_outbox', { finalization_id: `synthetic-narrative-${draftId}`, draft_id: draftId, chapter_number: chapter,
    chapter_title: title, content_hash: createHash('sha256').update(body).digest('hex'), content_revision: 3,
    content_snapshot: body, target_file_name: `${chapter}-synthetic-narrative.txt`, knowledge_document_id: '',
    publication_status: publication, last_error: '', published_at: null, created_at: EARLY, updated_at: EARLY })
}
function seedSource(db, options = {}) { seedDraft(db, options); seedOutbox(db, { ...options, draftId: options.id ?? 11 }) }
function seedEvent(db, { id = 1, plan = 1, draft = 11, type = 'planted', evidence = '旧门 缓缓开启', reason = '林舟发现线索', created = EARLY } = {}) {
  insert(db, 'narrative_thread_confirmations', { id, plan_id: plan, draft_id: draft, event_type: type, evidence, reason, created_at: created })
}
function seedExemption(db, { key = 'fact:key', reason = 'Seeded 合成理由 😀', revoked = 0 } = {}) {
  insert(db, 'consistency_exemptions', { stable_fact_key: key, reason, revoked })
}
function primary(db) { seedPlan(db); seedSource(db) }
function withEvent(db) { primary(db); seedEvent(db) }
function rejectTrigger(db, { name, operation, table, message }) {
  // Side effect precedes ABORT: the published after must prove that SQLite
  // rolls back BOTH this side effect and the enclosing statement/cascade.
  db.exec(`CREATE TRIGGER ${name} ${operation} ON ${table} BEGIN
    INSERT INTO consistency_exemptions(stable_fact_key,reason,revoked) VALUES('trigger-side-effect','must roll back',0);
    SELECT RAISE(ABORT,'${message}'); END`)
}
const definitions = []
const cases = []
const add = (name, method, options = {}) => definitions.push({ name, method, ...options })
const expectCount = count => assert.equal(definitions.length, count, 'reviewed fixture section count')
function dispatch(definition, input) {
  const args = definition.args ?? []
  switch (definition.method) {
    case 'CreatePlan': return NarrativeThreadRepository.createPlan(input)
    case 'UpdatePlan': return NarrativeThreadRepository.updatePlan(args[0], input)
    case 'DeletePlan': return NarrativeThreadRepository.deletePlan(...args)
    case 'GetPlan': return NarrativeThreadRepository.getPlan(...args)
    case 'ConfirmEvent': return NarrativeThreadRepository.confirmEvent(input)
    case 'List': return NarrativeThreadRepository.list()
    case 'Relevant': return NarrativeThreadRepository.listRelevantActive(input)
    case 'Exemptions': return ConsistencyExemptionRepository.list()
    case 'SaveExemption': return ConsistencyExemptionRepository.save(...args)
    case 'RevokeExemption': return ConsistencyExemptionRepository.revoke(...args)
    default: throw new Error(`unknown method ${definition.method}`)
  }
}

// 26 creates: JS trim and UTF-16 lengths, full numeric gates, validation order.
const create = (suffix, options = {}) => add(`create-${suffix}`, 'CreatePlan', { makeInput: planInput, ...options })
create('basic')
create('trim-all-text', { changeInput: input => { input.title = ' \t星钥之谜\n'; input.type = '\r 伏笔\t'; input.authorIntent = '\n 林舟调查 \r' } })
create('feff-is-trimmed', { changeInput: input => { input.title = '\uFEFF星钥\uFEFF'; input.type = '\uFEFF伏笔\uFEFF'; input.authorIntent = '\uFEFF调查\uFEFF' } })
create('nel-is-not-trimmed', { changeInput: input => { input.title = '\u0085'; input.type = '\u0085伏笔\u0085'; input.authorIntent = '\u0085调查\u0085' } })
for (const field of ['title', 'type', 'authorIntent']) create(`empty-${field}`, { changeInput: input => { input[field] = ' \t\n\uFEFF' } })
for (const [field, limit] of [['title', 120], ['type', 60], ['authorIntent', 1000]]) {
  create(`${field}-length-${limit}`, { changeInput: input => { input[field] = '文'.repeat(limit) } })
  create(`${field}-length-${limit + 1}`, { changeInput: input => { input[field] = '文'.repeat(limit + 1) } })
  create(`${field}-emoji-utf16-at-limit`, { changeInput: input => { input[field] = '😀'.repeat(limit / 2) } })
  create(`${field}-emoji-utf16-over-limit`, { changeInput: input => { input[field] = '😀'.repeat(limit / 2) + '文' } })
}
create('range-start-zero', { changeInput: input => { input.targetStartChapter = 0 } })
create('range-start-fraction', { changeInput: input => { input.targetStartChapter = 1.5 } })
create('range-end-before-start', { changeInput: input => { input.targetEndChapter = 4 } })
create('range-end-fraction', { changeInput: input => { input.targetEndChapter = 9.5 } })
create('range-end-unsafe', { changeInput: input => { input.targetEndChapter = Number.MAX_SAFE_INTEGER + 1 } })
create('text-validation-before-range', { changeInput: input => { input.title = ''; input.targetStartChapter = 0 } })
create('trigger-abort-rolls-back-insert-and-side-effect', { seed: db => rejectTrigger(db, { name: 'contract_abort_plan_create', operation: 'AFTER INSERT', table: 'narrative_thread_plans', message: 'SYNTHETIC_PLAN_CREATE_ABORT' }) })
expectCount(26)

// 10 updates: missing-plan errors come AFTER validation, no event destruction.
const update = (suffix, options = {}) => add(`update-${suffix}`, 'UpdatePlan', { args: [1], makeInput: planInput, seed: db => seedPlan(db), ...options })
update('trim-success', { changeInput: input => { input.title = '  新标题  '; input.type = '\n支线\t'; input.authorIntent = '  新意图  ' } })
update('missing-plan', { args: [999] })
update('missing-plan-invalid-text-first', { args: [999], changeInput: input => { input.title = '' } })
update('missing-plan-invalid-range-first', { args: [999], changeInput: input => { input.targetEndChapter = 1 } })
update('feff-trim-nel-retained', { changeInput: input => { input.title = '\uFEFF\u0085新谜\u0085\uFEFF' } })
update('inclusive-single-chapter-range', { changeInput: input => { input.targetStartChapter = 7; input.targetEndChapter = 7 } })
update('maximum-safe-integer-range', { changeInput: input => { input.targetStartChapter = Number.MAX_SAFE_INTEGER; input.targetEndChapter = Number.MAX_SAFE_INTEGER } })
update('retains-existing-confirmations', { seed: withEvent, changeInput: input => { input.title = '改名仍保留确认'; input.targetStartChapter = 1; input.targetEndChapter = 2 } })
update('emoji-utf16-invalid', { changeInput: input => { input.title = '😀'.repeat(60) + 'x' } })
update('trigger-abort-rolls-back-update-and-side-effect', { seed: db => { withEvent(db); rejectTrigger(db, { name: 'contract_abort_plan_update', operation: 'AFTER UPDATE', table: 'narrative_thread_plans', message: 'SYNTHETIC_PLAN_UPDATE_ABORT' }) } })
expectCount(36)

// 5 deletes, including real ON DELETE CASCADE and a failing cascade rollback.
add('delete-existing-with-cascade', 'DeletePlan', { args: [1], seed: withEvent })
add('delete-missing', 'DeletePlan', { args: [999], seed: withEvent })
add('delete-zero-id', 'DeletePlan', { args: [0] })
add('delete-plan-isolation', 'DeletePlan', { args: [1], seed: db => { withEvent(db); seedPlan(db, { id: 2, title: '保留支线' }); seedEvent(db, { id: 2, plan: 2 }) } })
add('delete-cascade-abort-restores-plan-events-and-side-effect', 'DeletePlan', { args: [1], seed: db => { withEvent(db); rejectTrigger(db, { name: 'contract_abort_event_delete', operation: 'AFTER DELETE', table: 'narrative_thread_confirmations', message: 'SYNTHETIC_PLAN_CASCADE_ABORT' }) } })
expectCount(41)

// 4 get projections. NOT NULL text columns still permit native SQLite BLOBs.
add('get-missing', 'GetPlan', { args: [999] })
add('get-existing-unicode', 'GetPlan', { args: [1], seed: db => seedPlan(db, { title: '星钥 😀 <&>\u2028\u2029' }) })
add('get-blob-storage-classes', 'GetPlan', { args: [1], seed: db => seedPlan(db, { title: Buffer.from('Blob 标题'), type: Buffer.from([0, 255, 34]), intent: Buffer.from('Blob 意图') }) })
add('get-id-isolation', 'GetPlan', { args: [9], seed: db => { seedPlan(db, { id: 3, title: '不是这个' }); seedPlan(db, { id: 9, title: '目标计划' }) } })
expectCount(45)

// 40 confirmations: original frozen source gate, exact JS whitespace semantics,
// UNIQUE constraint (NOT an invented idempotency rule), and statement rollback.
const confirm = (suffix, options = {}) => add(`confirm-${suffix}`, 'ConfirmEvent', { makeInput: eventInput, seed: primary, ...options })
for (const type of ['planted', 'progressing', 'resolved', 'abandoned']) confirm(`type-${type}`, { changeInput: input => { input.type = type } })
confirm('trim-evidence-reason-feff', { changeInput: input => { input.evidence = '\uFEFF \t旧门 缓缓开启\n\uFEFF'; input.reason = '\uFEFF 原因 😀 \uFEFF' } })
confirm('collapse-ascii-whitespace', { changeInput: input => { input.evidence = 'Alpha\t\r\nbeta gamma' } })
confirm('collapse-all-js-whitespace', { seed: db => { seedPlan(db); seedSource(db, { body: '甲\uFEFF\u00A0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200A\u2028\u2029\u202F\u205F\u3000乙' }) }, changeInput: input => { input.evidence = '甲\t\v\f \r\n乙' } })
confirm('evidence-feff-internal-is-removed', { changeInput: input => { input.evidence = '旧\uFEFF门缓缓开启' } })
confirm('nel-not-removed-from-frozen-content', { seed: db => { seedPlan(db); seedSource(db, { body: '甲\u0085乙' }) }, changeInput: input => { input.evidence = '甲乙' } })
confirm('nel-is-preserved-and-exactly-matches', { seed: db => { seedPlan(db); seedSource(db, { body: '\u0085甲\u0085乙\u0085' }) }, changeInput: input => { input.evidence = '\u0085甲\u0085乙\u0085' } })
confirm('evidence-240', { changeInput: input => { input.evidence = '证'.repeat(240) } })
confirm('evidence-241', { changeInput: input => { input.evidence = '证'.repeat(241) } })
confirm('evidence-emoji-240-utf16', { changeInput: input => { input.evidence = '😀'.repeat(120) } })
confirm('evidence-emoji-242-utf16', { changeInput: input => { input.evidence = '😀'.repeat(121) } })
confirm('reason-500', { changeInput: input => { input.reason = '因'.repeat(500) } })
confirm('reason-501', { changeInput: input => { input.reason = '因'.repeat(501) } })
confirm('reason-emoji-500-utf16', { changeInput: input => { input.reason = '😀'.repeat(250) } })
confirm('reason-emoji-502-utf16', { changeInput: input => { input.reason = '😀'.repeat(251) } })
confirm('invalid-type', { changeInput: input => { input.type = 'planned' } })
confirm('empty-evidence', { changeInput: input => { input.evidence = '\t\n\uFEFF' } })
confirm('empty-reason', { changeInput: input => { input.reason = ' \t\uFEFF' } })
confirm('invalid-input-before-source-and-plan', { seed: () => {}, changeInput: input => { input.type = 'unknown'; input.planId = 999; input.draftId = 999 } })
confirm('missing-draft', { seed: db => seedPlan(db) })
confirm('nonfinalized-source', { seed: db => { seedPlan(db); seedSource(db, { status: 'reviewed' }) } })
confirm('finalized-source-without-outbox', { seed: db => { seedPlan(db); seedDraft(db) } })
confirm('source-error-before-missing-plan', { seed: db => seedDraft(db), changeInput: input => { input.planId = 999 } })
confirm('evidence-error-before-missing-plan', { changeInput: input => { input.planId = 999; input.evidence = '不在冻结正文中' } })
confirm('missing-plan-after-valid-source-and-evidence', { changeInput: input => { input.planId = 999 } })
confirm('exact-duplicate-is-unique-error', { seed: withEvent })
confirm('same-evidence-different-type-allowed', { seed: withEvent, changeInput: input => { input.type = 'progressing' } })
confirm('same-normalized-evidence-different-literal-allowed', { seed: withEvent, changeInput: input => { input.evidence = '旧门\t缓缓开启' } })
confirm('same-evidence-other-plan-allowed', { seed: db => { withEvent(db); seedPlan(db, { id: 2 }) }, changeInput: input => { input.planId = 2 } })
confirm('frozen-outbox-survives-live-body-edits', { seed: db => { primary(db); db.prepare('UPDATE contents SET body=? WHERE id=(SELECT content_id FROM drafts WHERE id=11)').run('作者已改正文，不含旧证据') } })
confirm('live-body-only-evidence-rejected', { seed: db => { primary(db); db.prepare('UPDATE contents SET body=? WHERE id=(SELECT content_id FROM drafts WHERE id=11)').run('作者新增证据') }, changeInput: input => { input.evidence = '作者新增证据' } })
confirm('changed-frozen-snapshot-rejects-old-evidence', { seed: db => { primary(db); db.prepare('UPDATE finalization_outbox SET content_snapshot=? WHERE draft_id=11').run('另一个冻结快照') } })
confirm('failed-publication-still-valid-source', { seed: db => { seedPlan(db); seedSource(db, { publication: 'failed' }) } })
confirm('result-chapter-from-draft-not-outbox', { seed: db => { primary(db); db.prepare('UPDATE finalization_outbox SET chapter_number=70 WHERE draft_id=11').run() } })
confirm('old-still-finalized-version-is-not-latest-gated', { seed: db => { primary(db); seedSource(db, { id: 31, version: 4, body: '新版定稿' }) } })
confirm('trigger-abort-rolls-back-confirmation-and-side-effect', { seed: db => { primary(db); rejectTrigger(db, { name: 'contract_abort_confirm', operation: 'AFTER INSERT', table: 'narrative_thread_confirmations', message: 'SYNTHETIC_CONFIRM_ABORT' }) } })
confirm('empty-chapter-title-preserved', { seed: db => { seedPlan(db); seedSource(db, { title: '' }) } })
expectCount(85)

// 18 list cases: chapter ASC/id ASC determines the last event, not severity,
// creation time, or insertion order; MAX(finalized chapter) needs no outbox.
const list = (suffix, options = {}) => add(`list-${suffix}`, 'List', options)
list('empty')
list('plans-id-ascending', { seed: db => { seedPlan(db, { id: 9 }); seedPlan(db, { id: 2, title: '先列出的计划' }) } })
list('no-finalized-chapters-zero-dormancy', { seed: db => seedPlan(db) })
list('planned-dormancy-from-target-start', { seed: primary })
list('max-finalized-chapter-without-outbox-drives-overdue', { seed: db => { withEvent(db); seedDraft(db, { id: 32, chapter: 12, version: 1 }) } })
list('higher-nonfinalized-chapter-ignored', { seed: db => { withEvent(db); seedDraft(db, { id: 32, chapter: 99, version: 1, status: 'draft' }) } })
list('overdue-is-strictly-after-end', { seed: db => { seedPlan(db, { end: 7 }); seedSource(db); seedEvent(db) } })
list('active-last-event-dormancy', { seed: db => { withEvent(db); seedDraft(db, { id: 32, chapter: 10, version: 1 }) } })
list('resolved-terminal-never-overdue-or-dormant', { seed: db => { primary(db); seedEvent(db, { type: 'resolved' }); seedDraft(db, { id: 32, chapter: 30, version: 1 }) } })
list('abandoned-terminal-never-overdue-or-dormant', { seed: db => { primary(db); seedEvent(db, { type: 'abandoned' }); seedDraft(db, { id: 32, chapter: 30, version: 1 }) } })
list('chapter-order-before-id-and-created-time', { seed: db => { primary(db); seedSource(db, { id: 23, chapter: 3, version: 1 }); seedEvent(db, { id: 1, type: 'progressing', created: EARLY }); seedEvent(db, { id: 99, draft: 23, type: 'resolved', created: LATE }) } })
list('same-chapter-id-order-not-insertion-or-time', { seed: db => { primary(db); seedEvent(db, { id: 9, type: 'progressing', created: EARLY }); seedEvent(db, { id: 2, type: 'resolved', created: LATE }) } })
list('later-active-event-reopens-terminal', { seed: db => { primary(db); seedEvent(db, { id: 1, type: 'resolved' }); seedEvent(db, { id: 2, type: 'planted' }) } })
list('superseded-archived-events-disappear', { seed: db => { withEvent(db); db.prepare("UPDATE drafts SET status='archived' WHERE id=11").run(); seedSource(db, { id: 31, version: 4, body: '替代定稿' }) } })
list('missing-outbox-events-disappear-but-max-remains', { seed: db => { withEvent(db); db.prepare('DELETE FROM finalization_outbox WHERE draft_id=11').run() } })
list('nonfinalized-events-disappear', { seed: db => { withEvent(db); db.prepare("UPDATE drafts SET status='reviewed' WHERE id=11").run() } })
list('raw-event-blob-evidence-reason-preserved', { seed: db => { primary(db); seedEvent(db, { evidence: Buffer.from([0, 255, 34]), reason: Buffer.from('Blob 理由') }) } })
list('dormancy-clamped-before-target-start', { seed: db => { seedPlan(db, { start: 10, end: 15 }); seedSource(db) } })
expectCount(103)

// 20 relevant cases: terminal exclusion BEFORE matching; inclusive window OR
// case-sensitive trimmed character substring OR >=2 UTF-16-unit title match.
const relevant = (suffix, options = {}) => add(`relevant-${suffix}`, 'Relevant', { makeInput: relevantInput, seed: db => seedPlan(db), ...options })
relevant('empty', { seed: () => {} })
relevant('inclusive-start', { changeInput: input => { input.chapterNumber = 5 } })
relevant('inclusive-end', { changeInput: input => { input.chapterNumber = 9 } })
relevant('outside-window-no-match')
relevant('resolved-excluded-even-window-and-title-match', { seed: db => { primary(db); seedEvent(db, { type: 'resolved' }) }, changeInput: input => { input.chapterNumber = 7; input.title = '星钥之谜'; input.characters = ['林舟'] } })
relevant('abandoned-excluded-even-window-and-character-match', { seed: db => { primary(db); seedEvent(db, { type: 'abandoned' }) }, changeInput: input => { input.chapterNumber = 7; input.characters = ['林舟'] } })
relevant('character-trim-substring-title', { changeInput: input => { input.characters = [' \t星钥\n'] } })
relevant('character-matches-type', { changeInput: input => { input.characters = ['伏笔'] } })
relevant('character-matches-author-intent', { changeInput: input => { input.characters = ['林舟'] } })
relevant('character-matches-event-evidence', { seed: db => { primary(db); seedEvent(db, { evidence: '只有事件里的角色' }) }, changeInput: input => { input.characters = ['事件里的角色'] } })
relevant('character-matches-event-reason', { seed: db => { primary(db); seedEvent(db, { reason: '只在原因中的人物' }) }, changeInput: input => { input.characters = ['原因中的人物'] } })
relevant('empty-and-feff-only-characters-ignored', { changeInput: input => { input.characters = ['', ' \t', '\uFEFF'] } })
relevant('nel-character-is-not-trimmed', { changeInput: input => { input.characters = ['\u0085林舟\u0085'] } })
relevant('title-substring-in-context-title', { changeInput: input => { input.title = '追查星钥之谜的结局' } })
relevant('title-substring-in-key-events', { changeInput: input => { input.keyEvents = '他想起星钥之谜' } })
relevant('one-unit-title-does-not-match-context', { seed: db => seedPlan(db, { title: '钥' }), changeInput: input => { input.title = '钥'; input.keyEvents = '钥' } })
relevant('one-emoji-title-is-two-utf16-units', { seed: db => seedPlan(db, { title: '😀' }), changeInput: input => { input.keyEvents = '出现😀' } })
relevant('title-match-spans-context-newline', { seed: db => seedPlan(db, { title: '甲\n乙' }), changeInput: input => { input.title = '前文甲'; input.keyEvents = '乙后文' } })
relevant('matching-is-case-sensitive', { seed: db => seedPlan(db, { title: 'StarKey', type: 'Plot', intent: 'Alice investigates' }), changeInput: input => { input.title = 'starkey'; input.characters = ['alice'] } })
relevant('multiple-matches-keep-plan-id-order-and-hidden-events-do-not-match', { seed: db => { seedPlan(db, { id: 9, title: '后一个林舟' }); seedPlan(db, { id: 2, title: '先一个林舟' }); seedPlan(db, { id: 5, title: '无关', intent: '无关意图' }); seedSource(db, { status: 'archived' }); seedEvent(db, { plan: 5, evidence: '隐藏角色' }) }, changeInput: input => { input.characters = ['林舟', '隐藏角色'] } })
expectCount(123)

// 8 exemption reads. Respect the actual schema: revoked is NOT NULL CHECK 0/1,
// reason is NOT NULL; SQLite's TEXT PRIMARY KEY still permits NULL keys/BLOBs.
const exemptions = (suffix, seed) => add(`exemptions-${suffix}`, 'Exemptions', { seed })
exemptions('empty')
exemptions('active-is-false', db => seedExemption(db))
exemptions('revoked-is-true-only-integer-one', db => seedExemption(db, { revoked: 1 }))
exemptions('key-sql-order-not-rowid-order', db => { seedExemption(db, { key: 'z-last' }); seedExemption(db, { key: 'a-first', revoked: 1 }); seedExemption(db, { key: 'm-middle' }) })
exemptions('unicode-keys-and-reasons-preserved', db => seedExemption(db, { key: '事实:星钥😀\uFEFF\u0085', reason: '理由\n<&>\u2028\u2029' }))
exemptions('blob-key-and-reason-preserved', db => seedExemption(db, { key: Buffer.from([0, 255, 34]), reason: Buffer.from('Blob 原因') }))
exemptions('nullable-primary-key-preserved', db => seedExemption(db, { key: null }))
exemptions('raw-empty-key-reason-and-whitespace-not-read-trimmed', db => { seedExemption(db, { key: '', reason: '' }); seedExemption(db, { key: ' key ', reason: ' reason ', revoked: 1 }) })
expectCount(131)

// 15 saves: key validation precedes reason, upsert resets revoked, lengths are
// JS UTF-16 units. Native void is recorded as null, never a fabricated record.
const save = (suffix, options = {}) => add(`save-exemption-${suffix}`, 'SaveExemption', { args: ['fact:key', 'Reason 中文 😀'], ...options })
save('new')
save('trim-key-and-reason', { args: [' \tfact:key\n', ' \tReason\r\n'] })
save('feff-trimmed-nel-retained', { args: ['\uFEFF\u0085fact\u0085\uFEFF', '\uFEFF\u0085reason\u0085\uFEFF'] })
save('upsert-active', { seed: db => seedExemption(db) })
save('resave-revoked-resets-flag', { seed: db => seedExemption(db, { revoked: 1 }) })
save('upsert-trimmed-key-does-not-duplicate', { args: [' fact:key ', 'New reason'], seed: db => seedExemption(db, { revoked: 1 }) })
save('empty-key-before-empty-reason', { args: [' \uFEFF', ''] })
save('empty-reason', { args: ['fact:key', ' \t\uFEFF'] })
save('key-500', { args: ['键'.repeat(500), 'Reason'] })
save('key-501', { args: ['键'.repeat(501), 'Reason'] })
save('reason-500', { args: ['fact:key', '因'.repeat(500)] })
save('reason-501', { args: ['fact:key', '因'.repeat(501)] })
save('key-emoji-500-utf16', { args: ['😀'.repeat(250), 'Reason'] })
save('key-emoji-502-utf16', { args: ['😀'.repeat(251), 'Reason'] })
save('reason-emoji-502-utf16-preserves-existing-row', { args: ['fact:key', '😀'.repeat(251)], seed: db => seedExemption(db, { revoked: 1 }) })
expectCount(146)

// 8 revokes: unknown valid keys are no-ops; repeated revoke is still void/null.
const revoke = (suffix, options = {}) => add(`revoke-exemption-${suffix}`, 'RevokeExemption', { args: ['fact:key'], ...options })
revoke('active', { seed: db => seedExemption(db) })
revoke('already-revoked-noop', { seed: db => seedExemption(db, { revoked: 1 }) })
revoke('missing-key-noop')
revoke('trim-key', { args: [' \t\uFEFFfact:key\uFEFF\n'], seed: db => seedExemption(db) })
revoke('empty-key-invalid', { args: [' \t\uFEFF'] })
revoke('key-over-500-invalid', { args: ['键'.repeat(501)] })
revoke('key-emoji-500-utf16', { args: ['😀'.repeat(250)], seed: db => seedExemption(db, { key: '😀'.repeat(250) }) })
revoke('other-keys-isolated', { seed: db => { seedExemption(db); seedExemption(db, { key: 'other:key', reason: 'Keep active' }) } })
expectCount(154)

// Narrow reader/binder follow-up: original Array.join/template coercions decode
// BLOB text for matching while retaining raw Buffers in returned projections.
relevant('blob-author-intent-character-match', { seed: db => seedPlan(db, { intent: Buffer.from('Alice') }), changeInput: input => { input.characters = ['Alice'] } })
relevant('blob-title-utf8-byte-length-gate-and-context-match', { seed: db => seedPlan(db, { title: Buffer.from('钥') }), changeInput: input => { input.title = '发现钥'; input.keyEvents = '' } })
relevant('blob-event-evidence-character-match', { seed: db => { primary(db); seedEvent(db, { evidence: Buffer.from('Alice evidence') }) }, changeInput: input => { input.characters = ['Alice'] } })
relevant('blob-event-reason-character-match', { seed: db => { primary(db); seedEvent(db, { reason: Buffer.from('Alice reason') }) }, changeInput: input => { input.characters = ['Alice'] } })
// SQLite native binding rejects boolean IDs rather than coercing true to 1.
// Capture the driver's actual Error.message unchanged, with no guessed sentinel.
confirm('boolean-draft-id-native-binding-error', { seed: db => { seedPlan(db); seedSource(db, { id: 1 }) }, changeInput: input => { input.draftId = true } })
confirm('boolean-plan-id-native-binding-error', { changeInput: input => { input.planId = true } })
expectCount(160)
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
      for (const table of [...tables, 'drafts', 'finalization_outbox']) {
        assert.equal(db.prepare(`SELECT COUNT(*) AS n FROM ${table}`).get().n, 0, `${table} baseline must be synthetic and empty`)
      }
      definition.seed?.(db)
      const input = definition.makeInput?.()
      definition.changeInput?.(input)
      assert.deepEqual(db.pragma('foreign_key_check'), [], 'fixtures must have valid foreign keys')
      // Keep exact source facts, schema/trigger SQL, sequences, storage classes,
      // and timestamps. This is the preoperation DB for the native Go wrapper.
      before = db.serialize()
      let result = null
      let error
      try { result = dispatch(definition, input) ?? null } catch (failure) {
        assert(failure instanceof Error)
        error = failure.message
      }
      const after = normalize(snapshot(db))
      assert.deepEqual(db.pragma('foreign_key_check'), [])
      cases.push({ name: definition.name, database: filename, method: definition.method,
        ...(input === undefined ? {} : { input }), ...(definition.args === undefined ? {} : { args: definition.args }),
        result: normalize(result), ...(error === undefined ? {} : { error }), after })
    } finally {
      globalThis[globalKey] = null
      db.close()
      // Restore only AFTER close. Every exported database is the exact BEFORE
      // image, never the postoperation rows used to manufacture a native result.
      if (before) writeFileSync(filename, before, { mode: 0o600 })
    }
  }
  assert.equal(cases.length, 160)
  writeFileSync(target, JSON.stringify({ formatVersion: 1, cases }) + '\n', { flag: 'wx', mode: 0o600 })
} finally {
  delete globalThis[globalKey]
  hooks.deregister()
}
console.log(`Generated ${cases.length} original narrative/exemption contracts from private synthetic baseline-v7 clones`)
