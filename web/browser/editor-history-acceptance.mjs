import { chromium } from 'playwright'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { createHash } from 'node:crypto'

// Read-only passing subset, not full editor acceptance. No writable branch is shipped.
// No domain APIs/stores/injected mutations; own one CDP context, never close shared Chrome.
const baseURL = process.env.NOVEL_TEST_BROWSER_URL ?? 'http://ai-novel-native-test:6081'
assert.equal(baseURL, 'http://ai-novel-native-test:6081', 'Only the isolated test relay is permitted')
const projectName = '微型规划验收-2026-09-18-1940b'
const stamp = new Date().toISOString().replace(/[:.]/g, '-')
const output = `/workspace/.native-test-data/browser-editor/${stamp}`
fs.mkdirSync(output, { recursive: true, mode: 0o700 })
const report = { status: 'running', startedAt: new Date().toISOString(), steps: [], requests: [], errors: [], blocked: [], screenshots: [], limitations: [], results: {} }
const checkpoint = () => fs.writeFileSync(path.join(output, 'report.json'), JSON.stringify(report, null, 2), { mode: 0o600 })
const hash = value => createHash('sha256').update(value).digest('hex')
// Fail closed: every paid/generation/MCP/global-settings/domain mutation, including draft
// create/update, is denied. Existing fixture open may update its recency metadata; startup ack
// only acknowledges renderer appearance readback in memory. Neither changes global settings.
const readOnlyOperations = new Set([
  'startup:get-state', 'config:get', 'llm:list-models', 'llm:get-default-model',
  'llm:get-default-embedding-model', 'llm:get-stage-models', 'project:recent-list',
  'project:peek-overview', 'db:project-core-get', 'db:character-roster-read',
  'db:blueprint-get-all', 'db:blueprint-get', 'db:draft-list-all', 'db:draft-list',
  'db:draft-get-full', 'db:draft-get-meta', 'db:draft-get-latest',
  'db:review-cycle-list-pending-rechecks', 'db:revision-list', 'db:revision-get-pending',
  'db:review-list', 'db:review-get-full', 'db:review-cycle-get',
  'db:post-process-get-latest-run', 'db:post-process-get-steps',
  'chapter:list-incomplete-deletions', 'fs:read-json', 'fs:list-dir',
])
const lifecycleOnly = new Set(['project:open', 'startup:appearance-ack'])
const allowed = operation => readOnlyOperations.has(operation) || lifecycleOnly.has(operation)
const paid = operation => /:execute|generation.*:execute|^llm:(?:generate|embed|test|discover)|^mcp:/.test(operation)
let context, page, watchdog, activeStep = 'connect-owned-context'
let finalBody, finalVersion
const runtimeDeadline = Date.now() + 540_000
const step = async (name, action) => {
  activeStep = name
  const start = Date.now()
  assert.ok(Date.now() < runtimeDeadline, 'Bounded runtime expired')
  try {
    await action()
    report.steps.push({ name, status: 'passed', elapsedMs: Date.now() - start })
    console.log('EDITOR_STEP', name, 'PASS')
  } catch (error) {
    report.steps.push({ name, status: 'failed', elapsedMs: Date.now() - start })
    // Private diagnostics only; stdout/parent receive step names, status and counts.
    report.errors.push({ type: 'step', name, message: String(error.message).slice(0, 2000) })
    throw error
  } finally { checkpoint() }
}
const shot = async name => {
  const file = path.join(output, `${name}.png`)
  await page.screenshot({ path: file, fullPage: true })
  fs.chmodSync(file, 0o600)
  report.screenshots.push(path.basename(file)); checkpoint()
}
const editor = () => page.locator('.stage-ch-prose .cm-content').filter({ visible: true })
const openChapterOne = async () => {
  await page.locator('button[data-stage="manuscript"]').click()
  await page.locator('.stage-chapters .stage-chapter').nth(0).click()
  await editor().waitFor()
  await page.locator('.stage-ch-page[aria-busy="false"]').filter({ visible: true }).waitFor()
}
const history = async chapter => {
  await page.locator('button[data-resource="history"]').click()
  await page.locator('.stage-hist-chapter').nth(chapter - 1).click()
}
try {
  const browser = await chromium.connectOverCDP(process.env.NOVEL_TEST_CDP ?? 'ws://172.18.0.22:3000?timeout=600000', { timeout: 20_000 })
  context = await browser.newContext({ locale: 'zh-CN', viewport: { width: 1440, height: 1000 }, serviceWorkers: 'block' })
  watchdog = setTimeout(() => { report.status = 'timed-out'; checkpoint(); void context.close() }, 540_000)
  await context.route('**/*', async route => {
    const request = route.request(), url = new URL(request.url())
    if (url.origin !== new URL(baseURL).origin) {
      report.blocked.push({ reason: 'external-origin', method: request.method() }); checkpoint(); return route.abort('blockedbyclient')
    }
    const operation = url.pathname.startsWith('/api/operations/') ? decodeURIComponent(url.pathname.slice('/api/operations/'.length)) : ''
    if (operation && !allowed(operation)) {
      report.blocked.push({ operation, reason: paid(operation) ? 'paid-or-mcp' : 'not-read-only-allowlisted' }); checkpoint(); return route.abort('blockedbyclient')
    }
    // Outside the gateway allow native bootstrap/events and own-client departure cleanup only.
    const transportOnly = request.method() === 'GET' && ['/api/bootstrap', '/api/events'].includes(url.pathname) || request.method() === 'POST' && url.pathname === '/api/release'
    if (url.pathname.startsWith('/api/') && !operation && !transportOnly) {
      report.blocked.push({ path: url.pathname, reason: 'unknown-api-route' }); checkpoint(); return route.abort('blockedbyclient')
    }
    await route.continue()
  })
  page = await context.newPage()
  page.setDefaultTimeout(12_000)
  page.on('dialog', dialog => dialog.dismiss().catch(() => undefined))
  page.on('pageerror', error => { report.errors.push({ type: 'pageerror', message: error.message.slice(0, 1000) }); checkpoint() })
  page.on('requestfailed', request => {
    const pathname = new URL(request.url()).pathname
    if (pathname.startsWith('/api/') && pathname !== '/api/events') { report.errors.push({ type: 'requestfailed', path: pathname, message: request.failure()?.errorText }); checkpoint() }
  })
  page.on('response', response => {
    const pathname = new URL(response.url()).pathname
    if (!pathname.startsWith('/api/')) return
    const operation = pathname.startsWith('/api/operations/') ? decodeURIComponent(pathname.slice('/api/operations/'.length)) : null
    report.requests.push({ operation, status: response.status(), method: response.request().method() }); checkpoint()
  })
  await step('native-runtime-and-dedicated-fixture', async () => {
    await page.goto(baseURL, { waitUntil: 'domcontentloaded', timeout: 30_000 })
    await page.getByRole('button', { name: `预览《${projectName}》`, exact: true }).dblclick()
    await page.locator('.stage-work-name').filter({ hasText: projectName }).waitFor()
    assert.equal(await page.evaluate(() => typeof window.aiNovelAPI), 'undefined', 'Native web must not expose desktop preload')
    assert.equal(await page.evaluate(() => document.documentElement.dataset.runtime), 'web', 'Use real native Web runtime')
  })
  await step('finalized-chapter-readonly-baseline', async () => {
    await openChapterOne()
    await page.getByText('已定稿，只读', { exact: true }).waitFor()
    assert.equal(await editor().getAttribute('contenteditable'), 'false', 'Finalized CodeMirror must be readonly')
    finalBody = await editor().innerText()
    report.results.finalBaseline = { units: finalBody.length, sha256: hash(finalBody) }
    assert.ok(finalBody.length > 0, 'Finalized baseline must be nonempty')
    await page.getByRole('button', { name: /^本章版本：/ }).click()
    const current = await page.locator('[role="menuitemradio"][aria-checked="true"]').innerText()
    finalVersion = Number(current.match(/定稿 v(\d+)/)?.[1])
    assert.ok(finalVersion, 'Identify final version in rendered switcher')
    await page.keyboard.press('Escape')
    await shot('01-final-readonly')
  })
  await step('copy-finalized-chapter-via-rendered-menu', async () => {
    await page.getByRole('button', { name: '更多操作', exact: true }).click()
    await page.getByRole('menuitem', { name: '复制本章正文', exact: true }).click()
    // Success toast verifies only app copy/fallback feedback, not external clipboard equality.
    await page.getByText('已复制本章正文', { exact: true }).waitFor()
    report.results.copyFeedback = 'success'
  })
  await step('empty-chapter-no-manual-first-draft-ui', async () => {
    await page.locator('.stage-chapters .stage-chapter').nth(1).click()
    await page.getByText('这一章还是空白', { exact: true }).waitFor()
    assert.equal(await editor().count(), 0, 'Empty chapter has blueprint, not a writable CodeMirror')
    await page.getByRole('button', { name: '写作此章', exact: true }).waitFor()
    report.limitations.push('Empty chapter has AI-only first-draft entry; no manual first-draft UI was exercised and no generation button was clicked.')
    await shot('02-empty-chapter')
  })
  await step('history-browse-final-and-empty-chapters', async () => {
    await history(2)
    await page.getByText('这一章还没有稿。写出第一稿后会出现在这里。', { exact: true }).waitFor()
    await page.locator('.stage-hist-chapter').nth(0).click()
    await page.locator('.stage-hist-timeline').waitFor()
    const versions = await page.locator('.stage-hist-timeline > li').evaluateAll(items => items.map(item => ({ kind: item.dataset.kind, version: Number(item.querySelector('.stage-hist-version-name').textContent.match(/v(\d+)/)?.[1]) })))
    assert.deepEqual(versions, [{ kind: 'final', version: finalVersion }], 'Stop if this readonly fixture changes; do not enter an untested writable branch')
    report.results.baselineVersions = versions
    await page.locator('.stage-hist-version').filter({ hasText: new RegExp(`定稿 v${finalVersion}(?!\\d)`) }).click()
    const comparison = page.getByRole('article', { name: '与当前稿的差异', exact: true })
    await comparison.waitFor()
    assert.equal(await comparison.getAttribute('aria-readonly'), 'true', 'History diff must be read-only')
    await page.getByText('与当前稿相同', { exact: true }).waitFor()
    assert.equal(await page.getByRole('button', { name: '回退到此版本…', exact: true }).count(), 0, 'Current-only history must not offer a fabricated rollback target')
    await shot('03-final-history-comparison')
  })
  report.limitations.push('Only final v1 and an empty chapter are available. Chinese edit, undo, manual save/reload, autosave and new-draft rollback were not exercised; this is a fixture constraint, not a runtime defect.')
  report.steps.push({ name: 'rollback-edit-save-reload', status: 'unsupported' })
  await step('finalized-authority-preserved-after-acceptance', async () => {
    await openChapterOne()
    await page.getByText('已定稿，只读', { exact: true }).waitFor()
    assert.equal(await editor().getAttribute('contenteditable'), 'false', 'Baseline final remains readonly')
    assert.equal(await editor().innerText(), finalBody, 'Finalized rendered content must remain identical')
    report.results.finalAuthorityPreserved = true
    await shot('04-final-authority-preserved')
  })
  await step('no-paid-or-global-mutation-dispatch', async () => {
    assert.equal(report.requests.filter(item => item.operation && paid(item.operation)).length, 0, 'No paid/MCP operations may reach gateway')
    assert.equal(report.requests.filter(item => item.operation && !allowed(item.operation)).length, 0, 'No domain/global-settings mutation may reach gateway')
    assert.equal(report.requests.filter(item => ['db:draft-create', 'db:draft-update-content'].includes(item.operation)).length, 0, 'No draft writes permitted')
    assert.equal(report.blocked.length, 0, 'Acceptance must not attempt forbidden operations')
    assert.equal(report.errors.length, 0, 'No runtime/request errors expected')
    assert.equal(report.requests.filter(item => item.status >= 400).length, 0, 'No HTTP failure responses expected')
  })
  report.status = 'partial-pass'
} catch (error) {
  report.status = report.status === 'timed-out' ? 'timed-out' : 'stopped'
  report.results.stopStep = activeStep
  if (!report.errors.some(item => item.type === 'step' && item.name === activeStep)) report.errors.push({ type: 'fatal', message: String(error.message).slice(0, 2000) })
  if (page && !page.isClosed()) await shot('stopped').catch(() => undefined)
} finally {
  clearTimeout(watchdog)
  // Only this owned context is closed. Browser transport is disconnected by process exit.
  if (context) await context.close().catch(() => undefined)
  report.finishedAt = new Date().toISOString()
  report.counts = { passed: report.steps.filter(item => item.status === 'passed').length, failed: report.steps.filter(item => item.status === 'failed').length, unsupported: report.steps.filter(item => item.status === 'unsupported').length, requests: report.requests.length, blocked: report.blocked.length, errors: report.errors.length, screenshots: report.screenshots.length }
  checkpoint()
  console.log('EDITOR_FINAL', JSON.stringify({ status: report.status, counts: report.counts, steps: report.steps.map(({ name, status }) => ({ name, status })) }))
  process.exit(report.status === 'stopped' || report.status === 'timed-out' ? 1 : 0)
}
