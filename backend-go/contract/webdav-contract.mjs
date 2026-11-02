import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { existsSync, mkdirSync, readFileSync, realpathSync, writeFileSync } from 'node:fs'
import { registerHooks, stripTypeScriptTypes } from 'node:module'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

// CI-only original-source oracle. Never reads author files or configuration,
// never opens a socket, never persists credentials. No business code is copied,
// rewritten or re-exported: erased TS private methods remain runtime-callable.
assert.equal(process.env.GITHUB_ACTIONS, 'true')
const base = process.env.RUNNER_TEMP
const target = process.env.GO_WEBDAV_CONTRACT
assert(base && target && path.isAbsolute(base) && path.isAbsolute(target))
assert.equal(path.basename(path.dirname(target)), 'novel-go-webdav')
assert.equal(path.basename(target), 'contract.json')
const within = (root, value) => {
  const relative = path.relative(root, value)
  return relative && relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative)
}
assert(within(base, target))
assert(!existsSync(target), 'never overwrite oracle output')
assert(process.argv[2] === undefined || process.argv[2] === 'generate')
mkdirSync(path.dirname(target), { recursive: true, mode: 0o700 })
assert(within(realpathSync(base), path.join(realpathSync(path.dirname(target)), path.basename(target))))
const root = new URL('../../', import.meta.url)
const sourceNames = [
  'electron/services/webdav-backup-service.ts',
  'electron/services/cloud-credential-store.ts',
  'electron/platform/endpoint-fetch.ts',
]
const sources = new Map(sourceNames.map(name => {
  const url = new URL(name, root).href
  return [url, { name, text: readFileSync(fileURLToPath(url), 'utf8') }]
}))
const seams = new Map([
  [new URL('electron/utils/config-utils', root).href, 'export function tryReadJsonFile(){throw new Error("configuration forbidden")}; export function writeJsonFile(){throw new Error("configuration forbidden")}'],
  [new URL('electron/services/app-data-locator', root).href, 'export function getGlobalDataRoot(){throw new Error("configuration forbidden")}'],
  [new URL('electron/services/cloud-metadata-lock', root).href, 'export function withCloudMetadataLock(){throw new Error("configuration forbidden")}'],
])
const hooks = registerHooks({
  resolve(specifier, context) {
    if (specifier.startsWith('node:')) {
      assert(['node:crypto', 'node:fs', 'node:path'].includes(specifier))
      return { url: specifier, shortCircuit: true }
    }
    assert(context.parentURL)
    const url = new URL(specifier, context.parentURL).href
    if (seams.has(url)) return { url, shortCircuit: true }
    const resolved = sources.has(url) ? url : `${url}.ts`
    assert(sources.has(resolved), `unexpected dependency: ${specifier}`)
    return { url: resolved, shortCircuit: true }
  },
  load(url, context, nextLoad) {
    if (url.startsWith('node:')) return nextLoad(url, context)
    if (seams.has(url)) return { format: 'module', source: seams.get(url), shortCircuit: true }
    assert(sources.has(url), `unexpected source: ${url}`)
    // Syntax transform only, needed for constructor parameter properties.
    return { format: 'module', source: stripTypeScriptTypes(sources.get(url).text, { mode: 'transform', sourceUrl: url }), shortCircuit: true }
  },
})
const previousFetch = globalThis.fetch
const previousRuntime = process.env.AI_NOVEL_RUNTIME
const previousAllowed = process.env.NOVEL_ALLOWED_ENDPOINTS
globalThis.fetch = () => { throw new Error('real network forbidden') }
process.env.AI_NOVEL_RUNTIME = 'web'
process.env.NOVEL_ALLOWED_ENDPOINTS = 'https://example.invalid'
try {
  const { WebDavBackupService } = await import(new URL('electron/services/webdav-backup-service.ts', root).href)
  const { normalizeWebDavEndpoint } = await import(new URL('electron/services/cloud-credential-store.ts', root).href)
  const hash = bytes => createHash('sha256').update(bytes).digest('hex')
  const account = { endpoint: 'https://example.invalid/dav/', username: 'PUBLIC-writer', secret: 'PUBLIC-not-a-secret' }
  const a = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
  const b = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb'
  const project = '11111111-1111-4111-8111-111111111111'
  const created = '2026-09-21T00:00:00.000Z'
  const archiveBytes = Buffer.from('PUBLIC 合成 😀 archive\u0000')
  const manifest = (id = a, parents = ['parent-b', 'parent-a']) => ({
    version: 1, cloudBookId: 'book:@+', generationId: id, parentGenerationIds: parents,
    createdAt: created, originProjectId: project, portableSnapshotGeneration: 'portable-a',
    archive: { file: 'archive.ainovel', sha256: hash(archiveBytes), byteSize: archiveBytes.length },
  })
  const completion = (m, manifestBytes = Buffer.from(JSON.stringify(m))) => ({
    version: 1, cloudBookId: m.cloudBookId, generationId: m.generationId,
    manifest: { file: 'manifest.json', sha256: hash(manifestBytes), byteSize: manifestBytes.length },
    archive: m.archive, completedAt: created,
  })
  const outcome = async fn => {
    try { return { value: await fn() } } catch (error) {
      assert(typeof error.code === 'string' || error.message === 'CLOUD_ENDPOINT_INVALID')
      return { error: error.code ?? error.message }
    }
  }
  const endpointInputs = [
    ' https://EXAMPLE.invalid:443/dav ', 'http://localhost:80/dav', 'http://127.0.0.1:8080/dav/',
    'http://[::1]/dav', 'https://example.invalid/a/../dav/', 'https://example.invalid/a/%2e%2e/dav/',
    'https://example.invalid/dav%20space', 'https://example.invalid/',
    'http://example.invalid/', 'file:///dav', 'https://user:secret@example.invalid/dav',
    'https://example.invalid/dav?', 'https://example.invalid/dav#', 'https://example.invalid/\u0000', '',
  ]
  const endpoints = []
  for (const input of endpointInputs) endpoints.push({ input, ...await outcome(() => normalizeWebDavEndpoint(input)) })
  const locationInputs = ['book', 'book:@+', 'book.a-b_1', 'Book+root']
  const locations = locationInputs.map(book => {
    const source = new WebDavBackupService().locations(new URL(account.endpoint), book, a)
    return { endpoint: account.endpoint, book, generation: a, value: {
      collections: source.collections.map(String), generations: String(source.generations),
      archive: String(source.archive), manifest: String(source.manifest), completion: String(source.completion),
    } }
  })
  // The original pure parseManifest/parseCompletion/generationHrefs are NOT
  // exported. Exercise their actual call paths through inspectGeneration/list;
  // do not manufacture source exports or extract/rewrite their function bodies.
  async function inspect(m, c, manifestText = JSON.stringify(m)) {
    const manifestBytes = Buffer.from(manifestText)
    const service = new WebDavBackupService({ fetchImpl: async (url, init) => {
      const name = new URL(url).pathname.split('/').at(-1)
      if (name === 'completion.json') return new Response(JSON.stringify(c), { status: 200 })
      if (name === 'manifest.json') return new Response(manifestBytes, { status: 200 })
      if (name === 'archive.ainovel') {
        assert.equal(init.method, 'HEAD')
        return new Response(null, { status: 200, headers: { 'content-length': String(archiveBytes.length) } })
      }
      assert.fail('unexpected synthetic inspection request')
    } })
    return service.inspectGeneration(account, 'book:@+', a)
  }
  const manifestDefinitions = [
    ['valid', manifest()], ['empty-parents', manifest(a, [])],
    ['extra-key', { ...manifest(), extra: true }], ['missing-key', Object.fromEntries(Object.entries(manifest()).filter(([key]) => key !== 'createdAt'))],
    ['bad-version', { ...manifest(), version: 2 }], ['bad-uuid', { ...manifest(), generationId: 'not-uuid' }],
    ['duplicate-parents', manifest(a, ['p', 'p'])], ['bad-parent', manifest(a, ['../escape'])],
    ['bad-time', { ...manifest(), createdAt: '2026-09-21T00:00:00Z' }],
    ['bad-archive-file', { ...manifest(), archive: { ...manifest().archive, file: '../archive.ainovel' } }],
    ['bad-hash', { ...manifest(), archive: { ...manifest().archive, sha256: 'A'.repeat(64) } }],
    ['bad-size', { ...manifest(), archive: { ...manifest().archive, byteSize: -1 } }],
    ['bad-safe-integer', { ...manifest(), archive: { ...manifest().archive, byteSize: 9007199254740992 } }],
    ['bad-origin', { ...manifest(), originProjectId: 'project' }],
  ]
  const manifests = []
  for (const [name, value] of manifestDefinitions) {
    const text = JSON.stringify(value)
    const c = completion(value, Buffer.from(text))
    manifests.push({ name, input: text, ...await outcome(() => inspect(value, c, text)) })
  }
  const good = manifest()
  const completionDefinitions = [
    ['valid', completion(good)], ['extra-key', { ...completion(good), extra: true }],
    ['bad-version', { ...completion(good), version: 2 }], ['bad-time', { ...completion(good), completedAt: 'yesterday' }],
    ['bad-manifest-file', { ...completion(good), manifest: { ...completion(good).manifest, file: '../manifest.json' } }],
    ['bad-manifest-hash', { ...completion(good), manifest: { ...completion(good).manifest, sha256: 'bad' } }],
    ['bad-size', { ...completion(good), archive: { ...good.archive, byteSize: -1 } }],
  ]
  const completions = []
  for (const [name, value] of completionDefinitions) completions.push({ name, input: JSON.stringify(value), ...await outcome(() => inspect(good, value)) })
  const rootPath = '/dav/books/book%3A%40%2B/generations/'
  const rootURL = new URL(rootPath, account.endpoint).href
  const xml = hrefs => `<d:multistatus xmlns:d="DAV:">${hrefs.map(href => `<d:response><d:href>${href}</d:href></d:response>`).join('')}</d:multistatus>`
  async function list(body, inputs = [manifest()]) {
    const byId = new Map(inputs.map(m => [m.generationId, m]))
    const service = new WebDavBackupService({ fetchImpl: async (url, init) => {
      const u = new URL(url)
      if (init.method === 'PROPFIND') return new Response(body, { status: 207 })
      const parts = u.pathname.split('/'); const name = parts.pop(); const id = parts.pop()
      const m = byId.get(id)
      if (!m) return new Response(null, { status: 404 })
      if (name === 'completion.json') return new Response(JSON.stringify(completion(m)), { status: 200 })
      if (name === 'manifest.json') return new Response(JSON.stringify(m), { status: 200 })
      if (name === 'archive.ainovel') return new Response(archiveBytes, { status: 200, headers: { 'content-length': String(archiveBytes.length) } })
      assert.fail('unexpected synthetic catalog request')
    } })
    return service.listGenerations({ account, cloudBookId: 'book:@+' })
  }
  const hrefDefinitions = [
    ['valid-child', xml([rootPath, rootPath + a + '/'])],
    ['absolute-same-origin', xml([rootURL + a + '/'])],
    ['relative-child', xml([a + '/'])], ['duplicate-child', xml([rootPath + a + '/', rootPath + a + '/'])],
    ['outside-root', xml(['/outside/' + a + '/'])], ['cross-origin', xml(['https://evil.invalid/' + a + '/'])],
    ['scheme-relative', xml(['//evil.invalid/' + a + '/'])], ['encoded-traversal', xml([rootPath + '%2e%2e/' + a + '/'])],
    ['encoded-slash', xml([rootPath + a + '%2fchild/'])], ['backslash', xml([rootPath + a + '%5c'])],
    ['query', xml([rootPath + a + '/?secret=1'])], ['fragment', xml([rootPath + a + '/#fragment'])],
    ['external-entity', '<!DOCTYPE x [<!ENTITY e SYSTEM "file:///secret">]>' + xml(['&e;'])],
    ['cdata', '<d:multistatus xmlns:d="DAV:"><![CDATA[x]]></d:multistatus>'],
    ['bad-entity', xml(['&unknown;'])], ['empty-response', '<d:multistatus xmlns:d="DAV:"></d:multistatus>'],
  ]
  const hrefs = []
  for (const [name, body] of hrefDefinitions) {
    const result = await outcome(() => list(body))
    hrefs.push({ name, root: rootURL, input: body, ...(result.error ? { error: result.error } : { value: result.value.map(g => g.generationId) }) })
  }
  const catalogManifests = [manifest(a, ['p-b', 'p-a']), manifest(b, ['p-a', 'p-b'])]
  const catalogs = [{ name: 'parent-set-siblings-preserve-order', input: catalogManifests,
    value: await list(xml([rootPath + a + '/', rootPath + b + '/']), catalogManifests) }]
  const output = {
    formatVersion: 1,
    provenance: [...sources.values()].map(s => ({ path: s.name, sha256: hash(Buffer.from(s.text)) })),
    coverage: 'Original normalize export and untouched private locations/inspectGeneration plus public listGenerations; no pure private parser export exists.',
    endpoints, locations, manifests, completions, hrefs, catalogs,
  }
  writeFileSync(target, JSON.stringify(output) + '\n', { flag: 'wx', mode: 0o600 })
  console.log(JSON.stringify({ contract: target, cases: endpoints.length + locations.length + manifests.length + completions.length + hrefs.length + catalogs.length }))
} finally {
  hooks.deregister()
  globalThis.fetch = previousFetch
  if (previousRuntime === undefined) delete process.env.AI_NOVEL_RUNTIME; else process.env.AI_NOVEL_RUNTIME = previousRuntime
  if (previousAllowed === undefined) delete process.env.NOVEL_ALLOWED_ENDPOINTS; else process.env.NOVEL_ALLOWED_ENDPOINTS = previousAllowed
}
