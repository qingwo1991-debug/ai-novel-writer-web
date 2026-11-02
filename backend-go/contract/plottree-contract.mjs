import assert from 'node:assert/strict'
import { builtinModules, createRequire, registerHooks, stripTypeScriptTypes } from 'node:module'
import { chmodSync, constants, copyFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { createHash } from 'node:crypto'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

// GitHub-only synthetic oracle, never an application/runtime Node bridge.
// Execute BOTH unmodified original repositories; substitute ONLY getProjectDb.
assert.equal(process.env.GITHUB_ACTIONS, 'true', 'contract generation is GitHub-only')
const base = process.env.RUNNER_TEMP
const target = process.env.GO_PLOTTREE_CONTRACT
assert(base && target && path.isAbsolute(base) && path.isAbsolute(target))
assert.equal(target, path.join(base, 'novel-go-plottree', 'contract.json'), 'use the exact disposable plottree output path')
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
const databaseStub = 'plottree-contract:database'
const globalKey = '__GO_PLOTTREE_CONTRACT_DB__'
assert(!Object.hasOwn(globalThis, globalKey))
const Database = createRequire(import.meta.url)('better-sqlite3')
// better-sqlite3 loads its native binding lazily. Probe BEFORE installing hooks,
// including with NODE_PATH, so no native-driver/CJS dependency is intercepted.
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
const { PlotTreeRepository } = await import(new URL('electron/repositories/plot-tree-repository.ts', root).href)
// Import explicitly as well: PlotTreeRepository.read calls this actual class.
const { NarrativeThreadRepository } = await import(new URL('electron/repositories/narrative-thread-repository.ts', root).href)
assert.equal(typeof NarrativeThreadRepository.list, 'function')

const clockKeys = new Set(['created_at', 'updated_at', 'createdAt', 'updatedAt'])
function normalize(value) {
  if (Buffer.isBuffer(value)) return value.toJSON()
  if (Array.isArray(value)) return value.map(normalize)
  if (value !== null && typeof value === 'object') return Object.fromEntries(Object.entries(value)
    .filter(([key]) => !clockKeys.has(key)).map(([key, child]) => [key, normalize(child)]))
  return value
}
const EARLY = '2026-01-01 00:00:00'
const LATE = '2026-02-01 00:00:00'
const GENERATED_AT = '2026-03-04T05:06:07.000Z'
const STALE = '0'.repeat(64)
const UNICODE = '中文 😀 <&>\u2028\u2029 \ue000 \u{10000} {"\ue000":1,"\u{10000}":2,"10":3,"2":4}'
function insert(db, table, row) {
  const columns = Object.keys(row)
  return db.prepare(`INSERT INTO ${table}(${columns.join(',')}) VALUES(${columns.map(() => '?').join(',')})`).run(...Object.values(row)).lastInsertRowid
}
function core(db, values) {
  for (const [key, value] of Object.entries(values)) {
    assert(/^[a-z_]+$/u.test(key))
    db.prepare(`UPDATE project_core SET ${key}=? WHERE id='main'`).run(value)
  }
}
function blueprint(db, { chapter = 1, title = '第一章 星钥', purpose = '发现旧门', events = '拾起星钥', notes = '', notesUpdatedAt = '' } = {}) {
  insert(db, 'blueprints', { chapter_number: chapter, title, purpose, key_events: events, notes, notes_updated_at: notesUpdatedAt, created_at: EARLY, updated_at: LATE })
}
function draft(db, { id = 11, chapter = 1, version = 1, status = 'finalized' } = {}) {
  const contentId = insert(db, 'contents', { body: '合成冻结正文 旧门开启 😀', created_at: EARLY })
  insert(db, 'drafts', { id, chapter_number: chapter, version, status, source: 'write', content_id: contentId,
    source_dependencies: '[]', word_count: 12, created_at: EARLY, updated_at: LATE })
}
function outbox(db, { id = 11, chapter = 1, title = '冻结章标题', publication = 'pending' } = {}) {
  const body = '合成冻结正文 旧门开启 😀'
  insert(db, 'finalization_outbox', { finalization_id: `synthetic-plottree-${id}`, draft_id: id, chapter_number: chapter,
    chapter_title: title, content_hash: createHash('sha256').update(body).digest('hex'), content_revision: 1,
    content_snapshot: body, target_file_name: `${chapter}-synthetic.txt`, knowledge_document_id: '', publication_status: publication,
    last_error: '', published_at: null, created_at: EARLY, updated_at: LATE })
}
function finalized(db, options = {}) { draft(db, options); outbox(db, options) }
function summary(db, { id = 11, chapter = 1, notes = '冻结摘要' } = {}) {
  insert(db, 'summary_snapshots', { draft_id: id, chapter_number: chapter, chapter_notes: notes, created_at: EARLY })
}
function plan(db, { id = 1, title = '星钥之谜', type = '伏笔', start = 1, end = 3, intent = '查明旧门来历' } = {}) {
  insert(db, 'narrative_thread_plans', { id, title, type, target_start_chapter: start, target_end_chapter: end, author_intent: intent,
    created_at: EARLY, updated_at: LATE })
}
function event(db, { id = 1, planId = 1, draftId = 11, type = 'planted', evidence = '旧门开启', reason = '发现线索', created = EARLY } = {}) {
  insert(db, 'narrative_thread_confirmations', { id, plan_id: planId, draft_id: draftId, event_type: type, evidence, reason, created_at: created })
}
function snapshot(revision = STALE) {
  return { version: 1, generatedAt: GENERATED_AT, writingLanguage: 'zh-CN', sourceRevision: revision,
    tracks: [{ id: 'main', title: '星钥主线', role: 'main', startChapter: 1, endChapter: 1, summary: '发现秘密',
      events: [{ status: 'planned', chapterNumber: 1, summary: '开启旧门', sources: [{ type: 'blueprint', chapterNumber: 1 }] }] }] }
}
function stored(db, value) { core(db, { plot_tree_snapshot: typeof value === 'string' || Buffer.isBuffer(value) ? value : JSON.stringify(value) }) }
function abortUpdate(db, message) {
  // Change another project_core row BEFORE ABORT. Whole-table after comparison
  // proves rollback of the trigger side effect as well as the main UPDATE.
  insert(db, 'project_core', { id: 'trigger-witness', project_name: 'unchanged', plot_tree_snapshot: 'witness' })
  db.exec(`CREATE TRIGGER contract_abort_plot_tree AFTER UPDATE OF plot_tree_snapshot ON project_core
    WHEN NEW.id='main' BEGIN
      UPDATE project_core SET project_name='must roll back' WHERE id='trigger-witness';
      SELECT RAISE(ABORT,'${message}');
    END`)
}
const definitions = []
const cases = []
const sourceHashEvidence = []
const add = (name, method, options = {}) => definitions.push({ name, method, ...options })
const read = (suffix, seed) => add(`read-${suffix}`, 'Read', { seed })
const expectCount = count => assert.equal(definitions.length, count, 'reviewed fixture section count')

// 35 reads: complete original bundles, not a purpose-built replacement oracle.
read('empty-sources')
read('english-language', db => core(db, { writing_language: 'en-US', synopsis: 'English synopsis' }))
read('language-normalization-is-exact', db => core(db, { writing_language: ' en-US ', synopsis: 'not trimmed' }))
read('blob-language-and-synopsis', db => core(db, { writing_language: Buffer.from('en-US'), synopsis: Buffer.from([0, 255, 34, 10]) }))
read('nullable-synopsis', db => core(db, { synopsis: null }))
read('unicode-json-text-and-utf16-keys', db => core(db, { synopsis: UNICODE }))
read('blueprint-chapter-order', db => { blueprint(db, { chapter: 9 }); blueprint(db, { chapter: 2 }); blueprint(db, { chapter: 5 }) })
read('blueprint-raw-null-and-blob', db => {
  blueprint(db, { title: Buffer.from([0, 255, 34]), purpose: null, events: Buffer.from(UNICODE) })
  blueprint(db, { chapter: 2, purpose: Buffer.from([1, 0, 255]), events: null })
})
read('finalized-no-outbox-uses-blueprint', db => { blueprint(db); draft(db) })
read('finalized-no-outbox-or-blueprint', db => draft(db))
read('empty-outbox-title-wins-over-blueprint', db => { blueprint(db); finalized(db, { title: '' }) })
read('blob-finalized-title-and-summary', db => { finalized(db, { title: Buffer.from(UNICODE) }); summary(db, { notes: Buffer.from([0, 255, 34]) }) })
read('latest-version-beats-id-order', db => {
  // The real UNIQUE(chapter_number,version) and NOT NULL version remain enabled.
  // A duplicate-version tie cannot be admitted; never seed an impossible tie.
  finalized(db, { id: 90, version: 1, title: '旧版本大ID' }); finalized(db, { id: 12, version: 4, title: '新版本小ID' })
})
read('newer-nonfinalized-does-not-hide-finalized', db => { finalized(db); draft(db, { id: 90, version: 9, status: 'draft' }) })
read('finalized-chapter-order-and-publication-independent', db => {
  finalized(db, { id: 19, chapter: 9, publication: 'failed' }); finalized(db, { id: 12, chapter: 2, publication: 'pending' }); finalized(db, { id: 15, chapter: 5, publication: 'published' })
})
read('snapshot-summary-precedes-blueprint', db => { blueprint(db, { notes: '新蓝图摘要', notesUpdatedAt: LATE }); finalized(db); summary(db) })
read('empty-snapshot-summary-falls-back', db => { blueprint(db, { notes: '回退摘要', notesUpdatedAt: EARLY }); finalized(db); summary(db, { notes: '' }) })
read('notes-without-extraction-timestamp-not-source', db => { blueprint(db, { notes: '未提取不可用', notesUpdatedAt: '' }); draft(db) })
read('sql-trim-space-only-empty-timestamp', db => { blueprint(db, { notes: '空格时间不可用', notesUpdatedAt: '    ' }); draft(db) })
read('sql-trim-keeps-tab-newline-and-nbsp', db => {
  for (const [chapter, notesUpdatedAt] of [[1, '\t\n'], [2, '\u00a0'], [3, '\uFEFF']]) { blueprint(db, { chapter, notes: `保留${chapter}`, notesUpdatedAt }); draft(db, { id: 10 + chapter, chapter }) }
})
read('nullable-blueprint-notes-is-null-summary', db => { blueprint(db, { notes: null, notesUpdatedAt: EARLY }); draft(db) })
read('thread-projection-order-status-not-severity', db => {
  plan(db, { id: 8 }); plan(db, { id: 2, title: '先列计划' })
  finalized(db, { id: 11, chapter: 1 }); finalized(db, { id: 13, chapter: 3 })
  event(db, { id: 20, planId: 8, draftId: 13, type: 'resolved', created: LATE })
  event(db, { id: 30, planId: 8, draftId: 11, type: 'abandoned', created: LATE })
  event(db, { id: 40, planId: 8, draftId: 13, type: 'progressing', created: EARLY })
})
read('thread-events-require-finalized-and-outbox', db => {
  plan(db); draft(db); finalized(db, { id: 12, chapter: 2, status: 'draft' }); event(db); event(db, { id: 2, draftId: 12, type: 'resolved' })
})
read('thread-raw-blob-fields-and-real-range', db => {
  plan(db, { title: Buffer.from(UNICODE), type: Buffer.from([0, 255]), start: 1.5, end: 4.5, intent: Buffer.from('原始意图') })
  finalized(db); event(db, { evidence: Buffer.from([0, 255]), reason: Buffer.from(UNICODE) })
})
read('hash-real-chapter-exponents-and-negative-zero', db => {
  for (const [id, chapter] of [[11, 1e-7], [12, 1e-6], [13, 1e21], [14, -0]]) draft(db, { id, chapter })
  core(db, { synopsis: UNICODE })
})
read('stored-empty-string', db => stored(db, ''))
read('stored-malformed-json-isolated', db => stored(db, '{broken'))
read('stored-structurally-invalid-isolated', db => stored(db, { version: 1, generatedAt: GENERATED_AT, writingLanguage: 'zh-CN', tracks: [] }))
read('stored-event-status-source-mismatch-isolated', db => { const value = snapshot(); value.tracks[0].events[0].status = 'occurred'; stored(db, value) })
read('stored-historical-blueprint-preserved-with-no-current-source', db => {
  blueprint(db); stored(db, snapshot(PlotTreeRepository.read().sourceRevision)); db.prepare('DELETE FROM blueprints').run()
})
read('stored-historical-finalized-and-thread-preserved', db => {
  const value = snapshot(); const track = value.tracks[0]; track.endChapter = 3
  track.events = [
    { status: 'occurred', chapterNumber: 1, summary: '历史定稿', sources: [{ type: 'finalized-chapter', draftId: 999, chapterNumber: 1 }] },
    { status: 'occurred', chapterNumber: 2, summary: '历史叙事事件', sources: [{ type: 'narrative-thread', planId: 88, eventId: 77, chapterNumber: 2 }] },
    { status: 'planned', chapterNumber: 3, summary: '历史叙事计划', sources: [{ type: 'narrative-thread', planId: 66 }] },
  ]; stored(db, value)
})
read('stored-canonical-trimming-and-extra-fields', db => {
  const value = snapshot(); value.generatedAt = ` \t${GENERATED_AT}\n`; value.extra = UNICODE
  value.tracks[0].id = '\uFEFF main \uFEFF'; value.tracks[0].title = ' 标题 😀 '; value.tracks[0].events[0].summary = '\uFEFF 事件 \uFEFF'
  value.tracks[0].extra = 'discard'; value.tracks[0].events[0].sources[0].extra = 'discard'; stored(db, value)
})
read('stored-empty-blob-is-truthy-and-invalid', db => stored(db, Buffer.alloc(0)))
read('snapshot-and-core-clocks-do-not-enter-source-hash', db => { stored(db, snapshot()); core(db, { created_at: LATE, updated_at: EARLY, project_name: 'not a source' }) })
read('core-missing-error', db => db.prepare("DELETE FROM project_core WHERE id='main'").run())
expectCount(35)

// 20 saves. expectedSourceRevision=current is obtained from original.read
// BEFORE save, separately from the snapshot revision (which can be invalid).
const save = (suffix, options = {}) => add(`save-${suffix}`, 'Save', {
  seed: db => blueprint(db), expected: 'current', makeInput: bundle => snapshot(bundle.sourceRevision), ...options,
})
save('minimum-blueprint-success')
save('canonical-trim-extras-and-fixed-generatedAt', { changeInput: value => {
  value.generatedAt = ` \t${GENERATED_AT}\n`; value.extra = UNICODE
  value.tracks[0].id = '\uFEFFmain\uFEFF'; value.tracks[0].title = ' 主线 😀 '; value.tracks[0].summary = ' 摘要 '; value.tracks[0].extra = 'discard'
  value.tracks[0].events[0].summary = '\uFEFF事件\uFEFF'; value.tracks[0].events[0].sources[0].extra = 'discard'
} })
save('finalized-source-success', { seed: db => finalized(db), changeInput: value => {
  value.tracks[0].events[0].status = 'occurred'; value.tracks[0].events[0].sources = [{ type: 'finalized-chapter', draftId: 11, chapterNumber: 1 }]
} })
save('narrative-plan-source-success', { seed: db => plan(db), changeInput: value => { value.tracks[0].events[0].sources = [{ type: 'narrative-thread', planId: 1 }] } })
save('narrative-event-source-success', { seed: db => { plan(db); finalized(db); event(db) }, changeInput: value => {
  value.tracks[0].events[0].status = 'occurred'; value.tracks[0].events[0].sources = [{ type: 'narrative-thread', planId: 1, eventId: 1, chapterNumber: 1 }]
} })
save('main-and-subplot-success', { changeInput: value => { const child = structuredClone(value.tracks[0]); child.id = 'child'; child.role = 'subplot'; child.parentTrackId = 'main'; value.tracks.push(child) } })
save('stale-revision-precedes-invalid-shape', { expected: 'stale', makeInput: () => ({ version: 999, tracks: null }) })
save('snapshot-sourceRevision-mismatch', { changeInput: value => { value.sourceRevision = STALE } })
save('snapshot-language-mismatch', { changeInput: value => { value.writingLanguage = 'en-US' } })
save('null-root-shape', { makeInput: () => null })
save('invalid-generatedAt', { changeInput: value => { value.generatedAt = 'not-a-date' } })
save('empty-tracks', { changeInput: value => { value.tracks = [] } })
save('duplicate-track-id', { changeInput: value => { value.tracks.push(structuredClone(value.tracks[0])) } })
save('missing-current-source-reference', { seed: db => blueprint(db, { chapter: 2 }) })
save('occurred-blueprint-status-mismatch', { changeInput: value => { value.tracks[0].events[0].status = 'occurred' } })
save('track-range-outside-current-sources', { changeInput: value => { value.tracks[0].endChapter = 2 } })
save('existing-but-empty-source-has-no-usable-bounds', { seed: db => blueprint(db, { title: '', purpose: '', events: '' }) })
save('narrative-reference-requires-paired-event-chapter', { seed: db => plan(db), changeInput: value => { value.tracks[0].events[0].sources = [{ type: 'narrative-thread', planId: 1, eventId: 1 }] } })
save('sql-trigger-abort-rolls-back-whole-core', { seed: db => { blueprint(db); stored(db, snapshot()); abortUpdate(db, 'SYNTHETIC_PLOTTREE_SAVE_ABORT') } })
save('sql-trigger-ignore-produces-save-failed', { seed: db => {
  blueprint(db); stored(db, snapshot()); db.exec("CREATE TRIGGER contract_ignore_plot_tree BEFORE UPDATE OF plot_tree_snapshot ON project_core WHEN NEW.id='main' BEGIN SELECT RAISE(IGNORE); END")
} })
expectCount(55)

// Five clears: full project_core state, including independent row and rollback.
add('clear-already-empty', 'Clear')
add('clear-valid-stored-and-unrelated-core-retained', 'Clear', { seed: db => {
  blueprint(db); stored(db, snapshot()); core(db, { synopsis: UNICODE, core_outline: '原始纲要' }); insert(db, 'project_core', { id: 'other', project_name: '保留', plot_tree_snapshot: 'other snapshot' })
} })
add('clear-corrupt-stored-without-reading-it', 'Clear', { seed: db => stored(db, '{invalid') })
add('clear-core-missing-error', 'Clear', { seed: db => db.prepare("DELETE FROM project_core WHERE id='main'").run() })
add('clear-sql-trigger-abort-rolls-back-whole-core', 'Clear', { seed: db => { stored(db, snapshot()); abortUpdate(db, 'SYNTHETIC_PLOTTREE_CLEAR_ABORT') } })
expectCount(60)
assert.equal(new Set(definitions.map(definition => definition.name)).size, definitions.length)
assert.deepEqual(Object.fromEntries(['Read', 'Save', 'Clear'].map(method => [method, definitions.filter(definition => definition.method === method).length])), { Read: 35, Save: 20, Clear: 5 })

// This is diagnostic evidence, NOT a replacement source/hash computation.
// Capture JSON.stringify of original read's facts verbatim, in its original
// property order, BEFORE normalize converts native Buffers to their JSON form.
const evidenceQueries = [
  "SELECT id,writing_language,typeof(writing_language) AS language_storage,synopsis,typeof(synopsis) AS synopsis_storage FROM project_core ORDER BY rowid",
  'SELECT chapter_number,title,purpose,key_events,notes,notes_updated_at,typeof(title) AS title_storage,typeof(purpose) AS purpose_storage,typeof(key_events) AS events_storage,typeof(notes) AS notes_storage FROM blueprints ORDER BY chapter_number',
  'SELECT id,chapter_number,version,status,typeof(chapter_number) AS chapter_storage,typeof(version) AS version_storage FROM drafts ORDER BY id',
  'SELECT draft_id,chapter_number,chapter_title,publication_status,typeof(chapter_title) AS title_storage FROM finalization_outbox ORDER BY rowid',
  'SELECT draft_id,chapter_number,chapter_notes,typeof(chapter_notes) AS notes_storage FROM summary_snapshots ORDER BY rowid',
  'SELECT id,title,type,target_start_chapter,target_end_chapter,author_intent,typeof(title) AS title_storage,typeof(type) AS type_storage,typeof(target_start_chapter) AS start_storage,typeof(author_intent) AS intent_storage FROM narrative_thread_plans ORDER BY id',
  'SELECT id,plan_id,draft_id,event_type,evidence,reason,typeof(evidence) AS evidence_storage,typeof(reason) AS reason_storage FROM narrative_thread_confirmations ORDER BY id',
]
function captureSourceEvidence(name, bundle, db) {
  const { sourceRevision, snapshot: ignoredSnapshot, storedSnapshotInvalid: ignoredInvalid, ...facts } = bundle
  const factsJSON = JSON.stringify(facts)
  assert.equal(createHash('sha256').update(factsJSON).digest('hex'), sourceRevision, 'verbatim original fact JSON must reproduce original sourceRevision')
  sourceHashEvidence.push({ name, factsJSON, sourceRevision,
    queries: evidenceQueries.map(sql => ({ sql, rows: normalize(db.prepare(sql).all()) })) })
}

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
      for (const table of ['project_core', 'blueprints', 'drafts', 'finalization_outbox', 'summary_snapshots', 'narrative_thread_plans', 'narrative_thread_confirmations']) {
        assert.equal(db.prepare(`SELECT COUNT(*) AS n FROM ${table}`).get().n, 0, `${table} baseline must be synthetic and empty`)
      }
      insert(db, 'project_core', { id: 'main', project_name: 'Synthetic PlotTree', writing_language: 'zh-CN', synopsis: '', plot_tree_snapshot: '', created_at: EARLY, updated_at: LATE })
      definition.seed?.(db)
      assert.deepEqual(db.pragma('foreign_key_check'), [], 'fixtures must have valid foreign keys')
      let input
      let expectedSourceRevision
      if (definition.method === 'Save') {
        const originalSources = PlotTreeRepository.read()
        captureSourceEvidence(definition.name, originalSources, db)
        expectedSourceRevision = definition.expected === 'current' ? originalSources.sourceRevision : STALE
        input = definition.makeInput(originalSources)
        definition.changeInput?.(input)
      }
      // Export precisely the preoperation database, with all source facts,
      // native storage classes, trigger SQL, sequences and fixed timestamps.
      before = db.serialize()
      let result = null
      let error
      try {
        if (definition.method === 'Read') result = PlotTreeRepository.read()
        else if (definition.method === 'Save') result = PlotTreeRepository.save(input, expectedSourceRevision)
        else if (definition.method === 'Clear') result = PlotTreeRepository.clear() ?? null
        else throw new Error(`unknown method ${definition.method}`)
      } catch (failure) {
        assert(failure instanceof Error)
        error = failure.message
      }
      // Evidence assertions are outside the operation error capture: an oracle
      // diagnostic failure must stop generation, not masquerade as err.message.
      if (definition.method === 'Read' && error === undefined) captureSourceEvidence(definition.name, result, db)
      const after = normalize({ project_core: db.prepare('SELECT * FROM project_core ORDER BY rowid').all() })
      assert.deepEqual(db.pragma('foreign_key_check'), [])
      if (definition.method === 'Read') assert.deepEqual(db.serialize(), before, 'Read must not mutate even malformed/historical stored snapshots')
      cases.push({ name: definition.name, database: filename, method: definition.method,
        ...(definition.method === 'Save' ? { input, expectedSourceRevision } : {}),
        result: normalize(result), ...(error === undefined ? {} : { error }), after })
    } finally {
      globalThis[globalKey] = null
      db.close()
      // Restore only AFTER close: every fixture is an absolute CLOSED BEFORE DB,
      // never postoperation rows repackaged to manufacture native expectations.
      if (before) writeFileSync(filename, before, { mode: 0o600 })
    }
  }
  assert.equal(cases.length, 60)
  assert.equal(sourceHashEvidence.length, 54) // 34 successful reads + 20 save prereads.
  writeFileSync(target, JSON.stringify({ formatVersion: 1, cases, sourceHashEvidence }) + '\n', { flag: 'wx', mode: 0o600 })
} finally {
  delete globalThis[globalKey]
  hooks.deregister()
}
console.log(`FROZEN ${cases.length} original PlotTree contracts (Read=35 Save=20 Clear=5), ${sourceHashEvidence.length} verbatim original fact JSON/hash proofs`)
