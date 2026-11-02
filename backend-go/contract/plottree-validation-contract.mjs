import assert from 'node:assert/strict'
import { existsSync, mkdirSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import './register-typescript.mjs'

// GitHub-only, synthetic, database-free oracle. Execute the ACTUAL original
// shared module with its actual writing-language import, not a copied validator.
assert.equal(process.env.GITHUB_ACTIONS, 'true', 'contract generation is GitHub-only')
const base = process.env.RUNNER_TEMP
const target = process.env.GO_PLOTVALIDATION_CONTRACT
assert(base && target && path.isAbsolute(base) && path.isAbsolute(target))
assert.equal(target, path.join(base, 'novel-go-plotvalidation', 'contract.json'), 'use the exact disposable plot-validation path')
const directory = path.dirname(target)
assert(!existsSync(directory), 'never reuse a preexisting fixture directory')
const { assertStoredPlotTreeSnapshot, assertPlotTreeSnapshot, MAX_PLOT_TREE_CHAPTER_NUMBER } = await import('../../src/shared/plot-tree')
assert.equal(MAX_PLOT_TREE_CHAPTER_NUMBER, 4294967295)

const REVISION = 'a1'.repeat(32)
const clone = value => JSON.parse(JSON.stringify(value))
const reference = () => ({ type: 'blueprint', chapterNumber: 5 })
const event = () => ({ status: 'planned', chapterNumber: 5, summary: '  林舟发现星钥 😀  ', sources: [reference()] })
const track = () => ({ id: 'main', title: '  星钥主线  ', role: 'main', startChapter: 3, endChapter: 9, summary: '  主线摘要 😀  ', events: [event()] })
const snapshot = () => ({ version: 1, generatedAt: ' 2026-01-02T03:04:05.678Z ', writingLanguage: 'zh-CN', sourceRevision: REVISION, tracks: [track()] })
const sources = () => ({ writingLanguage: 'zh-CN', sourceRevision: REVISION, synopsis: { content: '合成故事' }, snapshot: null,
  blueprints: [{ chapterNumber: 3, title: '开篇', purpose: '', keyEvents: '' }, { chapterNumber: 5, title: '星钥', purpose: '', keyEvents: '' }, { chapterNumber: 9, title: '收束', purpose: '', keyEvents: '' }],
  finalizedChapters: [{ draftId: 11, chapterNumber: 5, title: '定稿标题', summary: '冻结摘要' }],
  narrativeThreads: [{ id: 7, title: '星钥之谜', type: '伏笔', targetStartChapter: 3, targetEndChapter: 9, authorIntent: '逐步揭示', status: 'planned',
    events: [{ id: 17, chapterNumber: 5, type: 'planted', evidence: '发现星钥', reason: '初见线索' }] }] })
const definitions = []
function add(name, method = 'stored', change = () => {}, makeValue = snapshot, makeSources = sources) {
  definitions.push({ name, method, change, makeValue, makeSources })
}
const t = value => value.tracks[0]
const e = value => t(value).events[0]
const r = value => e(value).sources[0]
const occurred = value => { e(value).status = 'occurred'; e(value).sources = [{ type: 'finalized-chapter', draftId: 11, chapterNumber: 5 }] }
const planRef = value => { e(value).sources = [{ type: 'narrative-thread', planId: 7 }] }
const threadEventRef = value => { e(value).status = 'occurred'; e(value).sources = [{ type: 'narrative-thread', planId: 7, eventId: 17, chapterNumber: 5 }] }

add('stored-basic-normalized')
add('validate-basic', 'validate')
for (const value of [null, [], '', 1, true, {}]) add(`root-${JSON.stringify(value)}`, 'stored', () => {}, () => value)
for (const value of [null, '1', 0, 2, true, 1.5]) add(`version-${JSON.stringify(value)}`, 'stored', input => { input.version = value })
for (const value of [null, '', 'zh', 'en', 'ZH-CN', ['zh-CN']]) add(`language-${JSON.stringify(value)}`, 'stored', input => { input.writingLanguage = value })
add('language-en-US', 'stored', input => { input.writingLanguage = 'en-US' })
add('version-before-time', 'stored', input => { input.version = 2; input.generatedAt = '' })
for (const value of [null, 1, true, '', ' \t\n\uFEFF', 'not-a-date', '2024-13-01', '2024-01-32', '2024-01-01T25:00:00Z', '2024-01-01T24:00:01Z']) add(`date-invalid-${JSON.stringify(value)}`, 'stored', input => { input.generatedAt = value })
for (const value of ['2024', '2024-02', '2024-02-29', '2023-02-29', '2024-02-30', '2024-01-01T24:00:00Z', '2024-01-01T12:30', '2024-01-01T12:30:45.123456+08:00', '+010000-01-01T00:00:00Z', 'Thu, 01 Jan 1970 00:00:00 GMT', 'January 2, 2026', '2026/1/2', '12/31/2026', '01-02-03', '0', '12', '49', '50', '\uFEFF2026-01-02\uFEFF']) add(`date-valid-${JSON.stringify(value)}`, 'stored', input => { input.generatedAt = value })
for (const value of [null, '', 'A'.repeat(64), 'a'.repeat(63), 'a'.repeat(65), 'g'.repeat(64), ' ' + REVISION, 1]) add(`revision-invalid-${JSON.stringify(value)}`, 'stored', input => { input.sourceRevision = value })
add('revision-omitted', 'stored', input => { delete input.sourceRevision })
for (const value of [null, {}, [], 'track', [null], [[]]]) add(`tracks-invalid-${JSON.stringify(value)}`, 'stored', input => { input.tracks = value })
for (const value of [null, '', 'MAIN', 'side', true, 1, {}]) add(`track-role-${JSON.stringify(value)}`, 'stored', input => { t(input).role = value })
add('track-role-single-array-original-coercion', 'stored', input => { t(input).role = ['main'] })
add('track-role-nested-array-original-coercion', 'stored', input => { t(input).role = [[['main']]] })
for (const field of ['id', 'title', 'summary']) {
  for (const value of [null, 1, '', ' \t\n\uFEFF']) add(`track-${field}-invalid-${JSON.stringify(value)}`, 'stored', input => { t(input)[field] = value })
}
add('track-text-feff-trim-nel-preserve', 'stored', input => { t(input).id = '\uFEFF main \uFEFF'; t(input).title = '\uFEFF\u0085标题\u0085\uFEFF'; t(input).summary = '\u0085' })
add('track-text-no-invented-length-cap', 'stored', input => { t(input).title = '😀'.repeat(1025); t(input).summary = '文'.repeat(8193) })
for (const field of ['startChapter', 'endChapter']) {
  for (const value of [null, true, '5', 0, -1, 1.5, 4294967296, Number.MAX_SAFE_INTEGER, 1e100]) add(`track-${field}-${JSON.stringify(value)}`, 'stored', input => { t(input)[field] = value })
}
add('range-inverted', 'stored', input => { t(input).startChapter = 6; t(input).endChapter = 5 })
add('range-ceiling-inclusive', 'stored', input => { t(input).startChapter = t(input).endChapter = e(input).chapterNumber = r(input).chapterNumber = MAX_PLOT_TREE_CHAPTER_NUMBER })
for (const value of [null, {}, [], 'events', [null], [[]], [{}]]) add(`events-invalid-${JSON.stringify(value)}`, 'stored', input => { t(input).events = value })
for (const value of [null, '', 'PLANNED', 'resolved', true, ['planned']]) add(`event-status-${JSON.stringify(value)}`, 'stored', input => { e(input).status = value })
for (const value of [null, [], {}, 'refs']) add(`event-sources-${JSON.stringify(value)}`, 'stored', input => { e(input).sources = value })
for (const value of [null, 0, '5', 1.5, 4294967296, 1e100]) add(`event-chapter-${JSON.stringify(value)}`, 'stored', input => { e(input).chapterNumber = value })
for (const value of [null, '', ' \uFEFF\t', 1]) add(`event-summary-${JSON.stringify(value)}`, 'stored', input => { e(input).summary = value })
add('event-chapter-before-ref', 'stored', input => { e(input).chapterNumber = 0; e(input).sources = [null] })
add('event-source-before-chapter', 'stored', input => { e(input).chapterNumber = 0; e(input).sources = [] })
add('event-ref-before-summary', 'stored', input => { e(input).summary = ''; e(input).sources = [null] })
add('event-mismatch-before-summary', 'stored', input => { e(input).summary = ''; e(input).status = 'occurred' })
add('event-summary-before-track-bounds', 'stored', input => { e(input).summary = ''; t(input).startChapter = 6 })
add('event-outside-track-low', 'stored', input => { t(input).startChapter = 6 })
add('event-outside-track-high', 'stored', input => { t(input).endChapter = 4 })
add('event-track-inclusive-ends', 'stored', input => { t(input).startChapter = t(input).endChapter = 5 })
for (const value of [null, [], '', 1, {}, { type: 'other' }, { type: ['blueprint'], chapterNumber: 5 }]) add(`reference-invalid-${JSON.stringify(value)}`, 'stored', input => { e(input).sources = [value] })
for (const value of [null, 0, '5', 1.25, 4294967296]) add(`blueprint-chapter-${JSON.stringify(value)}`, 'stored', input => { r(input).chapterNumber = value })
add('blueprint-wrong-chapter', 'stored', input => { r(input).chapterNumber = 6 })
add('blueprint-wrong-status', 'stored', input => { e(input).status = 'occurred' })
add('finalized-valid', 'stored', occurred)
for (const value of [null, '11', 0, -1, 1.5, Number.MAX_SAFE_INTEGER + 1, 1e100]) add(`finalized-id-${JSON.stringify(value)}`, 'stored', input => { occurred(input); r(input).draftId = value })
add('finalized-max-safe-id', 'stored', input => { occurred(input); r(input).draftId = Number.MAX_SAFE_INTEGER })
add('finalized-wrong-chapter', 'stored', input => { occurred(input); r(input).chapterNumber = 6 })
add('finalized-wrong-status', 'stored', input => { occurred(input); e(input).status = 'planned' })
add('finalized-id-before-chapter', 'stored', input => { occurred(input); r(input).draftId = 0; r(input).chapterNumber = 0 })
add('thread-plan-valid', 'stored', planRef)
add('thread-event-valid', 'stored', threadEventRef)
for (const field of ['planId', 'eventId']) {
  for (const value of [null, '7', 0, -1, 1.5, Number.MAX_SAFE_INTEGER + 1, 1e100]) add(`thread-${field}-${JSON.stringify(value)}`, 'stored', input => { threadEventRef(input); r(input)[field] = value })
}
add('thread-optional-chapter-null', 'stored', input => { planRef(input); r(input).chapterNumber = null })
add('thread-optional-chapter-only', 'stored', input => { planRef(input); r(input).chapterNumber = 5 })
add('thread-optional-event-only', 'stored', input => { planRef(input); r(input).eventId = 17 })
add('thread-optional-event-null', 'stored', input => { planRef(input); r(input).eventId = null })
add('thread-event-chapter-zero-before-pair', 'stored', input => { planRef(input); r(input).chapterNumber = 0 })
add('thread-plan-occurred-rejected', 'stored', input => { planRef(input); e(input).status = 'occurred' })
add('thread-event-planned-rejected', 'stored', input => { threadEventRef(input); e(input).status = 'planned' })
add('thread-event-chapter-mismatch', 'stored', input => { threadEventRef(input); r(input).chapterNumber = 6 })
add('mixed-all-support-occurred', 'stored', input => { occurred(input); e(input).sources.push({ type: 'narrative-thread', planId: 7, eventId: 17, chapterNumber: 5 }) })
add('mixed-one-mismatched-source', 'stored', input => { e(input).sources.push({ type: 'finalized-chapter', draftId: 11, chapterNumber: 5 }) })
add('all-refs-parse-before-any-support-check', 'stored', input => { e(input).status = 'occurred'; e(input).sources.push({ type: 'narrative-thread', planId: 0 }) })
add('extra-keys-stripped-at-all-levels', 'stored', input => { input.extra = true; t(input).extra = 'x'; e(input).extra = 1; r(input).extra = {}; r(input).draftId = null })
add('input-preserves-internal-whitespace', 'stored', input => { t(input).summary = '\uFEFF甲\t  乙\n😀\uFEFF'; e(input).summary = '  甲\u00A0乙  ' })
add('duplicate-id-after-trim', 'stored', input => { input.tracks.push({ ...track(), id: ' main ' }) })
add('multiple-main-allowed', 'stored', input => { input.tracks.push({ ...track(), id: 'main2' }) })
add('subplot-valid-parent', 'stored', input => { input.tracks.push({ ...track(), id: 'sub', role: 'subplot', parentTrackId: ' main ' }) })
add('subplot-before-parent', 'stored', input => { input.tracks.unshift({ ...track(), id: 'sub', role: 'subplot', parentTrackId: 'main' }) })
add('subplot-range-need-not-fit-parent', 'stored', input => { input.tracks.push({ ...track(), id: 'sub', role: 'subplot', parentTrackId: 'main', startChapter: 1, endChapter: 100 }) })
add('main-has-parent', 'stored', input => { t(input).parentTrackId = 'main' })
add('subplot-missing-parent', 'stored', input => { t(input).role = 'subplot' })
add('subplot-missing-target', 'stored', input => { t(input).role = 'subplot'; t(input).parentTrackId = 'missing' })
add('subplot-self-cycle', 'stored', input => { t(input).role = 'subplot'; t(input).parentTrackId = 'main' })
add('subplot-pair-cycle', 'stored', input => { t(input).role = 'subplot'; t(input).parentTrackId = 'sub'; input.tracks.push({ ...track(), id: 'sub', role: 'subplot', parentTrackId: 'main' }) })
for (const value of [null, '', ' \uFEFF ', 1]) add(`parent-invalid-${JSON.stringify(value)}`, 'stored', input => { t(input).parentTrackId = value })
add('event-error-before-parent-error', 'stored', input => { t(input).parentTrackId = null; e(input).summary = '' })
add('later-track-shape-before-duplicate-id', 'stored', input => { input.tracks.push(track(), null) })

// Current source authority is separate from immutable stored-shape authority.
add('validate-language-mismatch', 'validate', (input, source) => { source.writingLanguage = 'en-US' })
add('validate-language-before-revision', 'validate', (input, source) => { source.writingLanguage = 'en-US'; source.sourceRevision = 'b'.repeat(64) })
add('validate-revision-mismatch', 'validate', (input, source) => { source.sourceRevision = 'b'.repeat(64) })
add('validate-revision-missing-from-snapshot', 'validate', input => { delete input.sourceRevision })
add('validate-revision-both-omitted-original-equality', 'validate', (input, source) => { delete input.sourceRevision; delete source.sourceRevision })
add('validate-missing-blueprint', 'validate', (input, source) => { source.blueprints = [] })
add('validate-blueprint-exact-number-not-string', 'validate', (input, source) => { source.blueprints[1].chapterNumber = '5' })
add('validate-finalized', 'validate', occurred)
add('validate-finalized-id-exact', 'validate', input => { occurred(input); r(input).draftId = 12 })
add('validate-finalized-pair-exact', 'validate', (input, source) => { occurred(input); source.finalizedChapters[0].chapterNumber = 6 })
add('validate-finalized-id-number-not-string', 'validate', (input, source) => { occurred(input); source.finalizedChapters[0].draftId = '11' })
add('validate-plan', 'validate', planRef)
add('validate-plan-missing', 'validate', (input, source) => { planRef(input); source.narrativeThreads = [] })
add('validate-plan-id-exact', 'validate', (input, source) => { planRef(input); source.narrativeThreads[0].id = '7' })
add('validate-plan-first-matching-id-is-authority', 'validate', (input, source) => { planRef(input); source.narrativeThreads.unshift({ ...clone(source.narrativeThreads[0]), targetStartChapter: 6 }) })
add('validate-plan-event', 'validate', threadEventRef)
add('validate-event-id-exact', 'validate', input => { threadEventRef(input); r(input).eventId = 18 })
add('validate-event-chapter-pair-exact', 'validate', (input, source) => { threadEventRef(input); source.narrativeThreads[0].events[0].chapterNumber = 6 })
add('validate-event-id-number-not-string', 'validate', (input, source) => { threadEventRef(input); source.narrativeThreads[0].events[0].id = '17' })
add('validate-event-can-be-outside-plan-range', 'validate', (input, source) => { threadEventRef(input); source.narrativeThreads[0].targetStartChapter = 6 })
add('validate-plan-start-inclusive', 'validate', input => { planRef(input); e(input).chapterNumber = 3 })
add('validate-plan-end-inclusive', 'validate', input => { planRef(input); e(input).chapterNumber = 9 })
add('validate-plan-before-target', 'validate', (input, source) => { planRef(input); source.narrativeThreads[0].targetStartChapter = 6 })
add('validate-plan-after-target', 'validate', (input, source) => { planRef(input); source.narrativeThreads[0].targetEndChapter = 4 })
add('validate-plan-range-comparison-original-coercion', 'validate', (input, source) => { planRef(input); source.narrativeThreads[0].targetStartChapter = '3'; source.narrativeThreads[0].targetEndChapter = '9' })
add('validate-source-existence-before-current-bounds', 'validate', (input, source) => { source.blueprints = []; source.finalizedChapters = []; source.narrativeThreads = []; t(input).endChapter = 99 })
add('validate-all-refs-exist-before-plan-support', 'validate', (input, source) => { planRef(input); source.narrativeThreads[0].targetStartChapter = 6; e(input).sources.push({ type: 'blueprint', chapterNumber: 5 }); source.blueprints = [] })
add('validate-current-bounds-reject-wide-track', 'validate', input => { t(input).endChapter = 10 })
add('validate-current-bounds-reject-low-track', 'validate', input => { t(input).startChapter = 2 })
add('stored-immutable-bounds-ignore-current-range', 'stored', input => { t(input).startChapter = 1; t(input).endChapter = MAX_PLOT_TREE_CHAPTER_NUMBER })
add('stored-retains-nonexistent-historical-references', 'stored', input => { occurred(input); r(input).draftId = Number.MAX_SAFE_INTEGER })
add('validate-no-contentful-current-source', 'validate', (input, source) => { source.finalizedChapters = []; source.narrativeThreads = []; for (const b of source.blueprints) { b.title = b.purpose = b.keyEvents = '\uFEFF \t' } })
add('validate-synopsis-alone-does-not-establish-bounds', 'validate', (input, source) => { source.finalizedChapters = []; source.narrativeThreads = []; source.blueprints = [{ chapterNumber: 5, title: '', purpose: '', keyEvents: '' }] })
add('validate-blueprint-purpose-establishes-bounds', 'validate', (input, source) => { source.finalizedChapters = []; source.narrativeThreads = []; source.blueprints = [{ chapterNumber: 5, title: '', purpose: '目的', keyEvents: '' }]; t(input).startChapter = t(input).endChapter = 5 })
add('validate-blueprint-keyevents-establishes-bounds', 'validate', (input, source) => { source.finalizedChapters = []; source.narrativeThreads = []; source.blueprints = [{ chapterNumber: 5, title: '', purpose: '', keyEvents: '😀' }]; t(input).startChapter = t(input).endChapter = 5 })
add('validate-finalized-summary-establishes-bounds', 'validate', (input, source) => { occurred(input); source.blueprints = []; source.narrativeThreads = []; source.finalizedChapters[0].title = ''; t(input).startChapter = t(input).endChapter = 5 })
add('validate-thread-all-three-text-fields-needed-for-bounds', 'validate', (input, source) => { planRef(input); source.blueprints = []; source.finalizedChapters = []; source.narrativeThreads[0].authorIntent = ' \uFEFF ' })
add('validate-thread-invalid-range-does-not-establish-bounds', 'validate', (input, source) => { threadEventRef(input); source.blueprints = []; source.finalizedChapters = []; source.narrativeThreads[0].targetStartChapter = 10 })
add('validate-valid-confirmation-expands-bounds-beyond-plan', 'validate', (input, source) => { threadEventRef(input); e(input).chapterNumber = r(input).chapterNumber = 20; t(input).endChapter = 20; source.narrativeThreads[0].events[0].chapterNumber = 20 })
for (const field of ['id', 'chapterNumber', 'type', 'evidence', 'reason']) {
  add(`validate-invalid-confirmation-${field}-cannot-expand-bounds`, 'validate', (input, source) => {
    t(input).endChapter = 20
    const confirmation = { id: 19, chapterNumber: 20, type: 'resolved', evidence: '证据', reason: '原因' }
    confirmation[field] = field === 'id' || field === 'chapterNumber' ? 0 : ''
    source.narrativeThreads[0].events.push(confirmation)
  })
}
for (const type of ['planted', 'progressing', 'resolved', 'abandoned']) add(`validate-confirmation-${type}-expands-bounds`, 'validate', (input, source) => { t(input).endChapter = 20; source.narrativeThreads[0].events.push({ id: 19, chapterNumber: 20, type, evidence: '证据', reason: '原因' }) })
add('validate-invalid-blueprint-chapter-cannot-expand-bounds', 'validate', (input, source) => { t(input).endChapter = 20; source.blueprints.push({ chapterNumber: '20', title: '标题' }) })
add('validate-invalid-finalized-id-cannot-expand-bounds', 'validate', (input, source) => { t(input).endChapter = 20; source.finalizedChapters.push({ draftId: 0, chapterNumber: 20, title: '标题' }) })
add('validate-thread-status-not-a-bounds-gate', 'validate', (input, source) => { planRef(input); source.blueprints = []; source.finalizedChapters = []; source.narrativeThreads[0].status = 'anything' })
add('validate-reference-existence-does-not-demand-content', 'validate', (input, source) => { source.blueprints[1].title = source.blueprints[1].purpose = source.blueprints[1].keyEvents = '' })
add('validate-nel-counts-as-source-content', 'validate', (input, source) => { source.finalizedChapters = []; source.narrativeThreads = []; source.blueprints = [{ chapterNumber: 5, title: '\u0085' }]; t(input).startChapter = t(input).endChapter = 5 })
add('validate-non-array-bounds-lists-not-used', 'validate', (input, source) => { source.finalizedChapters = null; source.narrativeThreads = {}; t(input).startChapter = 3; t(input).endChapter = 9 })
add('stored-many-main-tracks-no-invented-cap', 'stored', input => { input.tracks = Array.from({ length: 257 }, (_, index) => ({ ...track(), id: `track-${index}` })) })
add('stored-many-events-no-invented-cap', 'stored', input => { t(input).events = Array.from({ length: 1025 }, event) })

function deepFreeze(value) {
  if (value !== null && typeof value === 'object') { for (const child of Object.values(value)) deepFreeze(child); Object.freeze(value) }
  return value
}
const cases = []
const names = new Set()
for (const definition of definitions) {
  assert(!names.has(definition.name), `duplicate case ${definition.name}`)
  names.add(definition.name)
  const value = definition.makeValue()
  const source = definition.makeSources()
  definition.change(value, source)
  const beforeValue = clone(value)
  const beforeSources = clone(source)
  deepFreeze(value)
  deepFreeze(source)
  let result = null
  let error
  try { result = definition.method === 'stored' ? assertStoredPlotTreeSnapshot(value) : assertPlotTreeSnapshot(value, source) }
  catch (failure) { error = failure instanceof Error ? failure.message : String(failure) }
  assert.deepEqual(value, beforeValue, `${definition.name}: original mutated input`)
  assert.deepEqual(source, beforeSources, `${definition.name}: original mutated sources`)
  cases.push({ name: definition.name, method: definition.method, value, ...(definition.method === 'validate' ? { sources: source } : {}), result, ...(error === undefined ? {} : { error }) })
}
assert(cases.length >= 80, 'full structural/semantic/immutability/bounds coverage required')
mkdirSync(directory, { recursive: true, mode: 0o700 })
writeFileSync(target, JSON.stringify({ formatVersion: 1, cases }), { flag: 'wx', mode: 0o600 })
console.log(`Wrote ${cases.length} original shared plot-tree validator cases to ${target}`)
