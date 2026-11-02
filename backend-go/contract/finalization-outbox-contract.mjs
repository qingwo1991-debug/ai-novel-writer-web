import assert from 'node:assert/strict'
import { createRequire, registerHooks, stripTypeScriptTypes, builtinModules } from 'node:module'
import { readFileSync, existsSync, mkdirSync, copyFileSync, constants, writeFileSync, chmodSync } from 'node:fs'
import { createHash } from 'node:crypto'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

// CI-only recursive original-repository oracle. No business implementation is
// substituted: database.ts alone supplies the private synthetic connection.
assert.equal(process.env.GITHUB_ACTIONS, 'true', 'contract generation is GitHub-only')
const base = process.env.RUNNER_TEMP
const target = process.env.GO_FINALIZATION_OUTBOX_CONTRACT
const sqliteContract = process.env.GO_SQLITE_CONTRACT
assert(base && target && sqliteContract && path.isAbsolute(base) && path.isAbsolute(target) && path.isAbsolute(sqliteContract))
assert.equal(target, path.join(base, 'novel-go-finalization-outbox', 'contract.json'))
const source = path.join(path.dirname(sqliteContract), 'baseline-v7.db')
const relativeSource = path.relative(base, source)
assert(relativeSource && !relativeSource.startsWith('..') && !path.isAbsolute(relativeSource), 'baseline must be disposable synthetic data')
assert(existsSync(source))
const directory = path.dirname(target)
assert(!existsSync(directory), 'never reuse or overwrite a fixture directory')
mkdirSync(directory, { recursive: true, mode: 0o700 })
const root = new URL('../../', import.meta.url)
const databaseURL = new URL('electron/database', root).href
const databaseStub = 'finalization-outbox-contract:database'
const globalKey = '__GO_FINALIZATION_OUTBOX_CONTRACT_DB__'
assert(!Object.hasOwn(globalThis, globalKey))
globalThis[globalKey] = null
const Database = createRequire(import.meta.url)('better-sqlite3')
const probe = new Database(':memory:')
probe.close()
const builtins = new Set(builtinModules.flatMap(name => [name, `node:${name}`]))
const hooks = registerHooks({
  resolve(specifier, context, nextResolve) {
    if (builtins.has(specifier)) return nextResolve(specifier, context)
    assert(specifier.startsWith('.') || specifier.startsWith('file:'), `unexpected dependency ${specifier}`)
    const url = new URL(specifier, context.parentURL ?? root).href
    if (url === databaseURL || url === `${databaseURL}.ts`) return { url: databaseStub, shortCircuit: true }
    const candidate = url.endsWith('.ts') ? url : `${url}.ts`
    const filename = fileURLToPath(candidate)
    const relative = path.relative(fileURLToPath(root), filename)
    assert(relative && !relative.startsWith('..') && !path.isAbsolute(relative), 'dependency escaped checkout')
    assert(relative.startsWith('electron/repositories/') || relative.startsWith('electron/services/') || relative.startsWith('electron/migrations/') || relative.startsWith('src/shared/') || relative.startsWith('src/utils/'), `unexpected original module ${relative}`)
    assert(existsSync(filename), `missing original module ${relative}`)
    return { url: candidate, shortCircuit: true }
  },
  load(url, context, nextLoad) {
    if (url === databaseStub) return { format: 'module', source: `export function getProjectDb(){return globalThis[${JSON.stringify(globalKey)}]} export function getCurrentProjectPath(){throw new Error('CONTRACT_PROJECT_PATH_FORBIDDEN')}`, shortCircuit: true }
    if (!url.startsWith(root.href) || !url.endsWith('.ts')) return nextLoad(url, context)
    return { format: 'module', source: stripTypeScriptTypes(readFileSync(new URL(url), 'utf8'), { mode: 'transform', sourceUrl: url }), shortCircuit: true }
  },
})

const timestampKeys = new Set(['created_at', 'updated_at', 'createdAt', 'updatedAt'])
function normalize(value) {
  if (Buffer.isBuffer(value)) return value.toJSON()
  if (Array.isArray(value)) return value.map(normalize)
  if (value && typeof value === 'object') return Object.fromEntries(Object.entries(value).filter(([key]) => !timestampKeys.has(key)).map(([key, child]) => [key, key === 'publishedAt' || key === 'published_at' ? (child === null ? null : '<published>') : normalize(child)]))
  return value
}
const tables = ['contents', 'drafts', 'finalization_outbox', 'continuity_projection_meta', 'character_identity_meta', 'character_identity_proposals', 'characters', 'character_aliases', 'reviews', 'revisions']
const snapshot = db => Object.fromEntries(tables.map(table => [table, db.prepare(`SELECT * FROM ${table} ORDER BY rowid`).all()]))
const hash = value => createHash('sha256').update(value, 'utf8').digest('hex')
const BODY = 'Synthetic 原稿 😀\r\nsecond line <&>\u2028\u2029'
const FROZEN = 'Final 中文𠮷 café cafe\u0301 l’amour 12 😀\n<&>\u2028\u2029'
const input = () => ({ finalizationId: 'synthetic-finalization-11', draftId: 11, chapterNumber: 7, chapterTitle: 'Title 中文 <&>\u2028', content: FROZEN, expectedContent: BODY, contentHash: hash(FROZEN), contentRevision: 3, targetFileName: '第7章 Title.txt' })
function insert(db, table, row) {
  const keys = Object.keys(row)
  return Number(db.prepare(`INSERT INTO ${table}(${keys.join(',')}) VALUES(${keys.map(() => '?').join(',')})`).run(...Object.values(row)).lastInsertRowid)
}
const patch = (db, table, id, changes) => db.prepare(`UPDATE ${table} SET ${Object.keys(changes).map(key => `${key}=?`).join(',')} WHERE id=?`).run(...Object.values(changes), id)
const bodyId = db => db.prepare('SELECT content_id FROM drafts WHERE id=11').pluck().get()
const definitions = []
const add = (name, method = 'Commit', options = {}) => definitions.push({ name, method, makeInput: input, ...options })
const cases = []
try {
  // The actual finalization repository recursively imports actual summary,
  // character-identity, roster, portable-freeze and shared source implementations.
  const { FinalizationRepository: Repository } = await import(new URL('electron/repositories/finalization-repository.ts', root).href)
  const committed = db => Repository.commit(input())
  add('commit-frozen-body-and-character-receipt')
  for (const status of ['revised', 'reviewed']) add(`commit-working-${status}`, 'Commit', { seed: db => patch(db, 'drafts', 11, { status }) })
  for (const status of ['finalized', 'superseded', 'archived']) add(`commit-reject-${status}`, 'Commit', { seed: db => patch(db, 'drafts', 11, { status }) })
  add('commit-missing-draft', 'Commit', { change: value => { value.draftId = 999 } })
  add('commit-chapter-mismatch', 'Commit', { change: value => { value.chapterNumber = 9 } })
  add('commit-stale-full-body', 'Commit', { change: value => { value.expectedContent += ' stale' } })
  add('commit-missing-baseline', 'Commit', { change: value => { delete value.expectedContent } })
  add('commit-null-baseline', 'Commit', { change: value => { value.expectedContent = null } })
  add('commit-blob-body-is-not-text', 'Commit', { seed: db => patch(db, 'contents', bodyId(db), { body: Buffer.from(BODY) }) })
  add('commit-empty-body', 'Commit', { change: value => { value.content = ''; value.contentHash = hash('') } })
  add('commit-supersedes-only-same-chapter', 'Commit', { seed: db => { patch(db, 'drafts', 22, { chapter_number: 7, status: 'finalized' }); patch(db, 'drafts', 33, { status: 'finalized' }) } })
  add('commit-preserves-earlier-stale-watermark', 'Commit', { seed: db => { patch(db, 'drafts', 22, { chapter_number: 7, status: 'finalized' }); db.prepare("UPDATE continuity_projection_meta SET generation=5,stale_from_chapter=2 WHERE id='main'").run() } })
  add('commit-replay-original-id-and-file', 'Commit', { seed: committed, change: value => { value.finalizationId = 'new-discarded-candidate'; value.targetFileName = 'different.txt'; value.expectedContent = 'stale replay baseline' } })
  add('commit-replay-does-not-read-live-prose', 'Commit', { seed: db => { committed(db); patch(db, 'contents', bodyId(db), { body: 'Later tampered prose' }) } })
  for (const key of ['chapterNumber', 'chapterTitle', 'contentRevision', 'content']) add(`commit-replay-conflicting-${key}`, 'Commit', { seed: committed, change: value => { value[key] = typeof value[key] === 'number' ? value[key] + 1 : `${value[key]} changed`; value.contentHash = hash(value.content) } })
  for (const method of ['Get', 'GetByDraftID']) {
    add(`${method}-missing`, method)
    add(`${method}-pending`, method, { seed: committed })
    add(`${method}-immutable-after-live-edit`, method, { seed: db => { committed(db); patch(db, 'contents', bodyId(db), { body: 'Live changed' }) } })
  }
  for (const method of ['MarkPending', 'MarkPublished']) {
    add(`${method}-missing`, method)
    add(`${method}-committed`, method, { seed: committed })
    add(`${method}-already-published`, method, { seed: db => { committed(db); Repository.markPublished(input().finalizationId) } })
  }
  add('Get-published', 'Get', { seed: db => { committed(db); Repository.markPublished(input().finalizationId) } })
  add('MarkPending-retains-previous-published-at', 'MarkPending', { seed: db => { committed(db); Repository.markPublished(input().finalizationId) } })
  for (const text of ['汉𠮷カナひらがな한글 café Ελληνικά русский العربية हिन्दी', "can't l’amour a’中 123", '\u0085\ufeff\u200b\u0000\t\n!😀', 'e\u0301 \u0301a A\u0301 １②Ⅳ']) add(`commit-units-${definitions.length}`, 'Commit', { change: value => { value.content = text; value.contentHash = hash(text) } })
  assert.equal(definitions.length, 39, 'keep the reviewed actual-method fixture count stable')
  assert(new Set(definitions.map(item => item.name)).size === definitions.length)
  for (const definition of definitions) {
    assert(/^[A-Za-z0-9_-]+$/u.test(definition.name))
    const filename = path.join(directory, `${definition.name}.db`)
    copyFileSync(source, filename, constants.COPYFILE_EXCL)
    chmodSync(filename, 0o600)
    const db = new Database(filename)
    let before
    try {
      db.pragma('foreign_keys=ON')
      db.pragma('journal_mode=DELETE')
      globalThis[globalKey] = db
      assert.equal(db.prepare('SELECT COUNT(*) FROM drafts').pluck().get(), 0)
      for (const [id, chapter] of [[11, 7], [22, 8], [33, 9]]) insert(db, 'drafts', { id, chapter_number: chapter, version: 3, status: 'draft', content_id: insert(db, 'contents', { body: id === 11 ? BODY : `Other ${id}` }), word_count: 19 })
      insert(db, 'characters', { character_id: 'character-a', name: 'Name 中文 <&>\u2028\u2029' })
      insert(db, 'character_aliases', { character_id: 'character-a', name: 'Alias 😀', source_key: 'synthetic', valid_from: 0, valid_through: null })
      insert(db, 'character_aliases', { character_id: 'character-a', name: 'Expired', source_key: 'synthetic-old', valid_from: 0, valid_through: 0 })
      db.prepare("UPDATE character_identity_meta SET revision=2 WHERE id='main'").run()
      definition.seed?.(db)
      const value = definition.makeInput()
      definition.change?.(value)
      assert.deepEqual(db.pragma('foreign_key_check'), [])
      before = db.serialize()
      let result = null
      let error
      try {
        switch (definition.method) {
          case 'Commit': result = Repository.commit(value); break
          case 'Get': result = Repository.get(value.finalizationId); break
          case 'GetByDraftID': result = Repository.getByDraftId(value.draftId); break
          case 'MarkPending': result = Repository.markPublicationPending(value.finalizationId, 'Synthetic publication failure 中文'); break
          case 'MarkPublished': result = Repository.markPublished(value.finalizationId); break
          default: assert.fail(`unknown original method ${definition.method}`)
        }
      } catch (failure) { assert(failure instanceof Error); error = failure.message }
      cases.push({ name: definition.name, database: filename, method: definition.method, input: value, result: normalize(result), ...(error === undefined ? {} : { error }), after: normalize(snapshot(db)) })
      assert.deepEqual(db.pragma('foreign_key_check'), [])
    } finally {
      globalThis[globalKey] = null
      db.close()
      if (before) writeFileSync(filename, before, { mode: 0o600 })
    }
  }
  writeFileSync(target, JSON.stringify({ formatVersion: 1, cases }) + '\n', { flag: 'wx', mode: 0o600 })
} finally {
  delete globalThis[globalKey]
  hooks.deregister()
}
console.log(`Generated ${cases.length} actual recursive FinalizationRepository contracts on private baseline-v7 clones; storage only, no publication or fake business methods`)
