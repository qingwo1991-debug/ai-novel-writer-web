import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { createRequire } from 'node:module'
import fs from 'node:fs'
import path from 'node:path'

import { getDesktopMigrationRegistry } from '../../electron/migrations/desktop-registry.ts'
import { verifyM03ReviewCycle } from '../../electron/migrations/m03-review-cycle.ts'
import { migrateSchema, verifySchema } from '../../electron/migrations/runner.ts'
import { SqliteSchemaAdapter, sqliteSchemaFingerprint } from '../../electron/migrations/sqlite-schema-adapter.ts'

// Original TypeScript upgrades a recognized synthetic v4 project with one
// populated M03 cycle. This runner-only oracle is not production admission.
assert.equal(process.env.GITHUB_ACTIONS, 'true')
assert.equal(process.platform, 'linux')
const runnerTemp = process.env.RUNNER_TEMP
const input = process.env.GO_M03_FULL_V4_CONTRACT
const output = process.env.GO_M03_FULL_V7_CONTRACT
assert(runnerTemp && input && output && path.isAbsolute(runnerTemp))
const insideRunner = file => {
  if (!file || !path.isAbsolute(file)) return false
  const relative = path.relative(runnerTemp, file)
  return relative && relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative)
}
assert(insideRunner(input) && insideRunner(output))
fs.mkdirSync(path.dirname(output), { recursive: true, mode: 0o700 })
const directory = fs.mkdtempSync(path.join(runnerTemp, 'novel-go-m03-full-v7-'))
fs.chmodSync(directory, 0o700)
const Database = createRequire(import.meta.url)('better-sqlite3')
const registry = getDesktopMigrationRegistry()
const hash = data => createHash('sha256').update(data).digest('hex')

const manifest = JSON.parse(fs.readFileSync(input, 'utf8'))
assert.equal(manifest.formatVersion, 1)
assert.equal(manifest.scope, 'recognized-v4-m03')
const source = manifest.cases.find(sample => sample.name === 'valid-full-v4-not-generated')
assert(source && source.expected && insideRunner(source.file))
assert.equal(hash(fs.readFileSync(source.file)), source.sha256)

const base = path.join(directory, 'valid-full-v7-not-generated.db')
fs.copyFileSync(source.file, base, fs.constants.COPYFILE_EXCL)
const db = new Database(base)
let fingerprint
try {
  db.pragma('foreign_keys=ON')
  assert.equal(sqliteSchemaFingerprint(db), manifest.schemaFingerprint)
  assert.equal(verifySchema(new SqliteSchemaAdapter(db), registry, 4).version, 4)
  assert.equal(verifyM03ReviewCycle(db), true)
  const upgraded = migrateSchema(new SqliteSchemaAdapter(db), registry, 7)
  assert.equal(upgraded.version, 7)
  assert.deepEqual([...upgraded.completedSteps], ['M04', 'M05', 'M06'])
  fingerprint = sqliteSchemaFingerprint(db)
  assert.equal(upgraded.fingerprint, fingerprint)
  assert.equal(verifySchema(new SqliteSchemaAdapter(db), registry, 7).fingerprint, fingerprint)
  assert.equal(verifyM03ReviewCycle(db), true)
  assert.equal(db.pragma('foreign_key_check').length, 0)
} finally { db.close() }

const cases = []
function addCase(name, mutate, expected) {
  const file = path.join(directory, `${name}.db`)
  if (file !== base) fs.copyFileSync(base, file, fs.constants.COPYFILE_EXCL)
  const handle = new Database(file)
  try {
    handle.pragma('foreign_keys=ON')
    if (mutate) mutate(handle)
    assert.equal(handle.pragma('user_version', { simple: true }), 7)
    assert.equal(sqliteSchemaFingerprint(handle), fingerprint)
    assert.equal(verifyM03ReviewCycle(handle), expected, `${name}: original M03 verifier`)
    let recognized = false
    try { recognized = verifySchema(new SqliteSchemaAdapter(handle), registry, 7).version === 7 }
    catch { recognized = false }
    assert.equal(recognized, expected, `${name}: original desktop v7 verifier`)
    cases.push({ name, file, expected,
      cycleCount: Number(handle.prepare('SELECT COUNT(*) FROM review_cycles').pluck().get()),
      findingCount: Number(handle.prepare('SELECT COUNT(*) FROM review_findings').pluck().get()) })
  } finally { handle.close() }
  cases.at(-1).sha256 = hash(fs.readFileSync(file))
}
addCase('valid-full-v7-not-generated', null, true)
addCase('full-v7-review-hash', handle => {
  handle.prepare('UPDATE review_cycles SET review_content_hash=?').run(hash('tampered review'))
}, false)
addCase('full-v7-discarded-artifact', handle => {
  handle.exec("UPDATE generation_artifacts SET status='discarded' WHERE attempt_id='attempt-review'")
}, false)

assert.equal(cases.length, 3)
assert.equal(cases.filter(sample => sample.expected).length, 1)
fs.writeFileSync(output, JSON.stringify({ formatVersion: 1, scope: 'recognized-v7-m03',
  schemaFingerprint: fingerprint, cases }) + '\n', { flag: 'wx', mode: 0o600 })
console.log(JSON.stringify({ formatVersion: 1, scope: 'recognized-v7-m03',
  cases: cases.map(({ name, expected }) => ({ name, expected })) }))
