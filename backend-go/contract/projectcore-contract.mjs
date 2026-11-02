import assert from 'node:assert/strict'
import { createRequire, registerHooks, stripTypeScriptTypes } from 'node:module'
import { readFileSync, existsSync, mkdirSync, copyFileSync, constants, writeFileSync } from 'node:fs'
import path from 'node:path'

// Only synthetic fixtures on the disposable runner. The original repository is
// used directly; getProjectDb is forbidden because every call receives its DB.
assert.equal(process.env.GITHUB_ACTIONS, 'true')
const target = process.env.GO_PROJECTCORE_CONTRACT
const base = process.env.RUNNER_TEMP
assert(base && target && path.isAbsolute(base) && path.isAbsolute(target))
const relative = path.relative(base, target)
assert(relative && !relative.startsWith('..') && !path.isAbsolute(relative))
assert(!existsSync(target))
mkdirSync(path.dirname(target), { recursive: true, mode: 0o700 })
const root = new URL('../../', import.meta.url)
const names = ['electron/repositories/project-core-repository.ts', 'src/shared/reasoning-types.ts', 'src/shared/writing-language.ts', 'src/shared/narrative-thread.ts']
const sources = new Map(names.map(name => [new URL(name, root).href, readFileSync(new URL(name, root), 'utf8')]))
const databaseStub = 'projectcore-contract:database'
const Database = createRequire(import.meta.url)('better-sqlite3')
// Force the driver's lazy native binding load before installing the deliberately
// narrow TS source loader. This database is memory-only and synthetic.
const driverProbe = new Database(':memory:')
driverProbe.close()
const hooks = registerHooks({
  resolve(specifier, context) {
    const url = new URL(specifier, context.parentURL).href
    if (url === new URL('electron/database', root).href) return { url: databaseStub, shortCircuit: true }
    const resolved = sources.has(url) ? url : `${url}.ts`
    assert(sources.has(resolved), `unexpected project-core dependency ${specifier}`)
    return { url: resolved, shortCircuit: true }
  },
  load(url) {
    if (url === databaseStub) return { format: 'module', source: 'export function getProjectDb(){throw new Error("IMPLICIT_DATABASE_FORBIDDEN")}', shortCircuit: true }
    assert(sources.has(url))
    return { format: 'module', source: stripTypeScriptTypes(sources.get(url), { mode: 'transform', sourceUrl: url }), shortCircuit: true }
  },
})
const { ProjectCoreRepository } = await import(new URL(names[0], root).href)
const source = path.join(path.dirname(process.env.GO_SQLITE_CONTRACT), 'baseline-v7.db')
const cases = []
const definitions = [
  { name: 'default-get', action: 'get' },
  { name: 'nullable-get', action: 'get', seed: { genre: null, total_chapters: null, synopsis: null } },
  { name: 'invalid-normalized-get', action: 'get', seed: { creative_strategy: 'future', writing_language: 'other', narrative_thread_dormant_threshold: '19' } },
  { name: 'known-normalized-get', action: 'get', seed: { creative_strategy: 'deep-planning', writing_language: 'en-US', narrative_thread_dormant_threshold: 4.9 } },
  { name: 'all-fields-update', action: 'update', data: { projectName: 'Synthetic 中文', genre: 'fantasy', subGenre: 'public', targetAudience: 'synthetic', totalChapters: 20, wordsPerChapter: 3000, writingLanguage: 'en-US', creativeStrategy: 'consistency-first', narrativeThreadDormantChapterThreshold: 8.7, plotStructure: 'public structure', narrativePov: 'first', writingStyle: 'style', referenceWorks: 'none', globalGuidance: 'public', goldenFinger: 'public', coreOutline: 'public', worldSetting: 'public', protagonistProfile: 'public', premise: 'premise', worldbuilding: 'world', synopsis: 'outline 😀', characterStates: 'state' } },
  { name: 'unknown-fields-ignored', action: 'update', data: { unknown: 'public', id: 'attacker', updated_at: 'attacker' } },
  { name: 'characters-arch-read-only-even-null', action: 'update', data: { projectName: 'must not change', charactersArch: null } },
  { name: 'nullable-update', action: 'update', data: { synopsis: null, narrativeThreadDormantChapterThreshold: null } },
]
for (const value of [-20, 0, 1, 3.8, 50, 100, '7', null, true]) definitions.push({ name: `threshold-${String(value)}`, action: 'update', data: { narrativeThreadDormantChapterThreshold: value } })
for (const key of ['synopsis', 'premise', 'charactersArch', 'worldbuilding', 'genre', 'totalChapters', 'wordsPerChapter', 'writingLanguage', 'plotStructure', 'narrativePov', 'globalGuidance']) definitions.push({ name: `synopsis-cas-drift-${key}`, action: 'commitSynopsis', drift: key })
definitions.push({ name: 'synopsis-cas-match', action: 'commitSynopsis' }, { name: 'synopsis-cas-unrelated-field-allowed', action: 'commitSynopsis', unrelated: true })
for (const definition of definitions) {
  const file = `${definition.name}.db`
  const filename = path.join(path.dirname(target), file)
  copyFileSync(source, filename, constants.COPYFILE_EXCL)
  const db = new Database(filename)
  try {
    db.prepare("INSERT OR IGNORE INTO project_core(id, project_name) VALUES('main', 'synthetic')").run()
    for (const [key, value] of Object.entries(definition.seed ?? {})) {
      assert(/^[a-z_]+$/u.test(key))
      db.prepare(`UPDATE project_core SET ${key}=? WHERE id='main'`).run(value)
    }
    if (definition.unrelated) db.prepare("UPDATE project_core SET writing_style='unrelated' WHERE id='main'").run()
    const initial = ProjectCoreRepository.get(db)
    const before = db.prepare("SELECT * FROM project_core WHERE id='main'").get()
    let input = definition.data
    if (definition.action === 'commitSynopsis') {
      const expected = Object.fromEntries(['synopsis', 'premise', 'charactersArch', 'worldbuilding', 'genre', 'totalChapters', 'wordsPerChapter', 'writingLanguage', 'plotStructure', 'narrativePov', 'globalGuidance'].map(key => [key, initial[key]]))
      if (definition.drift) expected[definition.drift] = typeof expected[definition.drift] === 'number' ? expected[definition.drift] + 1 : `${expected[definition.drift]} changed`
      input = { synopsis: 'new synthetic synopsis 中文😀', expected }
    }
    let result = null, error
    try {
      if (definition.action === 'get') result = ProjectCoreRepository.get(db)
      else if (definition.action === 'update') ProjectCoreRepository.update(input, db)
      else result = ProjectCoreRepository.commitSynopsis(input, db)
    } catch (failure) { error = failure.message }
    const after = db.prepare("SELECT * FROM project_core WHERE id='main'").get()
    // The Go test starts from precisely this pre-operation synthetic row, not a
    // separately initialized row with differing defaults or timestamps.
    db.prepare("DELETE FROM project_core WHERE id='main'").run()
    const columns = Object.keys(before)
    db.prepare(`INSERT INTO project_core(${columns.join(',')}) VALUES(${columns.map(() => '?').join(',')})`).run(...Object.values(before))
    cases.push({ name: definition.name, file, action: definition.action, input, result, error, after: Object.fromEntries(Object.entries(after).filter(([key]) => key !== 'updated_at')) })
  } finally { db.close() }
}
writeFileSync(target, JSON.stringify({ formatVersion: 1, cases }) + '\n', { flag: 'wx', mode: 0o600 })
hooks.deregister()
console.log(`Generated ${cases.length} original project-core contracts from synthetic SQLite`)
