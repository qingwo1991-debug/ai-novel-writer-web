import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { createRequire } from 'node:module'
import fs from 'node:fs'
import path from 'node:path'

import { verifyM03ReviewCycle } from '../../electron/migrations/m03-review-cycle.ts'
import { createHumanConfirmedReviewSnapshot, serializeHumanConfirmedReviewSnapshot } from '../../src/shared/human-confirmed-review.ts'

// This oracle extends the original synthetic baseline+M01+M03 database. It is
// only an isolated M03 verifier fixture, never a recognized project admission.
assert.equal(process.env.GITHUB_ACTIONS, 'true')
assert.equal(process.platform, 'linux')
const runnerTemp = process.env.RUNNER_TEMP
const input = process.env.GO_M03_NOTGENERATED_CONTRACT
const output = process.env.GO_M03_GENERATED_CONTRACT
assert(runnerTemp && input && output && path.isAbsolute(runnerTemp) && path.isAbsolute(input) && path.isAbsolute(output))
const insideRunner = file => {
  const relative = path.relative(runnerTemp, file)
  return relative && relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative)
}
assert(insideRunner(input) && insideRunner(output))
fs.mkdirSync(path.dirname(output), { recursive: true, mode: 0o700 })
const directory = fs.mkdtempSync(path.join(runnerTemp, 'novel-go-m03-generated-'))
fs.chmodSync(directory, 0o700)
const Database = createRequire(import.meta.url)('better-sqlite3')
const hash = value => createHash('sha256').update(value).digest('hex')
const sourceManifest = JSON.parse(fs.readFileSync(input, 'utf8'))
assert.equal(sourceManifest.scope, 'm03-verifier-only')
const sourceFixture = sourceManifest.cases.find(sample => sample.name === 'valid-not-generated')
assert(sourceFixture && sourceFixture.expected && insideRunner(sourceFixture.file))
assert.equal(hash(fs.readFileSync(sourceFixture.file)), sourceFixture.sha256)

function seedGenerated(db) {
  assert.equal(verifyM03ReviewCycle(db), true)
  const source = db.prepare(`SELECT r.base_draft_id,r.source_content,r.source_draft_chapter_number AS chapter_number,
    r.source_draft_version AS version,r.source_draft_status AS status,c.body AS review_body
    FROM review_cycles cycle JOIN reviews r ON r.id=cycle.review_id JOIN contents c ON c.id=r.content_id`).get()
  const originalReviewId = Number(db.prepare('SELECT review_id FROM review_cycles').pluck().get())
  const snapshot = createHumanConfirmedReviewSnapshot({ sourceReviewId: originalReviewId,
    sourceDraft: { id: source.base_draft_id, chapterNumber: source.chapter_number,
      version: source.version, status: source.status, content: source.source_content },
    summary: '作者已确认修稿。', authorGuidance: '请修正门闩状态。', items: [] })
  assert(snapshot)
  const confirmationBody = serializeHumanConfirmedReviewSnapshot(snapshot)
  const confirmationContentId = Number(db.prepare('INSERT INTO contents(body) VALUES(?)').run(confirmationBody).lastInsertRowid)
  const confirmationId = Number(db.prepare(`INSERT INTO reviews(base_draft_id,review_index,
    source_draft_chapter_number,source_draft_version,source_draft_status,source_content,content_id)
    VALUES(?,?,?,?,?,?,?)`).run(source.base_draft_id, 2, source.chapter_number,
    source.version, source.status, source.source_content, confirmationContentId).lastInsertRowid)

  const revisionText = '甲关上门闩。'
  const revisionContentId = Number(db.prepare('INSERT INTO contents(body) VALUES(?)').run(revisionText).lastInsertRowid)
  const revisionId = Number(db.prepare(`INSERT INTO revisions(base_draft_id,revision_index,revision_type,status,
    user_prompt,review_source_id,source_draft_chapter_number,source_draft_version,
    source_draft_status,source_content,content_id) VALUES(?,1,'review-fix','pending','',?,?,?,?,?,?)`)
    .run(source.base_draft_id, confirmationId, source.chapter_number, source.version,
      source.status, source.source_content, revisionContentId).lastInsertRowid)

  const rootId = 'root-not-generated'
  const attemptId = 'attempt-revision'
  const operation = 'refine-from-review'
  const sourceDraft = { id: source.base_draft_id, chapterNumber: source.chapter_number,
    version: source.version, status: source.status, content: source.source_content }
  const context = { version: 1, operation, sourceHash: hash(source.source_content), source: sourceDraft,
    config: {}, writingLanguage: 'zh-CN', uiLocale: 'zh-CN', authorInputs: [], characterStates: '（暂无）',
    worldbuilding: '', history: [], blueprints: [], frozenGoals: { chapterNumber: 1,
      coverage: 'not_configured', items: [] }, preflightFindings: [],
    confirmation: { reviewSourceId: confirmationId, content: confirmationBody,
      originalReviewContentHash: hash(source.review_body), snapshot } }
  const fingerprint = Object.fromEntries(['chapterBriefHash', 'authorGuidanceHash', 'dependencyHash',
    'contextSnapshotHash', 'templateHash', 'skillSnapshotHash', 'modelLeaseRevision', 'policyHash',
    'outputContractHash'].map((key, index) => [key, hash(`${attemptId}:${index}`)]))
  const contextJSON = JSON.stringify(context)
  const contextHash = hash(contextJSON)
  const binding = { projectId: 'project', epoch: 'epoch', fingerprint,
    contextSnapshotId: `context-${attemptId}`, sourceManifest: { operation,
      reviewRevisionContext: context, reviewRevisionContextHash: contextHash,
      authorInputs: [{ id: 'review-revision-context', text: contextJSON }] }, sourceRefs: [] }
  const runId = `run-${attemptId}`
  db.prepare('INSERT INTO generation_runs(run_id,root_action_id,binding_json,status,created_at_ms,open_key) VALUES(?,?,?,?,?,?)')
    .run(runId, rootId, JSON.stringify(binding), 'completed', 2, `open-${attemptId}`)
  const artifact = { artifactId: `artifact-${attemptId}`, attemptId, rootActionId: rootId,
    projectId: 'project', epoch: 'epoch', fingerprint, revision: 1,
    text: revisionText, textHash: hash(revisionText) }
  const artifactRef = { artifactId: artifact.artifactId, revision: 1, textHash: artifact.textHash }
  const usage = { artifactIdentity: { artifactId: artifact.artifactId, epoch: artifact.epoch, fingerprint },
    result: { usage: null, finishReason: 'stop' },
    visibleComposition: { algorithm: 'visible-append-v1', artifactIds: [artifact.artifactId],
      sources: [artifactRef], textHash: hash(revisionText) },
    reviewRevisionEffect: { kind: 'revision', id: revisionId, index: 1,
      contentHash: hash(revisionText), contextHash, compositionHash: hash(revisionText), artifact: artifactRef } }
  db.prepare(`INSERT INTO generation_attempts(attempt_id,reservation_id,run_id,root_action_id,
    attempt_json,usage_receipt_json,invocation_nonce) VALUES(?,?,?,?,?,?,?)`)
    .run(attemptId, `reservation-${attemptId}`, runId, rootId,
      JSON.stringify({ attemptId, reservationId: `reservation-${attemptId}`, rootActionId: rootId,
        status: 'settled', reservedTokens: 100, requestedOutputTokens: 100, actualTokens: 1 }),
      JSON.stringify(usage), `nonce-${attemptId}`)
  db.prepare('INSERT INTO generation_artifacts(artifact_id,attempt_id,run_id,artifact_json,revision,status) VALUES(?,?,?,?,?,?)')
    .run(artifact.artifactId, attemptId, runId, JSON.stringify(artifact), 1, 'partial')
  db.prepare(`UPDATE review_cycles SET confirmation_review_id=?,confirmation_content_hash=?,
    revision_id=?,revision_status='generated'`).run(confirmationId, hash(confirmationBody), revisionId)
  assert.equal(verifyM03ReviewCycle(db), true)
  return { revisionId, revisionContentId, revisionText, sourceDraftId: source.base_draft_id }
}

const generatedBase = path.join(directory, 'valid-generated.db')
fs.copyFileSync(sourceFixture.file, generatedBase, fs.constants.COPYFILE_EXCL)
const generatedDb = new Database(generatedBase)
let generated
try {
  generatedDb.pragma('foreign_keys=ON')
  generated = seedGenerated(generatedDb)
} finally { generatedDb.close() }

const mergedBase = path.join(directory, 'valid-merge-committed.db')
fs.copyFileSync(generatedBase, mergedBase, fs.constants.COPYFILE_EXCL)
const mergedDb = new Database(mergedBase)
try {
  mergedDb.pragma('foreign_keys=ON')
  mergedDb.prepare("UPDATE revisions SET status='merged',merged_to_draft_id=? WHERE id=?")
    .run(generated.sourceDraftId, generated.revisionId)
  mergedDb.prepare("UPDATE drafts SET content_id=?,status='revised',version=2 WHERE id=?")
    .run(generated.revisionContentId, generated.sourceDraftId)
  mergedDb.prepare("UPDATE review_cycles SET revision_status='merge-committed',merged_hash=?")
    .run(hash(generated.revisionText))
  assert.equal(verifyM03ReviewCycle(mergedDb), true)
} finally { mergedDb.close() }

const cases = []
function addCase(name, sourceFile, mutate, expected) {
  const file = path.join(directory, `${name}.db`)
  if (file !== sourceFile) fs.copyFileSync(sourceFile, file, fs.constants.COPYFILE_EXCL)
  const db = new Database(file)
  try {
    db.pragma('foreign_keys=ON')
    if (mutate) mutate(db)
    assert.equal(verifyM03ReviewCycle(db), expected, `${name}: original TypeScript verifier`)
    cases.push({ name, file, expected, cycleCount: Number(db.prepare('SELECT COUNT(*) FROM review_cycles').pluck().get()),
      findingCount: Number(db.prepare('SELECT COUNT(*) FROM review_findings').pluck().get()) })
  } finally { db.close() }
  cases.at(-1).sha256 = hash(fs.readFileSync(file))
}

addCase('valid-generated', generatedBase, null, true)
addCase('missing-composition-source', generatedBase, db => {
  db.exec("UPDATE generation_attempts SET usage_receipt_json=json_remove(usage_receipt_json,'$.visibleComposition.sources') WHERE attempt_id='attempt-revision'")
}, false)
addCase('discarded-revision-artifact', generatedBase, db => {
  db.exec("UPDATE generation_artifacts SET status='discarded' WHERE attempt_id='attempt-revision'")
}, false)
addCase('wrong-confirmation-binding', generatedBase, db => {
  db.exec("UPDATE revisions SET review_source_id=(SELECT review_id FROM review_cycles) WHERE id=(SELECT revision_id FROM review_cycles)")
}, false)
addCase('tampered-revision-body', generatedBase, db => {
  db.prepare("UPDATE contents SET body='乙打开窗。' WHERE id=?").run(generated.revisionContentId)
}, false)
addCase('valid-merge-committed', mergedBase, null, true)
addCase('merge-body-hash', mergedBase, db => {
  db.prepare('UPDATE review_cycles SET merged_hash=?').run(hash('另一份合并正文。'))
}, false)

assert.equal(cases.length, 7)
assert.equal(cases.filter(sample => sample.expected).length, 2)
fs.writeFileSync(output, JSON.stringify({ formatVersion: 1, scope: 'm03-verifier-only', cases }) + '\n',
  { flag: 'wx', mode: 0o600 })
console.log(JSON.stringify({ formatVersion: 1, scope: 'm03-verifier-only',
  cases: cases.map(({ name, expected }) => ({ name, expected })) }))
