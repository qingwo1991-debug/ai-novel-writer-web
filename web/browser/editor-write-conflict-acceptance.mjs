import { chromium } from 'playwright'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { createHash } from 'node:crypto'

// Only a dedicated UI-created, offline-seeded synthetic fixture is writable here.
const baseURL = process.env.NOVEL_TEST_BROWSER_URL ?? 'http://ai-novel-native-test:6081'
assert.equal(baseURL, 'http://ai-novel-native-test:6081')
const title = 'Native修复验收-2026-11-02T01-13-45-849Z-a87979'
const fixtureId = '43e85eaa-094d-4a06-aa19-d5e044b016e5'
const stamp = new Date().toISOString().replace(/[:.]/g, '-')
const output = `/workspace/.native-test-data/browser-editor-write/${stamp}`
fs.mkdirSync(output, { recursive: true, mode: 0o700 }); fs.chmodSync(output, 0o700)
const report = { status: 'running', startedAt: new Date().toISOString(), steps: [], requests: [], blocked: [], errors: [], results: {}, limitations: ['One synthetic draft/blueprint was prepared offline with gateway stopped; no AI workflow credit.', 'Chinese Unicode insertion is tested, not a physical OS IME composition session.', 'Delayed genuine save response tests newer typing; this is NOT a stale-server CAS conflict test.', 'No finalization, chapter creation, global settings or paid model operations.'] }
const checkpoint = () => { fs.writeFileSync(path.join(output, 'report.json'), JSON.stringify(report, null, 2), { mode: 0o600 }); fs.chmodSync(path.join(output, 'report.json'), 0o600) }
const hash = body => createHash('sha256').update(body).digest('hex')
const readOnly = new Set(['startup:get-state', 'startup:appearance-ack', 'config:get', 'llm:list-models', 'llm:get-default-model', 'llm:get-default-embedding-model', 'llm:get-stage-models', 'project:recent-list', 'project:peek-overview', 'project:overview-current', 'db:project-core-get', 'db:character-roster-read', 'db:blueprint-get-all', 'db:blueprint-get', 'db:draft-list-all', 'db:draft-list', 'db:draft-get-full', 'db:draft-get-meta', 'db:draft-get-latest', 'db:review-cycle-list-pending-rechecks', 'db:revision-list', 'db:revision-get-pending', 'db:review-list', 'db:review-get-full', 'db:review-cycle-get', 'db:post-process-get-latest-run', 'db:post-process-get-steps', 'chapter:list-incomplete-deletions', 'fs:read-json', 'fs:list-dir'])
const operationOf = request => { const pathname = new URL(request.url()).pathname; return pathname.startsWith('/api/operations/') ? decodeURIComponent(pathname.slice('/api/operations/'.length)) : null }
const argsOf = request => { try { return request.postDataJSON()?.args ?? [] } catch { return [] } }
let context, page, watchdog, fixturePath, draftId, delayNext = false, releaseDelay, committedSignal, commitArrived, expectedWrite, expectedBaseline, baseline, latest
const pending = new Set()
const writes = []
async function step(name, action) {
  const started = Date.now()
  try { await action(); report.steps.push({ name, status: 'passed', elapsedMs: Date.now() - started }); console.log('EDITOR_WRITE_STEP', name, 'PASS') }
  catch (error) { report.steps.push({ name, status: 'failed', elapsedMs: Date.now() - started }); report.errors.push({ step: name, errorType: error.name }); throw error }
  finally { checkpoint() }
}
const editor = () => page.locator('.stage-ch-prose .cm-content').filter({ visible: true })
const saved = () => page.locator('.stage-saved')
async function waitBody(value) { await page.waitForFunction(value => document.querySelector('.stage-ch-prose .cm-content')?.textContent === value, value) }
async function openChapter() {
  await page.locator('button[data-stage="manuscript"]').click()
  await page.locator('.stage-chapters .stage-chapter').nth(0).click()
  await editor().waitFor(); await Promise.all([...pending])
}
async function save(body, old) {
  expectedWrite = body; expectedBaseline = old
  const response = page.waitForResponse(r => operationOf(r.request()) === 'db:draft-update-content')
  await editor().press('Control+s')
  const result = await response; assert.equal(result.status(), 200)
  assert.equal((await result.json()).result?.success, true)
  await page.waitForFunction(() => document.querySelector('.stage-saved')?.textContent === '已保存')
}
try {
  const browser = await chromium.connectOverCDP('ws://172.18.0.22:3000?timeout=600000', { timeout: 20_000 })
  context = await browser.newContext({ locale: 'zh-CN', viewport: { width: 1440, height: 1000 }, serviceWorkers: 'block' })
  watchdog = setTimeout(() => { report.status = 'timed-out'; releaseDelay?.(); checkpoint(); void context.close() }, 300_000)
  await context.route('**/*', async route => {
    const request = route.request(), url = new URL(request.url()), operation = operationOf(request), args = argsOf(request)
    let allowed = url.origin === baseURL
    if (operation) {
      allowed &&= readOnly.has(operation)
      if (operation === 'project:open') {
        await Promise.all([...pending])
        allowed = url.origin === baseURL && typeof args[0] === 'string' && path.basename(args[0]) === title && (!fixturePath || args[0] === fixturePath)
      }
      if (operation === 'db:draft-update-content') {
        allowed = url.origin === baseURL && fixturePath && draftId && args[0] === draftId && args[3] === fixturePath && args[1] === expectedWrite && args[4] === expectedBaseline
        if (allowed) writes.push({ body: args[1], baseline: args[4] })
      }
    } else if (url.pathname.startsWith('/api/')) allowed &&= request.method() === 'GET' && ['/api/bootstrap', '/api/events'].includes(url.pathname) || request.method() === 'POST' && url.pathname === '/api/release'
    if (!allowed) { report.blocked.push({ operation, reason: 'not-owned-or-not-allowlisted' }); checkpoint(); return route.abort('blockedbyclient') }
    if (operation === 'db:draft-update-content' && delayNext) {
      delayNext = false
      const response = await route.fetch() // Actual server commit. Do not rewrite request/response.
      assert.equal(response.status(), 200); assert.equal((await response.json()).result?.success, true)
      committedSignal()
      await commitArrived
      await route.fulfill({ response })
    } else await route.continue()
  })
  page = await context.newPage(); page.setDefaultTimeout(12_000)
  page.on('pageerror', () => { report.errors.push({ type: 'pageerror' }); checkpoint() })
  page.on('response', response => {
    const operation = operationOf(response.request()), url = new URL(response.url())
    if (!url.pathname.startsWith('/api/')) return
    report.requests.push({ operation, status: response.status(), method: response.request().method() }); checkpoint()
    if (!['project:recent-list', 'project:open', 'db:draft-get-full'].includes(operation) || !response.ok()) return
    const task = response.json().then(body => {
      const result = body.result
      if (operation === 'project:recent-list') {
        const own = Array.isArray(result) ? result.find(x => x.name === title) : null
        if (own) fixturePath ??= own.path
      }
      if (operation === 'project:open' && result?.success) {
        assert.equal(result.project?.name, title); assert.equal(result.project?.id, fixtureId); assert.equal(result.project?.path, fixturePath)
        report.results.ownedProjectIdentity = true
      }
      if (operation === 'db:draft-get-full' && argsOf(response.request())[1] === fixturePath && result?.chapterNumber === 1 && result?.version === 1) draftId ??= result.id
    }).catch(() => { report.errors.push({ type: 'owned-metadata-check' }) })
    pending.add(task); void task.finally(() => pending.delete(task))
  })
  await step('open-owned-synthetic-writable-chapter', async () => {
    await page.goto(baseURL, { waitUntil: 'domcontentloaded' })
    await page.getByRole('button', { name: `预览《${title}》`, exact: true }).dblclick()
    await page.locator('.stage-work-name').filter({ hasText: title }).waitFor(); await openChapter()
    assert.equal(await page.evaluate(() => document.documentElement.dataset.runtime), 'web')
    assert.equal(await editor().getAttribute('contenteditable'), 'true')
    baseline = await editor().textContent(); assert(baseline.startsWith('雨夜的站台只剩一盏灯。'))
    assert(fixturePath && draftId && report.results.ownedProjectIdentity)
    report.results.syntheticBaseline = { units: baseline.length, sha256: hash(baseline) }
  })
  await step('chinese-input-undo-redo-keeps-text', async () => {
    latest = baseline + `手写验收：雨声未停。${stamp}`
    await editor().click(); await editor().press('Control+End')
    await page.keyboard.insertText(latest.slice(baseline.length)); await waitBody(latest)
    assert.equal(await saved().textContent(), '未保存')
    await editor().press('Control+z'); await waitBody(baseline)
    await editor().press('Control+Shift+z'); await waitBody(latest)
    report.results.chineseUnicodeInsert = true; report.results.undoRedoExact = true
  })
  await step('manual-save-reload-exact-persisted-body', async () => {
    await save(latest, baseline)
    await page.reload({ waitUntil: 'domcontentloaded' })
    await page.getByRole('button', { name: `预览《${title}》`, exact: true }).dblclick()
    await page.locator('.stage-work-name').filter({ hasText: title }).waitFor(); await openChapter(); await waitBody(latest)
    assert.equal(await saved().textContent(), '已保存'); baseline = latest
    report.results.explicitManualSaveReloadExact = true
  })
  await step('newer-typing-survives-genuine-delayed-save-response', async () => {
    const first = baseline + '先保存的一句。', newer = first + '保存期间继续输入，不能丢。'
    await editor().click(); await editor().press('Control+End'); await page.keyboard.insertText(first.slice(baseline.length)); await waitBody(first)
    const committed = new Promise(resolve => { committedSignal = resolve })
    commitArrived = new Promise(resolve => { releaseDelay = resolve })
    expectedWrite = first; expectedBaseline = baseline; delayNext = true
    await editor().press('Control+s'); await committed
    await editor().press('Control+End'); await page.keyboard.insertText(newer.slice(first.length)); await waitBody(newer)
    const response = page.waitForResponse(r => operationOf(r.request()) === 'db:draft-update-content')
    releaseDelay(); await response
    await page.waitForFunction(() => document.querySelector('.stage-ch-prose .cm-content')?.textContent?.endsWith('保存期间继续输入，不能丢。'))
    assert.equal(await saved().textContent(), '未保存')
    await save(newer, first); latest = newer
    await page.reload({ waitUntil: 'domcontentloaded' }); await page.getByRole('button', { name: `预览《${title}》`, exact: true }).dblclick()
    await page.locator('.stage-work-name').filter({ hasText: title }).waitFor(); await openChapter(); await waitBody(latest)
    report.results.delayedGenuineCommit = true; report.results.newerTypingRemainsDirty = true; report.results.newerTypingSecondSaveReloadExact = true
    report.results.persistedFinal = { units: latest.length, sha256: hash(latest) }
  })
  assert.equal(writes.length, 3); assert.equal(report.blocked.length, 0); assert.equal(report.errors.length, 0)
  report.status = 'passed'
} catch { if (report.status !== 'timed-out') report.status = 'stopped' }
finally {
  clearTimeout(watchdog); releaseDelay?.(); await context?.close().catch(() => undefined); await Promise.all([...pending])
  report.finishedAt = new Date().toISOString(); report.counts = { passed: report.steps.filter(x => x.status === 'passed').length, failed: report.steps.filter(x => x.status === 'failed').length, requests: report.requests.length, realDraftSaves: writes.length, blocked: report.blocked.length, errors: report.errors.length, paidDispatches: report.requests.filter(x => /:execute|^llm:(?:generate|embed|test|discover)|^mcp:/.test(x.operation ?? '')).length }
  checkpoint(); console.log('EDITOR_WRITE_FINAL', JSON.stringify({ status: report.status, counts: report.counts, results: report.results, privateArtifactDirectory: output })); process.exit(report.status === 'passed' ? 0 : 1)
}
