import { chromium } from 'playwright'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { createHash, randomBytes } from 'node:crypto'

// Rendered UI only. Never call domain APIs, import stores, reuse other contexts,
// close the shared browser, or exercise a provider. ZIP is not a supported skill input.
const stamp = new Date().toISOString().replace(/[:.]/g, '-')
const name = process.env.NOVEL_MANAGEMENT_PROJECT ?? `原生管理验收-${stamp}`
const modelName = `management-canary-${stamp}`
const skillName = `management-prompt-${stamp.toLowerCase()}`
const output = process.env.NOVEL_TEST_ARTIFACTS ?? `/workspace/.native-test-data/browser-management/${stamp}`
const baseURL = process.env.NOVEL_TEST_BROWSER_URL ?? 'http://ai-novel-native-test:6081'
assert.equal(baseURL, 'http://ai-novel-native-test:6081', 'Only the exact isolated relay URL is allowed')
const artifactRoot = '/workspace/.native-test-data/browser-management/'
assert.ok(path.resolve(output).startsWith(artifactRoot), 'Artifacts must stay under the ignored private evidence root')
assert.ok(name.startsWith('原生管理验收-'), 'Never open another lane project')
assert.equal(process.env.NOVEL_MANAGEMENT_ENDPOINT ?? 'http://127.0.0.1:1', 'http://127.0.0.1:1', 'Only the inert origin is allowed')
const mode = process.env.NOVEL_MANAGEMENT_MODE ?? 'inspect'
assert.ok(['inspect', 'all', 'content', 'models', 'cleanup'].includes(mode), 'Unknown bounded acceptance mode')
const report = { startedAt: new Date().toISOString(), name, mode, steps: [], results: {}, blocked: [], requests: [], errors: [], screenshots: [] }
let finishedNormally = false
fs.mkdirSync(output, { recursive: true, mode: 0o700 })
const checkpoint = () => fs.writeFileSync(path.join(output, 'report.json'), JSON.stringify(report, null, 2), { mode: 0o600 })
const digest = value => createHash('sha256').update(JSON.stringify(value)).digest('hex')
let originalModelFingerprint, latestModelFingerprint, latestModelCount, temporaryCredentialObserved = false, temporaryModelId
const responseTasks = new Set()
const browser = await chromium.connectOverCDP(process.env.NOVEL_TEST_CDP ?? 'ws://172.18.0.22:3000?timeout=600000')
const context = await browser.newContext({ locale: 'zh-CN', viewport: { width: 1440, height: 1000 }, serviceWorkers: 'block' })
const page = await context.newPage()
page.setDefaultTimeout(12000)
const budget = setTimeout(() => { report.errors.push('Bounded 9 minute browser budget expired'); checkpoint(); void context.close() }, 540000)
await context.route('**/*', async route => {
  const request = route.request(), url = new URL(request.url())
  const operation = decodeURIComponent(url.pathname.split('/api/operations/')[1] ?? '')
  let wrongModelMutation = false
  if (operation === 'llm:save-model' || operation === 'llm:delete-model') {
    try {
      const value = request.postDataJSON().args?.[0]
      if (operation === 'llm:save-model') {
        wrongModelMutation = value?.name !== modelName && value?.name !== modelName + '-edited'
        if (!wrongModelMutation) temporaryModelId = value.id
      } else wrongModelMutation = value !== temporaryModelId
    } catch { wrongModelMutation = true }
  }
  const forbidden = wrongModelMutation || url.origin !== new URL(baseURL).origin
    || /execute|generate|execution-lease|claim-tool|finish-tool|commit-domain-tool/i.test(operation)
    || /^mcp:(?!get-servers-status$|list-tools$|list-resources$)/.test(operation)
    || /^llm:(discover-models|test-connection)$/.test(operation)
    || /^skills:(inspect-github|install-github)$/.test(operation)
  if (forbidden) {
    report.blocked.push({ operation: operation || 'external-request', method: request.method() }); checkpoint()
    return route.abort('blockedbyclient')
  }
  return route.continue()
})
page.on('pageerror', error => { report.errors.push(error.message.slice(0, 300)); checkpoint() })
async function observeResponse(response) {
  const url = new URL(response.url())
  if (!url.pathname.startsWith('/api/')) return
  const operation = decodeURIComponent(url.pathname.split('/api/operations/')[1] ?? '')
  report.requests.push({ operation: operation || url.pathname, method: response.request().method(), status: response.status() })
  if (operation === 'llm:list-models') {
    try {
      const value = (await response.json()).result
      if (Array.isArray(value)) {
        const original = value.filter(model => model.name !== modelName && model.name !== modelName + '-edited')
        latestModelFingerprint = digest(original)
        originalModelFingerprint ??= latestModelFingerprint
        latestModelCount = original.length
        const temp = value.find(model => model.name === modelName || model.name === modelName + '-edited')
        if (temp) temporaryCredentialObserved = temp.hasCredential === true && !temp.apiKey
      }
    } catch { /* Do not log response bodies, including any credential field. */ }
  }
  checkpoint()
}
page.on('response', response => {
  const task = observeResponse(response)
  responseTasks.add(task)
  void task.finally(() => responseTasks.delete(task))
})
async function shot(label) {
  const file = path.join(output, `${label}.png`)
  await page.screenshot({ path: file, fullPage: true, mask: [page.locator('input[type="password"]')] })
  report.screenshots.push(path.basename(file)); checkpoint()
}
async function step(label, action) {
  const start = Date.now()
  try { await action(); report.steps.push({ label, status: 'passed', elapsedMs: Date.now() - start }); console.log('MANAGEMENT_STEP', label, 'PASS') }
  catch (error) {
    report.steps.push({ label, status: 'blocked', elapsedMs: Date.now() - start, message: error.message.slice(0, 600) })
    await shot(`blocked-${report.steps.length}`).catch(() => {})
    console.log('MANAGEMENT_STEP', label, 'BLOCKED')
    throw error
  } finally { checkpoint() }
}
async function settings(section) {
  if (!await page.locator('.stage-settings').isVisible()) {
    const direct = page.getByRole('button', { name: '设置', exact: true })
    await direct.waitFor()
    await direct.click()
  }
  await page.getByRole('button', { name: section, exact: true }).click()
}
async function reopen() {
  await page.reload({ waitUntil: 'domcontentloaded' })
  await page.getByRole('button', { name: `预览《${name}》`, exact: true }).dblclick()
  await page.locator('.stage-work-name').filter({ hasText: name }).waitFor()
}
async function createOwnProject() {
  await page.getByRole('button', { name: '新建作品', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: '新建作品', exact: true })
  await dialog.getByLabel('作品名', { exact: true }).fill(name)
  await dialog.getByRole('button', { name: '选择', exact: true }).click()
  await page.getByRole('dialog').filter({ hasText: '作品保存在服务器书库' }).getByRole('button', { name: '继续', exact: true }).click()
  await dialog.getByRole('button', { name: '创建作品', exact: true }).click()
  await page.locator('.stage-work-name').filter({ hasText: name }).waitFor()
  report.results.ownProjectCreated = true
}
async function prompts() {
  await settings('提示词模板')
  const headers = () => page.locator('.stage-set-legacy button.w-full')
  await headers().first().waitFor()
  const index = await headers().evaluateAll(items => items.findIndex(item => item.innerText.includes('内置')))
  assert.ok(index >= 0, 'Need an untouched built-in prompt to restore without overwriting custom prompts')
  const header = () => headers().nth(index)
  await header().click()
  const role = () => page.getByLabel('创作角色定位', { exact: true })
  const guidance = () => page.getByLabel('补充创作指导', { exact: true })
  // Retain the exact bounded diagnostic, not the unexecuted write branch.
  // This label lookup timed out in the real run before any prompt edit/save.
  await role().inputValue()
  await guidance().inputValue()
  throw new Error('Prompt mutation acceptance remains unverified; read-only diagnostic only')
}
async function skills() {
  await settings('写作 Skills')
  const originalBinding = await page.getByLabel('章节正文 Skill', { exact: true }).inputValue()
  assert.equal(originalBinding, '', 'The uniquely created project must have no binding')
  await page.getByRole('button', { name: '选择文件', exact: true }).click()
  const upload = page.getByRole('dialog').filter({ has: page.locator('input[type="file"]') })
  await upload.waitFor()
  const accept = await upload.locator('input[type="file"]').getAttribute('accept')
  report.results.skillSupportedInput = { accept, zipSupported: false, format: 'Agent Skills SKILL.md YAML frontmatter; no ZIP manifest' }
  assert.match(accept ?? '', /\.md/)
  const raw = `---\nname: ${skillName}\ndescription: Synthetic prompt-only management acceptance fixture.\nversion: 1.0.0\nlanguage: zh-CN\nstage: drafting\n---\n\nPreserve author facts. Prefer concrete character decisions and consequences.\n`
  await upload.locator('input[type="file"]').setInputFiles({ name: 'SKILL.md', mimeType: 'text/markdown', buffer: Buffer.from(raw) })
  await upload.getByRole('button', { name: '确认', exact: true }).click()
  const candidate = page.getByRole('group', { name: '待安装的 Skill', exact: true })
  await candidate.waitFor()
  assert.ok((await candidate.innerText()).includes(skillName))
  assert.equal(await candidate.getByRole('button', { name: '确认安装', exact: true }).isEnabled(), true)
  report.results.skillReadOnlyMetadataPreview = true
  report.results.skillBodyPreviewAvailable = false
  await shot('04-local-skill-inspected')
  let installed = false
  try {
    await candidate.getByRole('button', { name: '确认安装', exact: true }).click()
    await page.getByRole('dialog', { name: '安装写作 Skill', exact: true }).getByRole('button', { name: '确认', exact: true }).click()
    await page.getByRole('button', { name: `卸载 ${skillName}`, exact: true }).waitFor()
    installed = true; report.results.skillInstalled = true
    const stage = page.getByLabel('章节正文 Skill', { exact: true })
    const option = stage.locator('option').filter({ hasText: skillName })
    const skillId = await option.getAttribute('value')
    await stage.selectOption(skillId)
    await page.getByText('启用阶段: 章节正文', { exact: true }).waitFor()
    await reopen(); await settings('写作 Skills')
    await page.waitForFunction(id => document.querySelector('select[aria-label="章节正文 Skill"]')?.value === id, skillId)
    assert.equal(await page.getByLabel('章节正文 Skill', { exact: true }).inputValue(), skillId)
    report.results.skillBindingReload = true
    await shot('05-skill-installed-bound-reloaded')
  } finally {
    if (installed) {
      // Explicitly clear our own binding first; cleanup must not race settings hydration.
      const bound = page.getByLabel('章节正文 Skill', { exact: true })
      if ((await bound.inputValue()) !== originalBinding) {
        await bound.selectOption(originalBinding)
        await page.getByText('启用阶段: 章节正文', { exact: true }).waitFor({ state: 'detached' })
      }
      await page.getByRole('button', { name: `卸载 ${skillName}`, exact: true }).click()
      await page.getByRole('dialog', { name: '卸载写作 Skill', exact: true }).getByRole('button', { name: '确认', exact: true }).click()
      await page.getByRole('button', { name: `卸载 ${skillName}`, exact: true }).waitFor({ state: 'detached' })
      await page.waitForFunction(() => document.querySelector('select[aria-label="章节正文 Skill"]')?.value === '')
      assert.equal(await page.getByLabel('章节正文 Skill', { exact: true }).inputValue(), originalBinding)
      await reopen(); await settings('写作 Skills')
      assert.equal(await page.getByRole('button', { name: `卸载 ${skillName}`, exact: true }).count(), 0)
      assert.equal(await page.getByLabel('章节正文 Skill', { exact: true }).inputValue(), originalBinding)
      report.results.skillUninstalledBindingClearedReload = true
      await shot('06-skill-uninstalled-reloaded')
    }
  }
}
async function models() {
  await settings('AI 模型')
  const count = await page.locator('.stage-set-model').count()
  report.results.existingGenerationModelCount = count
  await page.getByRole('button', { name: '添加模型', exact: true }).click()
  const form = page.locator('.stage-set-legacy')
  const field = text => form.locator('div').filter({ has: page.locator('label').filter({ hasText: new RegExp(`^${text}$`) }) }).filter({ has: page.locator('input') }).last().locator('input').first()
  await form.locator('select').first().selectOption('custom')
  await field('显示名称').fill(modelName)
  await field('base_url').fill(process.env.NOVEL_MANAGEMENT_ENDPOINT ?? 'http://127.0.0.1:1')
  // Manual model ID is entered before the key, avoiding opening discovery dropdown with credentials.
  await form.getByRole('combobox', { name: 'model（模型名称）', exact: true }).fill('management-inert-model')
  await form.locator('input[type="password"]').fill(`canary-only-${randomBytes(12).toString('hex')}`)
  await page.getByRole('button', { name: '保存配置', exact: true }).click()
  const edit = () => page.getByRole('button', { name: `编辑「${modelName}」`, exact: true })
  if (process.env.NOVEL_MANAGEMENT_EXPECT_SAVE !== '1') {
    const alert = page.getByRole('dialog').filter({ hasText: /ENDPOINT|endpoint|允许|白名单|denied|拒绝/ })
    await alert.waitFor()
    report.results.modelSave = 'policy-denied-empty-endpoint-allowlist'
    await shot('07-model-save-policy-denied')
    await alert.getByRole('button').last().click()
    await page.getByRole('button', { name: '取消', exact: true }).click()
    assert.equal(await edit().count(), 0)
    return
  }
  try {
    await edit().waitFor()
    await edit().click()
    const key = page.locator('input[type="password"]')
    assert.equal(await key.inputValue(), '')
    assert.match(await key.getAttribute('placeholder') ?? '', /已配置，留空保留/)
    report.results.modelCredentialRedacted = true
    await field('显示名称').fill(modelName + '-edited')
    await page.getByRole('button', { name: '保存配置', exact: true }).click()
    await page.getByRole('button', { name: `编辑「${modelName}-edited」`, exact: true }).waitFor()
    await page.reload({ waitUntil: 'domcontentloaded' }); await settings('AI 模型')
    await page.getByRole('button', { name: `编辑「${modelName}-edited」`, exact: true }).click()
    assert.equal(await page.locator('input[type="password"]').inputValue(), '')
    assert.match(await page.locator('input[type="password"]').getAttribute('placeholder') ?? '', /已配置，留空保留/)
    report.results.modelBlankKeyEditPreservedReload = true
    await shot('07-model-credential-preserved')
    await page.getByRole('button', { name: '取消', exact: true }).click()
  } finally {
    const cancel = page.getByRole('button', { name: '取消', exact: true })
    if (await cancel.isVisible()) await cancel.click()
    const edited = await page.getByRole('button', { name: `「${modelName}-edited」更多操作`, exact: true }).count()
    const currentName = edited ? modelName + '-edited' : modelName
    await page.getByRole('button', { name: `「${currentName}」更多操作`, exact: true }).click()
    await page.getByRole('menuitem', { name: '删除模型…', exact: true }).click()
    await page.getByRole('button', { name: '删除模型', exact: true }).click()
    await page.getByRole('button', { name: `编辑「${currentName}」`, exact: true }).waitFor({ state: 'detached' })
    await page.reload({ waitUntil: 'domcontentloaded' }); await settings('AI 模型')
    assert.equal(await page.locator('.stage-set-model').count(), count)
    report.results.modelTemporaryDeletedReload = true
  }
}
try {
  await page.goto(baseURL, { waitUntil: 'domcontentloaded' })
  await page.getByRole('button', { name: '新建作品', exact: true }).waitFor()
  assert.equal(await page.evaluate(() => typeof window.aiNovelAPI), 'undefined')
  if (mode === 'inspect') {
    await settings('写作 Skills')
    await page.getByRole('heading', { name: '导入写作 Skill', exact: true }).waitFor()
    report.results.readOnlyInspection = true
    await shot('readonly-management')
  } else if (mode === 'cleanup') {
    await page.getByRole('button', { name: `预览《${name}》`, exact: true }).dblclick()
    await settings('写作 Skills')
    await page.waitForTimeout(500)
    await page.getByLabel('章节正文 Skill', { exact: true }).selectOption('')
    await page.waitForTimeout(500)
    report.results.ownPreviousBindingCleaned = true
  } else if (mode !== 'models') {
    await step('create-own-isolated-project', createOwnProject)
    await step('global-prompts-edit-save-reload-language-reset-restore', prompts).catch(() => {})
    await step('local-prompt-skill-inspect-install-bind-reload-uninstall', skills).catch(() => {})
  }
  if (mode === 'all' || mode === 'models') await step('temporary-model-crud-subset-no-provider', models).catch(() => {})
  await Promise.allSettled([...responseTasks])
  report.results.originalModelsUnchanged = originalModelFingerprint !== undefined && originalModelFingerprint === latestModelFingerprint
  report.results.originalModelCount = latestModelCount
  report.results.temporaryCredentialObserved = temporaryCredentialObserved
  report.results.discoveryAndConnection = 'NOT TESTED; all such requests blocked before gateway'
  report.results.noPaidExecutionForwarded = !report.requests.some(item => /execute|llm:generate|mcp:call-tool/.test(item.operation))
  report.finishedAt = new Date().toISOString(); finishedNormally = true; checkpoint()
  console.log('MANAGEMENT_SUMMARY', JSON.stringify({ output, passed: report.steps.filter(step => step.status === 'passed').length, blocked: report.steps.filter(step => step.status === 'blocked').length, results: report.results, blockedRequestCount: report.blocked.length, errors: report.errors.length }))
} finally {
  clearTimeout(budget)
  checkpoint()
  await context.close()
  // connectOverCDP transport has no public disconnect(); exit this driver only.
  // Never call browser.close(): shared browser stays alive.
  process.exit(finishedNormally && report.errors.length === 0 && report.steps.every(item => item.status === 'passed') ? 0 : 1)
}
