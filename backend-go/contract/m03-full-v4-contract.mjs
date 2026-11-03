import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { createRequire } from 'node:module'
import fs from 'node:fs'
import path from 'node:path'

import { getDesktopMigrationRegistry } from '../../electron/migrations/desktop-registry.ts'
import { verifyM03ReviewCycle } from '../../electron/migrations/m03-review-cycle.ts'
import { verifySchema } from '../../electron/migrations/runner.ts'
import { SqliteSchemaAdapter, sqliteSchemaFingerprint } from '../../electron/migrations/sqlite-schema-adapter.ts'

// All inputs come from original-TypeScript synthetic fixtures on one isolated
// GitHub runner. This creates a recognized v4 schema with a populated M03 row;
// it is an oracle, not author-data admission or a production database.
assert.equal(process.env.GITHUB_ACTIONS, 'true')
assert.equal(process.platform, 'linux')
const runnerTemp = process.env.RUNNER_TEMP
const sqliteManifestPath = process.env.GO_SQLITE_CONTRACT
const m03ManifestPath = process.env.GO_M03_NOTGENERATED_CONTRACT
const output = process.env.GO_M03_FULL_V4_CONTRACT
assert(runnerTemp && sqliteManifestPath && m03ManifestPath && output && path.isAbsolute(runnerTemp))
const insideRunner = file => {
  if (!file || !path.isAbsolute(file)) return false
  const relative = path.relative(runnerTemp, file)
  return relative && relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative)
}
assert(insideRunner(sqliteManifestPath) && insideRunner(m03ManifestPath) && insideRunner(output))
fs.mkdirSync(path.dirname(output), { recursive: true, mode: 0o700 })
const directory = fs.mkdtempSync(path.join(runnerTemp, 'novel-go-m03-full-v4-'))
fs.chmodSync(directory, 0o700)
const Database = createRequire(import.meta.url)('better-sqlite3')
const registry = getDesktopMigrationRegistry()
const hash = data => createHash('sha256').update(data).digest('hex')

const sqliteManifest = JSON.parse(fs.readFileSync(sqliteManifestPath, 'utf8'))
assert.equal(sqliteManifest.formatVersion, 1)
assert.equal(sqliteManifest.currentVersion, 7)
const baseline = sqliteManifest.fixtures.find(fixture => fixture.name === 'baseline-v4')
assert(baseline && baseline.version === 4 && baseline.file === 'baseline-v4.db')
const baselinePath = path.join(path.dirname(sqliteManifestPath), baseline.file)
assert(insideRunner(baselinePath) && fs.statSync(baselinePath).isFile())
const m03Manifest = JSON.parse(fs.readFileSync(m03ManifestPath, 'utf8'))
assert.equal(m03Manifest.formatVersion, 1)
assert.equal(m03Manifest.scope, 'm03-verifier-only')
const donor = m03Manifest.cases.find(sample => sample.name === 'valid-not-generated')
assert(donor && donor.expected && insideRunner(donor.file))
assert.equal(hash(fs.readFileSync(donor.file)), donor.sha256)

const base = path.join(directory, 'valid-full-v4-not-generated.db')
fs.copyFileSync(baselinePath, base, fs.constants.COPYFILE_EXCL)
const db = new Database(base)
try {
  db.pragma('foreign_keys=ON')
  assert.equal(sqliteSchemaFingerprint(db), baseline.fingerprint)
  assert.equal(verifySchema(new SqliteSchemaAdapter(db), registry, 4).version, 4)
  db.prepare('ATTACH DATABASE ? AS m03donor').run(donor.file)
  try {
    const tables = ['contents', 'drafts', 'reviews', 'generation_roots',
      'generation_runs', 'generation_attempts', 'generation_artifacts',
      'review_cycles', 'review_findings']
    // M02 adds other tables but must not alter these source/M01/M03 layouts.
    // Reject a schema change instead of silently dropping author columns.
    for (const table of tables) {
      const local = db.prepare(`PRAGMA main.table_info(${table})`).all().map(column => column.name)
      const source = db.prepare(`PRAGMA m03donor.table_info(${table})`).all().map(column => column.name)
      assert.deepEqual(local, source, `${table}: synthetic donor layout differs`)
    }
    db.transaction(() => {
      // The baseline exporter places one unreferenced marker in contents.
      assert.equal(Number(db.prepare('SELECT COUNT(*) FROM contents').pluck().get()), 1)
      db.exec('DELETE FROM contents')
      for (const table of tables) {
        assert.equal(Number(db.prepare(`SELECT COUNT(*) FROM ${table}`).pluck().get()), 0)
        db.exec(`INSERT INTO ${table} SELECT * FROM m03donor.${table}`)
      }
    })()
  } finally {
    db.exec('DETACH DATABASE m03donor')
  }
  assert.equal(db.pragma('foreign_key_check').length, 0)
  assert.equal(sqliteSchemaFingerprint(db), baseline.fingerprint)
  assert.equal(verifyM03ReviewCycle(db), true)
  assert.equal(verifySchema(new SqliteSchemaAdapter(db), registry, 4).version, 4)
} finally { db.close() }

const cases = []
function addCase(name, mutate, expected) {
  const file = path.join(directory, `${name}.db`)
  if (file !== base) fs.copyFileSync(base, file, fs.constants.COPYFILE_EXCL)
  const handle = new Database(file)
  try {
    handle.pragma('foreign_keys=ON')
    if (mutate) mutate(handle)
    assert.equal(sqliteSchemaFingerprint(handle), baseline.fingerprint)
    assert.equal(verifyM03ReviewCycle(handle), expected, `${name}: M03 verifier`)
    let accepted = false
    try { accepted = verifySchema(new SqliteSchemaAdapter(handle), registry, 4).version === 4 }
    catch { accepted = false }
    assert.equal(accepted, expected, `${name}: original desktop v4 verification`)
    cases.push({ name, file, expected,
      cycleCount: Number(handle.prepare('SELECT COUNT(*) FROM review_cycles').pluck().get()),
      findingCount: Number(handle.prepare('SELECT COUNT(*) FROM review_findings').pluck().get()) })
  } finally { handle.close() }
  cases.at(-1).sha256 = hash(fs.readFileSync(file))
}
addCase('valid-full-v4-not-generated', null, true)
addCase('full-v4-review-hash', handle => {
  handle.prepare('UPDATE review_cycles SET review_content_hash=?').run(hash('tampered review'))
}, false)
addCase('full-v4-discarded-artifact', handle => {
  handle.exec("UPDATE generation_artifacts SET status='discarded' WHERE attempt_id='attempt-review'")
}, false)

assert.equal(cases.length, 3)
assert.equal(cases.filter(sample => sample.expected).length, 1)
fs.writeFileSync(output, JSON.stringify({ formatVersion: 1, scope: 'recognized-v4-m03',
  schemaFingerprint: baseline.fingerprint, cases }) + '\n', { flag: 'wx', mode: 0o600 })
console.log(JSON.stringify({ formatVersion: 1, scope: 'recognized-v4-m03',
  cases: cases.map(({ name, expected }) => ({ name, expected })) }))
