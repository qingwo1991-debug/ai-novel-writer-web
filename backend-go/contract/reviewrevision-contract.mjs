import assert from 'node:assert/strict'
import { createRequire, registerHooks, stripTypeScriptTypes } from 'node:module'
import { builtinModules } from 'node:module'
import { readFileSync, existsSync, mkdirSync, copyFileSync, constants, writeFileSync, chmodSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

// GitHub-only synthetic oracle, never an application runtime bridge. The only
// substituted module is database.ts: original public methods with implicit DB
// parameters must see the same private connection as explicitly passed methods.
assert.equal(process.env.GITHUB_ACTIONS, 'true', 'contract generation is GitHub-only')
const base = process.env.RUNNER_TEMP
const target = process.env.GO_REVIEWREVISION_CONTRACT
assert(base && target && path.isAbsolute(base) && path.isAbsolute(target))
assert.equal(target, path.join(base, 'novel-go-reviewrevision', 'contract.json'), 'use the exact disposable review/revision output path')
assert(!existsSync(target), 'never overwrite an existing contract')
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
const databaseStub = 'reviewrevision-contract:database'
const globalKey = '__GO_REVIEWREVISION_CONTRACT_DB__'
assert(!Object.hasOwn(globalThis, globalKey))
globalThis[globalKey] = null
const Database = createRequire(import.meta.url)('better-sqlite3')
// better-sqlite3 loads its native binding lazily. Resolve it BEFORE the narrow
// loader, otherwise its CommonJS native loader can be intercepted accidentally.
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
// In particular, revision-repository imports the REAL review-cycle repository,
// its REAL migration verifier and shared parsers. No business function is mocked.
const { ReviewRepository } = await import(new URL('electron/repositories/review-repository.ts', root).href)
const { RevisionRepository } = await import(new URL('electron/repositories/revision-repository.ts', root).href)

const timestampKeys = new Set(['created_at', 'updated_at', 'createdAt', 'updatedAt'])
function normalize(value) {
  // Preserve SQLite BLOB storage class as the driver's ordinary JSON shape,
  // rather than coercing bytes into text or numeric-keyed plain objects.
  if (Buffer.isBuffer(value)) return value.toJSON()
  if (Array.isArray(value)) return value.map(normalize)
  if (value !== null && typeof value === 'object') return Object.fromEntries(Object.entries(value).filter(([key]) => !timestampKeys.has(key)).map(([key, child]) => [key, normalize(child)]))
  return value
}
const tableOrder = {
  contents: 'id', drafts: 'id', reviews: 'id', revisions: 'id',
  review_cycles: 'cycle_id', review_findings: 'cycle_id, finding_id', review_cycle_merges: 'cycle_id',
}
function snapshot(db) {
  return Object.fromEntries(Object.entries(tableOrder).map(([table, order]) => [table, db.prepare(`SELECT * FROM ${table} ORDER BY ${order}`).all()]))
}
const BODY = 'Synthetic 原稿 😀\nsecond line <&>\u2028\u2029'
const OTHER_BODY = 'Other chapter, independent source.'
const MERGED = 'Author-confirmed 合并正文 😀\nnot the AI candidate'
const sourceDraft = () => ({ id: 11, chapterNumber: 7, version: 3, status: 'draft', content: BODY })
const otherSource = () => ({ id: 22, chapterNumber: 8, version: 2, status: 'reviewed', content: OTHER_BODY })
const cases = []
const definitions = []
const add = (name, method, options = {}) => definitions.push({ name, method, ...options })
function insert(db, table, row) {
  const columns = Object.keys(row)
  return Number(db.prepare(`INSERT INTO ${table}(${columns.join(',')}) VALUES(${columns.map(() => '?').join(',')})`).run(...Object.values(row)).lastInsertRowid)
}
function content(db, body) { return insert(db, 'contents', { body }) }
function patch(db, table, id, changes) {
  const columns = Object.keys(changes)
  assert(columns.every(key => /^[a-z_]+$/u.test(key)))
  db.prepare(`UPDATE ${table} SET ${columns.map(key => `${key}=?`).join(',')} WHERE id=?`).run(...Object.values(changes), id)
}
function sourceColumns(value = sourceDraft()) {
  return { source_draft_chapter_number: value.chapterNumber, source_draft_version: value.version, source_draft_status: value.status, source_content: value.content }
}
function seedReview(db, { id = 301, index = 1, baseDraftId = 11, source: frozen = sourceDraft(), body = 'Synthetic review report 中文', ...metadata } = {}) {
  return insert(db, 'reviews', { id, base_draft_id: baseDraftId, review_index: index, content_id: content(db, body), ...sourceColumns(frozen), ...metadata })
}
function seedRevision(db, { id = 501, index = 1, baseDraftId = 11, source: frozen = sourceDraft(), body = 'Synthetic AI candidate, not merged text', ...metadata } = {}) {
  return insert(db, 'revisions', { id, base_draft_id: baseDraftId, revision_index: index, revision_type: 'refine', status: 'pending', merged_to_draft_id: null, user_prompt: '', review_source_id: null, content_id: content(db, body), word_count: 17, ...sourceColumns(frozen), ...metadata })
}
function reviewHistory(db) {
  // Deliberately make row IDs disagree with review index ordering, with gaps.
  seedReview(db, { id: 309, index: 2, body: 'earlier index' })
  seedReview(db, { id: 301, index: 9, body: 'latest index' })
  seedReview(db, { id: 315, index: 30, baseDraftId: 22, source: otherSource(), body: 'unrelated draft review' })
}
function revisionHistory(db) {
  seedRevision(db, { id: 509, index: 2 })
  seedRevision(db, { id: 501, index: 9, revision_type: 'review-fix', user_prompt: 'prompt 中文 😀' })
  seedRevision(db, { id: 505, index: 5, status: 'discarded' })
  seedRevision(db, { id: 507, index: 7, status: 'merged', merged_to_draft_id: 11 })
  seedRevision(db, { id: 515, index: 30, baseDraftId: 22, source: otherSource() })
}
function creationInput(method) {
  return { baseDraftId: 11, content: 'New synthetic output 中文 😀\n<&>', expectedSource: sourceDraft(), ...(method === 'CreateReview' ? {} : { revisionType: 'refine', wordCount: 23 }) }
}
function mergeInput() { return { revisionId: 501, targetDraftId: 11, expectedDraftContent: BODY, mergedContent: MERGED, wordCount: 31 } }
function draftContentId(db, id = 11) { return db.prepare('SELECT content_id FROM drafts WHERE id=?').get(id).content_id }
function mutateSourceInput(field, input) {
  input.expectedSource[field] = typeof input.expectedSource[field] === 'number' ? input.expectedSource[field] + 1 : `${input.expectedSource[field]} stale`
}
function dispatch(definition, db, input) {
  switch (definition.method) {
    case 'CreateReview': return ReviewRepository.create(input, db)
    case 'ListReviews': return ReviewRepository.listByDraft(definition.id)
    case 'LatestReview': return ReviewRepository.getLatestByDraft(definition.id)
    case 'FullReview': return ReviewRepository.getFull(definition.id, db)
    case 'NextReviewIndex': return ReviewRepository.getNextIndex(definition.id)
    case 'CreateRevision': return RevisionRepository.create(input)
    case 'ReplacePending': return RevisionRepository.replacePending(input, db)
    case 'ListRevisions': return definition.pending ? RevisionRepository.getPending(definition.id) : RevisionRepository.listByDraft(definition.id)
    case 'FullRevision': return RevisionRepository.getFull(definition.id, db)
    case 'NextRevisionIndex': return RevisionRepository.getNextIndex(definition.id)
    case 'MergeRevision': return RevisionRepository.mergeIntoDraft(input, db)
    case 'MarkDiscarded': return RevisionRepository.markDiscarded(definition.id, db)
    case 'MarkMerged': return RevisionRepository.markMerged(input.id, input.mergedToDraftId, db)
    default: throw new Error(`unknown method ${definition.method}`)
  }
}

// Creation guards compare every frozen field; caller-provided indices must be
// ignored, allocation is MAX scoped to the source draft, not count or row ID.
for (const method of ['CreateReview', 'CreateRevision', 'ReplacePending']) {
  const prefix = method.replace(/[A-Z]/gu, (letter, offset) => `${offset ? '-' : ''}${letter.toLowerCase()}`)
  const history = method === 'CreateReview' ? reviewHistory : revisionHistory
  const fresh = (suffix, options = {}) => add(`${prefix}-${suffix}`, method, { makeInput: () => creationInput(method), ...options })
  fresh('matching-frozen-source')
  fresh('empty-output', { changeInput: input => { input.content = '' } })
  fresh('missing-source', { changeInput: input => { delete input.expectedSource } })
  fresh('null-source', { changeInput: input => { input.expectedSource = null } })
  fresh('missing-base-row', { changeInput: input => { input.baseDraftId = 999; input.expectedSource.id = 999 } })
  for (const field of ['id', 'chapterNumber', 'version', 'status', 'content']) fresh(`source-drift-${field}`, { seed: history, changeInput: input => mutateSourceInput(field, input) })
  fresh('max-scoped-allocation-ignore-passed-index', { seed: history, changeInput: input => { input.reviewIndex = 99; input.revisionIndex = 99 } })
  for (const status of ['revised', 'reviewed', 'finalized', 'archived']) fresh(`matching-${status}-source`, { seed: db => patch(db, 'drafts', 11, { status }), changeInput: input => { input.expectedSource.status = status } })
  if (method !== 'CreateReview') {
    fresh('review-fix-metadata', { seed: db => seedReview(db), changeInput: input => { input.revisionType = 'review-fix'; input.userPrompt = 'Author prompt 中文 😀\n<&>'; input.reviewSourceId = 301 } })
    fresh('nullable-optional-metadata', { changeInput: input => { input.userPrompt = null; input.reviewSourceId = null; input.wordCount = 0 } })
  }
}

for (const [method, seed] of [['ListReviews', reviewHistory], ['LatestReview', reviewHistory], ['NextReviewIndex', reviewHistory], ['ListRevisions', revisionHistory], ['NextRevisionIndex', revisionHistory]]) {
  add(`${method}-empty`, method, { id: 11 })
  add(`${method}-history-order-and-draft-isolation`, method, { id: 11, seed })
  add(`${method}-missing-base`, method, { id: 999, seed })
}
add('ListRevisions-pending-history-only', 'ListRevisions', { id: 11, pending: true, seed: revisionHistory })
add('ListRevisions-pending-empty', 'ListRevisions', { id: 11, pending: true })
add('ListRevisions-pending-other-draft', 'ListRevisions', { id: 22, pending: true, seed: revisionHistory })
for (const [method, seed, id] of [['FullReview', seedReview, 301], ['FullRevision', seedRevision, 501]]) {
  add(`${method}-missing`, method, { id: 999 })
  add(`${method}-frozen-source`, method, { id, seed: db => seed(db) })
  add(`${method}-empty-body`, method, { id, seed: db => seed(db, { body: '' }) })
  add(`${method}-historical-source-after-live-author-edit`, method, { id, seed: db => { seed(db); patch(db, 'contents', draftContentId(db), { body: 'Author changed live body' }); patch(db, 'drafts', 11, { status: 'reviewed', version: 4 }) } })
  add(`${method}-legacy-all-source-null`, method, { id, seed: db => { seed(db); patch(db, method === 'FullReview' ? 'reviews' : 'revisions', id, Object.fromEntries(Object.keys(sourceColumns()).map(key => [key, null]))) } })
  for (const field of Object.keys(sourceColumns())) add(`${method}-nullable-${field}`, method, { id, seed: db => { seed(db); patch(db, method === 'FullReview' ? 'reviews' : 'revisions', id, { [field]: null }) } })
}
add('LatestReview-latest-legacy-source-null', 'LatestReview', { id: 11, seed: db => { reviewHistory(db); patch(db, 'reviews', 301, { source_content: null }) } })
add('LatestReview-empty-body-is-not-missing', 'LatestReview', { id: 11, seed: db => seedReview(db, { body: '' }) })
add('FullRevision-nullable-metadata', 'FullRevision', { id: 501, seed: db => seedRevision(db, { user_prompt: null, review_source_id: null, merged_to_draft_id: null, word_count: null }) })
add('ListRevisions-nullable-metadata', 'ListRevisions', { id: 11, seed: db => seedRevision(db, { user_prompt: null, review_source_id: null, merged_to_draft_id: null, word_count: null }) })

for (const method of ['MarkDiscarded', 'MarkMerged']) {
  const mark = (suffix, options = {}) => add(`${method}-${suffix}`, method, { id: 501, makeInput: method === 'MarkMerged' ? () => ({ id: 501, mergedToDraftId: 11 }) : undefined, seed: db => seedRevision(db), ...options })
  mark('pending')
  mark('missing', { id: 999, makeInput: method === 'MarkMerged' ? () => ({ id: 999, mergedToDraftId: 11 }) : undefined })
  mark('already-discarded', { seed: db => seedRevision(db, { status: 'discarded' }) })
  mark('already-merged', { seed: db => seedRevision(db, { status: 'merged', merged_to_draft_id: 11 }) })
  mark('isolated-from-other-pending', { seed: revisionHistory })
}
add('MarkMerged-other-existing-target-is-legacy-metadata-only', 'MarkMerged', { makeInput: () => ({ id: 501, mergedToDraftId: 22 }), seed: db => seedRevision(db) })
add('ReplacePending-no-pending-history', 'ReplacePending', { makeInput: () => creationInput('ReplacePending'), seed: db => { seedRevision(db, { status: 'discarded', index: 6 }); seedRevision(db, { id: 505, index: 11, status: 'merged', merged_to_draft_id: 11 }) } })

// Merge receipts and precedence are captured from the original operation, never
// hand-authored expectations. Replay setup calls the original merge first.
const merge = (suffix, options = {}) => add(`merge-${suffix}`, 'MergeRevision', { makeInput: mergeInput, seed: db => seedRevision(db), ...options })
merge('exact-receipt-and-atomic-state')
merge('unrelated-history-unchanged', { seed: revisionHistory })
for (const status of ['revised', 'reviewed']) merge(`editable-${status}`, { seed: db => { patch(db, 'drafts', 11, { status }); seedRevision(db, { source: { ...sourceDraft(), status } }) } })
merge('same-body-still-updates-status-and-count', { changeInput: input => { input.mergedContent = BODY; input.wordCount = 99 } })
merge('empty-merged-body', { changeInput: input => { input.mergedContent = ''; input.wordCount = 0 } })
merge('missing-draft-precedes-missing-revision', { changeInput: input => { input.targetDraftId = 999; input.revisionId = 999 } })
merge('missing-revision', { changeInput: input => { input.revisionId = 999 } })
merge('target-mismatch', { changeInput: input => { input.targetDraftId = 22; input.expectedDraftContent = OTHER_BODY } })
merge('target-mismatch-precedes-discarded', { seed: db => seedRevision(db, { status: 'discarded' }), changeInput: input => { input.targetDraftId = 22 } })
merge('target-mismatch-precedes-merged', { seed: db => seedRevision(db, { status: 'merged', merged_to_draft_id: 11 }), changeInput: input => { input.targetDraftId = 22 } })
merge('discarded-precedes-noneditable-and-stale', { seed: db => { seedRevision(db, { status: 'discarded' }); patch(db, 'drafts', 11, { status: 'finalized' }) }, changeInput: input => { input.expectedDraftContent = 'stale' } })
for (const status of ['finalized', 'archived']) merge(`noneditable-${status}-precedes-stale`, { seed: db => { seedRevision(db); patch(db, 'drafts', 11, { status }) }, changeInput: input => { input.expectedDraftContent = 'stale' } })
merge('stale-target-precedes-missing-source', { seed: db => { seedRevision(db); patch(db, 'revisions', 501, { source_content: null }) }, changeInput: input => { input.expectedDraftContent = 'stale' } })
merge('stale-expected-body', { changeInput: input => { input.expectedDraftContent = 'stale' } })
for (const field of Object.keys(sourceColumns())) merge(`missing-${field}`, { seed: db => { seedRevision(db); patch(db, 'revisions', 501, { [field]: null }) } })
merge('all-source-columns-null', { seed: db => { seedRevision(db); patch(db, 'revisions', 501, Object.fromEntries(Object.keys(sourceColumns()).map(key => [key, null]))) } })
for (const [field, value] of [['chapter_number', 9], ['version', 4], ['status', 'reviewed']]) merge(`live-source-drift-${field}`, { seed: db => { seedRevision(db); patch(db, 'drafts', 11, { [field]: value }) } })
merge('live-body-drift-expected-updated-still-source-mismatch', { seed: db => { seedRevision(db); patch(db, 'contents', draftContentId(db), { body: 'New author body' }) }, changeInput: input => { input.expectedDraftContent = 'New author body' } })
merge('word-count-only-live-drift-is-allowed', { seed: db => { seedRevision(db); patch(db, 'drafts', 11, { word_count: 900 }) } })
function mergedSetup(db) {
  seedRevision(db)
  RevisionRepository.mergeIntoDraft(mergeInput(), db)
}
merge('replay-idempotent', { seed: mergedSetup })
merge('replay-ignores-old-expected-body', { seed: mergedSetup, changeInput: input => { input.expectedDraftContent = 'not current body' } })
merge('replay-after-author-body-edit-rejected', { seed: db => { mergedSetup(db); patch(db, 'contents', draftContentId(db), { body: 'Post-merge author edit' }) } })
merge('replay-same-body-different-request-count-rejected', { seed: mergedSetup, changeInput: input => { input.wordCount += 1 } })
merge('replay-same-body-different-stored-count-rejected', { seed: db => { mergedSetup(db); patch(db, 'drafts', 11, { word_count: 32 }) } })
merge('replay-same-body-different-status-rejected', { seed: db => { mergedSetup(db); patch(db, 'drafts', 11, { status: 'reviewed' }) } })
merge('replay-different-merged-request-rejected', { seed: mergedSetup, changeInput: input => { input.mergedContent = 'Another proposed merge' } })
merge('replay-wrong-merged-to-rejected', { seed: db => { mergedSetup(db); patch(db, 'revisions', 501, { merged_to_draft_id: 22 }) } })
merge('replay-chapter-version-are-current-readback', { seed: db => { mergedSetup(db); patch(db, 'drafts', 11, { chapter_number: 9, version: 4 }) } })
merge('replay-does-not-revalidate-legacy-source', { seed: db => { mergedSetup(db); patch(db, 'revisions', 501, { source_content: null }) } })

// SQLite storage-class boundaries: strict JS source comparisons must not turn a
// BLOB into matching text, and read projections retain REAL numbers / Buffers.
for (const method of ['CreateReview', 'CreateRevision', 'ReplacePending']) add(`${method}-live-source-body-blob-is-not-string`, method, {
  makeInput: () => creationInput(method),
  seed: db => patch(db, 'contents', draftContentId(db), { body: Buffer.from(BODY) }),
})
for (const [method, seed, table, id] of [['FullReview', seedReview, 'reviews', 301], ['FullRevision', seedRevision, 'revisions', 501]]) {
  add(`${method}-body-blob-preserves-storage-class`, method, { id, seed: db => seed(db, { body: Buffer.from('Blob body 中文 😀') }) })
  add(`${method}-source-content-blob-is-unavailable`, method, { id, seed: db => { seed(db); patch(db, table, id, { source_content: Buffer.from(BODY) }) } })
  add(`${method}-source-status-blob-is-unavailable`, method, { id, seed: db => { seed(db); patch(db, table, id, { source_draft_status: Buffer.from('draft') }) } })
  for (const field of ['source_draft_chapter_number', 'source_draft_version']) add(`${method}-source-real-${field}`, method, { id, seed: db => { seed(db); patch(db, table, id, { [field]: 3.5 }) } })
}
add('LatestReview-body-blob-preserves-storage-class', 'LatestReview', { id: 11, seed: db => seedReview(db, { body: Buffer.from('Latest report blob') }) })
add('FullRevision-user-prompt-blob-preserves-storage-class', 'FullRevision', { id: 501, seed: db => seedRevision(db, { user_prompt: Buffer.from('Prompt blob 中文') }) })
add('ListRevisions-user-prompt-blob-preserves-storage-class', 'ListRevisions', { id: 11, seed: db => seedRevision(db, { user_prompt: Buffer.from('Prompt blob 中文') }) })
merge('target-body-blob-does-not-match-string', { seed: db => { seedRevision(db); patch(db, 'contents', draftContentId(db), { body: Buffer.from(BODY) }) } })
merge('frozen-source-blob-does-not-match-string', { seed: db => { seedRevision(db); patch(db, 'revisions', 501, { source_content: Buffer.from(BODY) }) } })
merge('replay-target-body-blob-rejected', { seed: db => { mergedSetup(db); patch(db, 'contents', draftContentId(db), { body: Buffer.from(MERGED) }) } })

for (const method of ['CreateReview', 'CreateRevision', 'ReplacePending']) {
  add(`${method}-partial-source-missing-version`, method, { makeInput: () => creationInput(method), changeInput: input => { delete input.expectedSource.version } })
  add(`${method}-partial-source-null-status`, method, { makeInput: () => creationInput(method), changeInput: input => { input.expectedSource.status = null } })
}
assert.equal(definitions.length, 162, 'keep the reviewed public-method fixture count stable')
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
      // The baseline contains only a public synthetic contents row, not author
      // data. Keep it, and add independently identifiable chapter fixtures.
      assert.equal(db.prepare('SELECT COUNT(*) AS n FROM drafts').get().n, 0)
      assert.equal(db.prepare('SELECT COUNT(*) AS n FROM reviews').get().n, 0)
      assert.equal(db.prepare('SELECT COUNT(*) AS n FROM revisions').get().n, 0)
      insert(db, 'drafts', { id: 11, chapter_number: 7, version: 3, status: 'draft', content_id: content(db, BODY), word_count: 19 })
      insert(db, 'drafts', { id: 22, chapter_number: 8, version: 2, status: 'reviewed', content_id: content(db, OTHER_BODY), word_count: 8 })
      definition.seed?.(db)
      const input = definition.makeInput?.()
      definition.changeInput?.(input)
      assert.deepEqual(db.pragma('foreign_key_check'), [], 'fixtures must not contain dangling SQLite relationships')
      // Serialize ALL preoperation bytes, including AUTOINCREMENT sequences,
      // schema/defaults and original timestamps. Native Go starts from these.
      before = db.serialize()
      let result = null
      let error
      try { result = dispatch(definition, db, input) ?? null } catch (failure) {
        assert(failure instanceof Error)
        error = failure.message
      }
      const after = normalize(snapshot(db))
      assert.deepEqual(db.pragma('foreign_key_check'), [])
      cases.push({ name: definition.name, database: filename, method: definition.method,
        ...(input === undefined ? {} : { input }), ...(definition.id === undefined ? {} : { id: definition.id }),
        ...(definition.pending === undefined ? {} : { pending: definition.pending }),
        result: normalize(result), ...(error === undefined ? {} : { error }), after })
    } finally {
      globalThis[globalKey] = null
      db.close()
      // Restore only after closing SQLite; the published file is the exact
      // seeded BEFORE image, never an expected post-operation database.
      if (before) writeFileSync(filename, before, { mode: 0o600 })
    }
  }
  writeFileSync(target, JSON.stringify({ formatVersion: 1, cases }) + '\n', { flag: 'wx', mode: 0o600 })
} finally {
  delete globalThis[globalKey]
  hooks.deregister()
}
// Scope exclusion is deliberate: native Go fails closed when review-cycle
// ownership is required. Bound cycle operations are NOT misrepresented as
// matching unsupported errors. No structurally invalid cycle is seeded; all
// unbound merge/discard/replace calls still execute real review-cycle functions.
console.log(`Generated ${cases.length} original review/revision contracts from private synthetic baseline-v7 clones; excludes review-cycle-owned revisions and nonempty cycle graphs`)
