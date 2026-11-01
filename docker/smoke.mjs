// Opt-in internal diagnostics only: run with NOVEL_DEBUG=1, never publish CDP ports.
// docker cp docker/smoke.mjs ai-novel-writer:/app/novel-smoke.mjs
// docker exec ai-novel-writer node /app/novel-smoke.mjs inspect
import assert from 'node:assert/strict';
import { chromium } from 'playwright';

async function mainEval(expression) {
  const [target] = await (await fetch('http://127.0.0.1:9229/json/list')).json();
  const socket = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((resolve, reject) => { socket.onopen = resolve; socket.onerror = reject; });
  try {
    const response = await new Promise((resolve, reject) => {
      socket.onmessage = event => { const data = JSON.parse(event.data); if (data.id === 1) resolve(data); };
      socket.onerror = reject;
      socket.send(JSON.stringify({id: 1, method: 'Runtime.evaluate', params: {expression, awaitPromise: true, returnByValue: true}}));
    });
    if (response.result?.exceptionDetails) throw new Error(JSON.stringify(response.result.exceptionDetails));
    return response.result?.result?.value;
  } finally { socket.close(); }
}

const browser = await chromium.connectOverCDP('http://127.0.0.1:9222');
const context = browser.contexts()[0];
const page = context.pages().find(p => p.url().startsWith('file:///app/dist/'));
assert(page, 'Electron renderer exists');
const action = process.argv[2] || 'inspect';
try {
  if (action === 'inspect') {
    console.log('RENDERER', await page.evaluate(() => ({title: document.title, language: document.documentElement.lang, text: document.body.innerText.slice(0, 5500)})));
    console.log('MAIN', await mainEval(`({node:process.versions.node,proxy:process.env.HTTPS_PROXY,optin:process.env.NODE_USE_ENV_PROXY,dispatchers:Object.getOwnPropertySymbols(globalThis).filter(s=>String(s).includes('undici')).map(s=>[String(s),globalThis[s]?.constructor?.name])})`));
    console.log('MAIN FETCH', await mainEval(`fetch('https://registry.npmmirror.com',{signal:AbortSignal.timeout(12000)}).then(r=>({status:r.status})).catch(e=>({error:e.message,cause:e.cause?.code}))`));
    await page.screenshot({path: '/tmp/novel-ui.png'});
  } else if (action === 'create') {
    await page.getByRole('button', {name: '新建作品', exact: true}).click();
    await page.getByLabel('作品名', {exact: true}).fill('部署验收测试');
    // The main process grants the actual native folder selection, never bypassed.
    await page.getByRole('button', {name: '选择', exact: true}).click();
    console.log('Native folder chooser ready; select /data/workspaces, then run finish-create.');
  } else if (action === 'finish-create') {
    console.log('DIALOG', await page.getByRole('dialog').innerText());
    await page.getByRole('button', {name: '创建作品', exact: true}).click();
    await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    console.log('CREATED', await page.evaluate(() => document.body.innerText.slice(0, 5500)));
  } else if (action === 'write') {
    const result = await page.evaluate(async () => {
      const api = window.aiNovelAPI;
      const recent = await api.invoke('project:recent-list');
      const project = recent.find(item => item.name === '部署验收测试');
      if (!project) throw new Error('Test project missing');
      const opened = await api.invoke('project:open', project.path, 'docker-smoke-open', null);
      if (!opened.success) throw new Error(JSON.stringify(opened));
      const session = opened.project;
      const ctx = {projectId: session.id, leaseId: session.sessionLease, projectPath: project.path};
      return {project, openedKeys: Object.keys(session), session, ctx};
    });
    const saved = await page.evaluate(async ({project, session, ctx}) => {
      const outline = '部署验收：小说内容保存在服务器磁盘，容器重启后应保持原样。';
      const result = await window.aiNovelAPI.invoke('project:update-config', session.id,
        {novelConfig: {...session.novelConfig, coreOutline: outline}}, project.path, ctx);
      return {result, outline, projectPath: project.path};
    }, result);
    assert(saved.result.success, JSON.stringify(saved));
    console.log('SAVED', JSON.stringify(saved));
  } else if (action === 'check-persistence') {
    const result = await page.evaluate(async () => {
      const api = window.aiNovelAPI;
      const recent = await api.invoke('project:recent-list');
      const project = recent.find(item => item.name === '部署验收测试');
      if (!project) throw new Error('Test project missing');
      const opened = await api.invoke('project:open', project.path, 'docker-smoke-reopen', null);
      return {success: opened.success, path: project.path, outline: opened.project?.novelConfig.coreOutline};
    });
    assert(result.success && result.outline === '部署验收：小说内容保存在服务器磁盘，容器重启后应保持原样。', JSON.stringify(result));
    console.log('PERSISTED', result);
  } else if (action === 'file-io') {
    const result = await page.evaluate(async () => {
      const api = window.aiNovelAPI;
      const project = (await api.invoke('project:recent-list')).find(x => x.name === '部署验收测试');
      const opened = await api.invoke('project:open', project.path, 'smoke-file-io', null);
      const ctx = {projectId: opened.project.id, leaseId: opened.project.sessionLease, projectPath: project.path};
      const tree = await api.invoke('fs:list-dir', project.path, project.path, ctx);
      const file = project.path + '/部署验收正文.txt';
      const written = await api.invoke('fs:write-file', file, '第一章：雨夜的列车驶入车站。\\n这是服务器持久化验证正文。', project.path, ctx);
      const read = await api.invoke('fs:read-file', file, project.path, ctx);
      return {entries: tree.length, written, read};
    });
    assert(result.written.success && result.read.success && result.read.content.includes('雨夜'), JSON.stringify(result));
    console.log('FILE IO', result);
  } else if (action === 'portal') {
    const portal = context.pages().find(p => p.url().startsWith('http://192.168.1.15:6080/'));
    assert(portal, 'Portal test window exists');
    const frame = portal.frameLocator('#desktop');
    await frame.locator('#noVNC_status').filter({hasText: /Connected|已连接/}).waitFor({state: 'attached', timeout: 20000});
    await portal.getByRole('button', {name: '中文剪贴板', exact: true}).click();
    await portal.locator('#text').fill('浏览器中文剪贴板端到端测试');
    await portal.locator('#copy').click();
    await portal.getByRole('status').filter({hasText: '已发送'}).waitFor();
    await portal.screenshot({path: '/tmp/portal-ui.png'});
    console.log('PORTAL connected noVNC + browser clipboard OK');
  } else if (action === 'eval') {
    console.log(await page.evaluate(process.argv[3]));
  } else if (action === 'main-eval') {
    console.log(await mainEval(process.argv[3]));
  } else if (action === 'ui-open') {
    await page.getByRole('button', {name: '预览《部署验收测试》', exact: true}).dblclick();
    await page.getByLabel('核心大纲 · 必填', {exact: true}).waitFor({timeout: 10000}).catch(() => {});
    await page.screenshot({path: '/tmp/novel-ui.png'});
    console.log('UI OPEN', await page.evaluate(() => document.body.innerText.slice(0, 5500)));
  } else if (action === 'screenshot') {
    console.log(await page.evaluate(() => document.body.innerText.slice(0, 6000)));
    await page.screenshot({path: '/tmp/novel-ui.png'});
  } else throw new Error('Unknown smoke action');
} finally { await browser.close(); }
