import { chromium } from 'playwright'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { randomBytes } from 'node:crypto'

// UI-only regression. Never point this script at production or close shared Chrome.
const baseURL = process.env.NOVEL_TEST_BROWSER_URL ?? 'http://ai-novel-native-test:6081'
assert.equal(baseURL, 'http://ai-novel-native-test:6081', 'Only the isolated regression relay is allowed')
const cdp = process.env.NOVEL_TEST_CDP ?? 'ws://172.18.0.22:3000?timeout=600000'
assert.equal(new URL(cdp).hostname, '172.18.0.22', 'Use only the already-managed test CDP browser')
const stamp = new Date().toISOString().replace(/[:.]/g, '-') + '-' + randomBytes(3).toString('hex')
const title = `Native修复验收-${stamp}`
const output = path.resolve(process.env.NOVEL_TEST_ARTIFACT_ROOT ?? '/workspace/.native-test-data/browser-library', stamp)
assert.match(output, /\/\.native-test-data\/browser-library\/[^/]+$/, 'Private artifact root must remain in the ignored test directory')
fs.mkdirSync(output, { recursive: true, mode: 0o700 }); fs.chmodSync(output, 0o700)
const report = { status: 'running', startedAt: new Date().toISOString(), fixtureTitle: title, steps: [], requests: [], blocked: [], errors: [], results: {}, limitations: ['Isolated copied data and trusted test relay, not production password acceptance.', 'No project deletion, AI generation, chapter edits or global settings changes are permitted.'] }
const checkpoint = () => { fs.writeFileSync(path.join(output, 'report.json'), JSON.stringify(report, null, 2), { mode: 0o600 }); fs.chmodSync(path.join(output, 'report.json'), 0o600) }
const safeErrors = error => String(error?.name ?? 'Error')
const readOnly = new Set(['startup:get-state', 'startup:appearance-ack', 'config:get', 'llm:list-models', 'llm:get-default-model', 'llm:get-default-embedding-model', 'llm:get-stage-models', 'project:recent-list', 'project:peek-overview', 'project:overview-current', 'project:get-runtime-context', 'db:project-core-get', 'db:character-roster-read', 'db:blueprint-get-all', 'db:blueprint-get', 'db:draft-list-all', 'db:draft-list', 'db:draft-get-full', 'db:draft-get-meta', 'db:draft-get-latest', 'db:review-cycle-list-pending-rechecks', 'db:revision-list', 'db:revision-get-pending', 'db:review-list', 'db:post-process-get-latest-run', 'db:post-process-get-steps', 'chapter:list-incomplete-deletions', 'fs:read-json', 'fs:list-dir'])
let phase = 'bootstrap', fixturePath, fixtureId, ownerLease, contextA, contextB, pageA, watchdog
const pendingResponses = new Set(), opens = []
const opOf = request => { const pathname = new URL(request.url()).pathname; return pathname.startsWith('/api/operations/') ? decodeURIComponent(pathname.slice('/api/operations/'.length)) : null }
const argsOf = request => { try { return request.postDataJSON()?.args ?? [] } catch { return [] } }
const block = async (route, operation, reason) => { report.blocked.push({ operation, reason }); checkpoint(); await route.abort('blockedbyclient') }
async function setupContext(label) {
  const context = await browser.newContext({ locale: 'zh-CN', viewport: { width: 1440, height: 1000 }, serviceWorkers: 'block' })
  await context.route('**/*', async route => {
    const request = route.request(), url = new URL(request.url()), operation = opOf(request), args = argsOf(request)
    if (url.origin !== baseURL) return block(route, null, 'non-isolated-origin')
    let allowed = operation && readOnly.has(operation)
    if (operation === 'dialog:select-folder') allowed = args[0] === 'project-create' && label === 'owner' && phase === 'create' || args[0] === 'project-open' && ['chooser-reopen', 'conflict'].includes(phase)
    if (operation === 'project:create') allowed = label === 'owner' && phase === 'create' && args[0]?.name === title && typeof args[0]?.parentGrantId === 'string'
    if (operation === 'project:recent-remove') allowed = label === 'owner' && phase === 'remove' && fixturePath && args[0] === fixturePath
    if (operation === 'project:open') {
      await Promise.all([...pendingResponses])
      allowed = Boolean(fixturePath) && ['create', 'chooser-reopen', 'conflict', 'owner-reload'].includes(phase) && (args[0] === fixturePath || phase !== 'create' && typeof args[0]?.grantId === 'string')
    }
    if (operation === 'db:close') allowed = label === 'owner' && Boolean(fixturePath) && args[0] === fixturePath
    if (operation && !allowed) return block(route, operation, 'operation-not-explicitly-allowed')
    if (!operation && url.pathname.startsWith('/api/')) {
      const ordinary = request.method() === 'GET' && ['/api/bootstrap', '/api/events', '/api/library'].includes(url.pathname) || request.method() === 'POST' && url.pathname === '/api/release'
      let dialog = false
      if (request.method() === 'POST' && url.pathname.startsWith('/api/dialogs/')) {
        let data; try { data = request.postDataJSON() } catch { data = {} }
        dialog = data?.canceled === true || phase === 'create' && label === 'owner' && data?.useLibrary === true || ['chooser-reopen', 'conflict'].includes(phase) && fixturePath && data?.projectPath === fixturePath
      }
      if (!ordinary && !dialog) return block(route, null, 'unknown-or-unowned-api-route')
    }
    await route.continue()
  })
  context.on('page', page => {
    page.setDefaultTimeout(12_000)
    page.on('pageerror', () => { report.errors.push({ context: label, type: 'pageerror' }); checkpoint() })
    page.on('response', response => {
      const request = response.request(), operation = opOf(request), url = new URL(response.url())
      if (!url.pathname.startsWith('/api/')) return
      report.requests.push({ context: label, operation, method: request.method(), status: response.status() }); checkpoint()
      if (['project:create', 'project:open'].includes(operation) && response.ok()) {
        const task = response.json().then(body => {
          if (operation === 'project:create' && body.result?.success && argsOf(request)[0]?.name === title) {
            const returned = body.result.projectPath
            assert.equal(typeof returned, 'string'); assert.equal(path.basename(returned), title)
            fixturePath = returned
          }
          const project = body.result?.project
          if (!project || project.name !== title || project.path !== fixturePath) return
          fixtureId ??= project.id
          if (project.sessionLease) opens.push({ context: label, lease: project.sessionLease, id: project.id })
        }).catch(() => undefined)
        pendingResponses.add(task); void task.finally(() => pendingResponses.delete(task))
      }
    })
  })
  return context
}
const browser = await chromium.connectOverCDP(cdp, { timeout: 20_000 })
async function step(name, action) {
  const started = Date.now()
  try { await action(); report.steps.push({ name, status: 'passed', elapsedMs: Date.now() - started }); console.log('LIBRARY_STEP', name, 'PASS') }
  catch (error) { report.steps.push({ name, status: 'failed', elapsedMs: Date.now() - started }); report.errors.push({ type: 'step', name, errorType: safeErrors(error) }); throw error }
  finally { checkpoint() }
}
const shelfBook = page => page.getByRole('button', { name: `预览《${title}》`, exact: true })
async function library(page) {
  if (await page.getByRole('button', { name: '新建作品', exact: true }).isVisible().catch(() => false)) return
  await page.getByRole('button', { name: `《${title}》作品菜单`, exact: true }).click()
  await page.getByRole('menuitem', { name: '回到书房', exact: true }).click()
  await page.getByRole('button', { name: '新建作品', exact: true }).waitFor()
}
async function chooser(page) {
  await page.getByRole('button', { name: '打开作品', exact: true }).click()
  const select = page.getByRole('dialog').locator('select')
  await select.waitFor()
  await page.waitForFunction(name => [...document.querySelectorAll('[role="dialog"] select option')].some(o => o.textContent === name), title)
  await select.selectOption({ label: title })
  await page.getByRole('dialog').getByRole('button', { name: '确认', exact: true }).click()
}
try {
  watchdog = setTimeout(() => { report.status = 'timed-out'; checkpoint(); void contextA?.close(); void contextB?.close() }, 300_000)
  contextA = await setupContext('owner'); pageA = await contextA.newPage()
  await step('isolated-native-library', async () => {
    await pageA.goto(baseURL, { waitUntil: 'domcontentloaded' })
    await pageA.getByRole('button', { name: '新建作品', exact: true }).waitFor()
    assert.equal(await pageA.evaluate(() => document.documentElement.dataset.runtime), 'web')
    assert.equal(await pageA.evaluate(() => typeof window.aiNovelAPI), 'undefined')
  })
  await step('new-project-auto-server-location-and-ui-create', async () => {
    phase = 'create'
    await pageA.getByRole('button', { name: '新建作品', exact: true }).click()
    const form = pageA.getByRole('dialog', { name: '新建作品', exact: true })
    await form.waitFor()
    assert.equal(await form.getByLabel('保存位置', { exact: true }).inputValue(), '服务器书库')
    assert.equal(await form.getByRole('button', { name: /^(?:选择|Choose)$/ }).count(), 0)
    assert.equal(await form.getByRole('button', { name: '创建作品', exact: true }).isDisabled(), true)
    await form.getByLabel('作品名', { exact: true }).fill(title)
    const auto = pageA.getByRole('dialog').filter({ has: pageA.getByRole('button', { name: '继续', exact: true }) })
    if (await auto.isVisible().catch(() => false)) await auto.getByRole('button', { name: '继续', exact: true }).click()
    await form.getByRole('button', { name: '创建作品', exact: true }).waitFor({ state: 'visible' })
    await pageA.waitForFunction(() => [...document.querySelectorAll('button')].some(b => b.textContent === '创建作品' && !b.disabled))
    const created = pageA.waitForResponse(r => opOf(r.request()) === 'project:create' && r.ok())
    await form.getByRole('button', { name: '创建作品', exact: true }).click(); await created
    await pageA.locator('.stage-work-name').filter({ hasText: title }).waitFor()
    await Promise.all([...pendingResponses]); assert(fixturePath && fixtureId)
    report.results.serverLibraryAutomatic = true; report.results.createdViaUi = true
  })
  await step('remove-own-shelf-entry-without-deleting-book', async () => {
    await library(pageA); await shelfBook(pageA).waitFor(); phase = 'remove'
    await pageA.getByRole('button', { name: `《${title}》更多操作`, exact: true }).click()
    await pageA.getByRole('menuitem', { name: '从最近项目移除…', exact: true }).click()
    const remove = pageA.getByRole('dialog', { name: `从书架移除《${title}》？`, exact: true })
    await remove.getByRole('button', { name: '移除记录', exact: true }).click()
    await shelfBook(pageA).waitFor({ state: 'hidden' })
    await pageA.reload({ waitUntil: 'domcontentloaded' }); await pageA.getByRole('button', { name: '新建作品', exact: true }).waitFor()
    assert.equal(await shelfBook(pageA).count(), 0)
    report.results.removalSurvivesReload = true
  })
  await step('open-chooser-restores-removed-book-with-files-retained', async () => {
    phase = 'chooser-reopen'; await chooser(pageA)
    await pageA.locator('.stage-work-name').filter({ hasText: title }).waitFor()
    await Promise.all([...pendingResponses]); assert.equal(opens.at(-1)?.id, fixtureId)
    ownerLease = opens.at(-1)?.lease; assert(ownerLease)
    await library(pageA); await shelfBook(pageA).waitFor()
    report.results.removedBookStillInChooser = true; report.results.originalIdentityRestored = true; report.results.filesNotDeleted = true
    await shelfBook(pageA).dblclick(); await pageA.locator('.stage-work-name').filter({ hasText: title }).waitFor()
    await Promise.all([...pendingResponses]); ownerLease = opens.at(-1)?.lease
  })
  await step('second-owned-context-conflict-no-silent-takeover', async () => {
    phase = 'conflict'; contextB = await setupContext('contender'); const pageB = await contextB.newPage()
    await pageB.goto(baseURL, { waitUntil: 'domcontentloaded' }); await shelfBook(pageB).waitFor()
    const conflict = pageB.waitForResponse(r => opOf(r.request()) === 'project:open' && r.status() === 409)
    await shelfBook(pageB).dblclick(); await conflict
    assert.equal(await pageB.locator('button[data-stage="setup"]').isVisible().catch(() => false), false)
    assert.equal(opens.filter(x => x.context === 'contender').length, 0)
    report.results.secondContextConflict409 = true
  })
  await step('first-context-reload-retains-own-lease', async () => {
    phase = 'owner-reload'; await pageA.reload({ waitUntil: 'domcontentloaded' })
    await pageA.getByRole('button', { name: '新建作品', exact: true }).waitFor(); await shelfBook(pageA).dblclick()
    await pageA.locator('.stage-work-name').filter({ hasText: title }).waitFor(); await Promise.all([...pendingResponses])
    assert.equal(opens.at(-1)?.context, 'owner'); assert.equal(opens.at(-1)?.id, fixtureId); assert.equal(opens.at(-1)?.lease, ownerLease)
    assert.equal(report.requests.filter(x => x.context === 'owner' && x.status === 409).length, 0)
    report.results.ownerLeaseUnchanged = true; report.results.ownerReloadOpensSafely = true
  })
  assert.equal(report.blocked.length, 0); assert.equal(report.errors.length, 0)
  report.status = 'passed'
} catch { if (report.status !== 'timed-out') report.status = 'stopped' }
finally {
  clearTimeout(watchdog)
  // Only owned contexts; NEVER browser.close(), never stop managed gateway/relay/browser.
  await contextB?.close().catch(() => undefined); await contextA?.close().catch(() => undefined)
  await Promise.all([...pendingResponses]); report.finishedAt = new Date().toISOString()
  report.counts = { passed: report.steps.filter(x => x.status === 'passed').length, failed: report.steps.filter(x => x.status === 'failed').length, blocked: report.blocked.length, errors: report.errors.length, requests: report.requests.length, paidDispatches: report.requests.filter(x => /:execute|^llm:(?:generate|embed|test|discover)|^mcp:/.test(x.operation ?? '')).length }
  checkpoint(); console.log('LIBRARY_FINAL', JSON.stringify({ status: report.status, counts: report.counts, results: report.results, privateArtifactDirectory: output })); process.exit(report.status === 'passed' ? 0 : 1)
}
