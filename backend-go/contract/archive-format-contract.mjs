// GitHub-only differential oracle: original exported functions, unmodified
// recursive TS imports. Only loader/transpilation and disposable fixtures are
// seams. No checks, schemas, filesystem guards or imports are rewritten.
import assert from 'node:assert/strict'
import { createRequire, registerHooks } from 'node:module'
import { pathToFileURL } from 'node:url'
import { readFileSync, writeFileSync, mkdirSync, existsSync, readdirSync } from 'node:fs'
import { createHash } from 'node:crypto'
import path from 'node:path'
import './register-typescript.mjs'
assert.equal(process.env.GITHUB_ACTIONS, 'true', 'archive oracle is GitHub-only')
const runner = process.env.RUNNER_TEMP
const target = process.env.GO_ARCHIVE_FORMAT_CONTRACT
assert(runner && target && path.isAbsolute(runner) && path.isAbsolute(target))
assert.equal(target, path.join(runner, 'novel-go-archive-format', 'contract.json'))
const root = path.dirname(target)
mkdirSync(root, { recursive: true, mode: 0o700 })
// TS imports JSON without import attributes (the desktop bundler normally
// handles that). Preserve original JSON value with an ordinary ESM loader.
// The runner installs original dependencies outside the checkout. CommonJS
// resolves NODE_PATH, but ESM bare imports do not; bridge resolution only.
const require = createRequire(import.meta.url)
const yauzlURL = pathToFileURL(require.resolve('yauzl')).href
registerHooks({
 resolve(specifier, context, next) {
   if (specifier === 'yauzl') return { url: yauzlURL, shortCircuit: true }
   return next(specifier, context)
 },
 load(url, context, next) {
  if (url.endsWith('/portable-project-field-policy.json')) return {
    format: 'module', source: `export default ${JSON.stringify(JSON.parse(readFileSync(new URL(url), 'utf8')))}`, shortCircuit: true,
  }
  return next(url, context)
} })
const format = await import('../../electron/services/portable-project-format.ts')
const archive = await import('../../electron/services/portable-project-archive.ts')
const freeze = await import('../../electron/services/portable-runtime-freeze.ts')
const authority = await import('../../electron/services/portable-transfer-authority.ts')
const hash = value => createHash('sha256').update(value).digest('hex')
const observe = async operation => { try { return { ok: true, value: await operation() } } catch (error) { return { ok: false, code: error.code ?? error.message } } }
const entry = (name, bytes, disposition = 'author-content') => ({ path: name, byteSize: bytes.length, sha256: hash(bytes), disposition })
const manifest = entries => ({ formatVersion: 1, sourceSchemaVersion: 7, originProjectId: '11111111-1111-4111-8111-111111111111', snapshotGeneration: 'fixture-generation', createdAt: '2026-09-20T00:00:00.000Z', declaredUncompressedBytes: entries.reduce((n, e) => n + e.byteSize, 0), declaredCompressedBytes: entries.reduce((n, e) => n + e.byteSize, 0), entries, semanticCounts: { chapters: entries.length }, omittedItems: [], transferReceiptIds: [], historyProjectionIds: [] })
const payload = Buffer.from('第一章：雨夜。\nCafé 灯火未熄。<&>\u2028\u2029', 'utf8')
const base = manifest([entry('正文/第一章.txt', payload)])
function crc32(bytes) { let c = 0xffffffff; for (const byte of bytes) { c ^= byte; for (let bit = 0; bit < 8; bit++) c = (c >>> 1) ^ ((c & 1) ? 0xedb88320 : 0) } return (c ^ 0xffffffff) >>> 0 }
function rawZip(entries, { zip64 = false, comment = Buffer.alloc(0) } = {}) {
  const locals = [], centrals = []; let offset = 0
  for (const item of entries) {
    const name = Buffer.isBuffer(item.name) ? item.name : Buffer.from(item.name)
    const localName = item.localName ? Buffer.from(item.localName) : name
    const bytes = item.bytes, flags = item.flags ?? 0x800, method = item.method ?? 0
    const size = item.size ?? bytes.length, compressed = item.compressed ?? size, crc = item.crc ?? crc32(bytes)
    const extra = item.extra ?? Buffer.alloc(0)
    const local = Buffer.alloc(30); local.writeUInt32LE(0x04034b50); local.writeUInt16LE(20, 4); local.writeUInt16LE(item.localFlags ?? flags, 6); local.writeUInt16LE(method, 8); local.writeUInt32LE(crc, 14); local.writeUInt32LE(compressed, 18); local.writeUInt32LE(size, 22); local.writeUInt16LE(localName.length, 26)
    locals.push(local, localName, bytes)
    const central = Buffer.alloc(46); central.writeUInt32LE(0x02014b50); central.writeUInt16LE(item.creator ?? ((3 << 8) | 20), 4); central.writeUInt16LE(item.version ?? 20, 6); central.writeUInt16LE(flags, 8); central.writeUInt16LE(method, 10); central.writeUInt32LE(crc, 16); central.writeUInt32LE(compressed, 20); central.writeUInt32LE(size, 24); central.writeUInt16LE(name.length, 28); central.writeUInt16LE(extra.length, 30); central.writeUInt16LE(item.internal ?? 0, 36); central.writeUInt32LE(item.attrs ?? ((0o100600 << 16) >>> 0), 38); central.writeUInt32LE(item.offset ?? offset, 42)
    centrals.push(central, name, extra); offset += local.length + localName.length + bytes.length
  }
  const central = Buffer.concat(centrals), end = Buffer.alloc(22); end.writeUInt32LE(0x06054b50); end.writeUInt16LE(zip64 ? 0xffff : entries.length, 8); end.writeUInt16LE(zip64 ? 0xffff : entries.length, 10); end.writeUInt32LE(zip64 ? 0xffffffff : central.length, 12); end.writeUInt32LE(zip64 ? 0xffffffff : offset, 16); end.writeUInt16LE(comment.length, 20)
  const tails = []
  if (zip64) { const z = Buffer.alloc(56), locator = Buffer.alloc(20); z.writeUInt32LE(0x06064b50); z.writeBigUInt64LE(44n, 4); z.writeUInt16LE(45, 12); z.writeUInt16LE(45, 14); z.writeBigUInt64LE(BigInt(entries.length), 24); z.writeBigUInt64LE(BigInt(entries.length), 32); z.writeBigUInt64LE(BigInt(central.length), 40); z.writeBigUInt64LE(BigInt(offset), 48); locator.writeUInt32LE(0x07064b50); locator.writeBigUInt64LE(BigInt(offset + central.length), 8); locator.writeUInt32LE(1, 16); tails.push(z, locator) }
  return Buffer.concat([...locals, central, ...tails, end, comment])
}
const rawEntries = (m = base, item = {}) => [{ name: 'manifest.json', bytes: Buffer.from(JSON.stringify(m)) }, { name: base.entries[0].path, bytes: payload, ...item }]
const fixtures = { paths: [], manifests: [], archives: [], freezes: [], authorities: [], builds: [], originalPublication: {}, nativeHardening: [] }
for (const value of ['正文/第一章.txt', 'a/Café.txt', 'Ａ/Ｆoo.txt', 'İ.txt', 'ΟΣ.txt', 'a/😀.txt', '../x', '/x', 'C:x', 'a\\b', 'a//b', 'a/./b', 'a/../b', 'a.', 'a ', 'NUL.txt', 'COM1', 'a\u0000b', 'a\u001fb', 'a<z', 'a/．．/b', 'a/／b']) fixtures.paths.push({ value, original: await observe(() => format.portablePathKey(value)) })
const manifestCases = [
 ['valid', base], ['empty', manifest([])], ['unknown-key', { ...base, unknown: true }], ['schema', { ...base, sourceSchemaVersion: 8 }], ['fraction', { ...base, declaredUncompressedBytes: 1.2 }], ['bad-date', { ...base, createdAt: '2026-09-20T00:00:00Z' }], ['ratio', { ...base, declaredCompressedBytes: 0 }], ['uppercase-hash', { ...base, entries: [{ ...base.entries[0], sha256: 'A'.repeat(64) }] }], ['unknown-disposition', { ...base, entries: [{ ...base.entries[0], disposition: 'machine-authority' }] }], ['duplicate-case', manifest([entry('a.txt', payload), entry('A.txt', payload)])], ['file-directory', manifest([entry('a', payload), entry('a/b', payload)])], ['reverse-file-directory', manifest([entry('a/b', payload), entry('A', payload)])], ['nfkc-duplicate', manifest([entry('Ａ.txt', payload), entry('A.txt', payload)])], ['reference-duplicate', { ...base, transferReceiptIds: ['x', 'x'] }], ['bad-count-name', { ...base, semanticCounts: { BAD: 1 } }], ['omission-duplicate', { ...base, omittedItems: [{ id: 'x', reason: 'rebuild-stale' }, { id: 'x', reason: 'rebuild-stale' }] }],
]
for (const [name, value] of manifestCases) fixtures.manifests.push({ name, json: JSON.stringify(value), original: await observe(() => format.parsePortableProjectManifest(value)) })
let caseIndex = 0
async function zipCase(name, bytes, options = {}) {
 const file = `raw-${caseIndex++}.zip`, archivePath = path.join(root, file), stagingParentPath = path.join(root, `stage-${caseIndex}`)
 writeFileSync(archivePath, bytes, { flag: 'wx' }); mkdirSync(stagingParentPath, { mode: 0o700 })
 const original = await observe(async () => { const result = await archive.extractPortableProjectArchive({ archivePath, stagingParentPath, limits: options.limits }); for (const e of result.manifest.entries) assert.equal(hash(readFileSync(path.join(result.stagingPath, e.path))), e.sha256); return { manifest: result.manifest, receipt: result.receipt } })
 if (!original.ok) assert.deepEqual(readdirSync(stagingParentPath), [], 'failed extraction cleans its own staging')
 const fixture = { name, file, limits: options.limits, original }
 if (options.stronger) { fixture.nativeReject = true; fixtures.nativeHardening.push({ name, original, reason: options.stronger }) }
 fixtures.archives.push(fixture)
}
await zipCase('valid-raw', rawZip(rawEntries()))
await zipCase('valid-zip64-end', rawZip(rawEntries(), { zip64: true }))
await zipCase('empty', rawZip([{ name: 'manifest.json', bytes: Buffer.from(JSON.stringify(manifest([]))) }]))
await zipCase('bad-hash', rawZip(rawEntries({ ...base, entries: [{ ...base.entries[0], sha256: '0'.repeat(64) }] })))
await zipCase('bad-crc', rawZip(rawEntries(base, { crc: 123 })))
await zipCase('bad-size', rawZip(rawEntries(base, { size: payload.length + 1 })))
await zipCase('zipbomb-declared-size', rawZip(rawEntries(base, { size: 0x80000001 })))
await zipCase('ratio-declared-stored-mismatch', rawZip(rawEntries(base, { size: 1000000, compressed: 1 })))
await zipCase('manifest-invalid-utf8', rawZip([{ name: 'manifest.json', bytes: Buffer.from([0xff, 0xfe]) }]))
await zipCase('manifest-byte-order-mark', rawZip([{ name: 'manifest.json', bytes: Buffer.concat([Buffer.from([0xef, 0xbb, 0xbf]), Buffer.from(JSON.stringify(base))]) }, rawEntries()[1]]))
await zipCase('extra-payload', rawZip([...rawEntries(), { name: 'extra.txt', bytes: Buffer.from('x') }]))
await zipCase('missing-payload', rawZip(rawEntries().slice(0, 1)))
await zipCase('duplicate-payload', rawZip([...rawEntries(), rawEntries()[1]]))
await zipCase('manifest-not-first', rawZip(rawEntries().reverse()))
await zipCase('duplicate-manifest', rawZip([rawEntries()[0], rawEntries()[0]]))
for (const name of ['../x', '/x', 'a\\b', 'C:x', 'CON.txt', 'a/../b', 'a//b', 'a.', 'a ', 'a\u0000b', 'a\u001fb']) await zipCase(`path-${caseIndex}`, rawZip(rawEntries(base, { name })))
await zipCase('non-utf8', rawZip(rawEntries(base, { name: Buffer.from([0xc0, 0xaf]) })))
await zipCase('missing-utf8-flag', rawZip(rawEntries(base, { flags: 0 })))
await zipCase('encrypted', rawZip(rawEntries(base, { flags: 0x801 })))
await zipCase('deflate-not-portable', rawZip(rawEntries(base, { method: 8 })))
await zipCase('symlink', rawZip(rawEntries(base, { attrs: ((0o120777 << 16) >>> 0) })))
await zipCase('directory', rawZip(rawEntries(base, { attrs: ((0o40700 << 16) >>> 0) | 0x10 })))
await zipCase('reparse', rawZip(rawEntries(base, { attrs: ((0o100600 << 16) >>> 0) | 0x400 })))
await zipCase('internal-attributes', rawZip(rawEntries(base, { internal: 1 })))
await zipCase('unsupported-extract-version', rawZip(rawEntries(base, { version: 46 })))
await zipCase('unknown-extra', rawZip(rawEntries(base, { extra: Buffer.from([2, 0, 0, 0]) })))
await zipCase('archive-comment', rawZip(rawEntries(), { comment: Buffer.from('x') }))
await zipCase('max-entry', rawZip(rawEntries()), { limits: { maxEntryBytes: 1 } })
await zipCase('max-total', rawZip(rawEntries()), { limits: { maxTotalBytes: 1 } })
await zipCase('max-compressed', rawZip(rawEntries()), { limits: { maxDeclaredCompressedBytes: 1 } })
await zipCase('max-path', rawZip(rawEntries()), { limits: { maxPathBytes: 13 } })
await zipCase('local-name-mismatch', rawZip(rawEntries(base, { localName: 'evil/ghost.txt' })), { stronger: 'native requires local and central names to match; original yauzl reads data by local name length' })
await zipCase('local-flags-mismatch', rawZip(rawEntries(base, { localFlags: 0x801 })), { stronger: 'native binds local flags to validated central flags' })
const duplicateJSON = JSON.stringify(base).replace('"formatVersion":1', '"formatVersion":9,"formatVersion":1')
await zipCase('duplicate-json-keys', rawZip([{ name: 'manifest.json', bytes: Buffer.from(duplicateJSON) }, rawEntries()[1]]), { stronger: 'native rejects duplicate JSON keys instead of JS last-key-wins' })
const buildCases = [['small', [{ path: base.entries[0].path, bytes: payload }]], ['stream-large', [{ path: 'assets/large.bin', bytes: Buffer.alloc(3 * 1024 * 1024 + 37, 0x5a) }, { path: '正文/a.txt', bytes: payload }]]]
for (const [name, payloads] of buildCases) {
 const definitions = payloads.map(p => entry(p.path, p.bytes)), m = manifest(definitions)
 const sources = payloads.map((p, i) => { const file = `${name}-source-${i}.bin`; writeFileSync(path.join(root, file), p.bytes, { flag: 'wx' }); return { entry: definitions[i], file } })
 const file = `${name}-built.zip`; let unpublished = false, completeBeforePublish = false
 const receipt = await archive.writePortableProjectArchive({ manifest: m, sources: sources.map(s => ({ entry: s.entry, sourcePath: path.join(root, s.file) })), targetPath: path.join(root, file), __testHooks: { beforePublish(temporary, output) { unpublished = !existsSync(output); completeBeforePublish = readFileSync(temporary).subarray(-22).readUInt32LE(0) === 0x06054b50 } } })
 assert(unpublished && completeBeforePublish); fixtures.builds.push({ name, manifest: m, sources, file, receipt }); await zipCase(`original-build-${name}`, readFileSync(path.join(root, file)))
}
// Original publication behavior is observed separately from the native stage
// API: codec scope is explicitly NOT complete export/import author lifecycle.
const badSource = path.join(root, 'publication-source.bin'); writeFileSync(badSource, payload, { flag: 'wx' })
const badEntry = { ...base.entries[0], sha256: '0'.repeat(64) }, badManifest = manifest([badEntry]), badOutput = path.join(root, 'incomplete-must-not-exist.zip')
fixtures.originalPublication.sourceChanged = await observe(() => archive.writePortableProjectArchive({ manifest: badManifest, sources: [{ entry: badEntry, sourcePath: badSource }], targetPath: badOutput }))
assert(!existsSync(badOutput)); assert.equal(fixtures.originalPublication.sourceChanged.code, 'PORTABLE_ARCHIVE_SOURCE_CHANGED')
const existing = path.join(root, 'existing.zip'); writeFileSync(existing, 'original', { flag: 'wx' })
fixtures.originalPublication.noOverwrite = await observe(() => archive.writePortableProjectArchive({ manifest: base, sources: [{ entry: base.entries[0], sourcePath: badSource }], targetPath: existing }))
assert.equal(readFileSync(existing, 'utf8'), 'original'); assert.equal(fixtures.originalPublication.noOverwrite.code, 'PORTABLE_ARCHIVE_TARGET_EXISTS')
const projection = { projectionId: 'p1', table: 'drafts', recordId: '1', terminalState: 'pending', projection: {}, projectionHash: '0'.repeat(64), excludedFields: ['machinePath'], nonReplayable: true, originalReceiptVerified: false }
const frozen = { version: 1, originProjectId: base.originProjectId, snapshotGeneration: base.snapshotGeneration, nonReplayable: true, requiresRuntimeFreezeGuard: true, records: [projection], avatarReferenceProjections: [] }
for (const [name, value] of [['valid', frozen], ['duplicate', { ...frozen, records: [projection, projection] }], ['replayable', { ...frozen, nonReplayable: false }], ['claims-original', { ...frozen, records: [{ ...projection, originalReceiptVerified: true }] }], ['invalid-projection', { ...frozen, records: [{ ...projection, projection: [] }] }]]) fixtures.freezes.push({ name, json: JSON.stringify(value), original: await observe(() => { const g = freeze.parsePortableRuntimeFreeze(Buffer.from(JSON.stringify(value))); return { active: g.active, frozen: g.isFrozen('drafts', '1'), mutable: (() => { try { g.assertMutable('drafts', '1'); return true } catch { return false } })() } }) })
const facts = { originProjectId: base.originProjectId, snapshotGeneration: base.snapshotGeneration, portableDatabaseSha256: 'a'.repeat(64), finalizations: [{ finalizationId: 'f1', draftId: 1, chapterNumber: 1, contentHash: 'b'.repeat(64) }], summarySources: [{ summaryId: 1, draftId: 1, chapterNumber: 1, sourceFinalizationId: 'f1', sourceContentHash: 'b'.repeat(64), projectionGeneration: 0 }] }
const transferred = { version: 1, receiptId: `portable-transfer:${hash(JSON.stringify(facts)).slice(0, 32)}`, originProjectId: facts.originProjectId, targetProjectId: null, snapshotGeneration: facts.snapshotGeneration, sourceSchemaVersion: 7, portableDatabaseSha256: facts.portableDatabaseSha256, requiresRuntimeFreezeGuard: true, finalizations: facts.finalizations, summarySources: facts.summarySources }
for (const [name, value] of [['valid', transferred], ['bad-receipt', { ...transferred, receiptId: 'portable-transfer:wrong' }], ['missing-freeze-guard', { ...transferred, requiresRuntimeFreezeGuard: false }], ['bad-binding', { ...transferred, summarySources: [{ ...facts.summarySources[0], sourceContentHash: 'c'.repeat(64) }] }], ['duplicate-draft', { ...transferred, finalizations: [facts.finalizations[0], { ...facts.finalizations[0], finalizationId: 'f2', chapterNumber: 2 }] }]]) fixtures.authorities.push({ name, json: JSON.stringify(value), original: await observe(() => authority.parsePortableTransferAuthority(value)) })
assert(!existsSync(target)); writeFileSync(target, JSON.stringify(fixtures, null, 2) + '\n', { flag: 'wx', mode: 0o600 })
console.log(`archive format original-export oracle: ${fixtures.paths.length} paths, ${fixtures.manifests.length} manifests, ${fixtures.archives.length} raw ZIP cases; ${fixtures.nativeHardening.length} separately labeled native-strengthening observations`)
