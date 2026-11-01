import { chromium } from 'playwright'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'

// Isolated real UI acceptance. Never import stores or invoke domain mutation APIs.
const baseURL = process.env.NOVEL_TEST_BROWSER_URL ?? 'http://ai-novel-native-test:6081'
const stamp = process.env.NOVEL_PLANNING_STAMP ?? new Date().toISOString().replace(/[:.]/g, '-')
const name = process.env.NOVEL_TEST_PROJECT_NAME ?? `微型规划验收-${stamp}`
const resumeClient = process.env.NOVEL_PLANNING_CLIENT_ID
const output = process.env.NOVEL_TEST_ARTIFACTS ?? `/workspace/.native-test-data/browser-planning/${stamp}`
const mode = process.env.NOVEL_PLANNING_MODE ?? 'architecture'
fs.mkdirSync(output, { recursive: true })
const report = { name, baseURL, mode, output, startedAt: new Date().toISOString(), steps: [], requests: [], errors: [], screenshots: [], approvals: [], results: {} }
const browser = await chromium.connectOverCDP(process.env.NOVEL_TEST_CDP ?? 'ws://172.18.0.22:3000?timeout=600000')
const context = await browser.newContext({ locale: 'zh-CN', viewport: { width: 1440, height: 1000 }, acceptDownloads: true, extraHTTPHeaders: { 'X-Novel-User': 'admin' } })
let page = await context.newPage()
if (resumeClient) await page.addInitScript(id => {
  try { if (!sessionStorage.getItem('ai-novel-writer-browser-client-id')) sessionStorage.setItem('ai-novel-writer-browser-client-id', id) }
  catch { /* about:blank and other opaque origins may deny storage access. */ }
}, resumeClient)
page.setDefaultTimeout(15000)
let failed = false
const checkpoint = () => fs.writeFileSync(path.join(output, 'report.json'), JSON.stringify(report, null, 2))
page.on('pageerror', error => { report.errors.push({ type: 'pageerror', message: error.message.slice(0, 1000) }); checkpoint() })
page.on('response', async response => {
  const url = new URL(response.url())
  if (!url.pathname.startsWith('/api/')) return
  const entry = { path: url.pathname, status: response.status(), method: response.request().method() }
  if (url.pathname.includes('/api/operations/')) {
    try {
      const body = response.request().postDataJSON()
      entry.projectId = body?.projectSession?.projectId
      const request = body?.args?.[0]
      if (request?.operation) entry.operation = request.operation
      if (request?.modelId) entry.modelId = request.modelId
      if (request?.task?.purpose) entry.purpose = request.task.purpose
      const result = await response.json()
      const value = result?.result
      if (url.pathname.endsWith('llm%3Alist-models') && Array.isArray(value)) report.results.savedModels = value.map(model => ({ id: model.id, name: model.name, modelName: model.modelName, protocol: model.protocol }))
      if (url.pathname.endsWith('llm%3Aget-stage-models')) report.results.stageModels = value
      if (value?.handle) entry.handle = value.handle
      if (value?.run?.handle) entry.handle = value.run.handle
      if (value?.outcome) entry.outcome = { status: value.outcome.status, finishReason: value.outcome.finishReason, usage: value.outcome.usage, failureCode: value.outcome.receipt?.failureCode, attemptId: value.outcome.receipt?.attemptId }
      if (value?.ledger) entry.ledger = value.ledger
      if (url.pathname.endsWith('db%3Areview-cycle-get') && value) {
        report.results.authoritativeCycle = { cycleId: value.cycleId, reviewId: value.reviewId, revisionStatus: value.revisionStatus,
          recheckCount: value.recheckCount, recheckDisposition: value.recheckDisposition,
          findingStatuses: value.findings?.map(finding => ({ kind: finding.kind, status: finding.status })) }
      }
      if (url.pathname.endsWith('db%3Arevision-merge') && value?.receipt) entry.mergeReceipt = value.receipt
      if (response.status() >= 400 || result?.error) entry.error = result?.error
    } catch { /* A navigated response may have no readable body. */ }
  }
  report.requests.push(entry); checkpoint()
})
page.on('requestfailed', request => {
  const pathname = new URL(request.url()).pathname
  if (pathname.startsWith('/api/') && pathname !== '/api/events') report.errors.push({ type: 'requestfailed', path: pathname, message: request.failure()?.errorText })
})
function observeNextPage(target) {
  target.setDefaultTimeout(15000)
  target.on('pageerror', error => { report.errors.push({ type: 'pageerror', message: error.message.slice(0, 1000) }); checkpoint() })
  target.on('response', response => {
    const url = new URL(response.url())
    if (url.pathname.startsWith('/api/')) { report.requests.push({ path: url.pathname, status: response.status(), method: response.request().method(), afterDeparture: true }); checkpoint() }
  })
}
async function screenshot(label) {
  const file = path.join(output, `${label}.png`)
  await page.screenshot({ path: file, fullPage: true }); report.screenshots.push(file); checkpoint()
}
async function step(label, action) {
  const start = Date.now()
  try { const result = await action(); report.steps.push({ label, status: 'passed', elapsedMs: Date.now() - start }); console.log('PLANNING_STEP', label, 'PASS'); checkpoint(); return result }
  catch (error) { report.steps.push({ label, status: 'failed', elapsedMs: Date.now() - start, message: error.message }); checkpoint(); throw error }
}
async function body() { return page.locator('body').innerText() }
async function waitUi(locator, label, timeout = 240000) {
  const start = Date.now()
  while (Date.now() - start < timeout) {
    if (await locator.isVisible()) return
    const failure = page.locator('article.run-card[data-state="failed"]')
    if (await failure.count()) throw new Error(`${label}: ${await failure.first().innerText()}`)
    await page.waitForTimeout(1000)
  }
  throw new Error(`${label}: bounded wait expired, no automatic retry`)
}
async function closeSheet() {
  const close = page.getByRole('button', { name: '收起运行侧页', exact: true })
  if (await close.isVisible()) await close.click()
}
async function waitWorkflow(label, timeout = 240000) {
  const start = Date.now()
  while (Date.now() - start < timeout) {
    const confirmation = page.getByRole('region', { name: '需要你确认', exact: true })
    if (await confirmation.isVisible()) {
      const selects = confirmation.locator('select[aria-label^="采用方式："]')
      for (let i = 0; i < await selects.count(); i++) await selects.nth(i).selectOption('create')
      const text = await confirmation.innerText()
      const button = confirmation.locator('button.run-confirm-button')
      if (await button.isEnabled()) { report.approvals.push({ label, text: text.slice(0, 1200), at: new Date().toISOString() }); await button.click(); checkpoint() }
    }
    const active = page.locator('article.run-card[data-state]').filter({ hasText: report.results.expectedWorkflowTitle ?? '' })
    const state = await active.evaluateAll(cards => cards.map(card => ({ state: card.getAttribute('data-state'), text: card.innerText.slice(0, 5000) })))
    report.results.liveWorkflow = { label, states: state, at: new Date().toISOString() }; checkpoint()
    if (state.some(card => card.state === 'failed')) throw new Error(`${label}: workflow failed: ${state.filter(card => card.state === 'failed').map(card => card.text).join('\n')}`)
    const history = await page.locator('.run-history-row').filter({ hasText: report.results.expectedWorkflowTitle ?? '' }).allTextContents()
    if (history.some(row => /未完成/.test(row))) throw new Error(`${label}: failed history entry: ${history.join('\n')}`)
    if ((state.some(card => card.state === 'completed') || history.some(row => /已完成/.test(row))) && !state.some(card => ['running', 'waiting', 'paused', 'cancelling'].includes(card.state))) {
      report.results[label] = { state, history }; await screenshot(label); return
    }
    await page.waitForTimeout(1000)
  }
  throw new Error(`${label}: bounded workflow wait expired; no request will be retried automatically`)
}
try {
  await step('native-browser-ready', async () => {
    await page.goto(baseURL, { waitUntil: 'domcontentloaded' })
    await page.getByRole('button', { name: '新建作品', exact: true }).waitFor()
    assert.equal(await page.evaluate(() => typeof window.aiNovelAPI), 'undefined')
    assert.equal(await page.evaluate(() => document.documentElement.dataset.runtime), 'web')
    report.results.browserClientId = await page.evaluate(() => sessionStorage.getItem('ai-novel-writer-browser-client-id'))
    report.results.cryptoCapabilities = await page.evaluate(() => ({ secureContext: isSecureContext, randomUUID: typeof crypto.randomUUID, getRandomValues: typeof crypto.getRandomValues, subtle: typeof crypto.subtle }))
    await screenshot('00-native-lobby')
  })
  if (mode === 'architecture') {
    await step('create-tiny-project', async () => {
      if (process.env.NOVEL_PLANNING_REOPEN === '1') {
        await page.getByRole('button', { name: `预览《${name}》`, exact: true }).dblclick()
        await page.locator('button[data-stage="setup"]').click()
        await page.locator('textarea.stage-setup-outline-input').waitFor()
        return
      }
      await page.getByRole('button', { name: '新建作品', exact: true }).click()
      const dialog = page.getByRole('dialog', { name: '新建作品', exact: true })
      await dialog.getByLabel('作品名', { exact: true }).fill(name)
      await dialog.getByRole('button', { name: '选择', exact: true }).click()
      const chooser = page.getByRole('dialog').filter({ hasText: '作品保存在服务器书库' })
      await chooser.waitFor(); await chooser.getByRole('button', { name: '继续', exact: true }).click()
      await dialog.getByRole('button', { name: '创建作品', exact: true }).click()
      await page.locator('textarea.stage-setup-outline-input').waitFor()
      await screenshot('01-project')
    })
    await step('save-two-chapter-setup', async () => {
      await page.locator('textarea.stage-setup-outline-input').fill('这是一部严格只有两章、每章约400字的微型现实悬疑故事，不增设第三章，不铺设长篇支线。第一章：雨夜旧车站值班员林岚收到一封没有署名的信，信中要求她在午夜前找到失踪父亲留下的铜钥匙。她与退休修表匠周叔核对钟表，发现车站时钟快了五分钟，最终在候车室长椅背后找到钥匙及编号17的保管柜线索。第二章：林岚打开17号柜，发现父亲为防洪巡查留下的水位记录及写给她的告别信，澄清失踪并非阴谋，而是父亲为救人遇难。她将记录交给镇档案室，与周叔修好车站时钟，释怀并结束故事。只有林岚与周叔两名主要角色，现实世界，无超自然能力，无隐藏组织。叙事克制清晰，第三人称有限视角，关键事实不反转，结局完整。')
      const genre = page.getByLabel(/^类型/)
      const genres = await genre.locator('option').evaluateAll(items => items.map(item => ({ value: item.value, text: item.text })))
      await genre.selectOption(genres.find(item => /悬疑/.test(item.text)).value)
      await page.getByLabel('总章数', { exact: true }).fill('2')
      await page.getByLabel('每章字数', { exact: true }).fill('400')
      const audience = page.getByLabel(/^目标读者/)
      const options = await audience.locator('option').evaluateAll(items => items.filter(item => !item.disabled).map(item => ({ value: item.value, text: item.text })))
      await audience.selectOption(options.find(item => item.value)?.value)
      await page.getByRole('button', { name: '下一步：故事架构', exact: true }).click()
      await page.getByRole('button', { name: 'AI 生成架构', exact: true }).waitFor()
      await screenshot('02-saved-setup')
    })
    await step('real-gpt-architecture', async () => {
      report.results.expectedWorkflowTitle = '生成故事架构'
      await page.getByRole('button', { name: 'AI 生成架构', exact: true }).click()
      const dialog = page.getByRole('dialog', { name: '生成架构', exact: true })
      await dialog.getByLabel('每章大纲目标字数', { exact: true }).fill('100')
      await dialog.getByRole('button', { name: /作者指导/ }).click()
      const inputs = dialog.locator('.arch-guidance-input')
      for (let i = 0; i < await inputs.count(); i++) await inputs.nth(i).fill('严格两章微型故事。保持两名主要角色、现实背景、不新增组织或超自然。内容精炼：前提及世界观各150字以内，角色只林岚与周叔，大纲每章约100字。遵守原始两章事实与结局。')
      await screenshot('03-architecture-confirm')
      await dialog.getByRole('button', { name: '开始生成 4 项', exact: true }).click()
      await page.locator('article.run-card').first().waitFor()
      await waitWorkflow('04-real-gpt-architecture', 300000)
    })
    await step('architecture-persisted-after-refresh', async () => {
      await closeSheet(); await page.reload({ waitUntil: 'domcontentloaded' })
      await page.getByRole('button', { name: `预览《${name}》`, exact: true }).dblclick()
      await page.locator('button[data-stage="architecture"]').click()
      await page.waitForFunction(() => document.querySelectorAll('.arch-section[data-state="ready"]').length === 4, undefined, { timeout: 15000 })
      const sections = await page.locator('.arch-section').evaluateAll(items => items.map(item => ({ section: item.dataset.section, state: item.dataset.state, text: item.innerText })))
      report.results.persistedArchitecture = sections
      assert.equal(sections.length, 4)
      assert.ok(sections.every(item => item.state === 'ready'), JSON.stringify(sections))
      await screenshot('05-persisted-architecture')
    })
    await step('real-gpt-blueprints', async () => {
      report.results.expectedWorkflowTitle = '章节蓝图'
      await page.locator('button[data-stage="blueprint"]').click()
      await page.getByRole('button', { name: 'AI 生成蓝图', exact: true }).click()
      const dialog = page.getByRole('dialog', { name: 'AI 生成蓝图', exact: true })
      await dialog.getByLabel('本次蓝图章数', { exact: true }).fill('2')
      await dialog.getByLabel('每章蓝图目标字数', { exact: true }).fill('100')
      await dialog.getByLabel(/节奏与风格指导/).fill('严格按两章大纲。每章约400字正文的紧凑蓝图，计划约100字；仅林岚、周叔；第一章找到钥匙，第二章揭晓父亲救人真相并收束。')
      await screenshot('06-blueprint-confirm')
      await dialog.getByRole('button', { name: '开始生成', exact: true }).click()
      await page.locator('article.run-card').first().waitFor()
      await waitWorkflow('07-real-gpt-blueprints', 240000)
    })
  } else {
    await step('reopen-isolated-project', async () => {
      await page.getByRole('button', { name: `预览《${name}》`, exact: true }).dblclick()
      await screenshot('resume-project')
    })
  }
  if (mode === 'inspect' || mode === 'finalize') {
    await step('inspect-authoritative-merged-cycle-read-only', async () => {
      await page.locator('button[data-stage="manuscript"]').click()
      await page.locator('.stage-chapter').first().click()
      await waitUi(page.getByRole('button', { name: '定稿', exact: true }), 'Open already merged chapter', 30000)
      await page.waitForFunction(() => document.querySelector('.stage-ch-hint')?.textContent?.includes('修稿已合并'), undefined, { timeout: 15000 })
      assert.equal(report.results.authoritativeCycle?.revisionStatus, 'merge-committed')
      assert.equal(report.results.authoritativeCycle?.recheckDisposition, 'not-required', 'Do not skip a required or unknown recheck')
      assert.equal(report.requests.filter(item => /execute/.test(decodeURIComponent(item.path))).length, 0)
      report.results.recheckNotRequired = true
      await screenshot('14-merged-authoritative-not-required')
    })
  }
  if (mode !== 'probe' && mode !== 'inspect') {
    if (mode !== 'finalize') {
    await step('blueprints-persisted-before-authoring', async () => {
      await closeSheet()
      await page.locator('button[data-stage="blueprint"]').click()
      await page.waitForFunction(() => document.querySelectorAll('.stage-bp-plan-item').length === 2)
      report.results.persistedBlueprints = await page.locator('.stage-bp-plan-item').allTextContents()
      await screenshot('08-blueprints-persisted')
    })
    await step('real-gemini-chapter-one-draft', async () => {
      report.results.expectedWorkflowTitle = '第 1 章'
      await page.getByRole('button', { name: '本次写作选项', exact: true }).click()
      const options = page.getByRole('dialog', { name: '本次写作选项', exact: true })
      const model = options.getByLabel(/本次模型/)
      const models = await model.locator('option').evaluateAll(items => items.map(item => ({ value: item.value, text: item.text })))
      report.results.draftModelOptions = models
      const gemini = models.find(item => /Gemini.*3\.8.*Flash/i.test(item.text)) ?? models.find(item => /Gemini/i.test(item.text))
      assert.ok(gemini, 'No saved Gemini drafting profile is available')
      await model.selectOption(gemini.value)
      await options.getByLabel(/作者微操指导/).fill('请严格只写第一章，约400汉字，不超过500字。保持蓝图和原始两章事实，不增添角色组织。开头为雨夜车站，结尾林岚与周叔在长椅后找到铜钥匙及17号柜线索；不要提前揭晓第二章。完整叙述并用自然段。')
      await screenshot('09-gemini-draft-options')
      await page.getByRole('button', { name: '本次写作选项', exact: true }).click()
      await page.getByRole('button', { name: '写第 1 章', exact: true }).click()
      await waitUi(page.getByRole('button', { name: 'AI 审稿', exact: true }), 'Gemini draft')
      report.results.draftBody = (await page.locator('.stage-ch-prose .cm-content').innerText()).slice(0, 6000)
      assert.ok(report.results.draftBody.length >= 100, 'Draft did not contain meaningful narrative')
      await screenshot('10-real-gemini-draft')
    })
    await step('real-gpt-review', async () => {
      await page.getByRole('button', { name: 'AI 审稿', exact: true }).click()
      await waitUi(page.getByRole('button', { name: '添加我的意见', exact: true }), 'GPT review')
      report.results.reviewBody = (await page.getByRole('complementary', { name: /审稿报告/ }).innerText()).slice(0, 14000)
      await screenshot('11-real-gpt-review')
    })
    await step('confirm-author-checklist-and-real-gpt-revision', async () => {
      await page.getByRole('button', { name: '添加我的意见', exact: true }).click()
      await page.getByLabel('审稿问题', { exact: true }).last().fill('保留既定剧情和人物事实；把末段找到铜钥匙及17号保管柜线索写得更具体清晰，删除重复形容词。只修本章，不提前揭晓第二章，不增加新事实；完整正文约400字。')
      await page.getByRole('button', { name: '修稿选项', exact: true }).click()
      const options = page.getByRole('dialog', { name: '修稿选项', exact: true })
      const model = options.getByLabel(/本次修稿模型/)
      const models = await model.locator('option').evaluateAll(items => items.map(item => ({ value: item.value, text: item.text })))
      const gpt = models.find(item => /GPT.*6\.1.*Sol/i.test(item.text)) ?? models.find(item => /GPT/i.test(item.text))
      assert.ok(gpt, 'No saved GPT revision profile is available')
      await model.selectOption(gpt.value)
      report.results.revisionModel = gpt.text
      await screenshot('12-gpt-revision-options')
      await page.getByRole('button', { name: '修稿选项', exact: true }).click()
      const reportClose = page.getByRole('button', { name: '关闭审稿报告', exact: true })
      if (await reportClose.isVisible()) await reportClose.click()
      await page.getByRole('button', { name: /按 \d+ 项意见修稿/ }).click()
      await waitUi(page.getByRole('button', { name: '全部用修稿', exact: true }), 'GPT revision')
      await screenshot('13-real-gpt-revision-diff')
    })
    await step('author-merges-and-gpt-rechecks', async () => {
      await page.getByRole('button', { name: '全部用修稿', exact: true }).click()
      const mergedResponse = page.waitForResponse(response => response.url().includes('db%3Arevision-merge') && response.status() === 200, { timeout: 30000 })
      await page.getByRole('button', { name: '完成合并', exact: true }).click()
      const merge = (await (await mergedResponse).json()).result
      assert.equal(merge?.success, true, 'Revision merge was not committed')
      report.results.mergeReviewCycle = merge.receipt?.reviewCycle
      const disposition = merge.receipt?.reviewCycle?.disposition
      if (disposition === 'required') {
        const recheckResponse = await page.waitForResponse(response => response.url().includes('review-revision%3Acommit-review') && response.status() === 200, { timeout: 240000 })
        const committed = (await recheckResponse.json()).result
        assert.equal(committed?.success, true, 'The merged review recheck was not committed')
        assert.equal(committed?.reviewCycle?.disposition, 'completed', 'Required recheck remained incomplete')
        report.results.recheckCompleted = true
      } else {
        assert.equal(disposition, 'not-required', 'Unknown merge recheck disposition')
        report.results.recheckNotRequired = true
      }
      await waitUi(page.getByRole('button', { name: '定稿', exact: true }), 'Merged draft')
      await closeSheet()
      report.results.mergedBody = (await page.locator('.stage-ch-prose .cm-content').innerText()).slice(0, 6000)
      await screenshot('14-merged-rechecked')
    })
    }
    await step('finalize-and-postprocess-once', async () => {
      await page.getByRole('button', { name: '定稿', exact: true }).click()
      const dialog = page.getByRole('dialog', { name: '定稿第 1 章？', exact: true })
      await dialog.getByRole('button', { name: '确认定稿', exact: true }).click()
      await page.locator('.stage-ch-post-step').first().waitFor({ timeout: 240000 })
      await page.waitForFunction(() => {
        const rows = [...document.querySelectorAll('.stage-ch-post-step')]
        return rows.length >= 2 && rows.every(row => ['success', 'failed', 'retired'].includes(row.dataset.state))
      }, undefined, { timeout: 240000 })
      report.results.postprocess = await page.locator('.stage-ch-post-step').evaluateAll(rows => rows.map(row => ({ state: row.dataset.state, text: row.innerText })))
      await screenshot('15-finalized-postprocess')
      assert.ok(report.results.postprocess.every(row => ['success', 'retired'].includes(row.state)), 'Finalized text persisted, but one or more postprocess steps failed; no automatic paid retry')
    })
    await step('finalized-durability-after-refresh', async () => {
      await closeSheet()
      const executionBefore = report.requests.filter(item => /execute/.test(decodeURIComponent(item.path))).length
      await page.reload({ waitUntil: 'domcontentloaded' })
      await page.getByRole('button', { name: `预览《${name}》`, exact: true }).dblclick()
      await page.locator('button[data-stage="manuscript"]').click()
      await page.locator('.stage-chapter[data-phase="final"]').first().waitFor()
      report.results.manuscriptAfterReload = (await body()).slice(0, 9000)
      await screenshot('16-final-persisted-after-refresh')
      const executionAfter = report.requests.filter(item => /execute/.test(decodeURIComponent(item.path))).length
      assert.equal(executionAfter, executionBefore, 'Reload dispatched a model unexpectedly')
      report.results.reloadNoModelCalls = true
    })
    await step('real-pagehide-idle-release-to-fresh-tab', async () => {
      const oldClient = await page.evaluate(() => sessionStorage.getItem('ai-novel-writer-browser-client-id'))
      const executionBefore = report.requests.filter(item => /execute/.test(decodeURIComponent(item.path))).length
      await page.goto('about:blank', { waitUntil: 'domcontentloaded' })
      const next = await context.newPage(); observeNextPage(next)
      page = next
      await page.goto(baseURL, { waitUntil: 'domcontentloaded' })
      await page.getByRole('button', { name: '新建作品', exact: true }).waitFor()
      // Allow the documented 1.5-second departure grace to expire, then try once.
      await page.waitForTimeout(1800)
      const nextClient = await page.evaluate(() => sessionStorage.getItem('ai-novel-writer-browser-client-id'))
      assert.notEqual(nextClient, oldClient, 'Fresh tab must have a new browser-client identity')
      await page.getByRole('button', { name: `预览《${name}》`, exact: true }).dblclick()
      await page.locator('.stage-work-name').filter({ hasText: name }).waitFor()
      await page.locator('button[data-stage="manuscript"]').click()
      await page.locator('.stage-chapter[data-phase="final"]').first().waitFor()
      await screenshot('17-fresh-tab-after-idle-pagehide')
      assert.equal(report.requests.filter(item => /execute/.test(decodeURIComponent(item.path))).length, executionBefore)
      report.results.idleDeparture = { oldClient, nextClient, freshTabOpened: true, noModelCalls: true }
    })
  }
} catch (error) {
  failed = true; report.failure = { message: error.message, stack: error.stack?.slice(0, 5000) }
  try { report.results.failureBody = (await body()).slice(0, 18000); await screenshot('failure') } catch { /* Preserve original failure. */ }
} finally {
  report.finishedAt = new Date().toISOString(); report.status = failed ? 'failed' : 'passed'; checkpoint()
  console.log('PLANNING_REPORT', JSON.stringify({ status: report.status, name, output, steps: report.steps, failure: report.failure }))
  await context.close(); await browser.close()
}
if (failed) process.exitCode = 1
