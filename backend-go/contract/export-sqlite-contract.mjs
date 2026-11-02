import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
import { mkdirSync, writeFileSync, existsSync } from 'node:fs'
import { createHash } from 'node:crypto'
import path from 'node:path'
import { CURRENT_DESKTOP_SCHEMA_VERSION, getDesktopMigrationRegistry } from '../../electron/migrations/desktop-registry.ts'
import { initializeLegacyBaselineSchema } from '../../electron/migrations/baseline-schema.ts'
import { migrateSchema, verifySchema } from '../../electron/migrations/runner.ts'
import { SqliteSchemaAdapter, sqliteSchemaFingerprint } from '../../electron/migrations/sqlite-schema-adapter.ts'
import { probeProjectSqlite } from '../../electron/services/sqlite-project-migration.ts'

// Runs ONLY on a disposable GitHub runner. The oracle uses checked-in TS DDL
// and synthetic rows; it is not a Node bridge in the application runtime.
assert.equal(process.env.GITHUB_ACTIONS, 'true', 'contract generation is GitHub-only')
const base = process.env.RUNNER_TEMP
const target = process.env.GO_SQLITE_CONTRACT
assert(base && target && path.isAbsolute(base) && path.isAbsolute(target))
const relative = path.relative(base, target)
assert(relative && !relative.startsWith('..') && !path.isAbsolute(relative), 'must use disposable runner directory')
const directory = path.dirname(target)
mkdirSync(directory, { recursive: true, mode: 0o700 })
assert(!existsSync(target), 'do not overwrite any existing contract')
const Database = createRequire(import.meta.url)('better-sqlite3')
const registry = getDesktopMigrationRegistry()
const fixtures = []
const domainProofs = []
for (const [name, donor, version] of [
  ['baseline-v7', false, 7], ['donor-v7', true, 7], ['baseline-v6', false, 6], ['donor-v6', true, 6],
  ['baseline-v6-upgraded-v7', false, 7], ['donor-v6-upgraded-v7', true, 7],
]) {
  const file = name + '.db'
  const filename = path.join(directory, file)
  assert(!existsSync(filename), 'never open an existing database')
  const db = new Database(filename)
  try {
    db.pragma('foreign_keys=ON')
    // Pass an explicit public synthetic key. No application credentials or
    // source identity key from the host can be loaded by baseline backfills.
    db.transaction(() => initializeLegacyBaselineSchema(db, Buffer.alloc(32, 0x23)))()
    if (donor) {
      const row = db.prepare("SELECT sql FROM sqlite_master WHERE type='table' AND name='characters'").get()
      const marker = "      cs_location TEXT DEFAULT '',"
      assert(row.sql.includes(marker))
      const sql = row.sql.replace(marker, "      avatar TEXT NOT NULL DEFAULT '',\n" + marker)
      db.exec('ALTER TABLE characters RENAME TO characters_without_avatar')
      db.exec(sql)
      db.exec('DROP TABLE characters_without_avatar')
    }
    migrateSchema(new SqliteSchemaAdapter(db), registry, name.includes('-upgraded-') ? 6 : version)
    db.prepare('INSERT INTO contents(body,created_at) VALUES(?,?)').run('Go 合成兼容样例 😀 <&>\u2028\u2029', '2026-01-01 00:00:00')
    if (name.includes('-upgraded-')) migrateSchema(new SqliteSchemaAdapter(db), registry, 7)
    verifySchema(new SqliteSchemaAdapter(db), registry, version)
    fixtures.push({ name, file, version, fingerprint: sqliteSchemaFingerprint(db) })
  } finally { db.close() }
  if (version === 7) {
    const evidence = probeProjectSqlite({ databasePath: filename })
    domainProofs.push({ file, domain: evidence.domain })
  }
}
// Independently stress storage-class JSON and UTF-16 row sorting. This schema
// is a test-only contract, never added to the production recognized directory.
const valuesFile = 'domain-values.db'
const values = new Database(path.join(directory, valuesFile))
try {
  values.exec('CREATE TABLE values_test (id INTEGER, real_value REAL, body TEXT, blob_value BLOB, nullable TEXT); PRAGMA user_version=7')
  const insert = values.prepare('INSERT INTO values_test VALUES(?,?,?,?,?)')
  for (const [id, real, body, blob] of [
    [9007199254740993n, 1e-7, '\u{10000}', Buffer.from([0, 255, 34])],
    [-9223372036854775808n, 1e21, '\ue000', Buffer.alloc(0)],
    [1n, -0, '<>&\u2028\u2029"\\\n', Buffer.from([1])],
    [2n, 0.000001, '中文 😀', null],
  ]) insert.run(id, real, body, blob, null)
  const rows = values.prepare('SELECT * FROM values_test').safeIntegers().raw().all()
  const encoded = rows.map(row => JSON.stringify(row, (_key, value) => typeof value === 'bigint' ? { integer: value.toString() } : value)).sort()
  domainProofs.push({ file: valuesFile, fingerprint: sqliteSchemaFingerprint(values), domain: {
    tableCounts: { values_test: encoded.length }, authorContentHash: createHash('sha256').update(JSON.stringify(['values_test', encoded])).digest('hex'),
  } })
} finally { values.close() }
writeFileSync(path.join(directory, 'domain-proofs.json'), JSON.stringify(domainProofs) + '\n', { flag: 'wx', mode: 0o600 })
writeFileSync(target, JSON.stringify({
  formatVersion: 1,
  currentVersion: CURRENT_DESKTOP_SCHEMA_VERSION,
  recognized: registry.recognizedSchemas,
  fixtures,
}, null, 2) + '\n', { flag: 'wx', mode: 0o600 })
console.log(JSON.stringify({ formatVersion: 1, currentVersion: 7, fixtureCount: fixtures.length, recognizedCount: registry.recognizedSchemas.length }))
