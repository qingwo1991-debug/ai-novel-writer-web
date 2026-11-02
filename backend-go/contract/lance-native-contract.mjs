#!/usr/bin/env node
/**
 * GitHub-only synthetic oracle for the native Rust -> Go/CGO Lance bridge.
 * No application modules, author databases, TS shims, or local dependency fallback.
 *
 * npm install --prefix "$RUNNER_TEMP/oracle" --no-save --package-lock=false \
 *   @lancedb/lancedb@0.22.3 apache-arrow@18.1.0
 * GO_LANCE_CONTRACTROOT="$RUNNER_TEMP/novel-lance-interop" node <this-file> generate
 * [native export copy/lancedb -> native-ipc; save native-export.json]
 * [native import restored/lancedb <- oracle-ipc + contract.json; save native-import.json]
 * [native export restored/lancedb -> native-restored-ipc; save native-restored-export.json]
 * GO_LANCE_CONTRACTROOT="$RUNNER_TEMP/novel-lance-interop" node <this-file> verify
 * Native manifest shape: { tables: [{ name, version, rows: number, file }] }.
 * All paths above are relative to GO_LANCE_CONTRACTROOT. Output is tiny synthetic data.
 */
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { createHash } from 'node:crypto'
import { createRequire } from 'node:module'

const fail = message => { throw new Error(`LANCE_NATIVE_CONTRACT: ${message}`) }
const byteOrder = (a, b) => Buffer.compare(Buffer.from(a, 'utf8'), Buffer.from(b, 'utf8'))
const digest = bytes => createHash('sha256').update(bytes).digest('hex')
const present = file => {
  try { fs.lstatSync(file); return true } catch (error) {
    if (error.code === 'ENOENT') return false
    throw error
  }
}
const inside = (base, candidate) => {
  const relative = path.relative(base, candidate)
  return relative !== '' && relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative)
}
const regular = file => {
  const stat = fs.lstatSync(file)
  assert(stat.isFile() && !stat.isSymbolicLink() && stat.nlink === 1, `unsafe/nonregular/hardlinked file: ${file}`)
  return stat
}
const directory = dir => {
  const stat = fs.lstatSync(dir)
  assert(stat.isDirectory() && !stat.isSymbolicLink(), `unsafe directory: ${dir}`)
  assert.equal(fs.realpathSync.native(dir), path.resolve(dir), `noncanonical directory: ${dir}`)
}

// Refuse before loading any native module or touching any fixture.
assert.equal(process.env.GITHUB_ACTIONS, 'true', 'real oracle is restricted to GitHub Actions')
const mode = process.argv[2]
assert(['generate', 'verify'].includes(mode) && process.argv.length === 3, 'usage: lance-native-contract.mjs generate|verify')
assert(process.env.RUNNER_TEMP && path.isAbsolute(process.env.RUNNER_TEMP), 'absolute RUNNER_TEMP required')
const runnerTemp = path.resolve(process.env.RUNNER_TEMP)
directory(runnerTemp)
const root = path.resolve(runnerTemp, 'novel-lance-interop')
assert.equal(process.env.GO_LANCE_CONTRACTROOT, root, 'GO_LANCE_CONTRACTROOT must equal $RUNNER_TEMP/novel-lance-interop')
const oraclePackage = path.resolve(runnerTemp, 'oracle', 'package.json')
const requireOracle = createRequire(oraclePackage)
function pinnedPackage(name, version, requireFrom = requireOracle) {
  const entry = fs.realpathSync.native(requireFrom.resolve(name))
  assert(inside(runnerTemp, entry), `${name} must be installed in RUNNER_TEMP, not workspace/global`)
  let dir = path.dirname(entry)
  while (inside(runnerTemp, dir)) {
    const file = path.join(dir, 'package.json')
    if (present(file)) {
      regular(file)
      const pkg = JSON.parse(fs.readFileSync(file, 'utf8'))
      if (pkg.name === name) {
        assert.equal(pkg.version, version, `${name} must be exactly ${version}`)
        return { entry, package: file, version }
      }
    }
    dir = path.dirname(dir)
  }
  return fail(`cannot identify installed package ${name}`)
}
const lancePackage = pinnedPackage('@lancedb/lancedb', '0.22.3')
const arrowPackage = pinnedPackage('apache-arrow', '18.1.0')
const lanceRequire = createRequire(lancePackage.entry)
const lanceArrow = pinnedPackage('apache-arrow', '18.1.0', lanceRequire)
assert.equal(lanceArrow.entry, arrowPackage.entry, 'LanceDB and oracle must share the actual CommonJS Arrow runtime')
const lance = requireOracle('@lancedb/lancedb')
const arrow = requireOracle('apache-arrow')
const { Field, Schema, FixedSizeList, Float32, Float64, Int32, Utf8, tableFromIPC, tableToIPC } = arrow
const tableNames = ['chunks', 'chunks__space_1', 'chunks__space_2', 'chunks__space_3', 'chunks__space_4', 'documents']
const at = (...parts) => path.join(root, ...parts)
const writeJSON = (file, value) => fs.writeFileSync(file, `${JSON.stringify(value, null, 2)}\n`, { flag: 'wx', mode: 0o600 })
const readJSON = file => { regular(file); return JSON.parse(fs.readFileSync(file, 'utf8')) }
const privateDirectory = dir => {
  fs.mkdirSync(dir, { recursive: true, mode: 0o700 })
  directory(dir)
  fs.chmodSync(dir, 0o700)
  assert.equal(fs.lstatSync(dir).mode & 0o777, 0o700, `directory must be private: ${dir}`)
}

// Keep field order, nullability, and FixedSizeList child exactly as the original
// electron/vector-store.ts canonicalChunkSchema()/embeddingChunkSchema().
function chunkSchema(dimension) {
  const fields = [
    new Field('id', new Utf8()),
    new Field('docId', new Utf8()),
    new Field('fileName', new Utf8()),
    new Field('chapterNumber', new Int32(), true),
    new Field('chapterTitle', new Utf8(), true),
    new Field('text', new Utf8()),
  ]
  if (dimension !== undefined) fields.push(new Field('vector', new FixedSizeList(dimension, new Field('item', new Float32())), false))
  fields.push(
    new Field('chunkIndex', new Int32()),
    new Field('totalChunks', new Int32()),
    new Field('importedAt', new Utf8()),
    new Field('corpusKind', new Utf8()),
  )
  return new Schema(fields)
}
// writeDocumentInfo originally infers all fields nullable, numbers Float64.
const documentSchema = () => new Schema([
  new Field('id', new Utf8(), true),
  new Field('fileName', new Utf8(), true),
  new Field('importedAt', new Utf8(), true),
  new Field('chunkCount', new Float64(), true),
  new Field('filePath', new Utf8(), true),
  new Field('corpusKind', new Utf8(), true),
])
function fieldShape(field) {
  assert.equal(field.metadata.size, 0, `unexpected metadata on ${field.name}`)
  return { name: field.name, nullable: field.nullable, type: field.type.toString(), children: (field.type.children ?? []).map(fieldShape) }
}
function schemaShape(schema) {
  assert.equal(schema.metadata.size, 0, 'unexpected schema metadata')
  return schema.fields.map(fieldShape)
}
function plain(value) {
  if (value === null || typeof value === 'string' || typeof value === 'boolean') return value
  if (typeof value === 'number') {
    assert(Number.isFinite(value), 'nonfinite fixture number')
    assert(!Object.is(value, -0), 'negative zero is deliberately outside this JSON contract')
    return value
  }
  // Fail rather than silently truncate an Int64/BigInt. Contract integers are Int32.
  assert.notEqual(typeof value, 'bigint', 'BigInt is unsupported; no lossy conversion permitted')
  if (Array.isArray(value)) return value.map(plain)
  if (ArrayBuffer.isView(value) && !(value instanceof DataView)) return Array.from(value, plain)
  if (value && typeof value.toArray === 'function') return plain(value.toArray())
  if (value && typeof value[Symbol.iterator] === 'function') return Array.from(value, plain)
  return fail(`unsupported Arrow value ${typeof value}`)
}
function rowsOf(table) {
  const rows = table.toArray().map(row => Object.fromEntries(table.schema.fields.map(field => [field.name, plain(row[field.name])])) )
  assert.equal(rows.length, table.numRows, 'Arrow row count mismatch')
  assert(rows.every(row => typeof row.id === 'string' && row.id.length > 0), 'invalid fixture id')
  assert.equal(new Set(rows.map(row => row.id)).size, rows.length, 'duplicate fixture id')
  return rows.sort((a, b) => byteOrder(a.id, b.id))
}
const expectedSchema = name => {
  const shape = schemaShape(name === 'documents' ? documentSchema() : chunkSchema(name === 'chunks' ? undefined : name === 'chunks__space_2' ? 3 : 2))
  // Real 0.22.3 createTable/createEmptyTable canonicalize vector child to nullable,
  // despite the application's nonnullable supplied item Field. Preserve and test
  // the actual persisted schema, not an invented schema from constructor input.
  if (name.startsWith('chunks__space_')) shape.find(field => field.name === 'vector').children[0].nullable = true
  return shape
}
function decodedIPC(file) {
  regular(file)
  const bytes = fs.readFileSync(file)
  // Rust uses StreamReader/StreamWriter, never Arrow file IPC.
  assert(bytes.length >= 8 && bytes.readUInt32LE(0) === 0xffffffff && bytes.subarray(0, 6).toString() !== 'ARROW1', `stream IPC required: ${file}`)
  return tableFromIPC(bytes)
}
function compareArrow(name, actual, expected, label) {
  assert.deepEqual(schemaShape(actual.schema), expected.schema, `${label}/${name}: exact fields, order, nullability, types, children`)
  assert.deepEqual(rowsOf(actual), expected.rows, `${label}/${name}: full rows by UTF-8 byte id order`)
}
async function readPinnedArrow(table, name) {
  const persistedSchema = await table.schema()
  assert.deepEqual(schemaShape(persistedSchema), expectedSchema(name), `${name}: persisted Lance schema`)
  const result = await table.query().toArrow()
  // 0.22.3 toArrow() builds Table(batches), which loses schema when the native
  // query emits no batches. Recover ONLY an actually empty table's persisted
  // schema using real Arrow; never hide a nonempty/malformed query result.
  if (result.numRows === 0 && result.schema.fields.length === 0) {
    assert.equal(await table.countRows(), 0, `${name}: schema-less result must really be empty`)
    return new arrow.Table(persistedSchema)
  }
  assert.deepEqual(schemaShape(result.schema), schemaShape(persistedSchema), `${name}: query/persisted schema equality`)
  return result
}

// Full membership + byte hashes. No timestamps/atime; never follow links or
// accept hardlinked/nonregular files. Includes the registry and knowledge copies
// OUTSIDE lancedb, so a native bridge cannot silently alter neighboring assets.
function physicalManifest(storageRoot) {
  directory(storageRoot)
  const entries = []
  const walk = relative => {
    const absolute = path.join(storageRoot, relative)
    const stat = fs.lstatSync(absolute)
    assert(!stat.isSymbolicLink(), `symlink in fixture: ${absolute}`)
    const portable = relative.split(path.sep).join('/') || '.'
    if (stat.isDirectory()) {
      entries.push({ path: portable, kind: 'directory' })
      for (const name of fs.readdirSync(absolute).sort(byteOrder)) walk(path.join(relative, name))
    } else {
      regular(absolute)
      const bytes = fs.readFileSync(absolute)
      entries.push({ path: portable, kind: 'file', size: bytes.length, sha256: digest(bytes) })
    }
  }
  walk('')
  return entries.sort((a, b) => byteOrder(a.path, b.path))
}
function assertUnchanged(before) {
  assert.deepEqual(physicalManifest(at('source')), before.source, 'original source membership/content changed')
  assert.deepEqual(physicalManifest(at('copy')), before.copy, 'physical copy membership/content changed')
  assert.deepEqual(before.source, before.copy, 'initial source/copy mismatch')
}

function fixtures() {
  const importedAt = '2025-01-02T03:04:05.000Z'
  const documents = [
    { id: "文档'🧭\n甲", fileName: "星海'🪐\n.txt", importedAt, chunkCount: 2, filePath: "knowledge-copy:文档'🧭\n甲", corpusKind: 'reference' },
    { id: 'doc-é-乙', fileName: '设定-é.md', importedAt, chunkCount: 1, filePath: 'knowledge-copy:doc-é-乙', corpusKind: 'project-knowledge' },
    { id: 'doc-unknown-丙', fileName: '未分类📜.txt', importedAt, chunkCount: 1, filePath: 'knowledge-copy:doc-unknown-丙', corpusKind: 'unknown' },
  ]
  const make = (id, document, chunkIndex, chapterNumber, chapterTitle, text) => ({
    id, docId: document.id, fileName: document.fileName, chapterNumber, chapterTitle, text,
    chunkIndex, totalChunks: document.chunkCount, importedAt, corpusKind: document.corpusKind,
  })
  const chunks = [
    make('🧭-chunk-甲', documents[0], 0, null, null, "第一行\n第二行 O'Brien — 雪与星🌌"),
    make("a'chunk\n乙", documents[0], 1, 7, "第七章 '归途' 🚀", '他说：“别怕。”\n\t风声仍在。'),
    make('é-chunk-丙', documents[1], 0, null, '仅标题📚', 'café / e\u0301 / Ελληνικά / 日本語'),
    make('中-chunk-丁', documents[2], 0, 2147483647, null, "未知语料：'apostrophe'\n终章🙂"),
  ]
  const vectors2 = [[Math.fround(0.1), -2.5], [3.125, Math.fround(-0.2)], [0, 65504], [-0.125, 1.5]]
  const vectors3 = [[-1.25, Math.fround(0.3), 2], [0.5, -8, Math.fround(1 / 3)]]
  const vectorRow = (row, vector) => ({ ...row, vector })
  const byName = {
    documents,
    chunks,
    chunks__space_1: chunks.map((row, i) => vectorRow(row, vectors2[i])),
    chunks__space_2: chunks.slice(0, 2).map((row, i) => vectorRow(row, vectors3[i])),
    chunks__space_3: [vectorRow(chunks[2], [-16, 0.0625])],
    chunks__space_4: [],
  }
  const registry = {
    version: 1, activeGeneration: 1,
    spaces: [
      { generation: 1, tableName: 'chunks__space_1', modelFingerprint: 'synthetic:active-🧭', vectorDimension: 2, distanceMetric: 'l2', status: 'active', createdAt: importedAt },
      { generation: 2, tableName: 'chunks__space_2', modelFingerprint: 'synthetic:inactive', vectorDimension: 3, distanceMetric: 'cosine', status: 'inactive', createdAt: importedAt },
      { generation: 3, tableName: 'chunks__space_3', modelFingerprint: 'synthetic:building', vectorDimension: 2, distanceMetric: 'dot', status: 'building', createdAt: importedAt },
      { generation: 4, tableName: 'chunks__space_4', modelFingerprint: 'synthetic:empty-schema', vectorDimension: 2, distanceMetric: 'l2', status: 'building', createdAt: importedAt },
    ],
  }
  return { documents, byName, registry }
}

async function generate() {
  assert(!present(root), 'generate refuses any existing contract root (including an empty directory)')
  privateDirectory(root)
  for (const dir of ['source/lancedb', 'source/knowledge-copies', 'oracle-ipc', 'native-ipc', 'native-restored-ipc', 'restored/lancedb']) privateDirectory(at(dir))
  const seed = fixtures()
  // Preserve noncanonical whitespace/newline bytes, not parsed/reserialized JSON.
  fs.writeFileSync(at('source', 'embedding-spaces.json'), `\n${JSON.stringify(seed.registry, null, 3)}\n\n`, { flag: 'wx', mode: 0o600 })
  for (const doc of seed.documents) {
    const content = `Synthetic ONLY: ${doc.id}\n${doc.fileName}\n'Omega' Ω🙂\n`
    writeJSON(at('source', 'knowledge-copies', `${digest(doc.id)}.json`), { content, indexedHash: digest(content), edited: false, indexDirty: false })
  }
  let history
  const db = await lance.connect(at('source', 'lancedb'))
  try {
    for (const name of tableNames) {
      assert(/^(documents|chunks|chunks__space_[0-9]+)$/.test(name), 'unexpected synthetic table name')
      const dimension = name === 'chunks__space_2' ? 3 : 2
      const data = seed.byName[name]
      const schema = name === 'documents' ? undefined : chunkSchema(name === 'chunks' ? undefined : dimension)
      const initial = name === 'chunks__space_1' ? data.slice(0, -1) : data
      const table = name === 'chunks__space_4'
        ? await db.createEmptyTable(name, schema)
        : await db.createTable(name, initial, schema ? { schema } : {})
      try {
        assert.deepEqual(schemaShape(await table.schema()), expectedSchema(name), `${name}: original application schema`)
        if (name === 'chunks__space_1') {
          const previousVersion = await table.version()
          await table.add(data.slice(-1))
          const latestVersion = await table.version()
          assert(Number.isSafeInteger(previousVersion) && latestVersion > previousVersion, 'history must contain a second committed version')
          history = { name, previousVersion, latestVersion, previousRows: initial.length, latestRows: data.length }
        }
      } finally { table.close() }
    }
  } finally { db.close() }

  const tables = []
  const expected = {}
  const readDB = await lance.connect(at('source', 'lancedb'))
  try {
    assert.deepEqual((await readDB.tableNames()).sort(byteOrder), tableNames, 'exact source table membership')
    for (const name of tableNames) {
      const table = await readDB.openTable(name)
      try {
        const version = await table.version()
        assert(Number.isSafeInteger(version) && version > 0, 'safe positive table version required')
        await table.checkout(version)
        const logical = await readPinnedArrow(table, name)
        const file = `${name}.arrows`
        fs.writeFileSync(at('oracle-ipc', file), tableToIPC(logical, 'stream'), { flag: 'wx', mode: 0o600 })
        const decoded = decodedIPC(at('oracle-ipc', file))
        const schema = expectedSchema(name)
        const records = seed.byName[name].map(row => Object.fromEntries(schema.map(field => [field.name, row[field.name]]))).sort((a, b) => byteOrder(a.id, b.id))
        expected[name] = { schema, rows: records }
        compareArrow(name, logical, expected[name], 'source')
        compareArrow(name, decoded, expected[name], 'original-stream-IPC')
        tables.push({ name, version, rows: logical.numRows, file })
      } finally { table.close() }
    }
  } finally { readDB.close() }
  assert.equal(tables.find(table => table.name === history.name).version, history.latestVersion, 'oracle must pin the latest historical table version')
  const source = physicalManifest(at('source'))
  fs.cpSync(at('source'), at('copy'), { recursive: true, force: false, errorOnExist: true, dereference: false })
  privateDirectory(at('copy'))
  privateDirectory(at('copy', 'lancedb'))
  const copy = physicalManifest(at('copy'))
  assert.deepEqual(copy, source, 'physical copy must have exact original file bytes and membership')
  writeJSON(at('before-copy-manifest.json'), { format: 1, source, copy })
  writeJSON(at('contract.json'), {
    format: 1, syntheticOnly: true,
    dependencies: { '@lancedb/lancedb': lancePackage.version, 'apache-arrow': arrowPackage.version },
    tables, expected, history,
    paths: { source: 'source', copy: 'copy', oracleIPC: 'oracle-ipc', nativeIPC: 'native-ipc', restored: 'restored', nativeRestoredIPC: 'native-restored-ipc' },
    registrySha256: digest(fs.readFileSync(at('source', 'embedding-spaces.json'))),
    rowComparison: 'full rows sorted by UTF-8 byte id; not the TS migration logicalHash',
  })
  console.log(JSON.stringify({ mode: 'generate', tables: tables.length, rows: tables.reduce((sum, table) => sum + table.rows, 0), historicalTable: history.name, emptyTable: 'chunks__space_4' }))
}

function checkedManifest(file, contract, versions) {
  const manifest = readJSON(file)
  assert(Array.isArray(manifest.tables), `missing tables: ${file}`)
  assert.equal(manifest.tables.length, contract.tables.length, `exact native table count: ${file}`)
  const names = manifest.tables.map(table => table.name)
  assert.equal(new Set(names).size, names.length, `duplicate native table: ${file}`)
  assert.deepEqual([...names].sort(byteOrder), tableNames, `native table membership: ${file}`)
  for (const item of manifest.tables) {
    const expected = contract.tables.find(table => table.name === item.name)
    assert.equal(item.file, `${item.name}.arrows`, `unsafe/unexpected IPC filename: ${file}`)
    assert.equal(item.rows, expected.rows, `native row count: ${file}/${item.name}`)
    assert(Number.isSafeInteger(item.version) && item.version > 0, `native table version: ${file}/${item.name}`)
    if (versions) assert.equal(item.version, versions[item.name], `native pinned version: ${file}/${item.name}`)
  }
  return manifest
}
function verifyIPC(dir, manifest, contract) {
  directory(dir)
  const native=path.basename(dir)!=='oracle-ipc'
  assert.deepEqual(fs.readdirSync(dir).sort(byteOrder), [...manifest.tables.map(table => table.file),...(native?['domain.json']:[])].sort(byteOrder), `IPC membership: ${dir}`)
  for (const table of manifest.tables) compareArrow(table.name, decodedIPC(path.join(dir, table.file)), contract.expected[table.name], path.basename(dir))
  if(native){const domain=readJSON(path.join(dir,'domain.json'));assert.deepEqual(domain.tables.map(t=>t.name).sort(byteOrder),tableNames,'native domain table membership');for(const t of domain.tables){assert.equal(t.version,manifest.tables.find(x=>x.name===t.name).version);assert.equal(t.metadata,false);const shape=f=>({name:f.name,nullable:f.nullable,type:f.type==='fixed_size_list'?`FixedSizeList[${f.listSize}]<${f.children[0].type==='float32'?'Float32':'unsupported'}>`:({utf8:'Utf8',int32:'Int32',float64:'Float64',float32:'Float32'}[f.type]),children:f.children.map(shape)});for(const f of t.schema){assert.equal(f.metadata,false);for(const child of f.children)assert.equal(child.metadata,false)};assert.deepEqual(t.schema.map(shape),contract.expected[t.name].schema,'native typed domain schema');assert.deepEqual([...t.records].sort((a,b)=>byteOrder(a.id,b.id)),contract.expected[t.name].rows,'native typed domain full records')}}
}
async function verify() {
  directory(root)
  const contract = readJSON(at('contract.json'))
  assert.equal(contract.format, 1)
  assert.equal(contract.syntheticOnly, true)
  assert.deepEqual(contract.dependencies, { '@lancedb/lancedb': '0.22.3', 'apache-arrow': '18.1.0' })
  assert.deepEqual(contract.tables.map(table => table.name), tableNames)
  const seed = fixtures()
  for (const table of contract.tables) {
    assert.deepEqual(contract.expected[table.name].schema, expectedSchema(table.name), 'original schemas must remain the authority')
    const expectedRows = seed.byName[table.name].map(row => Object.fromEntries(expectedSchema(table.name).map(field => [field.name, row[field.name]]))).sort((a, b) => byteOrder(a.id, b.id))
    assert.deepEqual(contract.expected[table.name].rows, expectedRows, 'fixture rows must remain the authority')
    assert.equal(table.rows, expectedRows.length)
    assert.equal(table.file, `${table.name}.arrows`)
  }
  const before = readJSON(at('before-copy-manifest.json'))
  assert.equal(before.format, 1)
  assertUnchanged(before)
  const originalVersions = Object.fromEntries(contract.tables.map(table => [table.name, table.version]))
  const nativeExport = checkedManifest(at('native-export.json'), contract, originalVersions)
  const nativeImport = checkedManifest(at('native-import.json'), contract)
  const restoredExport = checkedManifest(at('native-restored-export.json'), contract)
  verifyIPC(at('oracle-ipc'), contract, contract)
  verifyIPC(at('native-ipc'), nativeExport, contract)
  verifyIPC(at('native-restored-ipc'), restoredExport, contract)
  const stagedExport = checkedManifest(at('native-staged-export.json'), contract, originalVersions)
  verifyIPC(at('native-staged-ipc'), stagedExport, contract)
  const domainSummary=readJSON(at('native-domain-summary.json'))
  assert.equal(domainSummary.tableCount,6);assert.equal(domainSummary.documents,3);assert.equal(domainSummary.chunks,4);assert.equal(domainSummary.spaces,4);assert.equal(domainSummary.embeddingRows,7);assert.equal(domainSummary.activeGeneration,1)
  const staging = readJSON(at('native-staging-proof.json'))
  assert(staging.physical.digest && staging.physical.bytes > 0 && staging.physical.entries > 0, 'bounded Go physical-copy evidence required')
  assert.equal(staging.metadata.Copies, 3, 'all raw copied knowledge files validated')
  assert.equal(staging.metadata.Current, 3, 'current copies use original indexedHash semantics')
  assert.equal(staging.metadata.RegistrySHA256, contract.registrySha256, 'registry raw bytes preserved by Go staging')
  assert.deepEqual(fs.readdirSync(at('go-staged')), [], 'Go-owned staged author asset copies removed after native scan')
  const stagedSource = physicalManifest(at('stage-library', 'synthetic', '.ai-novel')).filter(entry => entry.path !== 'project.json' && entry.path !== 'project.db')
  assert.deepEqual(stagedSource, before.copy, 'Go synthetic staging source matches original copied assets byte-for-byte')

  // Reopen only attempt-owned restored data, never the source or physical copy.
  // Check directory existence BEFORE connect: Lance connect may create missing DBs.
  directory(at('restored'))
  directory(at('restored', 'lancedb'))
  assert.deepEqual(fs.readdirSync(at('restored', 'lancedb')).sort(byteOrder), tableNames.map(name => `${name}.lance`).sort(byteOrder), 'restored Lance table directory membership')
  const restoredVersions = {}
  const restored = await lance.connect(at('restored', 'lancedb'))
  try {
    assert.deepEqual((await restored.tableNames()).sort(byteOrder), tableNames, 'Node must reopen all native-imported tables, including empty schema-bearing table')
    for (const name of tableNames) {
      const table = await restored.openTable(name)
      try {
        const version = await table.version()
        restoredVersions[name] = version
        await table.checkout(version)
        compareArrow(name, await readPinnedArrow(table, name), contract.expected[name], 'Node-reopened-native-restored')
      } finally { table.close() }
    }
  } finally { restored.close() }
  for (const manifest of [nativeImport, restoredExport]) {
    for (const table of manifest.tables) assert.equal(table.version, restoredVersions[table.name], `restored native/Node version equality: ${table.name}`)
  }
  assert.equal(originalVersions[contract.history.name], contract.history.latestVersion)
  assert(contract.history.latestVersion > contract.history.previousVersion && contract.history.latestRows > contract.history.previousRows, 'nontrivial latest-version history fixture required')
  // Native Lance import/export does not own registry/knowledge-copy restoration.
  // Their unchanged raw bytes are proved on BOTH source and copied storage roots.
  assert.equal(digest(fs.readFileSync(at('copy', 'embedding-spaces.json'))), contract.registrySha256, 'registry raw-byte preservation')
  assertUnchanged(before)
  const summary = {
    format: 1, syntheticOnly: true, status: 'passed',
    dependencies: contract.dependencies,
    tables: contract.tables.map(table => ({ name: table.name, rows: table.rows, sourceVersion: table.version, restoredVersion: restoredVersions[table.name] })),
    checks: { nativeExportFullRowsAndSchemas: true, nativeRestoredIPCFullRowsAndSchemas: true, nodeReopenedNativeRestored: true, pinnedLatestVersion: true, emptySchemaBearingTable: true, sourceAndCopyBytesUnchanged: true, registryAndKnowledgeCopiesUnchanged: true, boundedGoStageNativeScanAndCleanup: true, nativeTypedDomainMatchesArrow: true, goCanonicalTableDomainAdmission: true },
    limitations: ['No historical checkout-version override test; latest-version pin only.', 'No author data, application business behavior, document-to-copy filename crosslink admission, or TS logicalHash parity claim.'],
  }
  writeJSON(at('summary.json'), summary)
  console.log(JSON.stringify({ mode: 'verify', status: summary.status, tables: contract.tables.length, summary: 'summary.json' }))
}

await (mode === 'generate' ? generate() : verify())
