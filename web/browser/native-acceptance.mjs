import { chromium } from 'playwright'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'

// Run inside the already managed, isolated ai-novel-native-test container:
// docker exec ai-novel-native-test node /workspace/web/browser/native-acceptance.mjs
// No domain operation is invoked directly. All mutations below use actual rendered UI controls.
const baseURL = process.env.NOVEL_TEST_BROWSER_URL ?? 'http://ai-novel-native-test:6081'
const stamp = process.env.NOVEL_TEST_FIXTURE_STAMP ?? new Date().toISOString().replace(/[:.]/g, '-')
const transferOnly = process.env.NOVEL_TEST_TRANSFER_ONLY === '1'
const name = process.env.NOVEL_TEST_PROJECT_NAME ?? `原生浏览器验收-${stamp}`
const output = process.env.NOVEL_TEST_ARTIFACTS ?? `/workspace/.native-test-data/browser-acceptance/${stamp}`
fs.mkdirSync(output, { recursive: true })
const report = { name, baseURL, startedAt: new Date().toISOString(), output, steps: [], errors: [], requests: [], screenshots: [], limitations: [], results: {} }
const browser = await chromium.connectOverCDP(process.env.NOVEL_TEST_CDP ?? 'ws://172.18.0.22:3000?timeout=600000')
const context = await browser.newContext({ locale: 'zh-CN', viewport: { width: 1440, height: 1000 }, acceptDownloads: true, extraHTTPHeaders: { 'X-Novel-User': 'admin' } })
const page = await context.newPage()
page.setDefaultTimeout(15_000)
const browserCdp = await browser.newBrowserCDPSession()
report.downloadEvents = []
browserCdp.on('Browser.downloadWillBegin', event => report.downloadEvents.push({ type: 'willBegin', guid: event.guid, suggestedFilename: event.suggestedFilename }))
browserCdp.on('Browser.downloadProgress', event => report.downloadEvents.push({ type: 'progress', guid: event.guid, state: event.state, receivedBytes: event.receivedBytes, totalBytes: event.totalBytes }))
const remoteDownloadDir = process.env.NOVEL_TEST_REMOTE_DOWNLOAD_DIR
if (remoteDownloadDir) {
  assert.match(remoteDownloadDir, /^\/tmp\/novel-native-acceptance-[a-zA-Z0-9_-]+$/)
  const pageCdp = await context.newCDPSession(page)
  const { targetInfo } = await pageCdp.send('Target.getTargetInfo')
  assert.ok(targetInfo.browserContextId, 'Remote download behavior must be scoped to this owned browser context')
  await browserCdp.send('Browser.setDownloadBehavior', { behavior: 'allowAndName', downloadPath: remoteDownloadDir, browserContextId: targetInfo.browserContextId, eventsEnabled: true })
  await pageCdp.detach()
}
page.on('pageerror', error => report.errors.push({ type: 'pageerror', message: error.message.slice(0, 1000) }))
page.on('response', async response => {
  const url = new URL(response.url())
  if (!url.pathname.startsWith('/api/')) return
  const entry = { method: response.request().method(), path: url.pathname, status: response.status() }
  report.requests.push(entry)
  if (response.status() >= 400) {
    try { const body = await response.json(); entry.errorCode = body.error?.code ?? body.code } catch { /* Retain HTTP status even for a closed response. */ }
  }
})
page.on('requestfailed', request => {
  const url = new URL(request.url())
  if (url.pathname.startsWith('/api/') && url.pathname !== '/api/events') report.errors.push({ type: 'requestfailed', path: url.pathname, message: request.failure()?.errorText })
})

async function screenshot(label) {
  const filename = path.join(output, `${label}.png`)
  await page.screenshot({ path: filename, fullPage: true })
  report.screenshots.push(filename)
}
async function step(label, action) {
  const started = Date.now()
  try {
    const result = await action()
    report.steps.push({ label, status: 'passed', elapsedMs: Date.now() - started })
    console.log('ACCEPTANCE_STEP', label, 'PASS')
    return result
  } catch (error) {
    report.steps.push({ label, status: 'failed', elapsedMs: Date.now() - started, message: String(error.message).slice(0, 2000) })
    console.error('ACCEPTANCE_STEP', label, 'FAIL', error.message)
    throw error
  }
}
async function checkStep(label, action) {
  try { await step(label, action) } catch { failed = true }
}
async function visible(locator) { await locator.waitFor({ state: 'visible' }); return locator }
async function saveActualDownload(download, saved) {
  if (remoteDownloadDir) {
    assert.equal(await download.failure(), null, 'Chrome must complete the real browser download before artifact copying')
    const event = [...report.downloadEvents].reverse().find(item => item.type === 'willBegin' && item.suggestedFilename === download.suggestedFilename())
    assert.ok(event, 'Real Browser.downloadWillBegin event is required')
    const completion = report.downloadEvents.find(item => item.guid === event.guid && item.state === 'completed') ?? await new Promise((resolve, reject) => {
      const onProgress = progress => {
        if (progress.guid !== event.guid) return
        if (progress.state === 'completed') { cleanup(); resolve({ receivedBytes: progress.receivedBytes, totalBytes: progress.totalBytes }) }
        else if (progress.state === 'canceled') { cleanup(); reject(new Error('Real Browser.downloadProgress canceled')) }
      }
      const timer = setTimeout(() => { cleanup(); reject(new Error('Real Browser.downloadProgress completed event timed out')) }, 15_000)
      const cleanup = () => { clearTimeout(timer); browserCdp.off('Browser.downloadProgress', onProgress) }
      browserCdp.on('Browser.downloadProgress', onProgress)
    })
    assert.ok(completion.receivedBytes > 0)
    const localPath = path.resolve(saved)
    const requestPath = path.join(output, 'download-copy-request.json')
    const copy = { guid: event.guid, remotePath: `${remoteDownloadDir}/${event.guid}`, localPath }
    const ready = new Promise((resolve, reject) => {
      const check = () => {
        if (fs.existsSync(localPath) && fs.statSync(localPath).isFile() && fs.statSync(localPath).size === completion.receivedBytes) { watcher.close(); clearTimeout(timer); resolve() }
      }
      const watcher = fs.watch(output, check)
      const timer = setTimeout(() => { watcher.close(); reject(new Error('Timed out waiting for the host to copy the completed real Chrome download')) }, 60_000)
      check()
    })
    const temporaryRequest = `${requestPath}.pending`
    fs.writeFileSync(temporaryRequest, JSON.stringify(copy, null, 2), { flag: 'wx' })
    fs.renameSync(temporaryRequest, requestPath)
    console.log('ACCEPTANCE_REMOTE_DOWNLOAD', JSON.stringify(copy))
    await ready
    report.results.remoteBrowserDownloads ??= []
    report.results.remoteBrowserDownloads.push({ guid: event.guid, suggestedFilename: event.suggestedFilename, browserCompletedBytes: completion.receivedBytes, copiedArtifact: localPath })
    return
  }
  try { await download.saveAs(saved) } catch (error) {
    const diagnostic = await page.evaluate(async url => {
      const response = await fetch(url, { credentials: 'same-origin' })
      return { status: response.status, contentType: response.headers.get('content-type'), disposition: response.headers.get('content-disposition'), bytes: (await response.arrayBuffer()).byteLength }
    }, download.url()).catch(cause => ({ error: cause.message }))
    report.results.downloadFailure = { suggestedFilename: download.suggestedFilename(), failure: await download.failure(), sameOriginFetchDiagnosticOnly: diagnostic }
    throw error
  }
}
async function menu(label) {
  await page.getByRole('button', { name: /作品菜单$/ }).click()
  await page.getByRole('menuitem', { name: label, exact: true }).click()
}
async function reopen(projectName = name, targetPage = page, expectedTitle = projectName) {
  await targetPage.getByRole('button', { name: `预览《${projectName}》`, exact: true }).dblclick()
  await targetPage.locator('.stage-work-name').filter({ hasText: expectedTitle }).waitFor({ state: 'visible' })
}
async function setup() {
  if (!(await page.locator('textarea.stage-setup-outline-input').isVisible())) {
    await page.locator('button[data-stage="setup"]').click()
  }
  await visible(page.locator('textarea.stage-setup-outline-input'))
}
async function layout(width, label) {
  await page.setViewportSize({ width, height: width <= 420 ? 900 : 1000 })
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))))
  const metrics = await page.evaluate(() => ({
    viewport: innerWidth, document: document.documentElement.scrollWidth, body: document.body.scrollWidth,
    overflowing: [...document.querySelectorAll('body *')].filter(element => {
      const rect = element.getBoundingClientRect(), style = getComputedStyle(element)
      return rect.width > 0 && rect.height > 0 && style.position !== 'fixed' && style.visibility !== 'hidden' && rect.right > innerWidth + 2
    }).slice(0, 12).map(element => ({ tag: element.tagName, className: String(element.className), right: Math.round(element.getBoundingClientRect().right) })),
  }))
  report.results[label] = metrics
  await screenshot(label)
  assert.ok(metrics.document <= width + 2 && metrics.body <= width + 2, `Horizontal document overflow at ${width}px: ${JSON.stringify(metrics)}`)
}

let failed = false
try {
  await step('load-native-browser-library', async () => {
    await page.goto(baseURL, { waitUntil: 'domcontentloaded', timeout: 30_000 })
    await visible(page.getByRole('button', { name: '新建作品', exact: true }))
    assert.equal(await page.evaluate(() => typeof window.aiNovelAPI), 'undefined')
    assert.equal(await page.evaluate(() => document.documentElement.dataset.runtime), 'web')
    await screenshot('01-library-desktop')
    report.results.initialLibraryText = (await page.locator('body').innerText()).slice(0, 3000)
  })
  if (process.env.NOVEL_TEST_PROBE_ONLY === '1') {
    report.results.dialogs = await page.getByRole('dialog').allTextContents()
  } else {
    const outline = `浏览器原生验收：雨夜里，中文作家在旧书店发现一封跨越二十年的来信。唯一标识 ${stamp}。`
    if (transferOnly) {
      await step('reopen-completed-fixture-without-model-dispatch', async () => {
        assert.ok(process.env.NOVEL_TEST_PROJECT_NAME && process.env.NOVEL_TEST_FIXTURE_STAMP, 'Transfer-only mode requires the exact existing acceptance fixture name and stamp')
        await reopen()
        await setup()
        assert.equal(await page.locator('textarea.stage-setup-outline-input').inputValue(), outline)
        await page.locator('button[data-stage="manuscript"]').click()
        await page.waitForFunction(() => document.querySelectorAll('.stage-chapters .stage-chapter[data-phase="final"]').length >= 2)
        report.results.resumedFixture = { name, originalStrictReport: process.env.NOVEL_TEST_ORIGINAL_REPORT, noModelDispatch: true }
      })
    } else {
    await step('create-via-native-managed-library-dialog', async () => {
      await page.getByRole('button', { name: '新建作品', exact: true }).click()
      const create = page.getByRole('dialog', { name: '新建作品', exact: true })
      await create.getByLabel('作品名', { exact: true }).fill(name)
      assert.equal(await create.getByRole('button', { name: '创建作品', exact: true }).isEnabled(), false)
      await create.getByRole('button', { name: '选择', exact: true }).click()
      const chooser = page.getByRole('dialog').filter({ hasText: '作品保存在服务器书库' })
      await visible(chooser)
      await screenshot('02-managed-library-dialog')
      assert.equal(await chooser.locator('input[type="file"]').count(), 0, 'Project creation must not ask for a local directory upload')
      await chooser.getByRole('button', { name: '继续', exact: true }).click()
      await visible(create)
      assert.match(await create.getByLabel('保存位置', { exact: true }).inputValue(), /^(?:服务器书库|workspaces)$/u)
      await create.getByRole('button', { name: '创建作品', exact: true }).click()
      await visible(page.locator('.stage-work-name').filter({ hasText: name }))
      await setup()
      await screenshot('03-new-project-setup')
    })

    await step('chinese-ime-composition-and-setup-save', async () => {
      const editor = page.locator('textarea.stage-setup-outline-input')
      await editor.focus()
      await page.evaluate(() => {
        window.__acceptanceComposition = []
        const target = document.querySelector('textarea.stage-setup-outline-input')
        for (const name of ['compositionstart', 'compositionupdate', 'compositionend', 'input']) target.addEventListener(name, event => {
          window.__acceptanceComposition.push({ type: event.type, data: event.data, isComposing: event.isComposing })
        })
      })
      const cdp = await context.newCDPSession(page)
      await cdp.send('Input.imeSetComposition', { text: 'zhongwen', selectionStart: 8, selectionEnd: 8 })
      await cdp.send('Input.imeSetComposition', { text: '中文原生输入', selectionStart: 6, selectionEnd: 6 })
      await cdp.send('Input.insertText', { text: '中文原生输入' })
      await cdp.detach()
      assert.equal(await editor.inputValue(), '中文原生输入')
      const composition = await page.evaluate(() => window.__acceptanceComposition)
      assert.ok(composition.some(event => event.type === 'compositionstart'))
      assert.ok(composition.some(event => event.type === 'compositionend'))
      assert.ok(composition.some(event => event.type === 'input' && event.isComposing === true))
      report.results.ime = composition
      await editor.fill(outline)
      await page.getByLabel(/^类型(?: · 必填)?$/).selectOption({ label: '悬疑' })
      await page.getByLabel('总章数', { exact: true }).fill('12')
      await page.getByLabel('每章字数', { exact: true }).fill('1500')
      await page.getByLabel(/^目标读者(?: · 必填)?$/).selectOption({ index: 1 })
      await page.getByRole('button', { name: /^保存/ }).click()
      await page.waitForFunction(() => document.querySelector('.stage-saved')?.textContent?.includes('已保存'))
      assert.equal(await editor.inputValue(), outline)
      await screenshot('04-setup-saved-ime')
    })

    await step('reload-and-reopen-persisted-project', async () => {
      await page.reload({ waitUntil: 'domcontentloaded' })
      await visible(page.getByRole('button', { name: '新建作品', exact: true }))
      await reopen()
      await setup()
      assert.equal(await page.locator('textarea.stage-setup-outline-input').inputValue(), outline)
      assert.equal(await page.getByLabel('总章数', { exact: true }).inputValue(), '12')
      assert.equal(await page.getByLabel('每章字数', { exact: true }).inputValue(), '1500')
      assert.equal(await page.getByLabel(/^类型(?: · 必填)?$/).inputValue(), '悬疑')
      await menu('回到书房')
      await visible(page.getByRole('button', { name: '新建作品', exact: true }))
      await reopen()
      await setup()
      assert.equal(await page.locator('textarea.stage-setup-outline-input').inputValue(), outline)
      report.results.persistence = { outline, totalChapters: 12, wordsPerChapter: 1500 }
      await screenshot('05-reopened-persisted-setup')
    })

    // Collect both narrow-view regressions without hiding later import/export failures.
    await checkStep('responsive-760px-setup', () => layout(760, '06-responsive-760'))
    await checkStep('responsive-390px-setup', () => layout(390, '07-responsive-390'))
    await page.setViewportSize({ width: 1440, height: 1000 })

    const manuscript = `第一章 雨夜来信\n\n雨水沿着旧书店的玻璃缓缓流下。林墨推开木门，发现柜台上放着一封写给自己的信。信纸上的日期竟是二十年前。\n\n验收正文唯一标识：${stamp}。这段文字必须完整出现在最终浏览器下载文件里。\n\n第二章 书页回声\n\n第二天清晨，林墨再次翻开旧书，夹在书页中的银杏叶已经变得透明。他决定沿着信中的地址寻找答案。\n`
    await step('txt-upload-through-browser-dialog-and-author-import', async () => {
      await menu('小说拆解与仿写…')
      await page.getByTestId('import-purpose-author').click()
      const inference = page.getByTestId('author-inference-option').getByRole('checkbox')
      if (await inference.isChecked()) await page.getByTestId('author-inference-option').locator('label').click()
      assert.equal(await inference.isChecked(), false)
      await page.getByTestId('import-source-choose').click()
      const upload = page.getByRole('dialog').filter({ has: page.locator('input[type="file"]') })
      await visible(upload)
      await upload.locator('input[type="file"]').setInputFiles({ name: `验收中文原稿-${stamp}.txt`, mimeType: 'text/plain', buffer: Buffer.from(manuscript, 'utf8') })
      await screenshot('08-native-upload-dialog')
      await upload.getByRole('button', { name: '确认', exact: true }).click()
      await page.getByTestId('author-import-confirmation-summary').filter({ hasText: '确认后将新增 2 章权威定稿' }).waitFor({ state: 'visible', timeout: 30_000 })
      await screenshot('09-native-import-preview')
      await page.getByRole('button', { name: '确认导入（2 章）', exact: true }).click()
      await page.getByTestId('author-import-confirmation-summary').waitFor({ state: 'hidden', timeout: 30_000 })
      // Workflow launch closes the dialog before mandatory AI postprocessing finishes.
      // Observe the real run sheet rather than treating early publication as full import success.
      try {
        await visible(page.locator('.stage-capsule'))
        await page.locator('.stage-capsule').click()
        const ended = page.locator('.run-card[data-state="completed"], .run-card[data-state="failed"]')
        await ended.waitFor({ state: 'visible', timeout: 240_000 })
        report.results.importRun = { state: await ended.getAttribute('data-state'), text: (await ended.innerText()).slice(0, 6000) }
        if (report.results.importRun.state !== 'completed') {
          const diagnostics = page.getByRole('tab', { name: '诊断', exact: true })
          if (await diagnostics.isVisible()) {
            await diagnostics.click()
            report.results.importDiagnostics = (await page.locator('.pi-sheet').innerText()).slice(-16000)
            await screenshot('10a-import-failure-diagnostics')
          }
        }
        assert.equal(report.results.importRun.state, 'completed', `Mandatory author-import workflow failed: ${report.results.importRun.text}`)
        await page.getByRole('button', { name: '收起运行侧页', exact: true }).click()
        await page.locator('button[data-stage="manuscript"]').click()
        await page.waitForFunction(() => document.querySelectorAll('.stage-chapters .stage-chapter[data-phase="final"]').length >= 2, undefined, { timeout: 15_000 })
      } catch (error) {
        if (process.env.NOVEL_TEST_ALLOW_IMPORT_POSTPROCESS_LIMITATION !== '1') throw error
        const missing = report.requests.filter(request => request.status === 404 && request.path.startsWith('/api/operations/'))
        assert.deepEqual(missing, [], 'Missing backend operations are not an acceptable model-configuration limitation')
        report.limitations.push('Authoritative import was confirmed, but mandatory AI continuity postprocessing did not finish before reload. Only durable manuscript publication/transfer is accepted below; AI postprocessing is not claimed successful.')
        await screenshot('10a-import-postprocess-not-complete')
        await page.reload({ waitUntil: 'domcontentloaded' })
        await visible(page.getByRole('button', { name: '新建作品', exact: true }))
        await reopen()
        await page.locator('button[data-stage="manuscript"]').click()
        await page.waitForFunction(() => document.querySelectorAll('.stage-chapters .stage-chapter[data-phase="final"]').length >= 2, undefined, { timeout: 15_000 })
      }
      report.results.import = { sourceBytes: Buffer.byteLength(manuscript), chapters: 2, inference: false, authoritativeReadProjection: true, postprocessCompletedBeforeReload: report.limitations.length === 0 }
      await screenshot('10-native-import-complete')
    })
    }

    let restoredName, restoredTitle
    if (process.env.NOVEL_TEST_RESTORED_ONLY === '1') {
      await step('reopen-restored-fixture-after-real-archive-transfer', async () => {
        restoredName = process.env.NOVEL_TEST_RESTORED_PROJECT_NAME
        assert.ok(restoredName?.includes('恢复副本'))
        await menu('回到书房')
        await reopen(restoredName, page, name)
        restoredTitle = await page.locator('.stage-work-name').textContent()
        await setup()
        assert.equal(await page.locator('textarea.stage-setup-outline-input').inputValue(), outline)
        await page.reload({ waitUntil: 'domcontentloaded' })
        await visible(page.getByRole('button', { name: '新建作品', exact: true }))
        await reopen(restoredName, page, restoredTitle)
        await setup()
        assert.equal(await page.locator('textarea.stage-setup-outline-input').inputValue(), outline)
        report.results.archive = { restoredProjectName: restoredName, libraryPersistence: true, originalTransferReport: process.env.NOVEL_TEST_TRANSFER_REPORT }
        await screenshot('13-restored-copy-reopened')
      })
    } else {
    await step('txt-export-through-dialog-and-real-browser-download', async () => {
      await menu('导出…')
      const exporting = page.getByRole('dialog', { name: '导出项目', exact: true })
      await visible(exporting)
      await exporting.getByText('纯文本 TXT', { exact: true }).click()
      await exporting.getByRole('button', { name: '选择目录并导出', exact: true }).click()
      const chooser = page.getByRole('dialog').filter({ hasText: '作品保存在服务器书库' })
      await visible(chooser)
      await chooser.getByRole('button', { name: '继续', exact: true }).click()
      await exporting.getByText(/已导出到：/).waitFor({ state: 'visible', timeout: 30_000 })
      await exporting.getByRole('button', { name: /关闭/ }).click()
      const anchor = page.getByRole('complementary', { name: '下载文件', exact: true }).getByRole('link').filter({ hasText: '点击下载' }).first()
      await visible(anchor)
      await screenshot('11-native-export-ready')
      const [download] = await Promise.all([page.waitForEvent('download', { timeout: 30_000 }), anchor.click()])
      const saved = path.join(output, 'downloaded-manuscript.txt')
      await saveActualDownload(download, saved)
      const downloaded = fs.readFileSync(saved)
      assert.ok(downloaded.length > 100)
      const text = downloaded.toString('utf8')
      assert.ok(text.includes(stamp), 'Actual downloaded bytes must include the unique uploaded manuscript text')
      assert.ok(text.includes('林墨推开木门'), 'UTF-8 Chinese prose must survive the complete import/export path')
      assert.ok(text.includes('银杏叶已经变得透明'), 'Both chapters must be in the real download')
      report.results.export = { suggestedFilename: download.suggestedFilename(), saved, bytes: downloaded.length, includesBothChapters: true }
      await page.getByRole('button', { name: '关闭下载提示', exact: true }).click()
    })
    await step('portable-archive-browser-download-upload-and-restore-copy', async () => {
      await menu('项目备份')
      const panel = page.getByTestId('project-backup-panel')
      await visible(panel)
      await panel.getByRole('button', { name: '导出本地存档', exact: true }).click()
      const save = page.getByRole('dialog').filter({ hasText: '下载文件名' })
      await visible(save)
      const archiveName = `native-acceptance-${stamp}.ainovel`
      await save.getByLabel('下载文件名', { exact: true }).fill(archiveName)
      await save.getByRole('button', { name: '生成下载文件', exact: true }).click()
      await panel.getByRole('status').filter({ hasText: '本地存档已导出：' }).waitFor({ state: 'visible', timeout: 45_000 })
      const archiveLink = page.getByRole('complementary', { name: '下载文件', exact: true }).getByRole('link').filter({ hasText: archiveName })
      const [download] = await Promise.all([page.waitForEvent('download', { timeout: 30_000 }), archiveLink.click()])
      const archivePath = path.join(output, 'downloaded-project.ainovel')
      await saveActualDownload(download, archivePath)
      const archive = fs.readFileSync(archivePath)
      assert.ok(archive.length > 1000, 'Portable archive must contain actual project bytes')
      assert.equal(archive.readUInt32LE(0), 0x04034b50, '.ainovel is a real ZIP archive')
      await panel.getByRole('button', { name: '从本地存档恢复副本', exact: true }).click()
      const upload = page.getByRole('dialog').filter({ has: page.locator('input[type="file"]') })
      await visible(upload)
      await upload.locator('input[type="file"]').setInputFiles({ name: archiveName, mimeType: 'application/zip', buffer: archive })
      await upload.getByRole('button', { name: '确认', exact: true }).click()
      const target = page.getByRole('dialog').filter({ hasText: '作品保存在服务器书库' })
      await visible(target)
      await target.getByRole('button', { name: '继续', exact: true }).click()
      await panel.getByRole('status').filter({ hasText: '已恢复新副本' }).waitFor({ state: 'visible', timeout: 45_000 })
      assert.equal(await page.locator('.stage-work-name').textContent(), name, 'Restore must not replace the open original project')
      report.results.archive = { downloadedFilename: download.suggestedFilename(), bytes: archive.length, saved: archivePath, restoredWithoutReplacingOriginal: true }
      await screenshot('12-portable-archive-restored')
      await page.getByRole('button', { name: '关闭下载提示', exact: true }).click()
      await menu('回到书房')
      // The restored copy is discovered through the same managed-library chooser as an ordinary user.
      await page.getByRole('button', { name: '打开作品', exact: true }).click()
      const chooser = page.getByRole('dialog').filter({ hasText: '服务器书库' })
      await visible(chooser)
      await chooser.locator('select option').filter({ hasText: `${name}-恢复副本` }).first().waitFor({ state: 'attached', timeout: 15_000 })
      const choices = await chooser.locator('select option').evaluateAll(options => options.map(option => ({ label: option.textContent, value: option.value })))
      const restored = choices.find(option => option.label?.includes(name) && option.label.includes('恢复副本'))
      assert.ok(restored, `Restored project missing from managed library: ${JSON.stringify(choices)}`)
      restoredName = restored.label
      assert.notEqual(restored.value.split('/').at(-1), name, 'Restore must create a separate managed-library directory')
      await chooser.locator('select').selectOption(restored.value)
      await chooser.getByRole('button', { name: '确认', exact: true }).click()
      await visible(page.locator('.stage-work-name').filter({ hasText: name }))
      restoredTitle = await page.locator('.stage-work-name').textContent()
      await setup()
      assert.equal(await page.locator('textarea.stage-setup-outline-input').inputValue(), outline)
      await page.reload({ waitUntil: 'domcontentloaded' })
      await visible(page.getByRole('button', { name: '新建作品', exact: true }))
      // Refresh/open repopulates the shelf from the managed library, using the restored directory label.
      await reopen(restoredName, page, restoredTitle)
      await setup()
      assert.equal(await page.locator('textarea.stage-setup-outline-input').inputValue(), outline)
      report.results.archive.restoredProjectName = restoredName
      report.results.archive.libraryPersistence = true
      await screenshot('13-restored-copy-reopened')
    })
    }

    await step('two-tabs-different-project-writes-never-bleed', async () => {
      // Each tab has independent sessionStorage client identity; both use the same authenticated browser session.
      const second = await context.newPage()
      second.setDefaultTimeout(15_000)
      second.on('pageerror', error => report.errors.push({ type: 'pageerror', message: error.message.slice(0, 1000) }))
      try {
        await second.goto(baseURL, { waitUntil: 'domcontentloaded' })
        await second.getByRole('button', { name: '新建作品', exact: true }).waitFor({ state: 'visible' })
        await reopen(name, second)
        await second.locator('button[data-stage="setup"]').click()
        const firstOutline = `${outline}\n恢复副本独立写入：A-${stamp}`
        const secondOutline = `${outline}\n原项目独立写入：B-${stamp}`
        await Promise.all([
          page.locator('textarea.stage-setup-outline-input').fill(firstOutline),
          second.locator('textarea.stage-setup-outline-input').fill(secondOutline),
        ])
        await Promise.all([
          page.getByRole('button', { name: /^保存/ }).click(),
          second.getByRole('button', { name: /^保存/ }).click(),
        ])
        await Promise.all([
          page.getByRole('button', { name: /^保存/ }).waitFor({ state: 'hidden' }),
          second.getByRole('button', { name: /^保存/ }).waitFor({ state: 'hidden' }),
        ])
        const clients = await Promise.all([page, second].map(target => target.evaluate(() => sessionStorage.getItem('ai-novel-writer-browser-client-id'))))
        assert.notEqual(clients[0], clients[1], 'Tabs must have distinct client identities')
        await Promise.all([page.reload({ waitUntil: 'domcontentloaded' }), second.reload({ waitUntil: 'domcontentloaded' })])
        await Promise.all([page, second].map(target => target.getByRole('button', { name: '新建作品', exact: true }).waitFor({ state: 'visible' })))
        await Promise.all([reopen(restoredName, page, restoredTitle), reopen(name, second)])
        await Promise.all([page, second].map(target => target.locator('button[data-stage="setup"]').click()))
        assert.equal(await page.locator('textarea.stage-setup-outline-input').inputValue(), firstOutline)
        assert.equal(await second.locator('textarea.stage-setup-outline-input').inputValue(), secondOutline)
        assert.ok(!(await page.locator('textarea.stage-setup-outline-input').inputValue()).includes(`B-${stamp}`))
        assert.ok(!(await second.locator('textarea.stage-setup-outline-input').inputValue()).includes(`A-${stamp}`))
        report.results.twoTabs = { differentClientIds: true, restoredProject: restoredName, originalProject: name, separatelyPersisted: true }
        await screenshot('14-two-tab-restored-independent')
        const screenshotB = path.join(output, '15-two-tab-original-independent.png')
        await second.screenshot({ path: screenshotB, fullPage: true })
        report.screenshots.push(screenshotB)
      } finally { await second.close() }
    })
    await step('no-renderer-errors', () => assert.deepEqual(report.errors.filter(error => error.type === 'pageerror'), []))
  }
} catch (error) {
  failed = true
  report.failure = { message: String(error.message).slice(0, 4000), stack: String(error.stack).slice(0, 5000) }
  try { report.results.failureBody = (await page.locator('body').innerText()).slice(0, 12000); await screenshot('failure') } catch { /* Preserve the original assertion. */ }
} finally {
  report.finishedAt = new Date().toISOString()
  report.status = failed ? 'failed' : 'passed'
  fs.writeFileSync(path.join(output, 'report.json'), JSON.stringify(report, null, 2))
  console.log('ACCEPTANCE_REPORT', JSON.stringify({ status: report.status, name, output, steps: report.steps, errors: report.errors }))
  await context.close()
  // Playwright disconnects a connectOverCDP connection; only our fresh context was closed above.
  await browser.close()
}
if (failed) process.exitCode = 1
