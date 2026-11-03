// GitHub-only ACTUAL source oracle. Admission/dialog/FS capabilities are seams;
// import-controller, inspection-store, identity repository, EPUB extraction and
// shared prose counter execute unmodified from this checkout, not copied code.
import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { DatabaseSync } from 'node:sqlite'
import { fileURLToPath } from 'node:url'

assert.equal(process.env.GITHUB_ACTIONS, 'true', 'import-source oracle is GitHub-only')
const base = process.env.RUNNER_TEMP, target = process.env.GO_IMPORT_SOURCE_CONTRACT
assert(base && target && path.isAbsolute(base) && path.isAbsolute(target))
assert.equal(target, path.join(base, 'novel-go-import-source', 'contract.json'))
const directory = path.dirname(target)
assert(!existsSync(directory), 'never reuse an oracle directory')
mkdirSync(directory, { recursive: true, mode: 0o700 })
const root = fileURLToPath(new URL('../../', import.meta.url))
const require = createRequire(import.meta.url)
// Required remotely only: esbuild@0.28.1, yauzl@2.10.0 (no Electron/SDK/DB install).
const { build } = require('esbuild')
assert.equal(require('esbuild/package.json').version, '0.28.1')
assert.equal(require('yauzl/package.json').version, '2.10.0')
const key = '__GO_IMPORT_SOURCE_ACTUAL__'
assert(!Object.hasOwn(globalThis, key))
const state = { handler: null, selection: [], snapshot: null, sizes: new Map(), db: null, uuidSequence: 0 }
state.uuid = () => `00000000-0000-4000-8000-${String(++state.uuidSequence).padStart(12, '0')}`
globalThis[key] = state
const S = `globalThis[${JSON.stringify(key)}]`
const originalModules = new Set()
const controller = path.join(root, 'electron/controllers/import-controller.ts')
const allowed = new Set([
  controller,
  path.join(root, 'electron/services/import-inspection-store.ts'),
  path.join(root, 'electron/repositories/import-source-identity-repository.ts'),
  path.join(root, 'electron/services/epub-extraction-service.ts'),
  path.join(root, 'electron/services/__tests__/epub-test-fixture.ts'),
  path.join(root, 'src/shared/draft-units.ts'),
  path.join(root, 'src/shared/import-limits.ts'),
])
const stubs = {
  'electron/platform.ts': `export const appHost={getLocale:()=> 'en-US'}; export const operations={handle:(name,fn)=>{if(name==='dialog:select-novel-files')${S}.handler=fn}}`,
  'electron/utils/remembered-dialog.ts': `export const showOpenDialogInLastDirectory=async()=>({canceled:false,filePaths:${S}.selection})`,
  'electron/services/external-file-grant-service.ts': 'export class ExternalFileGrantService {}; export const externalFileGrants={}',
  'electron/services/import-source-identity-secret.ts': `export const loadApplicationImportSourceSecret=()=>{throw Error('unexpected key read')}`,
  'electron/i18n.ts': 'export const mainText=(locale,zh,en)=>en',
  'electron/controllers/import-run-failure.ts': 'export const importRunLocale=()=>"en-US"',
  'src/i18n/app-errors.ts': 'export const knownAppErrorMessage=()=>undefined',
  'electron/security/windows-safe-file-system.ts': 'export const windowsSafeFileSystem={}',
  'electron/database.ts': `export const getCurrentProjectPath=()=>null; export const getProjectDb=()=>${S}.db`,
  'electron/services/project-access.ts': 'export const projectAccess={assertCurrentProjectContext:()=>{throw Error("unexpected project access")}}',
  'electron/utils/project-context.ts': 'export const assertRequiredExpectedProjectPath=()=>{throw Error("unexpected project context")}',
  'electron/repositories/import-run-repository.ts': 'export const ImportRunRepository={}',
}
const built = await build({
  stdin: { contents: `export {registerImportController} from './electron/controllers/import-controller.ts'; export {ImportInspectionStore} from './electron/services/import-inspection-store.ts'; export {ImportSourceIdentityRepository} from './electron/repositories/import-source-identity-repository.ts'; export {extractEpubChapters} from './electron/services/epub-extraction-service.ts'; export {storedZip} from './electron/services/__tests__/epub-test-fixture.ts'; export {countDraftUnits} from './src/shared/draft-units.ts'`, resolveDir: root, sourcefile: 'import-source-entry.mjs' },
  bundle: true, write: false, platform: 'node', format: 'esm', banner: { js: "import {createRequire as __createRequire} from 'node:module'; const require=__createRequire(import.meta.url);" },
  plugins: [{ name: 'actual-import-boundaries', setup(api) {
    // Node's createRequire honors the parent's isolated NODE_PATH install;
    // esbuild does not automatically inherit that resolution search path.
    api.onResolve({ filter: /^yauzl$/ }, () => ({ path: require.resolve('yauzl') }))
    api.onResolve({ filter: /^node:fs$/ }, args => args.importer === controller ? { path: 'captured-stat', namespace: 'seam' } : undefined)
    api.onResolve({ filter: /^node:crypto$/ }, args => args.namespace === 'file' ? { path: 'deterministic-crypto', namespace: 'seam' } : undefined)
    api.onResolve({ filter: /^\./ }, args => {
      if (args.importer && !allowed.has(args.importer) && !args.importer.endsWith('import-source-entry.mjs')) return undefined
      const filename = path.resolve(args.resolveDir, args.path.endsWith('.ts') ? args.path : args.path + '.ts')
      const relative = path.relative(root, filename).split(path.sep).join('/')
      if (Object.hasOwn(stubs, relative)) return { path: relative, namespace: 'seam' }
      assert(allowed.has(filename), `unexpected original dependency: ${relative}`)
      originalModules.add(relative)
      return { path: filename }
    })
    api.onLoad({ filter: /.*/, namespace: 'seam' }, args => ({ contents: args.path === 'captured-stat'
      ? `export const statSync=p=>({size:${S}.sizes.get(p)}); export const realpathSync=Object.assign(p=>p,{native:p=>p})`
      : args.path === 'deterministic-crypto'
        ? `export {createHash,createHmac,createDecipheriv} from 'node:crypto'; export const randomUUID=()=>${S}.uuid()`
        : stubs[args.path], loader: 'js' }))
  } }],
})
const bundle = path.join(directory, 'actual-import-oracle.mjs')
writeFileSync(bundle, built.outputFiles[0].contents, { flag: 'wx', mode: 0o600 })
const actual = await import(new URL(`file://${bundle}`).href)
const serverKey = Buffer.alloc(32, 107)
class CapturingStore extends actual.ImportInspectionStore {
  create(input) { state.snapshot = structuredClone(input); return super.create(input) }
}
const store = new CapturingStore()
const files = new Map()
const fileSystem = {
  readText: async ({ filePath }) => new TextDecoder('utf-8', { fatal: true }).decode(files.get(filePath)),
  readBytes: async ({ filePath }) => files.get(filePath),
}
const grants = {
  issueFile: ({ filePath }) => ({ grantId: filePath }),
  resolve: ({ grantId }) => ({ filePath: grantId }),
  revoke() {}, revokeWebContents() {},
}
actual.registerImportController(fileSystem, p => ({ canonicalLocation: p, fileIdentity: 'stage:synthetic:1' }), {}, grants, store, serverKey)
assert.equal(typeof state.handler, 'function')
const textInputs = [
  ['plain.txt', '一段中文 café e\u0301 123.'],
  ['第十二章 正文.md', '\ufeff第一段\r\n第二段'],
  ['headings.md', '# 第一章：开端\n中文 café\n\n## Chapter 2: Next\nHello world 123'],
  ['preamble.txt', '前言标题\n前言内容\n第十章 标题\n正文\n第十章 同号\n重复号正文\nChapter 0: Zero\nlast body'],
  ['title-fallback.txt', 'Chapter 1\nbody\n第十二章\n正文'],
  ['empty-heading.txt', '第一章 空\n第二章 有\nbody'],
  ['counts.txt', 'x\ufeffy\u0085z\u200Bw\n𠀀𰀀 café e\u0301 русский l’été 12 🙂'],
  ['crlf.txt', '第一章 标题\r\n正文\r\nChapter 2: Next\r\nbody\r\n'],
  ['heading-whitespace.md', '## Chapter\u00a02: Next\nbody\n第十章\u2028标题\n正文'],
]
const epubFiles = {
  'META-INF/container.xml': '<container><rootfiles><rootfile full-path="OEBPS/book.opf"/></rootfiles></container>',
  'OEBPS/book.opf': '<package><manifest><item id="b" href="b.xhtml" media-type="application/xhtml+xml"/><item id="a" href="a.xhtml" media-type="text/html"/></manifest><spine><itemref idref="a"/><itemref idref="b"/></spine></package>',
  'OEBPS/a.xhtml': '<html><head><title>Fallback</title></head><body><h1>开始</h1><p>中文 &amp; café</p><script>ignore</script><p>Next<br/>line</p></body></html>',
  'OEBPS/b.xhtml': '<html><body><h2>Chapter 8: Eight</h2><p>body &lt;safe&gt;</p></body></html>',
}
const epub = actual.storedZip(epubFiles)
const parseCases = []
for (const [name, raw] of [...textInputs.map(([name, content]) => [name, Buffer.from(content)]), ['novel.epub', epub]]) {
  const location = '/captured/' + name
  state.selection = [location]; state.sizes.set(location, raw.length); files.set(location, raw); state.snapshot = null
  const result = await state.handler({ sender: { id: 1, once() {} } }, 'reference')
  assert.equal(result.success, true, `${name}: ${JSON.stringify(result)}`)
  assert(state.snapshot)
  const { sources, chapters } = state.snapshot
  const summary = { ...result.inspection }; delete summary.inspectionId
  parseCases.push({ name, raw: raw.toString('base64'), identity: { canonicalLocation: location, fileIdentity: 'stage:synthetic:1' }, sources, chapters, summary })
}
const epubCases = []
for (const [name, archive] of [
  ['reading-order', epub],
  ['missing-container', actual.storedZip({ 'x.txt': 'x' })],
  ['traversal', actual.storedZip({ ...epubFiles, '../bad': 'x' })],
  ['duplicate-normalized', actual.storedZip({ ...epubFiles, 'OEBPS/../OEBPS/a.xhtml': 'duplicate' })],
  ['drm', actual.storedZip({ ...epubFiles, 'META-INF/encryption.xml': '<encryption><EncryptedData><EncryptionMethod Algorithm="drm"/><CipherReference URI="OEBPS/a.xhtml"/></EncryptedData></encryption>' })],
  ['bad-encoding', actual.storedZip({ ...epubFiles, 'OEBPS/a.xhtml': Buffer.from([255]) })],
]) {
  try { epubCases.push({ name, raw: archive.toString('base64'), result: await actual.extractEpubChapters(archive) }) }
  catch (error) { epubCases.push({ name, raw: archive.toString('base64'), error: error.message }) }
}
const identityCases = []
for (const [name, sources, purpose] of [
  ['nfc-trim', [{ canonicalLocation: '  /captured/cafe\u0301.txt\ufeff', fileIdentity: ' stage:1 ' }], 'reference'],
  ['purpose-independent-alias', [{ canonicalLocation: '/captured/café.txt', fileIdentity: 'stage:1' }], 'author-manuscript'],
  ['missing-file-alias', [{ canonicalLocation: '/captured/solo.txt' }], 'reference'],
  ['nul-rejected', [{ canonicalLocation: '/captured/\0.txt' }], 'reference'],
  ['blank-rejected', [{ canonicalLocation: '\ufeff \n' }], 'reference'],
]) {
  try { identityCases.push({ name, sources, purpose, result: actual.ImportSourceIdentityRepository.encodeSources(sources, purpose, serverKey) }) }
  catch (error) { identityCases.push({ name, sources, purpose, error: error.message }) }
}
// Execute ACTUAL identity repository SQL using Node's built-in SQLite. Only
// database admission/transaction shape is adapted; alias resolution is not.
const rawDB = new DatabaseSync(':memory:')
rawDB.exec('CREATE TABLE import_source_aliases(alias_digest TEXT PRIMARY KEY,alias_kind TEXT NOT NULL,source_id TEXT NOT NULL); CREATE TABLE import_legacy_identity_bridge(id TEXT PRIMARY KEY,ciphertext_hex TEXT,iv_hex TEXT,auth_tag_hex TEXT)')
state.db = {
  prepare: sql => rawDB.prepare(sql),
  transaction: fn => (...args) => {
    rawDB.exec('BEGIN')
    try { const result = fn(...args); rawDB.exec('COMMIT'); return result }
    catch (error) { rawDB.exec('ROLLBACK'); throw error }
  },
}
const resolverCases = []
for (const [name, locations, purpose] of [
  ['initial', [['/captured/original.txt', 'stage:file:1']], 'reference'],
  ['rename', [['/captured/renamed.txt', 'stage:file:1']], 'reference'],
  ['replace-in-place', [['/captured/renamed.txt', 'stage:file:2']], 'reference'],
  ['purpose-separated', [['/captured/renamed.txt', 'stage:file:2']], 'author-manuscript'],
  ['new-plus-existing', [['/captured/second.txt', 'stage:file:3'], ['/captured/original.txt', 'stage:file:1']], 'reference'],
  ['order-independent', [['/captured/original.txt', 'stage:file:1'], ['/captured/second.txt', 'stage:file:3']], 'reference'],
]) {
  const sources = locations.map(([canonicalLocation, fileIdentity]) => ({ canonicalLocation, fileIdentity }))
  const encoded = actual.ImportSourceIdentityRepository.encodeSources(sources, purpose, serverKey)
  const result = actual.ImportSourceIdentityRepository.resolveEncodedSources(encoded, purpose, serverKey)
  resolverCases.push({ name, sources, purpose, encoded, result, aliases: rawDB.prepare('SELECT alias_digest,alias_kind,source_id FROM import_source_aliases ORDER BY alias_digest').all() })
}
rawDB.close(); state.db = null
const countCases = ['', '中文 café e\u0301 123 🙂', 'x\ufeffy', 'x\u0085y', 'x\u200By', '𠀀𰀀', 'l’été русский Ελληνικά', '\u0345', 'a\u0301b', 'a’中b', 'a\u2028b'].map(text => ({ text, result: actual.countDraftUnits(text) }))
// Exercise the ACTUAL inspection store semantics separately from stronger native
// generation/HMAC/CAS protections; do not pretend TS has those new features.
let now = 1000
const originalStore = new actual.ImportInspectionStore({ now: () => now, ttlMs: 20, maxActive: 2 })
const specimen = state.snapshot
const first = originalStore.create({ ...specimen, webContentsId: 8 })
assert.throws(() => originalStore.peek(first.inspectionId, 9), /IMPORT_INSPECTION_EXPIRED/u)
assert.throws(() => originalStore.peek(first.inspectionId, 8, 'author-manuscript'), /IMPORT_INSPECTION_EXPIRED/u)
assert.equal(originalStore.consume(first.inspectionId, 8).chapters.length, specimen.chapters.length)
assert.throws(() => originalStore.consume(first.inspectionId, 8), /IMPORT_INSPECTION_EXPIRED/u)
const replacement = originalStore.create({ ...specimen, webContentsId: 8 })
originalStore.create({ ...specimen, webContentsId: 8 })
assert.throws(() => originalStore.peek(replacement.inspectionId, 8), /IMPORT_INSPECTION_EXPIRED/u)
now += 20
assert.equal(originalStore.activeCount(), 0)
const fixture = { format: 1, platform: process.platform, keyHex: serverKey.toString('hex'), originalModules: [...originalModules].sort(), parseCases, epubCases, identityCases, resolverCases, countCases, actualStoreChecks: ['caller', 'purpose', 'one-use', 'replacement', 'expiry'] }
writeFileSync(target, JSON.stringify(fixture, null, 2) + '\n', { flag: 'wx', mode: 0o600 })
assert(readFileSync(target).length > 0)
delete globalThis[key]
