import assert from 'node:assert/strict'
const base = process.env.NOVEL_TEST_URL ?? 'http://127.0.0.1:16081'
const origin = new URL(base).origin
function makeClient(id) {
  let cookie = '', csrf = ''
  const headers = extra => ({ 'X-Novel-User': 'admin', 'X-Client-Id': id, ...(cookie ? { Cookie: cookie } : {}), ...extra })
  async function request(route, options = {}) {
    const res = await fetch(base + route, { ...options, headers: headers(options.headers), redirect: 'error' })
    const next = res.headers.get('set-cookie'); if (next) cookie = next.split(';')[0]
    return res
  }
  return {
    id, request,
    async boot() { const r = await request('/api/bootstrap'); assert.equal(r.status, 200); csrf = (await r.json()).csrfToken; assert(csrf) },
    async invoke(operation, args = [], expected = 200) {
      const r = await request('/api/operations/' + encodeURIComponent(operation), { method: 'POST', headers: { Origin: origin, 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: JSON.stringify({ args }) })
      const data = await r.json(); assert.equal(r.status, expected, `${operation}: ${JSON.stringify(data)}`); return expected === 200 ? data.result : data
    },
    headers(extra) { return headers({ Origin: origin, 'X-CSRF-Token': csrf, ...extra }) },
  }
}
const client = makeClient('native-integration-0000000001')
await client.boot()
assert.equal((await fetch(base + '/')).status, 401, 'all pages require trusted authenticated ingress')
assert.equal((await client.request('/healthz')).status, 200)
assert.equal((await client.invoke('startup:get-state')).state, 'ready')
const models = await client.invoke('llm:list-models')
assert(models.length >= 2)
assert(models.every(model => model.apiKey === '' && model.hasCredential), 'no stored model key reaches browser')
await client.invoke('web:library-list', [], 404)
await client.invoke('window:close', [], 404)
const projects = await client.invoke('project:recent-list')
assert(projects.length)
const first = projects.find(project => project.name === '部署验收测试') ?? projects[0]
assert.equal((await client.invoke('project:peek-overview', [first.previewCapabilityId])).state, 'ready')
const opened = await client.invoke('project:open', [first.path, 'native-test-open'])
assert(opened.success, JSON.stringify(opened))
assert.equal(opened.project.path, first.path)
const ctx = { projectId: opened.project.id, leaseId: opened.project.sessionLease, projectPath: first.path }
const content = '第一章：原生网页持久化验证。\n中文输入不经过远程桌面。'
const file = first.path + '/原生网页验收.txt'
const written = await client.invoke('fs:write-file', [file, content, first.path, ctx])
assert(written.success, JSON.stringify(written))
assert.equal((await client.invoke('fs:read-file', [file, first.path, ctx])).content, content)
const badContext = { ...ctx, leaseId: 'stolen' }
await client.invoke('fs:read-file', [file, first.path, badContext], 409)
await client.invoke('project:open', ['/etc', 'bad-open'], 403)
const second = makeClient('native-integration-0000000002'); await second.boot()
await second.invoke('project:open', [first.path, 'conflicting-open'], 409)
const noCSRF = await client.request('/api/operations/config%3Aget', { method: 'POST', headers: { Origin: origin, 'Content-Type': 'application/json' }, body: JSON.stringify({ args: [] }) })
assert.equal(noCSRF.status, 403)
const wrongOrigin = await client.request('/api/operations/config%3Aget', { method: 'POST', headers: client.headers({ Origin: 'https://evil.invalid', 'Content-Type': 'application/json' }), body: JSON.stringify({ args: [] }) })
assert.equal(wrongOrigin.status, 403)
const model = models[0]
await client.invoke('llm:test-connection', [{ ...model, baseUrl: 'http://169.254.169.254/latest' }], 403)
const lease = await client.invoke('llm:begin-execution-lease', [model.id])
assert(lease.success && lease.lease.leaseId)
await second.invoke('llm:close-execution-lease', [lease.lease.leaseId], 403)
assert((await client.invoke('llm:close-execution-lease', [lease.lease.leaseId])).success)
await client.invoke('db:close', [first.path])
console.log(JSON.stringify({ status: 'passed', checks: ['auth/static', 'bootstrap', 'startup', 'model-redaction', 'explicit-policy', 'library', 'overview', 'project-open', 'Chinese-file-write/read', 'lease-isolation', 'outside-library', 'same-project-tab-lock', 'csrf', 'origin', 'endpoint', 'execution-lease-owner'], projects: projects.length, models: models.length }))
