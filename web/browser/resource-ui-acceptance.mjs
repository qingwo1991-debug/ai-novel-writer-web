import { chromium } from 'playwright'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'

// Read-only native UI navigation on an existing isolated acceptance fixture.
// Never invokes domain operations directly, installs tools, or dispatches models.
const name = process.env.NOVEL_TEST_PROJECT_NAME
assert.ok(name?.startsWith('原生浏览器验收-'), 'Use only a dedicated acceptance fixture')
const expectedTitle = process.env.NOVEL_TEST_PROJECT_TITLE ?? name.replace(/-恢复副本.*$/, '')
const baseURL = process.env.NOVEL_TEST_BROWSER_URL ?? 'http://ai-novel-native-test:6081'
const stamp = new Date().toISOString().replace(/[:.]/g, '-')
const output = process.env.NOVEL_TEST_ARTIFACTS ?? `/workspace/.native-test-data/browser-resources/${stamp}`
fs.mkdirSync(output, { recursive: true })
const report = { status: 'running', startedAt: new Date().toISOString(), runtimeImage: process.env.NOVEL_TEST_RUNTIME_IMAGE ?? 'not-supplied', steps: [], requests: [], errors: [], screenshots: [], limitations: [], noPaidDispatch: true }
const browser = await chromium.connectOverCDP(process.env.NOVEL_TEST_CDP ?? 'ws://172.18.0.22:3000?timeout=120000')
const context = await browser.newContext({ locale: 'zh-CN', viewport: { width: 1440, height: 1000 }, extraHTTPHeaders: { 'X-Novel-User': 'admin' } })
const pending = new Set()
const forbidden = /^(?:llm:generate(?:-stream)?|.*:execute|skills:(?:install|uninstall|pick|inspect)|mcp:(?:connect|disconnect|call-tool)|.*:(?:create|save|delete|remove|commit|update|write|import)(?:-|$))/
await context.route('**/api/**', async route => {
  const pathname = new URL(route.request().url()).pathname
  const operation = pathname.startsWith('/api/operations/') ? decodeURIComponent(pathname.slice('/api/operations/'.length)) : ''
  if (operation && forbidden.test(operation)) {
    report.errors.push({ type: 'blocked-unexpected-mutation-or-paid-dispatch', operation })
    report.noPaidDispatch = false
    await route.abort('blockedbyclient')
  } else await route.continue()
})
const attach = page => {
  page.setDefaultTimeout(10_000)
  page.on('pageerror', error => report.errors.push({ type: 'pageerror', message: error.message.slice(0, 500) }))
  page.on('request', request => { if (new URL(request.url()).pathname.startsWith('/api/') && !request.url().includes('/api/events')) pending.add(request) })
  page.on('requestfinished', request => pending.delete(request))
  page.on('requestfailed', request => pending.delete(request))
  page.on('response', response => {
    const pathname = new URL(response.url()).pathname
    if (!pathname.startsWith('/api/')) return
    report.requests.push({ method: response.request().method(), path: pathname.replace(/\/api\/(downloads|dialogs)\/[^/]+/, '/api/$1/[redacted]'), status: response.status() })
  })
}
let page = await context.newPage()
attach(page)
const settle = async () => {
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))))
  await page.waitForFunction(() => !document.querySelector('[role="dialog"] [role="alert"]'), undefined, { timeout: 1000 })
  // Await currently observed UI reads, excluding the intentionally persistent SSE.
  await Promise.all([...pending].map(request => request.response().then(response => response?.finished()).catch(() => undefined)))
  assert.equal(await page.getByText(/渲染失败|出了点问题|读取.*失败|加载.*失败/).filter({ visible: true }).count(), 0, 'Visible resource error UI')
}
const shot = async label => { const file = path.join(output, `${label}.png`); await page.screenshot({ path: file, fullPage: true }); report.screenshots.push(file) }
const step = async (label, action) => {
  const started = Date.now()
  try { await action(); await settle(); report.steps.push({ label, status: 'passed', elapsedMs: Date.now() - started }); console.log('RESOURCE_STEP', label, 'PASS') }
  catch (error) { report.steps.push({ label, status: 'failed', message: error.message.slice(0, 1000) }); throw error }
}
const open = async () => {
  await page.goto(baseURL, { waitUntil: 'domcontentloaded' })
  await page.getByRole('button', { name: `预览《${name}》`, exact: true }).dblclick()
  await page.locator('.stage-work-name').filter({ hasText: expectedTitle }).waitFor({ state: 'visible' })
}
try {
  await step('open-existing-fixture-with-native-runtime', async () => { await open(); assert.equal(await page.evaluate(() => typeof window.aiNovelAPI), 'undefined') })
  if (process.env.NOVEL_TEST_LIFECYCLE_ONLY !== '1') {
  await step('architecture-read-only', async () => { await page.locator('button[data-stage="architecture"]').click(); await page.locator('.arch-page').waitFor({ state: 'visible' }); await page.locator('.arch-page:not([data-loading])').waitFor({ state: 'visible' }); await shot('01-architecture') })
  await step('characters-and-relationship-graph-read-only', async () => {
    await page.getByRole('button', { name: '打开角色名单', exact: true }).click()
    await page.locator('.cast-page').waitFor({ state: 'visible' })
    await page.getByRole('button', { name: '关系图谱', exact: true }).click()
    await page.locator('.cast-graph').waitFor({ state: 'visible' })
    await shot('02-characters-graph')
  })
  await step('blueprint-read-only', async () => { await page.locator('button[data-stage="blueprint"]').click(); await page.locator('.stage-bp').waitFor({ state: 'visible' }); await shot('03-blueprint') })
  await step('manuscript-read-only', async () => { await page.locator('button[data-stage="manuscript"]').click(); await page.locator('.stage-chapter').first().click(); await page.getByText('已定稿，只读', { exact: true }).waitFor({ state: 'visible' }); await shot('04-manuscript') })
  await step('plot-tree-and-threads-read-only', async () => {
    await page.locator('button[data-resource="plot"]').click()
    await page.locator('.stage-plot').waitFor({ state: 'visible' })
    await page.getByRole('button', { name: '伏笔线索', exact: true }).click()
    await page.getByRole('button', { name: '剧情树', exact: true }).click()
    await shot('05-plot')
  })
  await step('knowledge-read-only', async () => { await page.locator('button[data-resource="knowledge"]').click(); await page.locator('.stage-kb-title').waitFor({ state: 'visible' }); await shot('06-knowledge') })
  await step('version-history-read-only', async () => { await page.locator('button[data-resource="history"]').click(); await page.locator('.stage-hist').waitFor({ state: 'visible' }); if (await page.locator('.stage-hist--empty').isVisible()) report.limitations.push('Version history rendered its empty-state because the imported fixture has no chapter blueprints; historical comparison rows were not exercised.'); await shot('07-history') })
  await step('prompts-and-skills-read-only', async () => {
    await page.locator('.stage-spine-foot').getByRole('button', { name: '设置', exact: true }).click()
    await page.locator('.stage-set-nav').getByRole('button', { name: '提示词模板', exact: true }).click()
    await page.locator('.stage-set-title').filter({ hasText: '提示词模板' }).waitFor({ state: 'visible' })
    await shot('08-prompts')
    await page.locator('.stage-set-nav').getByRole('button', { name: '写作 Skills', exact: true }).click()
    await page.locator('#writing-skill-library-title').waitFor({ state: 'visible' })
    await shot('09-skills')
    const mcp = page.getByRole('button', { name: /^MCP(?:\s|$)/ })
    if (await mcp.count()) { await mcp.first().click(); await shot('10-mcp') } else report.limitations.push('No MCP navigation entry exists in the native stage shell; not claimed as UI-covered.')
  })
  }
  await step('idle-pagehide-releases-project-for-fresh-tab', async () => {
    const initialId = await page.evaluate(() => sessionStorage.getItem('ai-novel-writer-browser-client-id'))
    // Request interception can pause an unloading document's keepalive fetch;
    // stop routing before testing the browser's genuine pagehide lifecycle.
    await context.unrouteAll({ behavior: 'wait' })
    let releaseHTTP200 = false, releaseRequestObserved = false
    const onRequest = request => { if (new URL(request.url()).pathname === '/api/release') releaseRequestObserved = true }
    const onResponse = response => { if (new URL(response.url()).pathname === '/api/release' && response.status() === 200) releaseHTTP200 = true }
    context.on('request', onRequest); context.on('response', onResponse)
    await page.goto('about:blank')
    // Product idle-release grace is 1500ms; this is a bounded behavior assertion,
    // not polling or waiting for an unrelated background job.
    await new Promise(resolve => setTimeout(resolve, 1800))
    await page.close()
    page = await context.newPage(); attach(page)
    await open()
    const reopenedId = await page.evaluate(() => sessionStorage.getItem('ai-novel-writer-browser-client-id'))
    assert.ok(initialId && reopenedId && initialId !== reopenedId)
    await shot('11-fresh-tab-reopened')
    context.off('request', onRequest); context.off('response', onResponse)
    report.lifecycle = { releaseRequestObserved, releaseHTTP200, freshTabDifferentClient: true, reopenedAfterMs: 1800, reopenedWithout409: true }
    if (!releaseHTTP200) report.limitations.push('Unloading keepalive response was not observable through CDP; lifecycle acceptance relies on actual fresh-client UI reopen without HTTP 409 after 1800ms, not on an invented release status.')
  })
  assert.deepEqual(report.errors, [])
  assert.deepEqual(report.requests.filter(request => request.status >= 400), [])
  report.status = 'passed'
} catch (error) {
  report.status = 'failed'
  report.failure = error.message.slice(0, 1500)
  await shot('failure').catch(() => undefined)
  process.exitCode = 1
} finally {
  // Trigger real pagehide on our remaining page; never close user contexts.
  if (!page.isClosed()) await page.goto('about:blank').catch(() => undefined)
  await context.close()
  await browser.close()
  report.finishedAt = new Date().toISOString()
  fs.writeFileSync(path.join(output, 'report.json'), JSON.stringify(report, null, 2))
  console.log('RESOURCE_REPORT', JSON.stringify({ status: report.status, steps: report.steps, errors: report.errors, apiErrors: report.requests.filter(request => request.status >= 400), lifecycle: report.lifecycle, output, failure: report.failure }))
}
