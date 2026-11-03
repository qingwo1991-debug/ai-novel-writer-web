import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { createRequire } from 'node:module'
import fs from 'node:fs'
import path from 'node:path'

import { initializeLegacyBaselineSchema } from '../../electron/migrations/baseline-schema.ts'
import { M01_GENERATION_SQL } from '../../electron/migrations/m01-generation-runs.ts'
import { applyM03ReviewCycle, canonicalM03FindingSetHash, verifyM03ReviewCycle } from '../../electron/migrations/m03-review-cycle.ts'
import { buildReviewGenerationReport } from '../../src/shared/review-generation-report.ts'

// Original TypeScript is the oracle. Every database is synthetic, created only
// on an isolated GitHub runner, and retained solely for Go's read-only tests.
// This baseline+M01+M03 schema probes the M03 verifier in isolation; it is not
// a recognized full v4 project and cannot admit author data.
assert.equal(process.env.GITHUB_ACTIONS, 'true')
assert.equal(process.platform, 'linux')
const runnerTemp = process.env.RUNNER_TEMP
const output = process.env.GO_M03_NOTGENERATED_CONTRACT
assert(runnerTemp && output && path.isAbsolute(runnerTemp) && path.isAbsolute(output))
const relativeOutput = path.relative(runnerTemp, output)
assert(relativeOutput && relativeOutput !== '..' && !relativeOutput.startsWith(`..${path.sep}`) && !path.isAbsolute(relativeOutput))
fs.mkdirSync(path.dirname(output), { recursive: true, mode: 0o700 })
const directory = fs.mkdtempSync(path.join(runnerTemp, 'novel-go-m03-notgenerated-'))
fs.chmodSync(directory, 0o700)
const Database = createRequire(import.meta.url)('better-sqlite3')
const hash = value => createHash('sha256').update(value).digest('hex')

function seedReview(db) {
  const source = '甲推开门，雨水顺着袖口落下。'
  const artifactText = JSON.stringify({ summary: '发现一处连续性问题。', items: [
    { category: 'continuity', severity: 'warning', description: '门闩状态需确认。', quote: '甲推开门' },
  ] })
  const report = buildReviewGenerationReport({ content: artifactText, sourceContent: source,
    frozenGoals: { chapterNumber: 1, coverage: 'not_configured', items: [] },
    writingLanguage: 'zh-CN', uiLocale: 'zh-CN', preflightFindings: [], reportVersion: 1 })
  const reviewBody = JSON.stringify(report, null, 2)
  const sourceContentId = Number(db.prepare('INSERT INTO contents(body) VALUES(?)').run(source).lastInsertRowid)
  const draftId = Number(db.prepare("INSERT INTO drafts(chapter_number,version,status,content_id) VALUES(1,1,'draft',?)")
    .run(sourceContentId).lastInsertRowid)
  const reviewContentId = Number(db.prepare('INSERT INTO contents(body) VALUES(?)').run(reviewBody).lastInsertRowid)
  const reviewId = Number(db.prepare(`INSERT INTO reviews(base_draft_id,review_index,source_draft_chapter_number,
    source_draft_version,source_draft_status,source_content,content_id) VALUES(?,1,1,1,'draft',?,?)`)
    .run(draftId, source, reviewContentId).lastInsertRowid)

  const rootId = 'root-not-generated'
  const attemptId = 'attempt-review'
  const operation = 'review-chapter'
  const context = { version: 1, operation, sourceHash: hash(source),
    source: { id: draftId, chapterNumber: 1, version: 1, status: 'draft', content: source },
    config: {}, writingLanguage: 'zh-CN', uiLocale: 'zh-CN', authorInputs: [], characterStates: '（暂无）',
    worldbuilding: '', history: [], blueprints: [],
    frozenGoals: { chapterNumber: 1, coverage: 'not_configured', items: [] }, preflightFindings: [] }
  const fingerprint = Object.fromEntries(['chapterBriefHash', 'authorGuidanceHash', 'dependencyHash',
    'contextSnapshotHash', 'templateHash', 'skillSnapshotHash', 'modelLeaseRevision', 'policyHash',
    'outputContractHash'].map((key, index) => [key, hash(`${attemptId}:${index}`)]))
  const contextJSON = JSON.stringify(context)
  const contextHash = hash(contextJSON)
  const binding = { projectId: 'project', epoch: 'epoch', fingerprint, contextSnapshotId: `context-${attemptId}`,
    sourceManifest: { operation, reviewRevisionContext: context, reviewRevisionContextHash: contextHash,
      authorInputs: [{ id: 'review-revision-context', text: contextJSON }] }, sourceRefs: [] }
  db.prepare('INSERT INTO generation_roots(root_action_id,idempotency_key,action_json,budget_json) VALUES(?,?,?,?)')
    .run(rootId, `key-${rootId}`, JSON.stringify({ projectId: 'project', epoch: 'epoch', operation,
      uiActionNonce: attemptId, frozenInputHash: hash(attemptId), rootActionId: rootId, status: 'active' }), '{}')
  db.prepare('INSERT INTO generation_runs(run_id,root_action_id,binding_json,status,created_at_ms,open_key) VALUES(?,?,?,?,?,?)')
    .run(`run-${attemptId}`, rootId, JSON.stringify(binding), 'completed', 1, `open-${attemptId}`)
  const artifact = { artifactId: `artifact-${attemptId}`, attemptId, rootActionId: rootId,
    projectId: 'project', epoch: 'epoch', fingerprint, revision: 1, text: artifactText, textHash: hash(artifactText) }
  const artifactRef = { artifactId: artifact.artifactId, revision: 1, textHash: artifact.textHash }
  const usage = { artifactIdentity: { artifactId: artifact.artifactId, epoch: artifact.epoch, fingerprint },
    result: { usage: null, finishReason: 'stop' }, reviewRevisionEffect: { kind: 'review', id: reviewId,
      index: 1, contentHash: hash(reviewBody), contextHash, artifact: artifactRef } }
  db.prepare(`INSERT INTO generation_attempts(attempt_id,reservation_id,run_id,root_action_id,
    attempt_json,usage_receipt_json,invocation_nonce) VALUES(?,?,?,?,?,?,?)`)
    .run(attemptId, `reservation-${attemptId}`, `run-${attemptId}`, rootId,
      JSON.stringify({ attemptId, reservationId: `reservation-${attemptId}`, rootActionId: rootId,
        status: 'settled', reservedTokens: 100, requestedOutputTokens: 100, actualTokens: 1 }),
      JSON.stringify(usage), `nonce-${attemptId}`)
  db.prepare('INSERT INTO generation_artifacts(artifact_id,attempt_id,run_id,artifact_json,revision,status) VALUES(?,?,?,?,?,?)')
    .run(artifact.artifactId, attemptId, `run-${attemptId}`, JSON.stringify(artifact), 1, 'partial')

  const finding = { findingId: 'uncertain', source: { projectId: 'project', epoch: 'epoch',
    sourceId: `draft:${draftId}`, revision: 1, contentHash: hash(source), span: null },
  excerptHash: null, occurrence: null, category: 'continuity', targetId: null,
  kind: 'objective', problemText: '证据无法唯一定位', expectedText: null,
  status: 'unverified', evidenceHash: null, confirmationItemIndex: null }
  db.prepare('INSERT INTO review_cycles VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)').run('cycle-u', rootId,
    reviewId, hash(reviewBody), null, null, null, hash(source), canonicalM03FindingSetHash([finding]),
    1, 'not-generated', null, 0, null)
  db.prepare('INSERT INTO review_findings VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)').run('cycle-u',
    finding.findingId, finding.source.projectId, finding.source.epoch, finding.source.sourceId,
    finding.source.revision, finding.source.contentHash, null, null, null, null, null,
    finding.category, null, finding.kind, finding.problemText, null, finding.status, null, null)
  return { source, reviewId, reviewBody, draftId, artifactText, finding }
}

const base = path.join(directory, 'valid-not-generated.db')
const baseDb = new Database(base)
try {
  baseDb.pragma('foreign_keys=ON')
  baseDb.transaction(() => {
    initializeLegacyBaselineSchema(baseDb, Buffer.alloc(32, 0x23))
    baseDb.exec(M01_GENERATION_SQL)
    applyM03ReviewCycle(baseDb)
  })()
  baseDb.pragma('user_version=4')
  seedReview(baseDb)
  assert.equal(verifyM03ReviewCycle(baseDb), true)
} finally { baseDb.close() }

const cases = []
function addCase(name, mutate, expected) {
  const file = path.join(directory, `${name}.db`)
  if (name !== 'valid-not-generated') fs.copyFileSync(base, file, fs.constants.COPYFILE_EXCL)
  const db = new Database(file)
  try {
    db.pragma('foreign_keys=ON')
    if (mutate) mutate(db)
    assert.equal(verifyM03ReviewCycle(db), expected, `${name}: original TypeScript verifier`)
    const cycleCount = Number(db.prepare('SELECT COUNT(*) FROM review_cycles').pluck().get())
    const findingCount = Number(db.prepare('SELECT COUNT(*) FROM review_findings').pluck().get())
    cases.push({ name, file, expected, cycleCount, findingCount })
  } finally { db.close() }
  cases.at(-1).sha256 = hash(fs.readFileSync(file))
}

addCase('valid-not-generated', null, true)
addCase('review-body-hash', db => {
  db.prepare('UPDATE review_cycles SET review_content_hash=?').run(hash('tampered-review'))
}, false)
addCase('missing-effect-index', db => {
  db.exec("UPDATE generation_attempts SET usage_receipt_json=json_remove(usage_receipt_json,'$.reviewRevisionEffect.index') WHERE attempt_id='attempt-review'")
}, false)
addCase('discarded-review-artifact', db => {
  db.exec("UPDATE generation_artifacts SET status='discarded' WHERE attempt_id='attempt-review'")
}, false)
addCase('rebound-report-artifact', db => {
  const text = JSON.stringify({ summary: '另一份合法审稿。', items: [
    { category: 'continuity', severity: 'pass', description: '未发现连续性问题。' },
  ] })
  const artifact = JSON.parse(db.prepare("SELECT artifact_json FROM generation_artifacts WHERE attempt_id='attempt-review'").pluck().get())
  artifact.text = text
  artifact.textHash = hash(text)
  db.prepare("UPDATE generation_artifacts SET artifact_json=? WHERE attempt_id='attempt-review'").run(JSON.stringify(artifact))
  const usage = JSON.parse(db.prepare("SELECT usage_receipt_json FROM generation_attempts WHERE attempt_id='attempt-review'").pluck().get())
  usage.reviewRevisionEffect.artifact.textHash = artifact.textHash
  db.prepare("UPDATE generation_attempts SET usage_receipt_json=? WHERE attempt_id='attempt-review'").run(JSON.stringify(usage))
}, false)
addCase('finding-set-hash', db => {
  db.prepare('UPDATE review_cycles SET finding_set_hash=?').run(hash('tampered-finding-set'))
}, false)
addCase('wrong-anchored-excerpt', db => {
  const source = db.prepare('SELECT source_content FROM reviews WHERE id=(SELECT review_id FROM review_cycles)').pluck().get()
  const finding = { findingId: 'uncertain', source: { projectId: 'project', epoch: 'epoch', sourceId: 'draft:1',
    revision: 1, contentHash: hash(source), span: { start: 0, end: 1, unit: 'utf16-code-unit' } },
  excerptHash: hash('not-the-excerpt'), occurrence: 1, category: 'continuity', targetId: 'target-1',
  kind: 'objective', problemText: '证据无法唯一定位', expectedText: null,
  status: 'unresolved', evidenceHash: null, confirmationItemIndex: null }
  db.prepare(`UPDATE review_findings SET span_start=0,span_end=1,span_unit='utf16-code-unit',
    excerpt_hash=?,occurrence=1,target_id='target-1',status='unresolved' WHERE finding_id='uncertain'`)
    .run(finding.excerptHash)
  db.prepare('UPDATE review_cycles SET finding_set_hash=?').run(canonicalM03FindingSetHash([finding]))
}, false)
addCase('frozen-source-binding', db => {
  const binding = JSON.parse(db.prepare("SELECT binding_json FROM generation_runs WHERE run_id='run-attempt-review'").pluck().get())
  const context = binding.sourceManifest.reviewRevisionContext
  context.source.content = '另一份冻结的源正文。'
  context.sourceHash = hash(context.source.content)
  const contextJSON = JSON.stringify(context)
  const contextHash = hash(contextJSON)
  binding.sourceManifest.reviewRevisionContextHash = contextHash
  binding.sourceManifest.authorInputs = [{ id: 'review-revision-context', text: contextJSON }]
  db.prepare("UPDATE generation_runs SET binding_json=? WHERE run_id='run-attempt-review'").run(JSON.stringify(binding))
  const usage = JSON.parse(db.prepare("SELECT usage_receipt_json FROM generation_attempts WHERE attempt_id='attempt-review'").pluck().get())
  usage.reviewRevisionEffect.contextHash = contextHash
  db.prepare("UPDATE generation_attempts SET usage_receipt_json=? WHERE attempt_id='attempt-review'").run(JSON.stringify(usage))
}, false)
addCase('generated-without-confirmation', db => {
  const source = db.prepare('SELECT source_content FROM reviews WHERE id=(SELECT review_id FROM review_cycles)').pluck().get()
  const contentId = Number(db.prepare('INSERT INTO contents(body) VALUES(?)').run('未授权修稿').lastInsertRowid)
  const revisionId = Number(db.prepare(`INSERT INTO revisions(base_draft_id,revision_index,revision_type,status,user_prompt,
    review_source_id,source_draft_chapter_number,source_draft_version,source_draft_status,source_content,content_id)
    VALUES(?,1,'review-fix','pending','',NULL,1,1,'draft',?,?)`)
    .run(1, source, contentId).lastInsertRowid)
  db.prepare("UPDATE review_cycles SET revision_status='generated',revision_id=?").run(revisionId)
}, false)

assert.equal(cases.length, 9)
assert.equal(cases.filter(sample => sample.expected).length, 1)
fs.writeFileSync(output, JSON.stringify({ formatVersion: 1, scope: 'm03-verifier-only', cases }) + '\n', { flag: 'wx', mode: 0o600 })
console.log(JSON.stringify({ formatVersion: 1, scope: 'm03-verifier-only',
  cases: cases.map(({ name, expected }) => ({ name, expected })) }))
