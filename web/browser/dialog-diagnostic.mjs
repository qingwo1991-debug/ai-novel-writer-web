import { chromium } from 'playwright'
import fs from 'node:fs'
import path from 'node:path'

const output = process.env.NOVEL_TEST_ARTIFACTS ?? '/workspace/.native-test-data/browser-dialog-diagnostic'
fs.mkdirSync(output, { recursive: true })
const browser = await chromium.connectOverCDP(process.env.NOVEL_TEST_CDP ?? 'ws://172.18.0.22:3000')
const context = await browser.newContext({ locale: 'zh-CN', viewport: { width: 1440, height: 1000 }, extraHTTPHeaders: { 'X-Novel-User': 'admin' } })
const page = await context.newPage()
page.setDefaultTimeout(15_000)
try {
  await page.goto(process.env.NOVEL_TEST_BROWSER_URL ?? 'http://ai-novel-native-test:6081', { waitUntil: 'domcontentloaded' })
  const book = process.env.NOVEL_TEST_PROJECT_NAME
    ? page.getByRole('button', { name: `预览《${process.env.NOVEL_TEST_PROJECT_NAME}》`, exact: true })
    : page.getByRole('button', { name: /^预览《原生浏览器验收-/ }).last()
  await book.dblclick()
  await page.getByRole('button', { name: /作品菜单$/ }).click()
  await page.getByRole('menuitem', { name: '小说拆解与仿写…', exact: true }).click()
  await page.getByTestId('import-purpose-author').click()
  await page.getByTestId('import-source-choose').click()
  await page.waitForFunction(() => [...document.querySelectorAll('[role="dialog"]')].some(element => element.textContent.includes('从本机选择文件并上传到服务器')))
  const state = await page.evaluate(() => {
    const detail = element => {
      const rect = element.getBoundingClientRect(), css = getComputedStyle(element)
      return { tag: element.tagName, className: String(element.className), role: element.getAttribute('role'), ariaHidden: element.getAttribute('aria-hidden'), rect: { x: rect.x, y: rect.y, width: rect.width, height: rect.height }, css: { zIndex: css.zIndex, position: css.position, display: css.display, visibility: css.visibility, pointerEvents: css.pointerEvents, transform: css.transform, isolation: css.isolation } }
    }
    return [...document.querySelectorAll('[role="dialog"]')].map(dialog => ({
      text: dialog.textContent.slice(0, 1000), detail: detail(dialog),
      ancestors: (() => { const result = []; for (let node = dialog.parentElement; node; node = node.parentElement) result.push(detail(node)); return result })(),
      inputs: [...dialog.querySelectorAll('input')].map(detail),
    }))
  })
  fs.writeFileSync(path.join(output, 'dialogs.json'), JSON.stringify(state, null, 2))
  await page.screenshot({ path: path.join(output, 'dialogs.png'), fullPage: true })
  console.log(JSON.stringify(state, null, 2))
} catch (error) {
  await page.screenshot({ path: path.join(output, 'diagnostic-failure.png'), fullPage: true })
  fs.writeFileSync(path.join(output, 'diagnostic-failure.txt'), `${error.message}\n\n${await page.locator('body').innerText()}`)
  throw error
} finally {
  await context.close()
  await browser.close()
}
