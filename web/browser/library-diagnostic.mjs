import { chromium } from 'playwright'
const browser = await chromium.connectOverCDP('ws://172.18.0.22:3000?timeout=60000')
const context = await browser.newContext({ extraHTTPHeaders: { 'X-Novel-User': 'admin' } })
const page = await context.newPage()
try {
  await page.goto('http://ai-novel-native-test:6081', { waitUntil: 'domcontentloaded' })
  await page.getByRole('button', { name: '打开作品', exact: true }).click()
  const chooser = page.getByRole('dialog').filter({ hasText: '服务器书库' })
  await chooser.waitFor({ state: 'visible' })
  await chooser.locator('select option').filter({ hasText: '恢复副本' }).first().waitFor({ state: 'attached', timeout: 15_000 })
  console.log('RESTORED_LIBRARY_CHOICES', JSON.stringify(await chooser.locator('select option').allTextContents()))
} finally { await context.close(); await browser.close() }
