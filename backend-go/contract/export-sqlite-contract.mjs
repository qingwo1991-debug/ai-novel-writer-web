import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
import { mkdirSync, writeFileSync, existsSync, copyFileSync, constants } from 'node:fs'
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
  ['baseline-v3',false,3],['donor-v3',true,3],
  ['baseline-v3-upgraded-v4',false,4],['donor-v3-upgraded-v4',true,4],
  ['baseline-v4',false,4],['donor-v4',true,4],['baseline-v5',false,5],['donor-v5',true,5],
  ['baseline-v4-upgraded-v5',false,5],['donor-v4-upgraded-v5',true,5],
  ['baseline-v5-upgraded-v6',false,6],['donor-v5-upgraded-v6',true,6],
]) {
  const file = name + '.db'
  const filename = path.join(directory, file)
  assert(!existsSync(filename), 'never open an existing database')
  // Expected upgrades start from the EXACT closed v6 bytes, not a separately
  // initialized database whose metadata timestamps/default rows may differ.
  const upgraded = name.match(/^(baseline|donor)-v(\d+)-upgraded-v(\d+)$/u)
  if (upgraded) copyFileSync(path.join(directory, `${upgraded[1]}-v${upgraded[2]}.db`), filename, constants.COPYFILE_EXCL)
  const db = new Database(filename)
  try {
    db.pragma('foreign_keys=ON')
    if (upgraded) {
      migrateSchema(new SqliteSchemaAdapter(db), registry, version)
    } else {
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
    migrateSchema(new SqliteSchemaAdapter(db), registry, version)
    db.prepare('INSERT INTO contents(body,created_at) VALUES(?,?)').run('Go 合成兼容样例 😀 <&>\u2028\u2029', '2026-01-01 00:00:00')
    }
    verifySchema(new SqliteSchemaAdapter(db), registry, version)
    fixtures.push({ name, file, version, fingerprint: sqliteSchemaFingerprint(db) })
  } finally { db.close() }
  if (version === 7) {
    const evidence = probeProjectSqlite({ databasePath: filename })
    domainProofs.push({ file, domain: evidence.domain })
  }
}
// M03 adds empty review-cycle tables without guessing cycles from historical
// reviews. These two files share exactly the same synthetic v3 author rows;
// only the original TypeScript migration may create the v4 target.
for (const [name, upgraded] of [
  ['baseline-v3-history', false], ['baseline-v3-history-upgraded-v4', true],
]) {
  const file = name + '.db'
  const filename = path.join(directory, file)
  const source = path.join(directory, upgraded ? 'baseline-v3-history.db' : 'baseline-v3.db')
  copyFileSync(source, filename, constants.COPYFILE_EXCL)
  const db = new Database(filename)
  try {
    db.pragma('foreign_keys=ON')
    if (upgraded) {
      migrateSchema(new SqliteSchemaAdapter(db), registry, 4)
    } else {
      db.prepare('INSERT INTO contents(id,body) VALUES(?,?)').run(900003, 'M03 前的草稿正文\r\n')
      db.prepare('INSERT INTO contents(id,body) VALUES(?,?)').run(900004, 'M03 前的审稿正文\r\n')
      db.prepare('INSERT INTO drafts(id,chapter_number,version,content_id) VALUES(?,?,?,?)')
        .run(900003, 900003, 1, 900003)
      db.prepare('INSERT INTO reviews(id,base_draft_id,review_index,content_id) VALUES(?,?,?,?)')
        .run(900003, 900003, 1, 900004)
    }
    verifySchema(new SqliteSchemaAdapter(db), registry, upgraded ? 4 : 3)
    fixtures.push({ name, file, version: upgraded ? 4 : 3, fingerprint: sqliteSchemaFingerprint(db) })
  } finally { db.close() }
}
// Original installed M04 over a genuinely populated, receipt-only v4 history.
// These are disposable synthetic copies of the recognized baseline v4 schema;
// the expected v5 fixture is produced by the ACTUAL TypeScript migration.
for (const [name, upgraded] of [
  ['baseline-v4-receipts', false], ['baseline-v4-receipts-upgraded-v5', true],
]) {
  const file = name + '.db'
  const filename = path.join(directory, file)
  const source = path.join(directory, upgraded ? 'baseline-v4-receipts.db' : 'baseline-v4.db')
  copyFileSync(source, filename, constants.COPYFILE_EXCL)
  const db = new Database(filename)
  try {
    db.pragma('foreign_keys=ON')
    if (upgraded) {
      migrateSchema(new SqliteSchemaAdapter(db), registry, 5)
    } else {
      db.prepare(`INSERT INTO import_runs(
        id,purpose,root_run_id,effect_namespace,source_fingerprint,manifest_fingerprint,locale,
        stage,status,total_chapters,manifest_chapter_count
      ) VALUES(?,?,?,?,?,?,?,?,?,?,?)`).run(
        'synthetic-m04-run','reference','synthetic-m04-run','import:reference:synthetic-m04-run',
        'synthetic-source','synthetic-manifest','zh-CN','global','running',1,1)
      const receipt = db.prepare(`INSERT INTO import_run_receipts(
        run_id,effect_namespace,effect_key,stage,batch_id,kind,payload_json,payload_hash,state
      ) VALUES(?,?,?,?,?,?,?,?,?)`)
      for (const [batch, state] of [['committed-batch','committed'],['prepared-batch','prepared']]) {
        receipt.run('synthetic-m04-run','import:reference:synthetic-m04-run',`synthetic-effect:${batch}`,
          'global',batch,'project-global-facts','{}','synthetic-hash',state)
      }
    }
    verifySchema(new SqliteSchemaAdapter(db), registry, upgraded ? 5 : 4)
    fixtures.push({ name, file, version: upgraded ? 5 : 4, fingerprint: sqliteSchemaFingerprint(db) })
  } finally { db.close() }
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
// CI-only >64MiB logical proof oracle. This tests total-row spilling, not a
// single oversized row. No production recognized schema is extended.
const largeFile = 'domain-large.db'
const large = new Database(path.join(directory, largeFile))
try {
  large.exec('CREATE TABLE large_rows (body TEXT NOT NULL); PRAGMA user_version=7')
  const insert = large.prepare('INSERT INTO large_rows(body) VALUES(?)')
  const payload = 'x'.repeat(256 * 1024)
  large.transaction(() => { for (let i = 0; i < 280; i++) insert.run(`${String((i * 97) % 281).padStart(3, '0')}:${payload}`) })()
  const rows = large.prepare('SELECT * FROM large_rows').raw().all().map(row => JSON.stringify(row)).sort()
  assert(rows.reduce((sum, row) => sum + Buffer.byteLength(row), 0) > 64 * 1024 * 1024)
  const digest = createHash('sha256').update('["large_rows",[')
  for (let i = 0; i < rows.length; i++) { if (i) digest.update(','); digest.update(JSON.stringify(rows[i])) }
  digest.update(']]')
  writeFileSync(path.join(directory, 'large-domain-proof.json'), JSON.stringify({ file: largeFile, fingerprint: sqliteSchemaFingerprint(large), domain: { tableCounts: { large_rows: rows.length }, authorContentHash: digest.digest('hex') } }) + '\n', { flag: 'wx', mode: 0o600 })
} finally { large.close() }
writeFileSync(path.join(directory, 'domain-proofs.json'), JSON.stringify(domainProofs) + '\n', { flag: 'wx', mode: 0o600 })
writeFileSync(target, JSON.stringify({
  formatVersion: 1,
  currentVersion: CURRENT_DESKTOP_SCHEMA_VERSION,
  recognized: registry.recognizedSchemas,
  fixtures,
}, null, 2) + '\n', { flag: 'wx', mode: 0o600 })
console.log(JSON.stringify({ formatVersion: 1, currentVersion: 7, fixtureCount: fixtures.length, recognizedCount: registry.recognizedSchemas.length }))
