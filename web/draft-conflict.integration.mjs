import assert from 'node:assert/strict'
import crypto from 'node:crypto'
import fs from 'node:fs'
import path from 'node:path'

// HTTP contract evidence, not browser/editor or paid-model evidence.
const base = 'http://127.0.0.1:16081'
assert.equal(process.env.NOVEL_CONFLICT_URL ?? base, base, 'Disposable gateway only')
const stamp = new Date().toISOString().replace(/[:.]/g, '-')
const title = `HTTP防覆盖验收-${stamp}`
const directory = path.resolve('.native-test-data/http-conflict', stamp)
fs.mkdirSync(directory, { recursive: true, mode: 0o700 })
const report = { status: 'running', startedAt: new Date().toISOString(), scope: 'isolated HTTP/manual-save and finalization CAS; no browser or provider dispatch', checks: [], paidDispatches: 0 }
const client = 'cas-' + crypto.randomBytes(16).toString('hex')
let cookie = '', csrf = '', projectPath, session
async function request(route, args) {
  const response = await fetch(base + route, {
    method: args ? 'POST' : 'GET', signal: AbortSignal.timeout(20000), redirect: 'error',
    headers: { 'X-Novel-User': 'native-cas-regression', 'X-Client-Id': client, ...(cookie ? { Cookie: cookie } : {}), ...(args ? { Origin: base, 'X-CSRF-Token': csrf, 'Content-Type': 'application/json' } : {}) },
    ...(args ? { body: JSON.stringify({ args }) } : {}),
  })
  const next = response.headers.get('set-cookie'); if (next) cookie = next.split(';')[0]
  return { status: response.status, body: await response.json() }
}
async function operation(name, args) {
  // A deliberately narrow allowlist; never model/config/MCP/import/author deletion.
  assert(['dialog:select-folder', 'project:create', 'project:open', 'db:draft-create', 'db:draft-update-content', 'db:draft-get-full', 'finalization:commit', 'db:close'].includes(name))
  return request('/api/operations/' + encodeURIComponent(name), args)
}
async function check(name, action) { await action(); report.checks.push({ name, status: 'passed' }) }
const full = async id => {
  const r = await operation('db:draft-get-full', [id, projectPath, session]); assert.equal(r.status, 200); return r.body.result
}
const save = (id, content, expected, context = session) => operation('db:draft-update-content', [id, content, content.length, projectPath, expected, context])
try {
  const boot = await request('/api/bootstrap'); assert.equal(boot.status, 200); csrf = boot.body.csrfToken; assert(csrf)
  const grant = await operation('dialog:select-folder', ['project-create']); assert.equal(grant.status, 200); assert(grant.body.result?.grantId)
  const created = await operation('project:create', [{ name: title, parentGrantId: grant.body.result.grantId, genre: '', targetAudience: '', writingLanguage: 'zh-CN' }])
  assert.equal(created.status, 200); assert.equal(created.body.result?.success, true)
  projectPath = created.body.result.projectPath; assert.equal(path.basename(projectPath), title)
  const opened = await operation('project:open', [projectPath, 'http-cas-own-project'])
  assert.equal(opened.status, 200); const project = opened.body.result?.project
  assert.equal(project?.path, projectPath); assert(project.sessionLease)
  session = { projectId: project.id, leaseId: project.sessionLease, projectPath }
  const makeDraft = async (chapterNumber, content) => {
    const result = await operation('db:draft-create', [{ chapterNumber, version: 1, source: 'write', content, wordCount: content.length }, projectPath, session])
    assert.equal(result.status, 200); assert.equal(result.body.result?.success, true); return result.body.result.id
  }
  const original = '共同保存基准：雨夜站台。', id = await makeDraft(1, original)
  await check('two concurrent same-baseline saves have exactly one winner', async () => {
    const answers = await Promise.all([save(id, '先候选甲：灯亮了。', original), save(id, '先候选乙：车来了。', original)])
    assert(answers.every(answer => answer.status === 200))
    const successful = answers.filter(answer => answer.body.result?.success); assert.equal(successful.length, 1)
    const refused = answers.find(answer => !answer.body.result?.success); assert.equal(refused.body.result.errorCode, 'DRAFT_CONTENT_CONFLICT')
    assert(['先候选甲：灯亮了。', '先候选乙：车来了。'].includes((await full(id)).content))
  })
  await check('missing baseline fails closed without changing body', async () => {
    const before = (await full(id)).content
    const result = await operation('db:draft-update-content', [id, '不得覆盖', 4, projectPath, session])
    assert.equal(result.status, 200); assert.equal(result.body.result.errorCode, 'DRAFT_CONTENT_CONFLICT'); assert.equal((await full(id)).content, before)
  })
  await check('stale project lease cannot save', async () => {
    const before = (await full(id)).content
    const result = await save(id, '旧租约不得覆盖', before, { ...session, leaseId: 'expired-test-lease' })
    assert(result.status !== 200 || result.body.result?.success !== true)
    assert.equal((await full(id)).content, before)
  })
  const frozen = { tabId: 'http-cas-synthetic', projectPath, projectSession: session, draftId: id, chapterNumber: 1, chapterTitle: '防覆盖测试', content: '冻结定稿候选，不得覆盖后续修改。', expectedContent: (await full(id)).content, contentRevision: 1 }
  const newer = '作者随后保存的新正文。'
  assert.equal((await save(id, newer, frozen.expectedContent)).body.result?.success, true)
  await check('stale frozen finalization cannot overwrite a subsequent manual save', async () => {
    const result = await operation('finalization:commit', [frozen, session])
    assert.equal(result.status, 200); assert.equal(result.body.result.success, false); assert.equal(result.body.result.committed, false)
    assert.match(result.body.result.error, /FINALIZATION_COMMIT_CONFLICT/)
    const draft = await full(id); assert.equal(draft.content, newer); assert.equal(draft.status, 'draft')
    assert.equal(frozen.content, '冻结定稿候选，不得覆盖后续修改。')
  })
  await check('new finalization without a saved baseline is refused', async () => {
    const missing = { ...frozen }; delete missing.expectedContent
    const result = await operation('finalization:commit', [missing, session])
    assert.equal(result.body.result.success, false); assert.equal(result.body.result.committed, false); assert.equal((await full(id)).content, newer)
  })
  let committed
  const current = { ...frozen, expectedContent: newer, content: newer, contentRevision: 2 }
  await check('current baseline finalization commits, and identical response-loss retry is idempotent', async () => {
    const result = await operation('finalization:commit', [current, session]); assert.equal(result.status, 200)
    committed = result.body.result; assert.equal(committed.committed, true); assert(committed.finalizationId)
    const legacyRetry = { ...current }; delete legacyRetry.expectedContent
    const retry = await operation('finalization:commit', [legacyRetry, session])
    assert.equal(retry.body.result.committed, true); assert.equal(retry.body.result.finalizationId, committed.finalizationId)
    assert.equal((await full(id)).status, 'finalized')
  })
  await check('even an equal-body baseline cannot reopen finalized authority', async () => {
    const result = await save(id, '定稿后不得覆盖', newer)
    assert.equal(result.body.result.errorCode, 'DRAFT_CONTENT_CONFLICT'); assert.equal((await full(id)).content, newer)
  })
  report.status = 'passed'
} catch (error) {
  report.status = 'failed'; report.failure = { name: error.name, code: error.code ?? null }
  process.exitCode = 1
} finally {
  if (session) { try { await operation('db:close', [projectPath, session]) } catch {} }
  report.finishedAt = new Date().toISOString()
  fs.writeFileSync(path.join(directory, 'report.json'), JSON.stringify(report, null, 2), { mode: 0o600 })
  console.log(JSON.stringify({ status: report.status, checks: report.checks.length, paidDispatches: 0, report: path.relative(process.cwd(), path.join(directory, 'report.json')) }))
}
