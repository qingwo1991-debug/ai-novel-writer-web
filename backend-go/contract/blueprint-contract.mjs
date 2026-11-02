import assert from 'node:assert/strict'
import { builtinModules, createRequire, registerHooks, stripTypeScriptTypes } from 'node:module'
import { closeSync, existsSync, mkdirSync, openSync, readFileSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

// GitHub-only synthetic oracle: execute the checked-in original repository, not
// a reimplementation of its schema, parsers, canonicalizer or business methods.
assert.equal(process.env.GITHUB_ACTIONS, 'true', 'contract generation is GitHub-only')
const base = process.env.RUNNER_TEMP
const target = process.env.GO_BLUEPRINT_CONTRACT
assert(base && target && path.isAbsolute(base) && path.isAbsolute(target))
assert.equal(target, path.join(base, 'novel-go-blueprint', 'contract.json'), 'use the exact disposable blueprint output path')
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
const databaseStub = 'blueprint-contract:database'
const globalKey = '__GO_BLUEPRINT_CONTRACT_DB__'
assert(!Object.hasOwn(globalThis, globalKey))
const Database = createRequire(import.meta.url)('better-sqlite3')
// Native bindings are lazy: probe BEFORE hooks, including with NODE_PATH.
const driverProbe = new Database(':memory:')
driverProbe.close()
globalThis[globalKey] = null
const builtins = new Set(builtinModules.flatMap(name => [name, `node:${name}`]))
const originalModules = new Set()
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
    assert(relative.startsWith('electron/') || relative.startsWith('src/shared/') || relative.startsWith('src/services/') || relative.startsWith('src/utils/'), `unexpected original module ${relative}`)
    assert(existsSync(filename), `missing original module ${relative}`)
    originalModules.add(relative)
    return { url: candidate, shortCircuit: true }
  },
  load(url, context, nextLoad) {
    if (url === databaseStub) return { format: 'module', source: `export function getProjectDb(){return globalThis[${JSON.stringify(globalKey)}]}\nexport function getCurrentProjectPath(){throw new Error('UNREACHABLE_BLUEPRINT_PROJECT_PATH')}`, shortCircuit: true }
    if (!url.startsWith(root.href) || !url.endsWith('.ts')) return nextLoad(url, context)
    return { format: 'module', source: stripTypeScriptTypes(readFileSync(new URL(url), 'utf8'), { mode: 'transform', sourceUrl: url }), shortCircuit: true }
  },
})
// In particular: baseline-blueprint-schema, character repositories, generation
// proof services and their recursive imports are real files. No business stubs.
const { BlueprintRepository } = await import(new URL('electron/repositories/blueprint-repository.ts', root).href)

const clockKeys = new Set(['created_at', 'updated_at', 'completed_at', 'createdAt', 'updatedAt', 'completedAt'])
function normalize(value) {
  if (Buffer.isBuffer(value)) return value.toJSON()
  if (Array.isArray(value)) return value.map(normalize)
  if (value !== null && typeof value === 'object') return Object.fromEntries(Object.entries(value)
    .filter(([key]) => !clockKeys.has(key)).map(([key, child]) => [key, normalize(child)]))
  return value
}
const EARLY = '2026-01-01 00:00:00'
const LATE = '2026-02-01 00:00:00'
const UNICODE = '中文 😀 <&>\u2028\u2029 \ue000 \u{10000} " \\ \n'
const tables = ['blueprints', 'blueprint_commit_operations', 'blueprint_character_sync_operations']
function snapshot(db) {
  return Object.fromEntries(tables.map(table => [table, db.prepare(`SELECT * FROM ${table} ORDER BY rowid`).all()]))
}
function insert(db, table, row) {
  const columns = Object.keys(row)
  assert(columns.every(key => /^[a-z_]+$/u.test(key)))
  return db.prepare(`INSERT INTO ${table}(${columns.join(',')}) VALUES(${columns.map(() => '?').join(',')})`).run(...Object.values(row))
}
function blueprint(chapterNumber = 1, changes = {}) {
  return { chapterNumber, title: `章节 ${chapterNumber}`, role: '铺垫', purpose: '打开旧门', keyEvents: '拾起星钥', characters: ['林秋', '星使'], suspenseHook: '门后是谁', userGuidance: '', notes: '', notesUpdatedAt: '', ...changes }
}
function raw(db, chapterNumber = 1, changes = {}) {
  return insert(db, 'blueprints', { chapter_number: chapterNumber, title: `旧章 ${chapterNumber}`, role: 'old-role', purpose: 'old-purpose', key_events: 'old-events', characters: '["旧人"]', suspense_hook: 'old-hook', user_guidance: '', notes: '', notes_updated_at: '', created_at: EARLY, updated_at: LATE, ...changes })
}
function request(changes = {}) {
  return { mode: 'replace-range', operationId: 'synthetic-blueprint', startChapter: 1, endChapter: 1, blueprints: [blueprint()], ...changes }
}
function seedCommit(db, changes = {}) {
  // Idempotency fixtures MUST originate in the actual original commit path,
  // BEFORE database serialization. Never synthesize its payload hash/receipt.
  return BlueprintRepository.commitRange(request(changes))
}
function syncPatch(db, changes, id = 'blueprint-sync-synthetic-blueprint') {
  const columns = Object.keys(changes)
  assert(columns.every(key => /^[a-z_]+$/u.test(key)))
  db.prepare(`UPDATE blueprint_character_sync_operations SET ${columns.map(key => `${key}=?`).join(',')} WHERE operation_id=?`).run(...Object.values(changes), id)
}
function commitPatch(db, changes) {
  const columns = Object.keys(changes)
  assert(columns.every(key => /^[a-z_]+$/u.test(key)))
  db.prepare(`UPDATE blueprint_commit_operations SET ${columns.map(key => `${key}=?`).join(',')} WHERE operation_id='synthetic-blueprint'`).run(...Object.values(changes))
}
function deleteSync(db) { db.prepare('DELETE FROM blueprint_character_sync_operations').run() }
function seedOrphanLedger(db, changes = {}) { seedCommit(db, changes); deleteSync(db) }
function abort(db, event, message, table = 'blueprints', when = '') {
  assert(/^[A-Z _]+$/u.test(event) && /^[A-Z_]+$/u.test(message))
  db.exec(`CREATE TRIGGER contract_abort ${event} ON ${table} ${when} BEGIN
    UPDATE blueprints SET notes='trigger must roll back' WHERE chapter_number=99;
    SELECT RAISE(ABORT,'${message}'); END`)
}
const definitions = []
const cases = []
const unicodeCanonicalEvidence = []
const malformedInputEvidence = []
const add = (name, method, options = {}) => definitions.push({ name, method, ...options })
const count = expected => assert.equal(definitions.length, expected, 'reviewed fixture section count')

// All: all native row storage classes and permissive JSON.parse projections.
add('all-empty', 'All')
add('all-numeric-chapter-order', 'All', { seed: db => { raw(db, 9); raw(db, 2); raw(db, 5) } })
add('all-zero-negative-chapters', 'All', { seed: db => { raw(db, 0); raw(db, -7); raw(db, 2) } })
add('all-nullable-fields', 'All', { seed: db => raw(db, 1, { role: null, purpose: null, key_events: null, characters: null, suspense_hook: null, user_guidance: null, notes: null, notes_updated_at: null }) })
add('all-blob-scalar-fields', 'All', { seed: db => raw(db, 1, { title: Buffer.from([0, 255, 34]), role: Buffer.from(UNICODE), purpose: Buffer.alloc(0), key_events: Buffer.from([1, 2]), suspense_hook: Buffer.from('hook'), user_guidance: Buffer.from('author'), notes: Buffer.from('notes'), notes_updated_at: Buffer.from('author-clock') }) })
for (const [name, value] of [['null-column', null], ['invalid-json', '{bad'], ['object', '{"x":1}'], ['string', '"林秋"'], ['number', '42'], ['boolean', 'true'], ['json-null', 'null'], ['mixed-array', '["林秋",1,null,true,{"x":"星使"}]'], ['blob-valid-json', Buffer.from('["星使"]')], ['blob-invalid-json', Buffer.from([0, 255])]]) {
  add(`all-characters-${name}`, 'All', { seed: db => raw(db, 1, { characters: value }) })
}
add('all-notes-author-clock-retained', 'All', { seed: db => raw(db, 1, { notes: UNICODE, notes_updated_at: 'author-not-a-clock-to-drop' }) })
add('all-large-safe-integer-chapter', 'All', { seed: db => raw(db, 9007199254740991) })
add('all-raw-numeric-text-affinity', 'All', { seed: db => raw(db, 1, { title: 42, role: 0, purpose: 3.5, notes_updated_at: 7 }) })
count(18)

// Get: no validation beyond the original native driver's scalar binding.
for (const [name, args] of [['present', [1]], ['missing', [7]], ['negative', [-1]], ['zero', [0]], ['fractional', [1.5]], ['numeric-string', ['1']], ['padded-string', ['01']], ['null', [null]], ['missing-argument', []], ['boolean-true-driver-reject', [true]], ['boolean-false-driver-reject', [false]], ['object-driver-reject', [{}]], ['array-driver-reject', [[1]]], ['text', ['not-a-chapter']], ['large-safe', [9007199254740991]], ['buffer-shaped-object-driver-reject', [{ type: 'Buffer', data: [49] }]]]) {
  add(`get-${name}`, 'Get', { args, seed: db => { raw(db, 1); raw(db, -1); raw(db, 0); raw(db, 9007199254740991) } })
}
count(34)

// Upsert: undefined scalars/characters are intentionally passed unchanged to
// better-sqlite3; the fixture records its actual NULL/error behavior.
const upsert = (name, input, seed) => add(`upsert-${name}`, 'Upsert', { input, seed })
upsert('complete-insert', blueprint())
upsert('replace-all-fields', blueprint(1, { title: 'replacement', notes: 'author replacement', notesUpdatedAt: 'author revision 2' }), db => raw(db))
upsert('unicode-values', blueprint(1, { title: UNICODE, characters: [UNICODE], notes: UNICODE, notesUpdatedAt: '作者时刻' }))
upsert('negative-chapter', blueprint(-5))
upsert('zero-chapter', blueprint(0))
upsert('fractional-chapter-datatype-mismatch', blueprint(1.5))
const noChapter = blueprint(); delete noChapter.chapterNumber
upsert('missing-chapter-allocates-native-rowid', noChapter, db => raw(db, 7))
upsert('null-chapter-allocates-native-rowid', blueprint(null), db => raw(db, 7))
upsert('null-title-constraint', blueprint(1, { title: null }))
const noTitle = blueprint(); delete noTitle.title
upsert('missing-title-constraint', noTitle)
upsert('partial-chapter-and-title-undefined-scalars', { chapterNumber: 1, title: 'partial' })
upsert('nullable-optional-fields', blueprint(1, { role: null, purpose: null, keyEvents: null, suspenseHook: null, userGuidance: null, notes: null, notesUpdatedAt: null }))
const noCharacters = blueprint(); delete noCharacters.characters
upsert('missing-characters-json-stringify-undefined', noCharacters)
for (const [name, characters] of [['object', { hero: '林秋' }], ['string', '林秋'], ['null', null], ['boolean', true], ['number', 3.5], ['mixed-array', [null, 4, true, { name: UNICODE }]]]) upsert(`characters-${name}`, blueprint(1, { characters }))
upsert('boolean-title-driver-reject', blueprint(1, { title: true }))
upsert('object-role-driver-reject', blueprint(1, { role: {} }))
upsert('boolean-chapter-driver-reject', blueprint(true))
upsert('numeric-title-text-affinity', blueprint(1, { title: 42, notesUpdatedAt: 7 }))
upsert('extras-not-persisted', blueprint(1, { relationshipHints: { hero: UNICODE }, newCharacterCandidates: [{ name: '星使' }] }))
count(58)

const many = (name, input, seed) => add(`upsert-many-${name}`, 'UpsertMany', { input, seed })
many('empty-array', [])
many('two-chapters', [blueprint(2), blueprint(1)])
many('duplicate-chapter-last-wins', [blueprint(1, { title: 'first' }), blueprint(1, { title: 'last', notes: 'last notes' })])
many('update-existing-and-insert', [blueprint(1), blueprint(3)], db => raw(db))
many('midbatch-not-null-atomic-rollback', [blueprint(1), blueprint(2, { title: null }), blueprint(3)], db => { raw(db); raw(db, 99) })
many('midbatch-boolean-binding-atomic-rollback', [blueprint(1), blueprint(2, { role: true })], db => raw(db))
many('midbatch-object-binding-atomic-rollback', [blueprint(1), blueprint(2, { purpose: {} })], db => raw(db))
many('midbatch-fractional-rowid-rollback', [blueprint(1), blueprint(1.5)], db => raw(db))
many('duplicate-then-error-restores-original', [blueprint(), blueprint(1, { title: 'second' }), blueprint(2, { title: null })], db => raw(db))
many('negative-and-zero-native-rowids', [blueprint(-2), blueprint(0)])
many('partial-items-undefined-bindings', [{ chapterNumber: 1, title: 'partial first' }, { chapterNumber: 2, title: 'partial second' }])
many('trigger-abort-rolls-back-prior-item-and-witness', [blueprint(1), blueprint(2)], db => { raw(db, 99); abort(db, 'AFTER INSERT', 'SYNTHETIC_BLUEPRINT_BATCH_ABORT', 'blueprints', 'WHEN NEW.chapter_number=2') })
count(70)

for (const [name, args] of [['present', [1]], ['missing', [8]], ['negative', [-1]], ['zero', [0]], ['fractional', [1.5]], ['numeric-string', ['1']], ['null', [null]], ['missing-argument', []], ['boolean-driver-reject', [true]]]) {
  add(`delete-${name}`, 'Delete', { args, seed: db => { raw(db); raw(db, -1); raw(db, 0); seedCommit(db) } })
}
add('delete-trigger-abort-atomic-witness', 'Delete', { args: [1], seed: db => { raw(db); raw(db, 99); abort(db, 'AFTER DELETE', 'SYNTHETIC_BLUEPRINT_DELETE_ABORT') } })
count(80)

// CommitRange: validation precedence, normal ranges, author truthiness groups,
// actual original hash/retry seeds, strict readback and whole-fact rollback.
const commit = (name, changes = {}, options = {}) => add(`commit-${name}`, 'CommitRange', { input: request(changes), ...options })
commit('single-chapter')
commit('full-seven-chapters', { mode: 'full', endChapter: 7, blueprints: Array.from({ length: 7 }, (_, i) => blueprint(i + 1)) })
commit('replace-seven-chapters-8-through-14', { startChapter: 8, endChapter: 14, blueprints: Array.from({ length: 7 }, (_, i) => blueprint(i + 8)) }, { seed: db => { raw(db, 1); raw(db, 20) } })
commit('unsorted-input-sorted-snapshot', { endChapter: 3, blueprints: [blueprint(3), blueprint(1), blueprint(2)] })
commit('full-deletes-both-outside-bounds', { mode: 'full', endChapter: 2, blueprints: [blueprint(1), blueprint(2)] }, { seed: db => { raw(db, -2); raw(db, 0); raw(db, 1); raw(db, 8) } })
commit('replace-keeps-outside-bounds', { startChapter: 3, endChapter: 4, blueprints: [blueprint(3), blueprint(4)] }, { seed: db => { raw(db, 0); raw(db, 1); raw(db, 9) } })
commit('unicode-values-and-ascii-hint-keys', { blueprints: [blueprint(1, { title: UNICODE, characters: [UNICODE], relationshipHints: { hero: UNICODE, list: ['中文', '\ue000', '\u{10000}'] } })] })
commit('ascii-locale-case-and-numeric-key-canonicalization', { blueprints: [blueprint(1, { relationshipHints: { z: 1, A: 2, a: 3, Z: 4, ab: 5, aB: 6, Ab: 7, AB: 8, 10: UNICODE, 2: 'two', nested: { Beta: 2, alpha: 1 } } })] })
commit('new-character-and-relationship-facts-in-receipt', { blueprints: [blueprint(1, { relationshipHints: [{ source: '林秋', target: '星使', relation: UNICODE }], newCharacterCandidates: [{ name: '星使', role: 'supporting', reason: UNICODE }] })] })
commit('empty-operation-precedes-mode-and-range', { operationId: ' \t\n', mode: 'bad', startChapter: 0, blueprints: [] })
commit('invalid-mode-precedes-range', { mode: 'bad', startChapter: 0, blueprints: [] })
commit('negative-range', { startChapter: -1 })
commit('zero-range', { startChapter: 0 })
commit('fractional-start', { startChapter: 1.5 })
commit('fractional-end', { endChapter: 1.5 })
commit('reverse-range', { startChapter: 3, endChapter: 2 })
commit('full-must-start-at-one', { mode: 'full', startChapter: 2, endChapter: 2, blueprints: [blueprint(2)] })
commit('empty-incomplete', { blueprints: [] })
commit('missing-one-of-seven', { endChapter: 7, blueprints: Array.from({ length: 6 }, (_, i) => blueprint(i + 1)) })
commit('duplicate-seven-member-input', { endChapter: 7, blueprints: [1, 2, 3, 4, 5, 6, 6].map(n => blueprint(n)) })
commit('extra-out-of-range-chapter', { blueprints: [blueprint(1), blueprint(2)] })
commit('chapter-number-string-is-not-integer-member', { blueprints: [blueprint('1')] })
commit('retry-notes-author-timestamp-conflict', { blueprints: [blueprint(1, { notesUpdatedAt: 'changed author timestamp' })] }, { seed: db => seedCommit(db) })
commit('characters-mixed-array-json-stringify-equality', { blueprints: [blueprint(1, { characters: [null, 1, true, { hero: UNICODE }] })] })
commit('preserve-notes-and-companion-as-group', { blueprints: [blueprint(1, { notes: '', notesUpdatedAt: 'new timestamp must not detach' })] }, { seed: db => raw(db, 1, { notes: '作者旧笔记', notes_updated_at: 'author-old', user_guidance: '作者指导' }) })
commit('truthy-notes-replaces-companion-even-empty', { blueprints: [blueprint(1, { notes: '作者新笔记', notesUpdatedAt: '' })] }, { seed: db => raw(db, 1, { notes: 'old', notes_updated_at: 'author-old' }) })
commit('whitespace-notes-and-guidance-are-truthy', { blueprints: [blueprint(1, { notes: ' ', notesUpdatedAt: 'author-new', userGuidance: '\t' })] }, { seed: db => raw(db, 1, { notes: 'old', user_guidance: 'old' }) })
commit('guidance-preserved-independently-of-new-notes', { blueprints: [blueprint(1, { notes: 'new', notesUpdatedAt: 'new-author' })] }, { seed: db => raw(db, 1, { user_guidance: '作者指定', notes: 'old', notes_updated_at: 'old-author' }) })
commit('truthy-guidance-and-empty-notes-preserve-old-notes', { blueprints: [blueprint(1, { userGuidance: 'new guide' })] }, { seed: db => raw(db, 1, { notes: 'old notes', notes_updated_at: 'old-author' }) })
commit('null-author-fields-fall-back', { blueprints: [blueprint(1, { userGuidance: null, notes: null, notesUpdatedAt: null })] }, { seed: db => raw(db, 1, { user_guidance: 'old guide', notes: 'old notes', notes_updated_at: 'old-author' }) })
commit('numeric-falsy-author-fields-normalized-to-empty', { blueprints: [blueprint(1, { userGuidance: 0, notes: false, notesUpdatedAt: 0 })] })
commit('null-role-persists-but-strict-projection-fails', { blueprints: [blueprint(1, { role: null })] })
commit('missing-role-persists-null-but-strict-readback-fails', { blueprints: [{ chapterNumber: 1, title: 'partial', characters: [] }] })
commit('numeric-title-text-affinity-strict-readback-fails', { blueprints: [blueprint(1, { title: 42 })] })
commit('characters-object-json-stringify-equality', { blueprints: [blueprint(1, { characters: { hero: UNICODE } })] })
commit('characters-null-json-stringify-equality', { blueprints: [blueprint(1, { characters: null })] })
commit('characters-string-json-stringify-equality', { blueprints: [blueprint(1, { characters: UNICODE })] })
commit('retry-equal-original-request', {}, { seed: db => seedCommit(db) })
commit('retry-changed-title-conflict', { blueprints: [blueprint(1, { title: 'conflicting title' })] }, { seed: db => seedCommit(db) })
commit('retry-mode-conflict', { mode: 'full' }, { seed: db => seedCommit(db) })
commit('retry-input-order-conflict', { endChapter: 2, blueprints: [blueprint(2), blueprint(1)] }, { seed: db => seedCommit(db, { endChapter: 2, blueprints: [blueprint(1), blueprint(2)] }) })
commit('retry-reads-current-persisted-snapshot', {}, { seed: db => { seedCommit(db); db.prepare("UPDATE blueprints SET title='author changed after commit',notes='current author',notes_updated_at='current author timestamp' WHERE chapter_number=1").run() } })
commit('retry-backfills-missing-sync-ledger', {}, { seed: db => seedOrphanLedger(db) })
commit('retry-missing-range-precedes-malformed-sync', {}, { seed: db => { seedCommit(db); db.prepare('DELETE FROM blueprints').run(); syncPatch(db, { character_sync_input: '{bad' }) } })
commit('retry-malformed-commit-input-precedes-range', {}, { seed: db => { seedCommit(db); commitPatch(db, { character_sync_input: '{bad' }); db.prepare('DELETE FROM blueprints').run() } })
commit('retry-nonarray-commit-input', {}, { seed: db => { seedCommit(db); commitPatch(db, { character_sync_input: '{}' }) } })
commit('retry-malformed-sync-input', {}, { seed: db => { seedCommit(db); syncPatch(db, { character_sync_input: '{bad' }) } })
commit('trigger-modifies-title-strict-readback-rollback', {}, { seed: db => { raw(db, 99); db.exec("CREATE TRIGGER contract_change AFTER INSERT ON blueprints WHEN NEW.chapter_number=1 BEGIN UPDATE blueprints SET title='tampered',notes='trigger must roll back' WHERE chapter_number IN (1,99); END") } })
commit('trigger-modifies-notes-author-clock-strict-readback', {}, { seed: db => { raw(db, 99); db.exec("CREATE TRIGGER contract_change AFTER INSERT ON blueprints WHEN NEW.chapter_number=1 BEGIN UPDATE blueprints SET notes_updated_at='tampered author timestamp' WHERE chapter_number=1; UPDATE blueprints SET notes='trigger must roll back' WHERE chapter_number=99; END") } })
commit('trigger-ignores-insert-range-incomplete-rollback', {}, { seed: db => { raw(db, 99); db.exec('CREATE TRIGGER contract_ignore BEFORE INSERT ON blueprints WHEN NEW.chapter_number=1 BEGIN SELECT RAISE(IGNORE); END') } })
commit('full-midbatch-null-title-rolls-back-outside-deletes', { mode: 'full', endChapter: 2, blueprints: [blueprint(1), blueprint(2, { title: null })] }, { seed: db => { raw(db, 1); raw(db, 99) } })
commit('sync-insertion-abort-rolls-back-blueprints-and-commit-ledger', {}, { seed: db => { raw(db, 99); abort(db, 'AFTER INSERT', 'SYNTHETIC_BLUEPRINT_SYNC_ABORT', 'blueprint_character_sync_operations') } })
commit('commit-ledger-insertion-abort-rolls-back-blueprints', {}, { seed: db => { raw(db, 99); abort(db, 'AFTER INSERT', 'SYNTHETIC_BLUEPRINT_LEDGER_ABORT', 'blueprint_commit_operations') } })
commit('retry-conflict-precedes-corrupt-ledgers', { blueprints: [blueprint(1, { title: 'conflict' })] }, { seed: db => { seedCommit(db); commitPatch(db, { character_sync_input: '{bad' }); syncPatch(db, { character_sync_input: '{bad' }) } })
count(134)

const pending = (name, seed) => add(`pending-sync-${name}`, 'PendingSync', { seed })
pending('empty')
pending('one-valid', db => seedCommit(db))
pending('multiple-created-time-then-operation-id', db => {
  for (const id of ['z', 'b', 'a']) seedCommit(db, { operationId: id })
  db.prepare('UPDATE blueprint_character_sync_operations SET created_at=?').run(LATE)
  syncPatch(db, { created_at: EARLY }, 'blueprint-sync-z')
})
pending('backfill-missing-sync-on-read', db => seedOrphanLedger(db))
pending('backfill-two-with-one-already-present', db => { seedOrphanLedger(db); seedCommit(db, { operationId: 'second' }) })
pending('backfill-malformed-original-commit-input', db => { seedOrphanLedger(db); commitPatch(db, { character_sync_input: '{bad' }) })
pending('malformed-sync-input-json', db => { seedCommit(db); syncPatch(db, { character_sync_input: '{bad' }) })
pending('nonarray-sync-input-object', db => { seedCommit(db); syncPatch(db, { character_sync_input: '{}' }) })
pending('nonarray-sync-input-null', db => { seedCommit(db); syncPatch(db, { character_sync_input: 'null' }) })
pending('malformed-completion-receipt-precedes-input', db => { seedCommit(db); syncPatch(db, { completion_receipt: '{bad', character_sync_input: '{also bad' }) })
pending('array-completion-receipt-invalid', db => { seedCommit(db); syncPatch(db, { completion_receipt: '[]' }) })
pending('pending-with-object-completion-state-invalid', db => { seedCommit(db); syncPatch(db, { completion_receipt: '{}' }) })
count(146)

const getSync = (name, args, seed) => add(`get-sync-${name}`, 'GetSync', { args, seed })
getSync('missing', ['unknown'])
getSync('valid-pending', ['blueprint-sync-synthetic-blueprint'], db => seedCommit(db))
getSync('empty-operation-required', [''])
getSync('whitespace-operation-required', [' \t\n'])
getSync('null-completion-receipt-json-is-invalid', ['blueprint-sync-synthetic-blueprint'], db => { seedCommit(db); syncPatch(db, { completion_receipt: 'null' }) })
getSync('empty-sync-input-array-valid', ['blueprint-sync-synthetic-blueprint'], db => { seedCommit(db); syncPatch(db, { character_sync_input: '[]' }) })
getSync('backfill-before-lookup', ['blueprint-sync-synthetic-blueprint'], db => seedOrphanLedger(db))
getSync('backfill-even-when-query-id-missing', ['unknown'], db => seedOrphanLedger(db))
getSync('malformed-json-input', ['blueprint-sync-synthetic-blueprint'], db => { seedCommit(db); syncPatch(db, { character_sync_input: '{bad' }) })
getSync('nonarray-input-valid-json', ['blueprint-sync-synthetic-blueprint'], db => { seedCommit(db); syncPatch(db, { character_sync_input: 'true' }) })
getSync('json-array-input-is-not-element-validated', ['blueprint-sync-synthetic-blueprint'], db => { seedCommit(db); syncPatch(db, { character_sync_input: '[null,1,"林秋",{"x":"星使"}]' }) })
getSync('completion-json-primitive-invalid', ['blueprint-sync-synthetic-blueprint'], db => { seedCommit(db); syncPatch(db, { completion_receipt: '1' }) })
getSync('pending-completion-state-precedes-input-error', ['blueprint-sync-synthetic-blueprint'], db => { seedCommit(db); syncPatch(db, { completion_receipt: '{}', character_sync_input: '{bad' }) })
getSync('blob-json-pending-input-real-parser', ['blueprint-sync-synthetic-blueprint'], db => { seedCommit(db); syncPatch(db, { character_sync_input: Buffer.from('[{"chapterNumber":1,"title":"星使"}]') }) })
count(160)

// ICU's localeCompare('en-US') order for arbitrary Unicode/punctuation object
// keys is deliberately NOT claimed compatible by Go's ASCII-only boundary.
// Still capture the actual original outcome, separately, without tampering with
// hashes or omitting these important original canonicalization observations.
const evidenceDefinitions = [
  { name: 'evidence-unicode-relationship-hint-keys', method: 'CommitRange', input: request({ blueprints: [blueprint(1, { relationshipHints: { '\ue000': 1, '\u{10000}': 2, '中文': UNICODE, 'é': 'accent', e: 'plain' } })] }) },
  { name: 'evidence-punctuation-relationship-hint-keys', method: 'CommitRange', input: request({ blueprints: [blueprint(1, { relationshipHints: { 'a-b': 1, a_b: 2, 'a b': 3, 'a.b': 4 } })] }) },
]
// These malformed JavaScript argument shapes are deliberately outside the Go
// typed public request boundary. Preserve the original native TypeError text as
// evidence, never replace it with Go's fail-closed validation error.
const malformedDefinitions = [
  { name: 'evidence-commit-missing-operation-id-native-trim-error', method: 'CommitRange', input: request({ operationId: undefined }) },
  { name: 'evidence-commit-missing-blueprints-native-map-error', method: 'CommitRange', input: request({ blueprints: undefined }) },
  { name: 'evidence-get-sync-missing-operation-native-trim-error', method: 'GetSync', args: [] },
  { name: 'evidence-get-sync-numeric-operation-native-trim-error', method: 'GetSync', args: [1] },
]
const methodCounts = { All: 18, Get: 16, Upsert: 24, UpsertMany: 12, Delete: 10, CommitRange: 54, PendingSync: 12, GetSync: 14 }
assert.deepEqual(Object.fromEntries(Object.keys(methodCounts).map(method => [method, definitions.filter(item => item.method === method).length])), methodCounts)
assert.equal(new Set([...definitions, ...evidenceDefinitions, ...malformedDefinitions].map(item => item.name)).size, 166)

function invoke(definition) {
  switch (definition.method) {
    case 'All': return BlueprintRepository.getAll()
    case 'Get': return BlueprintRepository.getByChapter(...definition.args)
    case 'Upsert': return BlueprintRepository.upsert(definition.input) ?? null
    case 'UpsertMany': return BlueprintRepository.upsertMany(definition.input) ?? null
    case 'Delete': return BlueprintRepository.delete(...definition.args) ?? null
    case 'CommitRange': return BlueprintRepository.commitRange(definition.input)
    case 'PendingSync': return BlueprintRepository.listPendingCharacterSyncOperations()
    case 'GetSync': return BlueprintRepository.getCharacterSyncOperation(...definition.args)
    default: throw new Error(`unknown blueprint method ${definition.method}`)
  }
}

try {
  const baselineBytes = readFileSync(source)
  for (const definition of [...definitions, ...evidenceDefinitions, ...malformedDefinitions]) {
    assert(/^[A-Za-z0-9_-]+$/u.test(definition.name))
    const filename = path.join(directory, `${definition.name}.db`)
    // Establish 0600 BEFORE writing any baseline bytes. This is a complete,
    // byte-for-byte baseline copy, not invented/minimized application DDL.
    const descriptor = openSync(filename, 'wx', 0o600)
    try { writeFileSync(descriptor, baselineBytes) } finally { closeSync(descriptor) }
    const db = new Database(filename)
    let before
    try {
      db.pragma('foreign_keys=ON')
      db.pragma('journal_mode=DELETE')
      assert.equal(db.pragma('user_version', { simple: true }), 7)
      globalThis[globalKey] = db
      for (const table of tables) assert.equal(db.prepare(`SELECT COUNT(*) AS n FROM ${table}`).get().n, 0, `${table} baseline must be synthetic and empty`)
      // committed_at is NOT an excluded clock key. Keep this raw field and use
      // a visible deterministic fixture trigger present in BOTH BEFORE DBs.
      // We do not replace any repository method or alter the original schema.
      db.exec(`CREATE TRIGGER contract_fixed_commit_clock AFTER INSERT ON blueprint_commit_operations
        BEGIN UPDATE blueprint_commit_operations SET committed_at='${EARLY}' WHERE operation_id=NEW.operation_id; END`)
      definition.seed?.(db)
      assert.deepEqual(db.pragma('foreign_key_check'), [], 'valid synthetic foreign keys required')
      before = db.serialize()
      const beforeFacts = normalize(snapshot(db))
      // Ensure exported JSON requests are exactly the arguments actually used:
      // remove undefined object properties via JSON's own omission semantics.
      if (Object.hasOwn(definition, 'input')) definition.input = JSON.parse(JSON.stringify(definition.input))
      let result = null
      let error
      try { result = invoke(definition) } catch (failure) {
        assert(failure instanceof Error)
        assert(!failure.message.startsWith('UNREACHABLE_'), 'an unreachable environment seam was called; this is not an oracle business error')
        error = failure.message
      }
      const after = normalize(snapshot(db))
      assert.deepEqual(db.pragma('foreign_key_check'), [])
      if (['All', 'Get'].includes(definition.method)) assert.deepEqual(db.serialize(), before, 'plain reads must not mutate')
      if (error !== undefined && ['UpsertMany', 'Delete', 'CommitRange'].includes(definition.method)) assert.deepEqual(after, beforeFacts, 'these atomic error fixtures must leave all raw facts unchanged')
      // ensureBlueprintCommitSchema's INSERT OR IGNORE backfill is deliberately
      // outside commitRange's transaction. Error fixtures above all have an
      // already-complete sync ledger (or no commit ledger); separate successful
      // retry/pending/get fixtures capture maintenance backfill faithfully.
      const item = { name: definition.name, database: filename, method: definition.method,
        ...(Object.hasOwn(definition, 'input') ? { input: definition.input } : {}),
        ...(Object.hasOwn(definition, 'args') ? { args: definition.args } : {}),
        result: normalize(result), ...(error === undefined ? {} : { error }), after }
      if (definitions.includes(definition)) cases.push(item)
      else if (evidenceDefinitions.includes(definition)) unicodeCanonicalEvidence.push(item)
      else malformedInputEvidence.push(item)
    } finally {
      globalThis[globalKey] = null
      db.close()
      // Restore EXACT closed BEFORE bytes after observing the real operation.
      if (before) writeFileSync(filename, before, { mode: 0o600 })
    }
  }
  assert.equal(cases.length, 160)
  assert.equal(unicodeCanonicalEvidence.length, 2)
  assert.equal(malformedInputEvidence.length, 4)
  writeFileSync(target, JSON.stringify({ formatVersion: 1, cases, unicodeCanonicalEvidence, malformedInputEvidence, originalModules: [...originalModules].sort() }) + '\n', { flag: 'wx', mode: 0o600 })
} finally {
  delete globalThis[globalKey]
  hooks.deregister()
}
console.log(`FROZEN ${cases.length} original Blueprint contracts (All=18 Get=16 Upsert=24 UpsertMany=12 Delete=10 CommitRange=54 PendingSync=12 GetSync=14); ${unicodeCanonicalEvidence.length} separate original ICU canonicalization observations; ${malformedInputEvidence.length} separate original malformed-argument observations`)
