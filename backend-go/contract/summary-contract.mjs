// GitHub-only oracle: all business code and recursive dependencies are original.
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { builtinModules, createRequire, registerHooks, stripTypeScriptTypes } from 'node:module'
import { closeSync, existsSync, mkdirSync, openSync, readFileSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

assert.equal(process.env.GITHUB_ACTIONS, 'true', 'original summary oracle is GitHub-only')
const base = process.env.RUNNER_TEMP, target = process.env.GO_SUMMARY_CONTRACT
assert(base && target && path.isAbsolute(base) && path.isAbsolute(target))
assert.equal(target, path.join(base, 'novel-go-summary', 'contract.json'))
const sqliteContract = process.env.GO_SQLITE_CONTRACT
assert(sqliteContract && path.isAbsolute(sqliteContract))
const baseline = path.join(path.dirname(sqliteContract), 'baseline-v7.db')
const relativeBaseline = path.relative(base, baseline)
assert(relativeBaseline && !relativeBaseline.startsWith('..') && !path.isAbsolute(relativeBaseline))
assert(existsSync(baseline), 'actual migrated synthetic baseline must already exist')
const directory = path.dirname(target)
assert(!existsSync(directory), 'never reuse an oracle directory')
mkdirSync(directory, { recursive: true, mode: 0o700 })
const root = new URL('../../', import.meta.url), databaseURL = new URL('electron/database', root).href
const key = '__GO_SUMMARY_ORIGINAL_DB__', databaseStub = 'summary-contract:database'
assert(!Object.hasOwn(globalThis, key))
const Database = createRequire(import.meta.url)('better-sqlite3')
const probe = new Database(':memory:'); probe.close() // Resolve lazy native bindings before hooks.
globalThis[key] = null
const builtins = new Set(builtinModules.flatMap(name => [name, `node:${name}`]))
const originalModules = new Set()
const hooks = registerHooks({
  resolve(specifier, context, next) {
    if (builtins.has(specifier)) return next(specifier, context)
    assert(specifier.startsWith('.') || specifier.startsWith('file:'), `unexpected original dependency ${specifier}`)
    const url = new URL(specifier, context.parentURL ?? root).href
    if (url === databaseURL || url === databaseURL + '.ts') return { url: databaseStub, shortCircuit: true }
    const candidate = url.endsWith('.ts') ? url : url + '.ts'
    const filename = fileURLToPath(candidate), relative = path.relative(fileURLToPath(root), filename)
    assert(relative && !relative.startsWith('..') && !path.isAbsolute(relative))
    assert(/^(electron|src\/(shared|services|utils))\//u.test(relative), relative)
    assert(existsSync(filename), `missing original module ${relative}`)
    originalModules.add(relative)
    return { url: candidate, shortCircuit: true }
  },
  load(url, context, next) {
    if (url === databaseStub) return { format: 'module', source: `export function getProjectDb(){return globalThis[${JSON.stringify(key)}]}\nexport function getCurrentProjectPath(){return null}`, shortCircuit: true }
    if (!url.startsWith(root.href) || !url.endsWith('.ts')) return next(url, context)
    return { format: 'module', source: stripTypeScriptTypes(readFileSync(new URL(url), 'utf8'), { mode: 'transform', sourceUrl: url }), shortCircuit: true }
  },
})
const { SummaryRepository: R, freezeFinalizedCharacterSnapshot } = await import(new URL('electron/repositories/summary-repository.ts', root).href)
const { RECEIPTED_DRAFT_STATUSES } = await import(new URL('src/shared/draft-status.ts', root).href)
assert.deepEqual(RECEIPTED_DRAFT_STATUSES, ['finalized', 'superseded'])
const UNICODE = '中文 café e\u0301 🙂 <&>\u2028\u2029 " \\ \n'
const BODY = 'Lin found the silver key. The door remains locked.'
const hash = value => createHash('sha256').update(value, 'utf8').digest('hex')
function insert(db, table, row) {
  const columns = Object.keys(row)
  assert(/^[a-z_]+$/u.test(table) && columns.every(column => /^[a-z_]+$/u.test(column)))
  return db.prepare(`INSERT INTO ${table}(${columns.join(',')}) VALUES(${columns.map(() => '?').join(',')})`).run(...Object.values(row))
}
function patch(db, table, changes, where = '') {
  const columns = Object.keys(changes)
  assert(/^[a-z_]+$/u.test(table) && columns.every(column => /^[a-z_]+$/u.test(column)))
  db.prepare(`UPDATE ${table} SET ${columns.map(column => column + '=?').join(',')} ${where}`).run(...Object.values(changes))
}
function blueprint(db, chapter = 1, title = 'Mutable blueprint title') {
  insert(db, 'blueprints', { chapter_number: chapter, title })
}
function draft(db, options = {}) {
  const { chapter = 1, version = 1, status = 'finalized', body = BODY, receipt = true, title = 'Frozen title', finalizationId } = options
  const contentId = Number(insert(db, 'contents', { body }).lastInsertRowid)
  const draftId = Number(insert(db, 'drafts', { chapter_number: chapter, version, status, content_id: contentId }).lastInsertRowid)
  const source = { draftId, finalizationId: finalizationId ?? `summary-fixed-finalization-${draftId}`, chapterNumber: chapter, contentHash: hash(body) }
  if (receipt) insert(db, 'finalization_outbox', { finalization_id: source.finalizationId, draft_id: draftId, chapter_number: chapter, chapter_title: title, content_hash: source.contentHash, content_revision: 1, content_snapshot: body, target_file_name: `synthetic-${draftId}.txt` })
  return { source, contentId, body }
}
function summary(db, record, changes = {}) {
  const { source } = record
  insert(db, 'summary_snapshots', { draft_id: source.draftId, chapter_number: source.chapterNumber, chapter_notes: 'The silver key was found.', continuity_facts: '[]', source_finalization_id: source.finalizationId, source_content_hash: source.contentHash, ...changes })
}
function legacy(db, chapter = 1, states = 'legacy states', changes = {}) {
  insert(db, 'summary_snapshots', { chapter_number: chapter, character_states: states, ...changes })
}
function character(db, id = 'char-lin', name = 'Lin') { insert(db, 'characters', { character_id: id, name, role: 'protagonist' }) }
function freeze(db, record) { db.transaction(() => freezeFinalizedCharacterSnapshot(db, record.source))() }
function frozenPatch(db, change, badHash = false) {
  const row = db.prepare("SELECT proposal_id,raw_value FROM character_identity_proposals WHERE source_key='finalized-character-snapshot:v1'").get()
  const snapshot = JSON.parse(row.raw_value)
  change(snapshot)
  const raw = JSON.stringify(snapshot)
  db.prepare('UPDATE character_identity_proposals SET raw_value=?,source_hash=? WHERE proposal_id=?').run(raw, badHash ? '0'.repeat(64) : hash(raw), row.proposal_id)
}
const fact = (changes = {}) => ({ category: 'plot', entities: ['key'], statement: ' The key was found. ', sourceChapter: 1, evidence: 'silver key', ...changes })
const candidate = (changes = {}) => ({ characterName: ' Lin ', field: 'keyItems', value: ' silver key ', ...changes })
const definitions = [], add = (name, method, options = {}) => definitions.push({ name, method, ...options })
const list = (name, seed, options = {}) => add(`list-${name}`, 'ListBefore', { chapter: 10, seed, ...options })
const one = (db, changes = {}) => { const record = draft(db); summary(db, record, changes); return record }

// Legacy writes only cover the deliberately supported signed safe-integer domain.
// Native TypeErrors from better-sqlite3 for boolean/object/array are not API parity.
for (const [label, chapter, text] of [['ordinary', 1, 'legacy states'], ['empty', 2, ''], ['unicode', 3, UNICODE], ['negative', -7, 'negative'], ['negative-zero', -0, 'zero'], ['max-safe', Number.MAX_SAFE_INTEGER, 'large'], ['min-safe', Number.MIN_SAFE_INTEGER, 'small']]) {
  add(`save-${label}`, 'SaveSnapshot', { chapter, text })
}
add('save-appends-not-upserts', 'SaveSnapshot', { chapter: 1, text: 'new states', seed: db => { legacy(db, 1, 'old states'); one(db) } })
add('save-trigger-abort-unchanged', 'SaveSnapshot', { chapter: 1, text: 'blocked', seed: db => {
  legacy(db, 99, 'witness')
  db.exec("CREATE TRIGGER contract_summary_abort AFTER INSERT ON summary_snapshots BEGIN UPDATE summary_snapshots SET character_states='must roll back' WHERE chapter_number=99; SELECT RAISE(ABORT,'SYNTHETIC_SUMMARY_ABORT'); END")
} })
add('save-no-project', 'SaveSnapshot', { chapter: 1, text: 'ignored', nilDB: true })
add('latest-empty', 'LatestSnapshot')
add('latest-no-project', 'LatestSnapshot', { nilDB: true })
add('latest-empty-text-is-present', 'LatestSnapshot', { seed: db => legacy(db, 1, '') })
add('latest-rowid-not-chapter', 'LatestSnapshot', { seed: db => { legacy(db, 99, 'older'); legacy(db, -2, 'newest') } })
add('latest-ignore-later-formal-row', 'LatestSnapshot', { seed: db => { legacy(db, 9, UNICODE); one(db, { character_states: 'formal state must be ignored' }) } })
add('latest-only-formal-is-null', 'LatestSnapshot', { seed: db => one(db) })
add('latest-null-states-preserved', 'LatestSnapshot', { seed: db => legacy(db, 1, null) })

// Immutable source validation, including the full body, not merely its prefix.
const sourceCase = (name, seed, options = {}) => add(`source-${name}`, 'ReadFinalizedSource', { draftID: 1, seed, ...options })
sourceCase('missing')
sourceCase('no-project', undefined, { nilDB: true })
for (const value of [0, -1, 1.5, '1', null, true, {}, [], 9007199254740992]) sourceCase(`invalid-input-${definitions.length}`, undefined, { draftID: value })
sourceCase('valid-ascii', db => draft(db))
sourceCase('valid-unicode', db => draft(db, { body: UNICODE, title: UNICODE }))
sourceCase('valid-empty-body', db => draft(db, { body: '' }))
sourceCase('valid-full-long-body', db => draft(db, { body: BODY.repeat(300) + ' The final correction is authoritative.' }))
sourceCase('valid-generation-watermark', db => { draft(db); patch(db, 'continuity_projection_meta', { generation: 7, stale_from_chapter: 1 }) })
sourceCase('legacy-blueprint-title', db => { draft(db, { receipt: false }); blueprint(db) })
sourceCase('legacy-no-blueprint', db => draft(db, { receipt: false }))
sourceCase('frozen-title-over-blueprint', db => { draft(db); blueprint(db) })
sourceCase('empty-frozen-title-no-fallback', db => { draft(db, { title: '' }); blueprint(db) })
for (const status of ['draft', 'revised', 'reviewed', 'superseded', 'archived']) sourceCase(`status-${status}`, db => draft(db, { status }))
sourceCase('legacy-superseded-invalid', db => draft(db, { status: 'superseded', receipt: false }))
for (const [label, changes] of [['blank-finalization-id', { finalization_id: ' \uFEFF ' }], ['nonhex-hash', { content_hash: 'z'.repeat(64) }], ['uppercase-hash', { content_hash: hash(BODY).toUpperCase() }], ['short-hash', { content_hash: 'ab' }], ['wrong-valid-hash', { content_hash: hash('different prose') }], ['snapshot-tail-changed', { content_snapshot: BODY + ' tail' }]]) {
  sourceCase(label, db => { draft(db); patch(db, 'finalization_outbox', changes) })
}
sourceCase('live-body-changed', db => { const record = draft(db); patch(db, 'contents', { body: BODY + ' edited' }, `WHERE id=${record.contentId}`) })
sourceCase('missing-projection-meta', db => { draft(db); db.exec('DELETE FROM continuity_projection_meta') })
sourceCase('old-still-finalized-readable', db => { draft(db); draft(db, { version: 2, body: 'New authoritative body.' }) })

// List SQL: selection by summary rowid and authoritative draft(version,id).
list('empty')
list('no-project', undefined, { nilDB: true })
for (const value of [0, -1, 1.5, '2', null, false, {}, [], 9007199254740992]) list(`invalid-target-${definitions.length}`, undefined, { chapter: value })
list('one-missing-frozen-legacy', db => one(db))
list('strict-before-boundary', db => { one(db); const b = draft(db, { chapter: 2 }); summary(db, b) }, { chapter: 2 })
list('numeric-chapter-order', db => { for (const chapter of [9, 2, 5]) { const record = draft(db, { chapter }); summary(db, record) } })
list('legacy-state-rows-ignored', db => { legacy(db, 1, UNICODE); one(db); legacy(db, 2, 'later') })
list('empty-notes-excluded', db => one(db, { chapter_notes: '' }))
list('whitespace-notes-preserved', db => one(db, { chapter_notes: ' \uFEFF \n' }))
list('unbound-legacy-source', db => one(db, { source_finalization_id: '', source_content_hash: '' }))
list('partially-bound-source', db => one(db, { source_content_hash: '' }))
list('missing-receipt-no-title-fallback', db => { const record = draft(db, { receipt: false }); blueprint(db); summary(db, record) })
for (const status of ['finalized', 'superseded', 'draft', 'revised', 'reviewed', 'archived', 'unknown']) list(`receipt-status-${status}`, db => { const record = draft(db, { status }); summary(db, record) })
list('superseded-fallback-current-id', db => { const old = draft(db, { status: 'superseded' }); summary(db, old); draft(db, { version: 2, title: 'New frozen title', body: 'New prose' }) })
list('current-max-version-not-id', db => { draft(db, { version: 3, title: 'Version three' }); const older = draft(db, { version: 2, title: 'Version two', body: 'Older prose' }); summary(db, older) })
list('newer-working-draft-does-not-displace-finalized', db => { one(db); draft(db, { version: 9, status: 'draft' }) })
list('newer-archived-does-not-displace-finalized', db => { one(db); draft(db, { version: 9, status: 'archived' }) })
list('latest-summary-id-not-finalized-version', db => { const newer = draft(db, { version: 3 }); summary(db, newer); const older = draft(db, { version: 1, body: 'Old body' }); summary(db, older) })
list('newer-ineligible-summary-still-suppresses', db => { one(db); const newer = draft(db, { version: 2, status: 'archived' }); summary(db, newer) })
list('newer-legacy-notes-still-suppress', db => { one(db); legacy(db, 1, 'legacy', { chapter_notes: 'Legacy notes suppress earlier formal row' }) })
list('newer-empty-summary-does-not-suppress', db => { one(db); const newer = draft(db, { version: 2 }); summary(db, newer, { chapter_notes: '' }) })
list('tie-version-max-id-corrupt-index-fixture', db => { db.exec('DROP INDEX idx_drafts_chapter_version'); one(db); draft(db, { version: 1, title: 'Tie winner', body: 'Tie winner body' }) })
list('current-body-tampered', db => { const record = one(db); patch(db, 'contents', { body: 'tampered' }, `WHERE id=${record.contentId}`) })
list('current-snapshot-tampered', db => { one(db); patch(db, 'finalization_outbox', { content_snapshot: 'tampered' }) })
list('current-hash-tampered', db => { one(db); patch(db, 'finalization_outbox', { content_hash: hash('tampered') }) })
list('summary-hash-mismatch-fallback', db => one(db, { source_content_hash: hash('different source') }))
list('summary-finalization-mismatch-fallback', db => one(db, { source_finalization_id: 'different-finalization' }))
list('projection-meta-missing-excludes-all', db => { one(db); db.exec('DELETE FROM continuity_projection_meta') })
for (const [label, generation, stale, projection] of [['equal-stale-marker', 3, 1, 3], ['old-invalidated', 3, 1, 2], ['old-before-cutoff', 3, 2, 2], ['old-no-cutoff', 3, null, 2], ['future-generation-not-invalidated', 3, 1, 4]]) {
  list(`generation-${label}`, db => { const record = one(db, { projection_generation: projection }); freeze(db, record); patch(db, 'continuity_projection_meta', { generation, stale_from_chapter: stale }) })
}

// Parsers deliberately retain rich optional metadata and fail soft on malformed storage.
for (const [label, value] of [['invalid-json', '{bad'], ['object', '{}'], ['null', 'null'], ['too-many', JSON.stringify(Array.from({ length: 13 }, () => fact()))], ['wrong-chapter', JSON.stringify([fact({ sourceChapter: 2 })])], ['wrong-category', JSON.stringify([fact({ category: 'unknown' })])], ['empty-evidence', JSON.stringify([fact({ evidence: ' ' })])], ['bad-ref', JSON.stringify([fact({ characterRefs: [{ characterId: '', displayNameSnapshot: 'key' }] })])], ['mixed-invalid-discards-all', JSON.stringify([fact(), null])], ['normalized-valid', JSON.stringify([fact({ entities: [' key ', 'key'], extra: 'not projected' })])], ['unicode-full-text', JSON.stringify([fact({ statement: UNICODE, evidence: UNICODE })])]]) {
  list(`facts-${label}`, db => one(db, { continuity_facts: value }))
}
for (const [label, value] of [['invalid-json', '{bad'], ['null', 'null'], ['object', '{}'], ['wrong-version', JSON.stringify({ version: 1, pending: [candidate()] })], ['trim-legacy', JSON.stringify([candidate()])], ['invalid-filter-preserves-valid', JSON.stringify([null, 1, candidate({ field: 'unknown' }), candidate()])], ['partial-bound-rejected', JSON.stringify([candidate({ candidateKey: 'only-one-bound-key' })])], ['metadata-preserved', JSON.stringify([candidate({ characterId: 'char-lin', evidence: { start: 0, end: 3, text: 'Lin' }, selectionKey: '', displayName: UNICODE, rawValue: { created_at: 'author-original', nested: { created_at: 'keep', values: [null, true, UNICODE, 1.25] } }, reason: 'author-protected' })])], ['v2-pending-only', JSON.stringify({ version: 2, pending: [candidate()], receipts: [{ ignored: true }] })]]) {
  list(`candidates-${label}`, db => one(db, { character_state_candidates: value }))
}
// Keep numeric lexemes as raw database JSON: constructing a JS object first
// would pre-round n and underflow tiny before the actual repository parses it.
list('candidates-raw-value-number-rounding-underflow', db => one(db, {
  character_state_candidates: '[{"characterName":" Lin ","field":"keyItems","value":" silver key ","rawValue":{"n":9007199254740993,"tiny":1e-999}}]',
}))
list('candidates-bound-preserve-value-and-source', db => {
  const record = draft(db)
  summary(db, record, { character_state_candidates: JSON.stringify([candidate({ characterId: 'char-lin', value: ' \uFEFF bound value \n', candidateKey: 'opaque-key', source: { ...record.source, extra: UNICODE }, expectedFieldRevision: 7, expectedFieldValueHash: hash('previous value'), reason: 'legacy-protected', rawValue: null, evidence: 'truthy-evidence-preserved' })]) })
})

// Freeze through the real exported implementation, not a fabricated identity reader.
list('frozen-empty-current', db => { const record = one(db); freeze(db, record) })
list('frozen-character-ref-current', db => { character(db); const record = one(db, { continuity_facts: JSON.stringify([fact({ entities: ['Lin'], characterRefs: [{ characterId: 'char-lin', displayNameSnapshot: 'Lin' }] })]) }); freeze(db, record) })
list('frozen-character-missing-ref-legacy', db => { character(db); const record = one(db, { continuity_facts: JSON.stringify([fact({ entities: ['Lin'] })]) }); freeze(db, record) })
list('frozen-wrong-ref-stale', db => { character(db); const record = one(db, { continuity_facts: JSON.stringify([fact({ entities: ['Lin'], characterRefs: [{ characterId: 'wrong-id', displayNameSnapshot: 'Lin' }] })]) }); freeze(db, record) })
list('frozen-duplicate-refs-stale', db => { character(db); const ref = { characterId: 'char-lin', displayNameSnapshot: 'Lin' }; const record = one(db, { continuity_facts: JSON.stringify([fact({ entities: ['Lin'], characterRefs: [ref, ref] })]) }); freeze(db, record) })
list('frozen-same-name-disambiguation-stale', db => { character(db); character(db, 'char-second', 'Lin'); const record = one(db, { continuity_facts: JSON.stringify([fact({ entities: ['Lin'], characterRefs: [{ characterId: 'char-lin', displayNameSnapshot: 'Lin' }] })]) }); freeze(db, record) })
list('frozen-candidate-without-id-legacy', db => { character(db); const record = one(db, { character_state_candidates: JSON.stringify([candidate()]) }); freeze(db, record) })
list('frozen-candidate-wrong-id-stale', db => { character(db); const record = one(db, { character_state_candidates: JSON.stringify([candidate({ characterId: 'wrong-id' })]) }); freeze(db, record) })
list('frozen-candidate-correct-id-current', db => { character(db); const record = one(db, { character_state_candidates: JSON.stringify([candidate({ characterId: 'char-lin' })]) }); freeze(db, record) })
list('frozen-not-mutable-roster-authority', db => { character(db); const record = one(db, { character_state_candidates: JSON.stringify([candidate({ characterId: 'char-lin' })]) }); freeze(db, record); patch(db, 'characters', { name: 'Renamed now', retired: 1 }) })
for (const [label, change, badHash] of [['hash-corrupt', snapshot => { snapshot.identityRevision = 1 }, true], ['kind-invalid', snapshot => { snapshot.kind = 'wrong' }], ['source-mismatch', snapshot => { snapshot.source.draftId = 999 }], ['revision-fractional', snapshot => { snapshot.identityRevision = 1.5 }], ['characters-not-array', snapshot => { snapshot.characters = {} }], ['duplicate-id', snapshot => { snapshot.characters.push(snapshot.characters[0]) }], ['aliases-invalid', snapshot => { snapshot.characters[0].aliases = [1] }]]) {
  list(`frozen-${label}-stale`, db => { character(db); const record = one(db); freeze(db, record); frozenPatch(db, change, badHash) })
}
list('frozen-json-corrupt-stale', db => { const record = one(db); freeze(db, record); patch(db, 'character_identity_proposals', { raw_value: '{bad' }) })
list('without-identity-schema-current', db => { one(db); db.exec('DROP TABLE character_identity_meta') })

function writeNew(filename, bytes) {
  const fd = openSync(filename, 'wx', 0o600)
  try { writeFileSync(fd, bytes) } finally { closeSync(fd) }
}
function snapshot(db) { return db.prepare('SELECT * FROM summary_snapshots ORDER BY rowid').all().map(({ created_at: _clock, ...row }) => row) }
const cases = [], strongerAtomic = []
try {
  const bytes = readFileSync(baseline)
  const names = new Set()
  for (const definition of definitions) {
    assert(!names.has(definition.name)); names.add(definition.name)
    const file = path.join(directory, definition.name + '.db')
    writeNew(file, bytes)
    const db = new Database(file)
    try {
      db.pragma('foreign_keys=ON')
      assert(db.prepare("SELECT 1 FROM sqlite_master WHERE name='character_identity_meta' AND type='table'").get(), 'baseline-v7 must have actual M02 identity schema')
      assert.equal(db.prepare('SELECT COUNT(*) AS count FROM summary_snapshots').get().count, 0, 'deterministic summary IDs require empty actual baseline')
      globalThis[key] = db
      definition.seed?.(db)
      // Every input database is captured AFTER all seeding, BEFORE the actual call.
      const before = db.serialize()
      writeNew(path.join(directory, definition.name + '-before.db'), before)
      globalThis[key] = definition.nilDB ? null : db
      let result = null, error = ''
      try {
        switch (definition.method) {
          case 'SaveSnapshot': R.saveSnapshot(definition.chapter, definition.text); break
          case 'LatestSnapshot': result = R.getLatestSnapshot(); break
          case 'ReadFinalizedSource': result = R.readFinalizedSource(definition.draftID); break
          case 'ListBefore': result = R.listFinalizedContinuityBefore(definition.chapter); break
          default: assert.fail(definition.method)
        }
      } catch (caught) { assert(caught instanceof Error); error = caught.message }
      const unchanged = before.equals(db.serialize())
      if (definition.method !== 'SaveSnapshot' || error || definition.nilDB) assert(unchanged, `original read/error mutated ${definition.name}`)
      cases.push({ name: definition.name, method: definition.method, chapter: definition.chapter, draftID: definition.draftID, text: definition.text, nilDB: definition.nilDB ?? false, result, error, after: snapshot(db), unchanged })
    } finally { globalThis[key] = null; db.close() }
  }
  // Original saveSnapshot is a single nontransactional INSERT. RAISE(FAIL)
  // intentionally preserves trigger writes there, so this stronger Go promise
  // must NEVER be described as or compared with original failure parity.
  const name = 'go-stronger-atomic-fail-trigger', file = path.join(directory, name + '.db')
  writeNew(file, bytes)
  const db = new Database(file)
  try {
    db.pragma('foreign_keys=ON')
    legacy(db, 99, 'atomic witness')
    db.exec("CREATE TRIGGER contract_summary_fail AFTER INSERT ON summary_snapshots BEGIN UPDATE summary_snapshots SET character_states='partial trigger write' WHERE chapter_number=99; SELECT RAISE(FAIL,'SYNTHETIC_SUMMARY_ATOMIC_FAIL'); END")
    writeNew(path.join(directory, name + '-before.db'), db.serialize())
    strongerAtomic.push({ name, chapter: 1, text: 'must roll back', error: 'SYNTHETIC_SUMMARY_ATOMIC_FAIL', after: snapshot(db) })
  } finally { db.close() }
  assert(cases.length >= 80, `expected broad original coverage, got ${cases.length}`)
  assert(originalModules.size >= 3)
  writeNew(target, JSON.stringify({ format: 1, cases, strongerAtomic, originalModules: [...originalModules].sort(), receiptedStatuses: RECEIPTED_DRAFT_STATUSES, excludedLegacyWriteDomain: 'saveSnapshot chapterNumber accepts signed safe integers only in Go; native TypeErrors outside that domain are not oracle parity', normalization: 'Only top-level raw summary_snapshots row created_at SQL clocks are excluded. Every returned field, including author rawValue.created_at, IDs, source strings, hashes and rich parser metadata, is compared.' }))
  console.error(`Original SummaryRepository emitted ${cases.length} actual cases; ${strongerAtomic.length} separately labeled stronger Go-only atomic fixture; ${originalModules.size} actual recursive modules`)
} finally { hooks.deregister(); delete globalThis[key] }
