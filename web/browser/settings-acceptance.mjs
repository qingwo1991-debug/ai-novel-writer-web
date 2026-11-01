import { chromium } from 'playwright'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'

// Read-only settings acceptance: no model requests, config writes or project opens.
const output = process.env.NOVEL_TEST_ARTIFACTS ?? `/workspace/.native-test-data/browser-settings/${new Date().toISOString().replace(/[:.]/g, '-')}`
fs.mkdirSync(output, { recursive: true })
const browser = await chromium.connectOverCDP(process.env.NOVEL_TEST_CDP ?? 'ws://172.18.0.22:3000?timeout=180000')
const context = await browser.newContext({ locale: 'zh-CN', viewport: { width: 1360, height: 1000 }, extraHTTPHeaders: { 'X-Novel-User': 'admin' } })
const page = await context.newPage()
const errors = [], requests = []
page.on('pageerror', error => errors.push(error.message))
page.on('response', response => {
  const url = new URL(response.url())
  if (url.pathname.startsWith('/api/')) requests.push({ method: response.request().method(), path: url.pathname, status: response.status() })
})
try {
  await page.goto(process.env.NOVEL_TEST_BROWSER_URL ?? 'http://ai-novel-native-test:6081', { waitUntil: 'domcontentloaded' })
  await page.getByRole('button', { name: '新建作品', exact: true }).waitFor()
  assert.equal(await page.locator('.stage-window-controls').count(), 0)
  await page.getByRole('button', { name: '设置', exact: true }).click()
  await page.getByRole('button', { name: '网络代理', exact: true }).click()
  await page.getByText('服务器管理的网络代理', { exact: true }).waitFor()
  assert.equal(await page.getByRole('button', { name: '保存代理设置', exact: true }).count(), 0)
  assert.equal(await page.getByLabel('主机地址', { exact: true }).count(), 0)
  await page.screenshot({ path: path.join(output, 'server-managed-proxy.png'), fullPage: true })
  await page.getByRole('button', { name: '关于与更新', exact: true }).click()
  await page.getByText('服务器管理更新', { exact: true }).waitFor()
  await page.getByText(/作品、设定与草稿保存在服务器书库/).waitFor()
  assert.equal(await page.getByRole('button', { name: '检查更新', exact: true }).count(), 0)
  const [popup] = await Promise.all([context.waitForEvent('page'), page.getByRole('button', { name: '官方主页', exact: true }).click()])
  await popup.waitForURL(url => url.protocol === 'https:')
  assert.equal(await popup.evaluate(() => window.opener === null), true)
  const externalOrigin = new URL(popup.url()).origin
  await popup.close()
  await page.screenshot({ path: path.join(output, 'server-library-about-updates.png'), fullPage: true })
  assert.deepEqual(errors, [])
  assert.deepEqual(requests.filter(request => request.status >= 400), [])
  assert.equal(requests.filter(request => /update%3A|llm%3Atest|llm%3Agenerate|config%3Aset/i.test(request.path)).length, 0)
  const report = { passed: true, externalOrigin, errors, requests, output, scope: ['no desktop window controls', 'read-only server-managed proxy', 'server-library about/version', 'deployment-managed updates', 'fixed HTTPS official link with no opener'] }
  fs.writeFileSync(path.join(output, 'report.json'), JSON.stringify(report, null, 2))
  console.log('NATIVE_SETTINGS_ACCEPTANCE', JSON.stringify(report))
} catch (error) {
  await page.screenshot({ path: path.join(output, 'failure.png'), fullPage: true }).catch(() => {})
  fs.writeFileSync(path.join(output, 'report.json'), JSON.stringify({ passed: false, error: error.message, errors, requests, output }, null, 2))
  throw error
} finally { await context.close(); await browser.close() }
