import { chromium } from 'playwright'
import assert from 'node:assert/strict'
const browser = await chromium.connectOverCDP(process.env.NOVEL_TEST_CDP ?? 'http://172.18.0.22:3000')
const context = await browser.newContext({ locale: 'zh-CN', extraHTTPHeaders: { 'X-Novel-User': 'admin' } })
const page = await context.newPage()
const errors = []
page.on('pageerror', error => errors.push(error.message))
page.on('console', message => { if (message.type() === 'error') console.log('BROWSER_CONSOLE', message.text().slice(0, 500)) })
try {
  await page.goto(process.env.NOVEL_TEST_BROWSER_URL ?? 'http://ai-novel-native-test:6081', { waitUntil: 'domcontentloaded', timeout: 30000 })
  await page.waitForFunction(() => document.body.innerText.includes('新建作品'), { timeout: 20000 })
  const text = await page.locator('body').innerText()
  assert(!text.includes('Electron'))
  assert.equal(await page.evaluate(() => typeof window.aiNovelAPI), 'undefined')
  assert.equal(await page.evaluate(() => document.documentElement.dataset.runtime), 'web')
  await page.screenshot({ path: '/workspace/.native-test-data/native-library.png', fullPage: true })
  console.log('NATIVE_BROWSER_LIBRARY', JSON.stringify({ text: text.slice(0, 5000), errors }))
  assert.deepEqual(errors, [])
} finally { await context.close(); await browser.close() }
