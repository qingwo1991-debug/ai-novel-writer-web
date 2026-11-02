// GitHub-only original oracle. Only the environment database seam is replaced;
// every recursively imported business module and every builtin remains original.
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { builtinModules, createRequire, registerHooks, stripTypeScriptTypes } from 'node:module'
import { closeSync, existsSync, mkdirSync, openSync, readFileSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

assert.equal(process.env.GITHUB_ACTIONS, 'true', 'original characters oracle is GitHub-only')
const base = process.env.RUNNER_TEMP, target = process.env.GO_CHARACTERS_CONTRACT
assert(base && target && path.isAbsolute(base) && path.isAbsolute(target))
assert.equal(target, path.join(base, 'novel-go-characters', 'contract.json'))
const sqliteContract = process.env.GO_SQLITE_CONTRACT
assert(sqliteContract && path.isAbsolute(sqliteContract))
const baseline = path.join(path.dirname(sqliteContract), 'baseline-v7.db')
const relativeBaseline = path.relative(base, baseline)
assert(relativeBaseline && !relativeBaseline.startsWith('..') && !path.isAbsolute(relativeBaseline))
assert(existsSync(baseline), 'actual centrally migrated M02 baseline must already exist')
const directory = path.dirname(target)
assert(!existsSync(directory), 'never reuse an oracle directory')
mkdirSync(directory, { recursive: true, mode: 0o700 })
const root = new URL('../../', import.meta.url), databaseURL = new URL('electron/database', root).href
const key = '__GO_CHARACTERS_ORIGINAL_DB__', databaseSeam = 'characters-contract:database'
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
    if (url === databaseURL || url === databaseURL + '.ts') return { url: databaseSeam, shortCircuit: true }
    const candidate = url.endsWith('.ts') ? url : url + '.ts'
    const filename = fileURLToPath(candidate), relative = path.relative(fileURLToPath(root), filename)
    assert(relative && !relative.startsWith('..') && !path.isAbsolute(relative))
    assert(/^(electron|src\/(shared|services|utils))\//u.test(relative), relative)
    assert(existsSync(filename), `missing original module ${relative}`)
    originalModules.add(relative)
    return { url: candidate, shortCircuit: true }
  },
  load(url, context, next) {
    if (url === databaseSeam) return { format: 'module', source: `export function getProjectDb(){return globalThis[${JSON.stringify(key)}]}\nexport function getCurrentProjectPath(){return null}`, shortCircuit: true }
    if (!url.startsWith(root.href) || !url.endsWith('.ts')) return next(url, context)
    return { format: 'module', source: stripTypeScriptTypes(readFileSync(new URL(url), 'utf8'), { mode: 'transform', sourceUrl: url }), shortCircuit: true }
  },
})
const { CharacterRepository } = await import(new URL('electron/repositories/character-repository.ts', root).href)
const { CharacterRosterRepository, readCharacterIdentitySnapshot, refreshCharacterIdentityProjection } = await import(new URL('electron/repositories/character-roster-repository.ts', root).href)
const { currentDerivedCharacterFields } = await import(new URL('electron/services/current-character-projection.ts', root).href)
const { CharacterProposalService } = await import(new URL('electron/services/character-proposal-service.ts', root).href)
const CLOCK = '2001-02-03 04:05:06', UPDATED = '2002-03-04T05:06:07.890Z'
const PROJECT = 'characters-contract-project'
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
function character(db, changes = {}) {
  insert(db, 'characters', { character_id: 'char-lin', name: 'Lin', role: 'protagonist', created_at: CLOCK, updated_at: UPDATED, ...changes })
}
function ready(db) { db.transaction(() => refreshCharacterIdentityProjection(db))() }
function aliases(db) {
  insert(db, 'character_aliases', { character_id: 'char-lin', name: 'Old Lin', source_key: 'source:z', valid_from: 0, valid_through: 3 })
  insert(db, 'character_aliases', { character_id: 'char-lin', name: 'Lin', source_key: 'source:b', valid_from: 2, valid_through: null })
  insert(db, 'character_aliases', { character_id: 'char-lin', name: 'Lin', source_key: 'source:a', valid_from: 1, valid_through: null })
  patch(db, 'character_identity_meta', { revision: 5 })
}
function graph(db) {
  character(db)
  character(db, { character_id: 'char-z', name: 'Zhou', role: 'supporting' })
  character(db, { character_id: 'char-retired', name: 'Retired', retired: 1 })
  aliases(db)
  insert(db, 'character_aliases', { character_id: 'char-retired', name: 'Retired alias', source_key: 'old', valid_from: 0, valid_through: null })
  insert(db, 'character_identity_approvals', { operation_id: 'approval-fixed', payload_hash: hash('approved'), receipt_json: '{"revision":5,"created_at":"author-clock"}' })
  for (const [id, from, to, relation] of [['edge-z', 'char-lin', 'char-z', 'ally'], ['edge-a', 'char-lin', 'char-z', 'friend'], ['edge-retired-target', 'char-lin', 'char-retired', 'past'], ['edge-retired-source', 'char-retired', 'char-lin', 'old']]) {
    insert(db, 'character_relationships', { relationship_id: id, source_character_id: from, target_character_id: to, relation, source_display_snapshot: 'Original source', target_display_snapshot: 'Original target', provenance_json: '{"created_at":"keep","nested":{"order":[2,1]}}', approval_id: 'approval-fixed' })
  }
  for (const id of ['proposal-z', 'proposal-a']) insert(db, 'character_identity_proposals', { proposal_id: id, owner_character_id: 'char-retired', source_key: 'legacy:raw', source_hash: hash(UNICODE), raw_value: UNICODE, candidate_ids_json: '["char-z","char-lin"]', resolved_character_id: null, approval_id: null })
}
function draft(db, options = {}) {
  const { chapter = 1, version = 1, status = 'finalized', body = BODY, summary = true, receipt = true, generation = 0 } = options
  const contentID = Number(insert(db, 'contents', { body, created_at: CLOCK }).lastInsertRowid)
  const draftID = Number(insert(db, 'drafts', { chapter_number: chapter, version, status, content_id: contentID, created_at: CLOCK, updated_at: UPDATED }).lastInsertRowid)
  const source = { draftId: draftID, finalizationId: `characters-finalization-${draftID}`, chapterNumber: chapter, contentHash: hash(body) }
  if (receipt) insert(db, 'finalization_outbox', { finalization_id: source.finalizationId, draft_id: draftID, chapter_number: chapter, chapter_title: UNICODE, content_hash: source.contentHash, content_revision: 1, content_snapshot: body, target_file_name: `synthetic-${draftID}.txt` })
  if (summary) insert(db, 'summary_snapshots', { draft_id: draftID, chapter_number: chapter, chapter_notes: 'Key found.', source_finalization_id: source.finalizationId, source_content_hash: source.contentHash, projection_generation: generation, created_at: CLOCK })
  return { source, contentID, draftID, version, generation }
}
function derived(db, options = {}) {
  const record = draft(db, options)
  const provenance = { kind: 'derived', source: record.source, revision: 1, sourceOrder: { chapterNumber: record.source.chapterNumber, authoritativeFinalizationRevision: record.version, continuityEpoch: `${PROJECT}:${record.generation}` } }
  character(db, { cs_updated_at_chapter: record.source.chapterNumber, cs_location: 'North tower', cs_provenance: JSON.stringify({ location: provenance }) })
  return { ...record, provenance }
}
function editProvenance(db, edit) {
  const value = JSON.parse(db.prepare('SELECT cs_provenance FROM characters WHERE character_id=?').pluck().get('char-lin'))
  edit(value)
  patch(db, 'characters', { cs_provenance: JSON.stringify(value) }, "WHERE character_id='char-lin'")
}
const definitions = [], add = (name, method, options = {}) => definitions.push({ name, method, ...options })
const all = (name, seed, options = {}) => add(`all-${name}`, 'All', { seed, ...options })
const roster = (name, seed, options = {}) => add(`roster-${name}`, 'ReadRoster', { seed, ...options })
const identity = (name, seed) => add(`identity-raw-${name}`, 'IdentitySnapshot', { seed })
const view = (name, seed) => add(`identity-view-${name}`, 'IdentityView', { seed })
const fields = (name, seed, options = {}) => add(`derived-${name}`, 'CurrentDerivedFields', { seed, projectID: PROJECT, ...options })

// Raw CharacterRepository rows retain the original JS truthiness and parser shape.
all('empty')
all('no-project', undefined, { nilDB: true })
all('canonical-role-order', db => { for (const [index, role] of ['minor', 'antagonist', 'supporting', 'protagonist'].entries()) character(db, { character_id: `role-${index}`, role, name: role }) })
all('role-aliases-and-fallback', db => { for (const [index, role] of [' MAIN ', '女主', 'villain', '配角', '次要角色', 'MAİN', 'MİNOR', 'unknown', null, Buffer.from('main')].entries()) character(db, { character_id: `alias-${index}`, role, name: `Name ${index}` }) })
all('active-only', graph)
all('duplicate-display-names', db => { character(db); character(db, { character_id: 'same-name', name: 'Lin' }) })
all('unicode-exact-fields', db => character(db, Object.fromEntries(['name', 'gender', 'age', 'appearance', 'personality', 'background', 'abilities', 'motivation', 'relationships', 'arc', 'notes'].map(key => [key, UNICODE]))))
all('null-optional-columns', db => character(db, Object.fromEntries(['gender', 'age', 'appearance', 'personality', 'background', 'abilities', 'motivation', 'relationships', 'arc', 'notes', 'cs_location'].map(key => [key, null]))))
all('blob-columns-buffer-json', db => character(db, { name: Buffer.from('Raw name'), gender: Buffer.from([0, 255, 65]), relationships: Buffer.from('raw relation'), notes: Buffer.alloc(0), cs_updated_at_chapter: 0, cs_location: Buffer.from(UNICODE), cs_provenance: Buffer.from('{"location":{"kind":"legacy"}}') }))
all('numeric-chapter-and-text-affinity', db => character(db, { age: 42, notes: 0, cs_updated_at_chapter: 1.5, cs_location: 7 }))
for (const [label, chapter] of [['zero', 0], ['negative', -7], ['max-safe', Number.MAX_SAFE_INTEGER], ['min-safe', Number.MIN_SAFE_INTEGER], ['text', 'not-a-number'], ['null-absent', null]]) all(`state-chapter-${label}`, db => character(db, { cs_updated_at_chapter: chapter, cs_location: UNICODE }))
for (const [label, provenance] of [['invalid-json', '{bad'], ['empty', ''], ['array', '[]'], ['null', 'null'], ['number', '7'], ['empty-object', '{}'], ['whitespace', ' \n '], ['unknown-keys', '{"custom":{"created_at":"keep","nested":[null,true,"🙂"]}}'], ['raw-numeric-parser', '{"location":{"n":9007199254740993,"tiny":1e-999,"created_at":"author-clock"}}']]) all(`provenance-${label}`, db => character(db, { cs_updated_at_chapter: 0, cs_location: 'Visible raw', cs_provenance: provenance }))

// M02 read must do no legacy DDL. Ready hashes are sealed by the actual original
// refresh implementation inside its required actual transaction, never fabricated.
roster('empty')
roster('no-project', undefined, { nilDB: true })
roster('unsealed-cards-inconsistent', db => character(db))
roster('ready-one-zh', db => { character(db, { gender: '女', age: '二十', notes: UNICODE }); ready(db) })
roster('ready-all-role-labels-en', db => { patch(db, 'project_core', { writing_language: 'en-US' }); for (const [index, role] of ['minor', 'antagonist', 'supporting', 'protagonist'].entries()) character(db, { character_id: `role-${index}`, role, name: `${role} ${UNICODE}`, gender: 'female', age: '20', appearance: 'red coat', personality: 'calm', background: 'village', abilities: 'sword', motivation: 'home', arc: 'growth', notes: 'author notes' }); ready(db) })
roster('ready-all-role-labels-zh', db => { for (const [index, role] of ['minor', 'antagonist', 'supporting', 'protagonist'].entries()) character(db, { character_id: `role-${index}`, role, name: `角色${index}`, gender: '女', age: '二十' }); ready(db) })
roster('utf16-astral-before-bmp-names-and-ids', db => { character(db, { character_id: '\uE000', name: '\uE000', role: 'supporting' }); character(db, { character_id: '🙂', name: '🙂', role: 'supporting' }); ready(db) })
roster('identity-edges-retired-aliases-proposals', db => { graph(db); ready(db) })
roster('raw-legacy-notes-with-id-edges', db => { graph(db); patch(db, 'characters', { relationships: ` Unparsed author prose ${UNICODE} ` }, "WHERE character_id='char-lin'"); ready(db) })
roster('raw-json-relationships-do-not-authorize-id-edges', db => { character(db, { relationships: '[{"target":"Ghost","relation":"unproved"}]' }); ready(db) })
roster('previous-zh-projection-after-language-change', db => { character(db); ready(db); patch(db, 'project_core', { writing_language: 'en-US' }) })
roster('previous-en-projection-after-language-change', db => { patch(db, 'project_core', { writing_language: 'en-US' }); character(db); ready(db); patch(db, 'project_core', { writing_language: 'zh-CN' }) })
roster('unknown-language-falls-back-zh', db => { character(db); patch(db, 'project_core', { writing_language: 'unknown' }); ready(db) })
for (const [label, table, changes] of [['projection-hash', 'character_roster_meta', { projection_hash: hash('wrong') }], ['fact-hash', 'character_roster_meta', { fact_hash: hash('wrong') }], ['projection-text', 'project_core', { characters_arch: 'manually changed projection' }], ['fact-state', 'characters', { cs_updated_at_chapter: 0, cs_location: 'unsealed change' }], ['fact-static', 'characters', { notes: 'unsealed note' }]]) roster(`bad-${label}`, db => { character(db); ready(db); patch(db, table, changes) })
roster('missing-meta-row-domain-error', db => db.exec('DELETE FROM character_roster_meta'))
for (const [label, state, legacy, projection, cards] of [['legacy-cards', 'legacy_cards_preserved', 'original cards', '', true], ['legacy-markdown', 'legacy_markdown_pending', ' original Markdown \n', '', false], ['legacy-markdown-with-cards', 'legacy_markdown_pending', 'original', '', true], ['empty-with-projection', 'empty', '', 'unowned prose', false], ['empty-with-legacy-whitespace', 'empty', ' \uFEFF \n', ' \n', false], ['ready-empty', 'ready', '', '', false]]) roster(`status-${label}`, db => { if (cards) character(db); if (state === 'ready') ready(db); else { patch(db, 'character_roster_meta', { migration_state: state, legacy_markdown: legacy }); patch(db, 'project_core', { characters_arch: projection }) } })
for (const [label, provenance] of [['nested-insertion-order', '{"location":{"kind":"author","chapterNumber":0,"extra":{"z":1,"a":{"y":2,"x":3}},"created_at":"keep"},"powerLevel":{"kind":"legacy"}}'], ['nested-reversed-order', '{"powerLevel":{"kind":"legacy"},"location":{"created_at":"keep","extra":{"a":{"x":3,"y":2},"z":1},"chapterNumber":0,"kind":"author"}}'], ['integer-key-enumeration', '{"10":{"z":1,"a":2},"2":[3,2,1],"location":{"kind":"legacy","10":"ten","2":"two"}}'], ['raw-numeric-parser', '{"location":{"kind":"legacy","n":9007199254740993,"tiny":1e-999}}']]) roster(`hash-${label}`, db => { character(db, { cs_updated_at_chapter: 0, cs_location: UNICODE, cs_provenance: provenance }); ready(db) })

// Raw internal identity snapshot: ALL rows, exact raw JSON and SQL clocks,
// including retired characters, closed aliases, edges and proposals.
identity('empty')
identity('all-rows-ordered', graph)
identity('exact-stored-clock-strings', db => character(db, { created_at: 'author-created-clock-not-iso', updated_at: '1999-12-31 23:59:59' }))
identity('raw-json-not-parsed', db => character(db, { static_provenance: '{bad', cs_provenance: '{also-bad', cs_updated_at_chapter: 0 }))
identity('raw-null-numeric-blob', db => character(db, { name: null, role: Buffer.from('main'), gender: null, notes: Buffer.from([0, 255]), cs_updated_at_chapter: 1.25, identity_revision: 3 }))
identity('sqlite-order-bmp-before-astral', db => { character(db, { character_id: '🙂', name: 'Astral' }); character(db, { character_id: '\uE000', name: 'BMP' }) })
identity('proposal-resolved-with-receipt', db => { graph(db); patch(db, 'character_identity_proposals', { resolved_character_id: 'char-z', approval_id: 'approval-fixed' }, "WHERE proposal_id='proposal-a'") })
identity('alias-same-name-revision-order', db => { character(db); aliases(db); insert(db, 'character_aliases', { character_id: 'char-lin', name: 'Lin', source_key: 'source:z', valid_from: 0, valid_through: 1 }) })
identity('raw-unicode-source-strings', db => { character(db, { legacy_key: UNICODE, static_provenance: JSON.stringify({ created_at: UNICODE, raw: [1, true, null] }) }); insert(db, 'character_identity_proposals', { proposal_id: 'raw', owner_character_id: null, source_key: UNICODE, source_hash: hash(UNICODE), raw_value: ' {"z":9007199254740993,"tiny":1e-999} ', candidate_ids_json: ' [ "char-lin" ] ', resolved_character_id: null, approval_id: null }) })

// Public identity API is the actual service DTO, NOT the raw repository helper.
// The constructor's write-proof callback is an assertion sentinel, never a stub
// for invoked business behavior: these reads must not request proof authority.
view('empty')
view('all-rows-retired-and-edges', graph)
view('canonical-fields', db => character(db, { gender: 'female', age: '20', appearance: UNICODE, personality: 'calm', background: 'village', abilities: 'sword', motivation: 'home', arc: 'growth', notes: 'notes' }))
view('null-fields-become-empty-strings', db => character(db, Object.fromEntries(['name', 'role', 'gender', 'age', 'appearance', 'personality', 'background', 'abilities', 'motivation', 'arc', 'notes'].map(key => [key, null]))))
view('numeric-and-valid-utf8-blob-string-conversion', db => character(db, { role: 42, gender: Buffer.from('woman'), age: 0, notes: Buffer.from(UNICODE), appearance: Buffer.alloc(0), identity_revision: 7 }))
view('static-provenance-author', db => character(db, { static_provenance: '{"kind":"author","chapterNumber":0,"revision":4,"created_at":"keep"}' }))
view('static-provenance-generated', db => character(db, { static_provenance: JSON.stringify({ kind: 'generated', modelRevision: 'fixed-model', source: { projectId: PROJECT, epoch: 'epoch-fixed', sourceId: 'artifact-fixed', revision: 1, contentHash: hash(BODY) } }) }))
view('static-provenance-parser-numbers', db => character(db, { static_provenance: '{"kind":"legacy","n":9007199254740993,"tiny":1e-999,"nested":{"created_at":"keep"}}' }))
view('static-provenance-null-retained', db => character(db, { static_provenance: 'null' }))
view('static-provenance-array-retained', db => character(db, { static_provenance: '[1,true,"🙂",null]' }))
view('raw-role-not-normalized', db => character(db, { role: ' 主角 ' }))
view('sqlite-id-order', db => { character(db, { character_id: '🙂', name: UNICODE }); character(db, { character_id: '\uE000', name: 'BMP' }) })

// Display grants require the full original current-character-projection proof.
fields('empty')
fields('unproved-legacy-state-not-derived', db => character(db, { cs_updated_at_chapter: 0, cs_location: 'Unknown', cs_provenance: '{}' }))
fields('valid-source', db => derived(db))
fields('full-unicode-body-sha256', db => derived(db, { body: UNICODE }))
fields('empty-body-sha256', db => derived(db, { body: '' }))
fields('full-long-body-not-prefix', db => { const record = derived(db, { body: BODY.repeat(300) + ' authoritative final tail' }); patch(db, 'contents', { body: BODY.repeat(300) + ' tampered tail' }, `WHERE id=${record.contentID}`) })
fields('valid-full-long-body', db => derived(db, { body: BODY.repeat(300) + UNICODE }))
fields('missing-summary-no-authority', db => derived(db, { summary: false }))
fields('missing-receipt-no-authority', db => derived(db, { receipt: false }))
fields('wrong-project-epoch', db => derived(db), { projectID: 'different-project' })
fields('empty-project-no-authority', db => derived(db), { projectID: '' })
for (const [label, table, changes] of [['live-body', 'contents', { body: 'changed body' }], ['snapshot-tail', 'finalization_outbox', { content_snapshot: BODY + ' changed tail' }], ['hash-mismatch', 'finalization_outbox', { content_hash: hash('wrong') }], ['uppercase-hash', 'finalization_outbox', { content_hash: hash(BODY).toUpperCase() }], ['summary-finalization', 'summary_snapshots', { source_finalization_id: 'wrong' }], ['summary-hash', 'summary_snapshots', { source_content_hash: hash('wrong') }], ['summary-chapter', 'summary_snapshots', { chapter_number: 2 }], ['version-zero', 'drafts', { version: 0 }], ['version-fractional', 'drafts', { version: 1.5 }], ['version-unsafe', 'drafts', { version: Number.MAX_SAFE_INTEGER + 1 }], ['projection-generation-negative', 'summary_snapshots', { projection_generation: -1 }], ['projection-generation-fractional', 'summary_snapshots', { projection_generation: 0.5 }], ['projection-generation-future', 'summary_snapshots', { projection_generation: 1 }]]) fields(`invalid-${label}`, db => { derived(db); patch(db, table, changes) })
for (const status of ['draft', 'revised', 'reviewed', 'superseded', 'archived']) fields(`noncurrent-status-${status}`, db => derived(db, { status }))
fields('newer-finalized-version-suppresses-old', db => { derived(db); draft(db, { version: 2, body: 'New version' }) })
fields('newer-working-draft-does-not-suppress', db => { derived(db); draft(db, { version: 2, status: 'draft' }) })
fields('max-version-not-insertion-id', db => { derived(db, { version: 3 }); draft(db, { version: 2 }) })
fields('equal-version-max-id', db => { db.exec('DROP INDEX idx_drafts_chapter_version'); derived(db); draft(db) })
for (const [label, generation, stale, proofGeneration] of [['same-generation-stale-marker', 3, 1, 3], ['old-invalidated', 3, 1, 2], ['old-before-cutoff', 3, 2, 2], ['old-without-cutoff', 3, null, 2]]) fields(`watermark-${label}`, db => { derived(db, { generation: proofGeneration }); patch(db, 'continuity_projection_meta', { generation, stale_from_chapter: stale }) })
for (const [label, change] of [
  ['extra-provenance-key', p => { p.location.extra = true }],
  ['missing-revision', p => { delete p.location.revision }],
  ['zero-revision', p => { p.location.revision = 0 }],
  ['fractional-revision', p => { p.location.revision = 1.5 }],
  ['wrong-source-draft', p => { p.location.source.draftId = 999 }],
  ['wrong-source-chapter', p => { p.location.source.chapterNumber = 2 }],
  ['blank-finalization-id', p => { p.location.source.finalizationId = ' \uFEFF ' }],
  ['source-finalization-not-string', p => { p.location.source.finalizationId = 7 }],
  ['wrong-source-hash', p => { p.location.source.contentHash = hash('wrong') }],
  ['wrong-source-order-chapter', p => { p.location.sourceOrder.chapterNumber = 2 }],
  ['wrong-source-order-version', p => { p.location.sourceOrder.authoritativeFinalizationRevision = 2 }],
  ['wrong-source-order-epoch', p => { p.location.sourceOrder.continuityEpoch = `${PROJECT}:9` }],
  ['extra-source-order-key', p => { p.location.sourceOrder.extra = 'not allowed' }],
  ['source-order-array', p => { p.location.sourceOrder = [] }],
  ['source-order-missing', p => { delete p.location.sourceOrder }],
  ['unknown-kind', p => { p.location.kind = 'generated' }],
  ['field-array', p => { p.location = [] }],
  // The original permits extra SOURCE keys, but never extra provenance/order keys.
  ['source-extra-metadata-permitted', p => { p.location.source.extra = { created_at: 'keep' } }],
  ['reordered-source-keys', p => { const s = p.location.source; p.location.source = { contentHash: s.contentHash, chapterNumber: s.chapterNumber, finalizationId: s.finalizationId, draftId: s.draftId } }],
]) fields(`provenance-${label}`, db => { derived(db); editProvenance(db, change) })
for (const [label, raw] of [['bad-json', '{bad'], ['array', '[]'], ['null', 'null'], ['scalar', '7']]) fields(`provenance-${label}`, db => { derived(db); patch(db, 'characters', { cs_provenance: raw }) })
fields('author-legacy-not-derived', db => { derived(db); editProvenance(db, p => { p.location = { kind: 'author', chapterNumber: 0, revision: 4 }; p.powerLevel = { kind: 'legacy', revision: 0 }; p.keyItems = { kind: 'author', chapterNumber: 0, extra: true } }); patch(db, 'characters', { cs_power_level: 'known legacy', cs_key_items: 'invalid author' }) })
fields('all-six-fields-original-order', db => { derived(db); editProvenance(db, p => { for (const field of ['recentEvents', 'keyItems', 'mentalState', 'physicalState', 'powerLevel']) p[field] = p.location }); patch(db, 'characters', { cs_power_level: 'level', cs_physical_state: 'healthy', cs_mental_state: 'calm', cs_key_items: 'key', cs_recent_events: 'found' }) })
fields('null-and-blob-values-not-trusted', db => { derived(db); editProvenance(db, p => { p.powerLevel = p.location; p.keyItems = p.location }); patch(db, 'characters', { cs_location: null, cs_power_level: Buffer.from('blob'), cs_key_items: '' }) })
fields('retired-characters-still-have-display-grants', db => { derived(db); patch(db, 'characters', { retired: 1 }) })
fields('chapter-null-does-not-grant-authority', db => { derived(db); patch(db, 'characters', { cs_updated_at_chapter: null }) })

function writeNew(filename, bytes) {
  const fd = openSync(filename, 'wx', 0o600)
  try { writeFileSync(fd, bytes) } finally { closeSync(fd) }
}
const quote = name => '"' + name.replaceAll('"', '""') + '"'
function poststate(db) {
  return Object.fromEntries(db.prepare("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name").all().map(({ name }) => [name, db.prepare(`SELECT * FROM ${quote(name)} ORDER BY rowid`).all()]))
}
function stabilizeClocks(db) {
  // Characters already receive explicit fixture clocks, including intentional
  // non-ISO author strings; never replace those stored values.
  for (const table of ['character_roster_meta', 'project_core', 'contents', 'drafts', 'summary_snapshots', 'finalization_outbox']) {
    const names = db.prepare(`PRAGMA table_info(${quote(table)})`).all().map(row => row.name)
    for (const [column, value] of [['created_at', CLOCK], ['updated_at', UPDATED]]) if (names.includes(column)) patch(db, table, { [column]: value })
  }
}
function invoke(definition, db) {
  switch (definition.method) {
    case 'All': return CharacterRepository.getAll()
    case 'ReadRoster': return CharacterRosterRepository.read()
    case 'IdentitySnapshot': return readCharacterIdentitySnapshot(db)
    case 'IdentityView': {
      let proofCalls = 0
      const service = new CharacterProposalService(db, PROJECT, () => { proofCalls++; throw new Error('IDENTITY_READ_MUST_NOT_REQUEST_WRITE_PROOF') })
      const result = service.identitySnapshot()
      assert.equal(proofCalls, 0, 'read requested write proof authority')
      return result
    }
    case 'CurrentDerivedFields': return currentDerivedCharacterFields(db, definition.projectID)
    default: assert.fail(definition.method)
  }
}
const cases = [], unsupportedBoundaries = []
try {
  const bytes = readFileSync(baseline), names = new Set()
  for (const definition of definitions) {
    assert(!names.has(definition.name)); names.add(definition.name)
    const file = path.join(directory, definition.name + '.db')
    writeNew(file, bytes)
    const db = new Database(file)
    try {
      db.pragma('foreign_keys=ON')
      assert(db.prepare("SELECT 1 FROM sqlite_master WHERE name='character_identity_meta' AND type='table'").get(), 'actual M02 schema required')
      assert.equal(db.prepare('SELECT COUNT(*) AS count FROM characters').get().count, 0, 'deterministic fixture requires empty actual baseline')
      assert.equal(db.prepare('SELECT COUNT(*) AS count FROM drafts').get().count, 0)
      // The schema baseline deliberately has no application project row. Seed
      // that ordinary fixture row directly, without changing its actual schema.
      db.prepare("INSERT OR IGNORE INTO project_core(id,project_name,created_at,updated_at) VALUES('main',?,?,?)").run('Synthetic characters contract', CLOCK, UPDATED)
      patch(db, 'continuity_projection_meta', { generation: 0, stale_from_chapter: null })
      globalThis[key] = db
      definition.seed?.(db)
      stabilizeClocks(db)
      const before = db.serialize()
      writeNew(path.join(directory, definition.name + '-before.db'), before)
      globalThis[key] = definition.nilDB ? null : db
      let result = null, error = ''
      try { result = invoke(definition, db) } catch (caught) { assert(caught instanceof Error); assert(!(caught instanceof TypeError) && !(caught instanceof SyntaxError), `native unsupported error must not masquerade as parity: ${definition.name}: ${caught}`); error = caught.message }
      assert(before.equals(db.serialize()), `original read changed database bytes: ${definition.name}`)
      cases.push({ name: definition.name, method: definition.method, projectID: definition.projectID ?? PROJECT, nilDB: definition.nilDB ?? false, result, error, after: poststate(db), unchanged: true })
    } finally { globalThis[key] = null; db.close() }
  }
  // Native undefined-row TypeErrors are explicitly OUTSIDE parity. Go promises
  // one deterministic domain error instead; the original error is evidence only.
  for (const method of ['IdentitySnapshot', 'IdentityView']) {
    const name = `go-only-missing-identity-meta-${method}`, file = path.join(directory, name + '.db')
    writeNew(file, bytes)
    const db = new Database(file)
    try {
      db.pragma('foreign_keys=ON')
      db.exec('DELETE FROM character_identity_meta')
      stabilizeClocks(db)
      const before = db.serialize()
      writeNew(path.join(directory, name + '-before.db'), before)
      let originalError = '', originalErrorName = ''
      try { invoke({ method }, db); assert.fail('expected native original TypeError') } catch (caught) { assert(caught instanceof TypeError); originalError = caught.message; originalErrorName = caught.name }
      assert(before.equals(db.serialize()))
      unsupportedBoundaries.push({ name, method, expectedGoError: 'CHARACTER_ID_META_MISSING', originalError, originalErrorName, after: poststate(db) })
    } finally { db.close() }
  }
  assert(cases.length >= 60)
  assert(originalModules.has('electron/services/character-proposal-service.ts'))
  const counts = Object.fromEntries([...new Set(cases.map(item => item.method))].map(method => [method, cases.filter(item => item.method === method).length]))
  for (const method of ['All', 'ReadRoster', 'IdentitySnapshot', 'IdentityView', 'CurrentDerivedFields']) assert(counts[method] > 0)
  assert(counts.IdentityView >= 10)
  writeNew(target, JSON.stringify({ format: 1, cases, unsupportedBoundaries, originalModules: [...originalModules].sort(), counts, normalization: 'None: every returned field, raw JSON string, source/hash/revision, Buffer JSON, SQL created_at/updated_at stored string and full raw table post-state remains exact. UseNumber on Go oracle decode preserves numeric input lexemes until actual JSON wire comparison.', authority: 'Actual original centrally migrated database; original recursive modules; actual transaction + refreshCharacterIdentityProjection seals ready fixtures. Manual raw state fixtures are not generation/author authority. Public identity DTO invokes actual CharacterProposalService.identitySnapshot; its write-proof callback must never be invoked.', excludedNativeDomain: 'Native TypeError/SyntaxError failures are not parity. Missing identity meta is separately labeled deterministic Go-only boundary.' }))
  console.error(`Original characters emitted ${cases.length} actual cases ${JSON.stringify(counts)}; ${unsupportedBoundaries.length} separate Go-only unsupported boundaries; ${originalModules.size} actual recursive modules`)
} finally { hooks.deregister(); delete globalThis[key] }
