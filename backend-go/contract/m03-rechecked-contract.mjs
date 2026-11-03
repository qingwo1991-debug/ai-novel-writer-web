import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { createRequire } from 'node:module'
import fs from 'node:fs'
import path from 'node:path'

import { canonicalM03FindingSetHash, verifyM03ReviewCycle } from '../../electron/migrations/m03-review-cycle.ts'
import { buildReviewCycleRecheckReport } from '../../src/shared/review-cycle.ts'

// Original TypeScript is the oracle. This synthetic baseline+M01+M03 fixture
// probes M03 only; it is not a recognized full-project admission snapshot.
assert.equal(process.env.GITHUB_ACTIONS, 'true')
assert.equal(process.platform, 'linux')
const runnerTemp = process.env.RUNNER_TEMP
const input = process.env.GO_M03_GENERATED_CONTRACT
const output = process.env.GO_M03_RECHECKED_CONTRACT
const insideRunner = file => {
  if (!runnerTemp || !file || !path.isAbsolute(file)) return false
  const relative = path.relative(runnerTemp, file)
  return relative && relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative)
}
assert(insideRunner(input) && insideRunner(output))
fs.mkdirSync(path.dirname(output), { recursive: true, mode: 0o700 })
const directory = fs.mkdtempSync(path.join(runnerTemp, 'novel-go-m03-rechecked-'))
fs.chmodSync(directory, 0o700)
const Database = createRequire(import.meta.url)('better-sqlite3')
const hash = value => createHash('sha256').update(value).digest('hex')
const manifest = JSON.parse(fs.readFileSync(input, 'utf8'))
assert.equal(manifest.formatVersion, 1)
assert.equal(manifest.scope, 'm03-verifier-only')
const donor = manifest.cases.find(sample => sample.name === 'valid-merge-committed')
assert(donor && donor.expected && insideRunner(donor.file))
assert.equal(hash(fs.readFileSync(donor.file)), donor.sha256)

function addRecheck(db) {
  assert.equal(verifyM03ReviewCycle(db), true)
  const cycle = db.prepare('SELECT * FROM review_cycles').get()
  const source = db.prepare('SELECT source_content FROM reviews WHERE id=?').pluck().get(cycle.review_id)
  const merged = db.prepare(`SELECT c.body FROM drafts d JOIN contents c ON c.id=d.content_id
    WHERE d.id=(SELECT base_draft_id FROM reviews WHERE id=?)`).pluck().get(cycle.review_id)
  const draft = db.prepare('SELECT id,chapter_number,version,status FROM drafts WHERE id=(SELECT base_draft_id FROM reviews WHERE id=?)')
    .get(cycle.review_id)
  assert.equal(draft.status, 'revised')
  assert.equal(hash(merged), cycle.merged_hash)
  const oldQuote = '甲推开门'
  const start = source.indexOf(oldQuote)
  assert.equal(start, 0)
  const finding = { findingId: 'uncertain', source: { projectId: 'project', epoch: 'epoch',
    sourceId: `draft:${draft.id}`, revision: 1, contentHash: hash(source),
    span: { start, end: start + oldQuote.length, unit: 'utf16-code-unit' } },
  excerptHash: hash(oldQuote), occurrence: 1, category: 'continuity', targetId: 'target-1',
  kind: 'objective', problemText: '证据无法唯一定位', expectedText: null,
  status: 'unresolved', evidenceHash: null, confirmationItemIndex: null }
  const findingSetHash = canonicalM03FindingSetHash([finding])
  db.prepare(`UPDATE review_findings SET span_start=?,span_end=?,span_unit='utf16-code-unit',
    excerpt_hash=?,occurrence=1,target_id=?,status='unresolved' WHERE cycle_id=? AND finding_id=?`)
    .run(start, start + oldQuote.length, finding.excerptHash, finding.targetId, cycle.cycle_id, finding.findingId)
  db.prepare('UPDATE review_cycles SET finding_set_hash=? WHERE cycle_id=?').run(findingSetHash, cycle.cycle_id)

  const recheckContext = { version: 1, cycleId: cycle.cycle_id,
    comparisonVersion: cycle.comparison_version, mergedHash: hash(merged), findingSetHash,
    findings: [{ findingId: finding.findingId, targetId: finding.targetId,
      category: finding.category, kind: finding.kind, problem: finding.problemText,
      sourceSpan: finding.source.span, occurrence: 1, sourceExcerpt: oldQuote }] }
  const modelOutput = JSON.stringify({ summary: '复核发现门闩已关闭。', items: [{
    findingId: finding.findingId, targetId: finding.targetId, resolved: true,
    evidenceQuote: '甲关上门闩', reason: '新正文明确写出门闩已关闭。' }] })
  const built = buildReviewCycleRecheckReport(modelOutput, merged, recheckContext, 'zh-CN', 1)
  assert.equal(built.decisions.length, 1)
  assert.equal(built.decisions[0].status, 'resolved')
  const reviewBody = JSON.stringify({ summary: built.summary, items: built.items }, null, 2)
  const reviewItem = JSON.parse(reviewBody).items[0]
  const evidenceHash = hash(JSON.stringify({ findingId: finding.findingId, targetId: finding.targetId,
    reviewItemIndex: 0, item: reviewItem, resolved: true }))
  const receipt = { version: 1, cycleId: cycle.cycle_id, comparisonVersion: cycle.comparison_version,
    mergedHash: hash(merged), findingSetHash,
    findings: [{ findingId: finding.findingId, targetId: finding.targetId,
      reviewItemIndex: 0, evidenceHash, resolved: true }] }
  const contentId = Number(db.prepare('INSERT INTO contents(body) VALUES(?)').run(reviewBody).lastInsertRowid)
  const reviewId = Number(db.prepare(`INSERT INTO reviews(base_draft_id,review_index,source_draft_chapter_number,
    source_draft_version,source_draft_status,source_content,content_id)
    VALUES(?,3,?,?,'revised',?,?)`).run(draft.id, draft.chapter_number,
      draft.version, merged, contentId).lastInsertRowid)

  const attemptId = 'attempt-recheck'
  const rootId = cycle.root_action_id
  const operation = 'review-chapter'
  const context = { version: 1, operation, sourceHash: hash(merged),
    source: { id: draft.id, chapterNumber: draft.chapter_number, version: draft.version,
      status: 'revised', content: merged },
    config: {}, writingLanguage: 'zh-CN', uiLocale: 'zh-CN', authorInputs: [],
    characterStates: '（暂无）', worldbuilding: '', history: [], blueprints: [],
    frozenGoals: { chapterNumber: 1, coverage: 'not_configured', items: [] },
    preflightFindings: [], recheck: recheckContext }
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
    .run(runId, rootId, JSON.stringify(binding), 'completed', 3, `open-${attemptId}`)
  const artifact = { artifactId: `artifact-${attemptId}`, attemptId, rootActionId: rootId,
    projectId: 'project', epoch: 'epoch', fingerprint, revision: 1,
    text: modelOutput, textHash: hash(modelOutput) }
  const artifactRef = { artifactId: artifact.artifactId, revision: 1, textHash: artifact.textHash }
  const usage = { artifactIdentity: { artifactId: artifact.artifactId, epoch: artifact.epoch, fingerprint },
    result: { usage: null, finishReason: 'stop' }, reviewRevisionEffect: { kind: 'review',
      id: reviewId, index: 3, contentHash: hash(reviewBody), contextHash, artifact: artifactRef },
    reviewCycleRecheck: receipt }
  db.prepare(`INSERT INTO generation_attempts(attempt_id,reservation_id,run_id,root_action_id,
    attempt_json,usage_receipt_json,invocation_nonce) VALUES(?,?,?,?,?,?,?)`)
    .run(attemptId, `reservation-${attemptId}`, runId, rootId,
      JSON.stringify({ attemptId, reservationId: `reservation-${attemptId}`, rootActionId: rootId,
        status: 'settled', reservedTokens: 100, requestedOutputTokens: 100, actualTokens: 1 }),
      JSON.stringify(usage), `nonce-${attemptId}`)
  db.prepare('INSERT INTO generation_artifacts(artifact_id,attempt_id,run_id,artifact_json,revision,status) VALUES(?,?,?,?,?,?)')
    .run(artifact.artifactId, attemptId, runId, JSON.stringify(artifact), 1, 'partial')
  db.prepare(`UPDATE review_cycles SET recheck_count=1,recheck_attempt_id=? WHERE cycle_id=?`)
    .run(attemptId, cycle.cycle_id)
  db.prepare(`UPDATE review_findings SET status='resolved',evidence_hash=? WHERE cycle_id=? AND finding_id=?`)
    .run(evidenceHash, cycle.cycle_id, finding.findingId)
  assert.equal(verifyM03ReviewCycle(db), true)
  return { cycleId: cycle.cycle_id, findingId: finding.findingId, attemptId,
    reviewId, evidenceHash, mergedHash: hash(merged) }
}

const base = path.join(directory, 'valid-rechecked-v1.db')
fs.copyFileSync(donor.file, base, fs.constants.COPYFILE_EXCL)
const baseDb = new Database(base)
let seeded
try { baseDb.pragma('foreign_keys=ON'); seeded = addRecheck(baseDb) }
finally { baseDb.close() }

const cases = []
function addCase(name, mutate, expected) {
  const file = path.join(directory, `${name}.db`)
  if (file !== base) fs.copyFileSync(base, file, fs.constants.COPYFILE_EXCL)
  const db = new Database(file)
  try {
    db.pragma('foreign_keys=ON')
    if (mutate) mutate(db)
    assert.equal(verifyM03ReviewCycle(db), expected, `${name}: original TypeScript verifier`)
    cases.push({ name, file, expected,
      cycleCount: Number(db.prepare('SELECT COUNT(*) FROM review_cycles').pluck().get()),
      findingCount: Number(db.prepare('SELECT COUNT(*) FROM review_findings').pluck().get()) })
  } finally { db.close() }
  cases.at(-1).sha256 = hash(fs.readFileSync(file))
}

addCase('valid-rechecked-v1', null, true)
addCase('wrong-recheck-merged-hash', db => {
  db.exec("UPDATE generation_attempts SET usage_receipt_json=json_set(usage_receipt_json,'$.reviewCycleRecheck.mergedHash','bad') WHERE attempt_id='attempt-recheck'")
}, false)
addCase('missing-recheck-receipt', db => {
  db.exec("UPDATE generation_attempts SET usage_receipt_json=json_remove(usage_receipt_json,'$.reviewCycleRecheck') WHERE attempt_id='attempt-recheck'")
}, false)
addCase('wrong-recheck-evidence-hash', db => {
  db.exec("UPDATE generation_attempts SET usage_receipt_json=json_set(usage_receipt_json,'$.reviewCycleRecheck.findings[0].evidenceHash','bad') WHERE attempt_id='attempt-recheck'")
}, false)
addCase('finding-evidence-rebound', db => {
  db.prepare('UPDATE review_findings SET evidence_hash=? WHERE cycle_id=? AND finding_id=?')
    .run(hash('other'), seeded.cycleId, seeded.findingId)
}, false)
addCase('discarded-recheck-artifact', db => {
  db.prepare("UPDATE generation_artifacts SET status='discarded' WHERE attempt_id=?").run(seeded.attemptId)
}, false)
addCase('wrong-recheck-source', db => {
  const binding = JSON.parse(db.prepare("SELECT binding_json FROM generation_runs WHERE run_id='run-attempt-recheck'").pluck().get())
  binding.sourceManifest.reviewRevisionContext.source.status = 'draft'
  const contextJSON = JSON.stringify(binding.sourceManifest.reviewRevisionContext)
  binding.sourceManifest.reviewRevisionContextHash = hash(contextJSON)
  binding.sourceManifest.authorInputs = [{ id: 'review-revision-context', text: contextJSON }]
  const usage = JSON.parse(db.prepare("SELECT usage_receipt_json FROM generation_attempts WHERE attempt_id='attempt-recheck'").pluck().get())
  usage.reviewRevisionEffect.contextHash = hash(contextJSON)
  db.prepare("UPDATE generation_runs SET binding_json=? WHERE run_id='run-attempt-recheck'").run(JSON.stringify(binding))
  db.prepare("UPDATE generation_attempts SET usage_receipt_json=? WHERE attempt_id='attempt-recheck'").run(JSON.stringify(usage))
}, false)

assert.equal(cases.length, 7)
assert.equal(cases.filter(sample => sample.expected).length, 1)
fs.writeFileSync(output, JSON.stringify({ formatVersion: 1, scope: 'm03-verifier-only', cases }) + '\n',
  { flag: 'wx', mode: 0o600 })
console.log(JSON.stringify({ formatVersion: 1, scope: 'm03-verifier-only',
  cases: cases.map(({ name, expected }) => ({ name, expected })) }))
