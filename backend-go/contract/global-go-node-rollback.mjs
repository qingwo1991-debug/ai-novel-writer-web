import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { spawn } from 'node:child_process'
import fs from 'node:fs'
import net from 'node:net'
import path from 'node:path'
import { setTimeout as delay } from 'node:timers/promises'

import { runGlobalDataMigration } from '../../electron/services/global-data-migration.ts'

// GitHub-only, disposable same-volume rollback check. No author path, host
// secret, published image, or production service participates in this test.
assert.equal(process.env.GITHUB_ACTIONS, 'true')
assert.equal(process.platform, 'linux')
const runnerTemp = process.env.RUNNER_TEMP
assert(runnerTemp && path.isAbsolute(runnerTemp) && fs.statSync(runnerTemp).isDirectory())
const binary = path.resolve('out/novel-server')
const policy = path.resolve('../web/operations.json')
assert(fs.statSync(binary).isFile() && fs.statSync(policy).isFile())

// The original migration uses default mkdir modes. The restricted umask makes
// its journal, staging and installed generation private from first creation.
const originalUmask = process.umask(0o077)
const parent = fs.mkdtempSync(path.join(runnerTemp, 'novel-go-node-rollback-'))
fs.chmodSync(parent, 0o700)
const roots = {
  legacySource: path.join(parent, 'legacy'),
  canonicalTarget: path.join(parent, 'canonical'),
  userData: path.join(parent, 'user-data'),
  exclusiveAccess: true,
}
for (const root of [roots.legacySource, roots.canonicalTarget, roots.userData]) {
  fs.mkdirSync(root, { mode: 0o700 })
}
function put(root, relative, value) {
  const file = path.join(root, relative)
  fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 })
  fs.writeFileSync(file, value, { flag: 'wx', mode: 0o600 })
}
put(roots.legacySource, 'models.json', JSON.stringify([{ id: 'synthetic-model', provider: 'openai', name: 'Synthetic' }]))
put(roots.legacySource, 'config.json', JSON.stringify({ theme: 'dark', defaultModelId: 'synthetic-model' }))
put(roots.legacySource, 'logs/retained.txt', 'Original synthetic source stays untouched')
put(roots.userData, 'author-sentinel.txt', 'Synthetic runtime state stays untouched')

function inventory(root) {
  const entries = []
  function walk(directory) {
    for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
      assert(!entry.isSymbolicLink())
      const file = path.join(directory, entry.name)
      const relative = path.relative(root, file).split(path.sep).join('/')
      if (entry.isDirectory()) {
        entries.push({ path: relative, kind: 'directory' })
        walk(file)
      } else {
        assert(entry.isFile())
        const bytes = fs.readFileSync(file)
        entries.push({ path: relative, kind: 'file', size: bytes.length,
          sha256: createHash('sha256').update(bytes).digest('hex') })
      }
    }
  }
  walk(root)
  return entries.sort((left, right) => left.path < right.path ? -1 : left.path > right.path ? 1 : 0)
}

const before = { legacy: inventory(roots.legacySource), userData: inventory(roots.userData) }
const original = runGlobalDataMigration(roots)
assert.equal(original.state, 'ready')
assert.match(original.globalGeneration, /^[a-f0-9-]{36}$/u)
assert.deepEqual(inventory(roots.legacySource), before.legacy)
assert.deepEqual(inventory(roots.userData), before.userData)
const receiptPath = path.join(roots.canonicalTarget, '.migration', 'receipt.json')
const receiptBefore = fs.readFileSync(receiptPath)

async function unusedLoopbackPort() {
  const listener = net.createServer()
  await new Promise((resolve, reject) => listener.once('error', reject).listen(0, '127.0.0.1', resolve))
  const address = listener.address()
  assert(address && typeof address !== 'string')
  await new Promise((resolve, reject) => listener.close(error => error ? reject(error) : resolve()))
  return address.port
}

const port = await unusedLoopbackPort()
const address = `127.0.0.1:${port}`
const baseURL = `http://${address}`
const childEnv = { ...process.env, TMPDIR: runnerTemp }
delete childEnv.NODE_ENV
delete childEnv.NOVEL_DATA_ROOT
const child = spawn(binary, ['-listen', address, '-policy', policy,
  '-synthetic-completed-global-parent', parent,
  '-synthetic-model-origins', 'https://synthetic.example'], {
  cwd: path.resolve('.'), env: childEnv, stdio: ['ignore', 'pipe', 'pipe'],
})
let childLog = ''
let processClosed = false
for (const stream of [child.stdout, child.stderr]) {
  stream.on('data', chunk => { childLog = (childLog + chunk.toString('utf8')).slice(-4096) })
}
const exited = new Promise(resolve => {
  child.once('close', (code, signal) => { processClosed = true; resolve({ code, signal }) })
  child.once('error', error => {
    childLog = `Go process start failed: ${error.code ?? 'unknown'}`
    processClosed = true
    resolve({ code: null, signal: null })
  })
})
async function stopChild() {
  if (processClosed || child.exitCode !== null || child.signalCode !== null) return await exited
  child.kill('SIGTERM')
  let timeout
  const stopped = await Promise.race([exited, new Promise(resolve => {
    timeout = setTimeout(() => resolve(null), 12_000)
  })])
  clearTimeout(timeout)
  if (stopped) return stopped
  child.kill('SIGKILL')
  await exited
  throw new Error('Go synthetic process did not stop after SIGTERM')
}
async function request(url, options = {}) {
  return fetch(url, { ...options, signal: AbortSignal.timeout(3000) })
}
async function rpc(operation, args, cookie, csrf) {
  const response = await request(`${baseURL}/api/operations/${operation}`, {
    method: 'POST',
    headers: {
      'X-Novel-User': 'synthetic-author', 'X-Client-Id': 'synthetic-rollback-tab',
      'X-CSRF-Token': csrf, Origin: baseURL, Cookie: cookie,
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({ args }),
  })
  const body = await response.json()
  assert.equal(response.status, 200, `${operation} rejected: ${response.status}`)
  return body.result
}

let stopped
try {
  let healthy = false
  for (let attempt = 0; attempt < 80; attempt++) {
    if (processClosed || child.exitCode !== null || child.signalCode !== null) {
      throw new Error(`Go synthetic process exited early: ${childLog}`)
    }
    try {
      const response = await request(`${baseURL}/healthz`)
      if (response.ok) { healthy = true; break }
    } catch { /* The loopback listener may not have started yet. */ }
    await delay(100)
  }
  assert(healthy, `Go synthetic process did not become healthy: ${childLog}`)
  const bootstrap = await request(`${baseURL}/api/bootstrap`, {
    headers: { 'X-Novel-User': 'synthetic-author' },
  })
  assert.equal(bootstrap.status, 200)
  const cookie = bootstrap.headers.get('set-cookie')?.split(';', 1)[0]
  const { csrfToken } = await bootstrap.json()
  assert(cookie && typeof csrfToken === 'string' && csrfToken.length > 0)
  const startup = await rpc('startup:get-state', [], cookie, csrfToken)
  assert.equal(startup.state, 'ready')
  assert.equal(startup.globalGeneration, original.globalGeneration)
  assert.equal(startup.migrationNotice.legacySourceIgnored, true)
  const saved = await rpc('config:set', [{ theme: 'light' }], cookie, csrfToken)
  assert.deepEqual(saved, { success: true })
  const config = await rpc('config:get', [], cookie, csrfToken)
  assert.equal(config.theme, 'light')
} finally {
  stopped = await stopChild()
}
assert.deepEqual(stopped, { code: 0, signal: null }, `Go synthetic process exited unexpectedly: ${childLog}`)

const reopened = runGlobalDataMigration(roots)
assert.equal(reopened.state, 'ready')
assert.equal(reopened.globalGeneration, original.globalGeneration)
assert.equal(reopened.dataRoot, original.dataRoot)
assert.deepEqual(fs.readFileSync(receiptPath), receiptBefore)
const committed = JSON.parse(fs.readFileSync(path.join(reopened.dataRoot, 'config.json'), 'utf8'))
assert.equal(committed.theme, 'light')
assert.equal(committed.defaultModelId, 'synthetic-model')
assert.deepEqual(inventory(roots.legacySource), before.legacy)
assert.deepEqual(inventory(roots.userData), before.userData)
process.umask(originalUmask)
console.log(JSON.stringify({ state: reopened.state, sameGeneration: true,
  goWriteReopenedByOriginalTypeScript: true, sourceAndUserDataUnchanged: true }))
