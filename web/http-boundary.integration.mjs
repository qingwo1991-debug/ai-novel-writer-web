import assert from 'node:assert/strict'
import crypto from 'node:crypto'
import fs from 'node:fs'
import path from 'node:path'

// Direct HTTP boundary test, NOT browser or nginx-password acceptance.
// Fail closed to the disposable loopback gateway; never target production.
const base = process.env.NOVEL_BOUNDARY_URL ?? 'http://127.0.0.1:16081'
assert.equal(base, 'http://127.0.0.1:16081', 'Only the isolated test gateway is permitted')
const origin = new URL(base).origin
const stamp = new Date().toISOString().replace(/[:.]/g, '-')
const output = process.env.NOVEL_BOUNDARY_OUTPUT ?? `.native-test-data/http-boundary/${stamp}`
assert(path.resolve(output).startsWith(path.resolve('.native-test-data') + path.sep), 'Evidence must remain private')
fs.mkdirSync(output, { recursive: true, mode: 0o700 })
const report = { status: 'running', startedAt: new Date().toISOString(), runtimeImage: process.env.NOVEL_BOUNDARY_IMAGE ?? 'not-supplied', scope: 'isolated trusted-proxy HTTP contract; not password login or provider execution', checks: [], paidDispatches: 0 }
function client(principal) {
  const id = 'boundary-' + crypto.randomBytes(16).toString('hex')
  let cookie = '', csrf = ''
  return {
    async request(route, options = {}, { authenticated = true, useCSRF = true } = {}) {
      const response = await fetch(base + route, {
        ...options, redirect: 'error', signal: AbortSignal.timeout(15_000),
        headers: {
          ...(authenticated ? { 'X-Novel-User': principal } : {}),
          'X-Client-Id': id, ...(cookie ? { Cookie: cookie } : {}),
          ...(options.method && options.method !== 'GET' ? { Origin: origin, ...(useCSRF ? { 'X-CSRF-Token': csrf } : {}) } : {}),
          ...options.headers,
        },
      })
      const next = response.headers.get('set-cookie')
      if (next) cookie = next.split(';')[0]
      return response
    },
    async bootstrap() {
      const r = await this.request('/api/bootstrap')
      assert.equal(r.status, 200)
      csrf = (await r.json()).csrfToken
      assert(csrf)
      return r
    },
    operation(name, args, headers = {}, controls) {
      return this.request('/api/operations/' + encodeURIComponent(name), { method: 'POST', headers: { 'Content-Type': 'application/json', ...headers }, body: JSON.stringify({ args }) }, controls)
    },
  }
}
const admin = client('admin'), reader = client('bounded-reader')
async function check(name, run) {
  await run()
  report.checks.push({ name, status: 'passed' })
}
async function error(response, status, code) {
  assert.equal(response.status, status)
  const data = await response.json()
  assert.equal(data.error?.code, code)
}
try {
  await check('native Node health', async () => {
    const r = await fetch(base + '/healthz', { signal: AbortSignal.timeout(5000) })
    assert.equal(r.status, 200)
    const h = await r.json(); assert.equal(h.runtime, 'node'); assert.equal(h.electron, false)
  })
  await check('no trusted identity rejected', async () => { await error(await admin.request('/api/bootstrap', {}, { authenticated: false }), 401, 'AUTHENTICATION_REQUIRED') })
  await check('bootstrap cookie is HttpOnly and SameSite Strict', async () => {
    const r = await admin.bootstrap(); const cookie = r.headers.get('set-cookie')
    assert.match(cookie, /HttpOnly/); assert.match(cookie, /SameSite=Strict/)
  })
  await reader.bootstrap()
  await check('missing CSRF rejected', async () => { await error(await admin.operation('config:get', [], {}, { useCSRF: false }), 403, 'CSRF_DENIED') })
  await check('wrong Origin rejected', async () => { await error(await admin.operation('config:get', [], { Origin: 'https://untrusted.example' }), 403, 'ORIGIN_DENIED') })
  await check('cross-site request rejected', async () => { await error(await admin.operation('config:get', [], { 'Sec-Fetch-Site': 'cross-site' }), 403, 'CROSS_SITE_DENIED') })
  await check('non-JSON operation rejected', async () => { await error(await admin.request('/api/operations/config%3Aget', { method: 'POST', headers: { 'Content-Type': 'text/plain' }, body: '{}' }), 415, 'JSON_REQUIRED') })
  await check('malformed JSON rejected', async () => { await error(await admin.request('/api/operations/config%3Aget', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{' }), 400, 'INVALID_JSON') })
  await check('non-array arguments rejected', async () => { await error(await admin.operation('config:get', {}), 400, 'INVALID_ARGUMENTS') })
  await check('prototype-bearing arguments rejected', async () => {
    await error(await admin.request('/api/operations/config%3Aget', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"args":[{"__proto__":{"polluted":true}}]}' }), 400, 'INVALID_OBJECT_KEY')
  })
  await check('over-deep arguments rejected', async () => {
    let nested = {}; for (let n = 0; n < 36; n++) nested = { nested }
    await error(await admin.operation('config:get', [nested]), 400, 'ARGUMENT_COMPLEXITY')
  })
  for (const op of ['window:close', 'web:library-list', 'mcp:get-config-path', 'nonexistent:operation']) {
    await check('excluded operation: ' + op, async () => { await error(await admin.operation(op, []), 404, 'OPERATION_NOT_EXPOSED') })
  }
  await check('ordinary author cannot configure MCP', async () => { await error(await reader.operation('mcp:load-config', []), 403, 'MCP_SERVER_ADMIN_ONLY') })
  await check('ordinary author cannot spawn MCP server', async () => { await error(await reader.operation('mcp:connect', ['nonexistent-test-server']), 403, 'MCP_SERVER_ADMIN_ONLY') })
  await check('unknown global config field rejected', async () => { await error(await admin.operation('config:set', [{ arbitraryCommand: 'not-executed' }]), 400, 'CONFIG_FIELD_DENIED') })
  await check('browser cannot replace deployment proxy', async () => { await error(await admin.operation('config:set', [{ proxy: { enabled: true, url: 'http://127.0.0.1:1' } }]), 403, 'PROXY_SERVER_ADMIN_ONLY') })
  await check('project outside managed library rejected', async () => { await error(await admin.operation('project:open', ['/etc', 'boundary-no-open']), 403, 'PROJECT_OUTSIDE_LIBRARY') })
  await check('empty endpoint policy rejects test connection before provider', async () => {
    await error(await admin.operation('llm:test-connection', [{ id: 'boundary-never-dispatched', provider: 'openai', protocol: 'openai', baseUrl: 'http://127.0.0.1:1', apiKey: '' }]), 403, 'ENDPOINT_NOT_ALLOWED')
  })
  await check('unknown browser dialog rejected', async () => { await error(await admin.request('/api/dialogs/not-issued', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"canceled":true}' }), 404, 'DIALOG_EXPIRED') })
  await check('unknown download capability rejected', async () => { await error(await admin.request('/api/downloads/not-issued'), 404, 'DOWNLOAD_EXPIRED') })
  await check('cross-origin SSE rejected before connection', async () => { await error(await admin.request('/api/events?client=boundary-unused-client', { headers: { Origin: 'https://untrusted.example' } }), 403, 'ORIGIN_DENIED') })
  let folder
  await check('owned staged folder created', async () => {
    const r = await admin.request('/api/upload-folders', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' })
    assert.equal(r.status, 201); folder = (await r.json()).folderUploadId; assert(folder)
  })
  async function upload(who, relative) {
    return who.request('/api/uploads', { method: 'POST', headers: { 'Content-Type': 'application/octet-stream', 'X-Upload-Name': 'boundary.txt', 'X-Upload-Folder': folder, 'X-Upload-Relative': encodeURIComponent(relative) }, body: 'Synthetic isolated upload; no author data.' })
  }
  await check('foreign session cannot append staged folder', async () => { await error(await upload(reader, 'foreign.txt'), 403, 'UPLOAD_FOLDER_NOT_OWNED') })
  for (const [name, relative] of [['parent traversal', '../escape.txt'], ['absolute path', '/tmp/escape.txt'], ['backslash path', 'nested\\escape.txt'], ['dot component', 'nested/./escape.txt'], ['NUL component', 'bad\0.txt']]) {
    await check('folder upload rejects ' + name, async () => { await error(await upload(admin, relative), 400, 'INVALID_PATH') })
  }
  await check('valid nested upload accepted inside owned folder', async () => { const r = await upload(admin, 'nested/合法文件.txt'); assert.equal(r.status, 201); assert((await r.json()).uploadId) })
  report.status = 'passed'
} catch (failure) {
  report.status = 'failed'; report.failure = { name: failure.name, message: failure.message.slice(0, 600) }; process.exitCode = 1
} finally {
  report.finishedAt = new Date().toISOString()
  fs.writeFileSync(path.join(output, 'report.json'), JSON.stringify(report, null, 2), { mode: 0o600 })
  console.log(JSON.stringify({ status: report.status, checks: report.checks.length, paidDispatches: report.paidDispatches, report: path.join(output, 'report.json'), failure: report.failure }))
}
