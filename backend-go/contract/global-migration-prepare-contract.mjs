import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'

import { runGlobalDataMigration } from '../../electron/services/global-data-migration.ts'

// The original TypeScript S03 state machine supplies checkpoint and error
// oracles. All project-like files below are synthetic and live under /tmp.
assert.equal(process.env.GITHUB_ACTIONS, 'true')
assert.equal(process.platform, 'linux')
const output = process.env.GO_GLOBAL_PREPARE_CONTRACT
const runnerTemp = process.env.RUNNER_TEMP
assert(output && runnerTemp && path.isAbsolute(output) && path.isAbsolute(runnerTemp))
const outputRelative = path.relative(runnerTemp, output)
assert(outputRelative && outputRelative !== '..' && !outputRelative.startsWith(`..${path.sep}`) && !path.isAbsolute(outputRelative))
fs.mkdirSync(path.dirname(output), { recursive: true, mode: 0o700 })

const caseRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'novel-go-global-prepare-'))
fs.chmodSync(caseRoot, 0o700)

function roots(name, lane) {
  const parent = path.join(caseRoot, name, lane)
  fs.mkdirSync(parent, { recursive: true, mode: 0o700 })
  fs.chmodSync(parent, 0o700)
  const canonicalTarget = path.join(parent, 'canonical')
  const userData = path.join(parent, 'user-data')
  fs.mkdirSync(canonicalTarget, { mode: 0o700 })
  fs.mkdirSync(userData, { mode: 0o700 })
  return { legacySource: path.join(parent, 'legacy'), canonicalTarget, userData, exclusiveAccess: true }
}

function put(root, relative, value) {
  const file = path.join(root, relative)
  fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 })
  fs.writeFileSync(file, typeof value === 'string' ? value : JSON.stringify(value), { flag: 'wx', mode: 0o600 })
}

function populateLegacy(root, key = 'synthetic-key-a') {
  put(root, 'models.json', [{ id: 'model-a', name: 'Synthetic model', provider: 'openai', apiKey: key }])
  put(root, 'config.json', { defaultModelId: 'model-a' })
  put(root, 'recent-projects.json', [])
  put(root, 'mcp_config.json', { mcpServers: {} })
  put(root, 'prompts/author.json', { content: 'Synthetic prompt' })
  put(root, 'skills/author/SKILL.md', '# Synthetic skill\n')
  put(root, 'skins/manifest.json', { version: 1 })
  put(root, 'logs/retained.txt', 'Synthetic non-migration file')
}

function inventory(root) {
  if (!fs.existsSync(root)) return []
  const found = []
  function walk(directory) {
    for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
      const file = path.join(directory, entry.name)
      assert(!entry.isSymbolicLink())
      if (entry.isDirectory()) walk(file)
      else {
        assert(entry.isFile())
        const bytes = fs.readFileSync(file)
        found.push({ path: path.relative(root, file).split(path.sep).join('/'),
          sha256: createHash('sha256').update(bytes).digest('hex'), size: bytes.length })
      }
    }
  }
  walk(root)
  return found.sort((a, b) => a.path.localeCompare(b.path, 'en'))
}

function journalAt(options) {
  const file = path.join(options.canonicalTarget, '.migration', 'journal.json')
  if (!fs.existsSync(file)) return { journalRaw: null, journal: null }
  const journalRaw = fs.readFileSync(file, 'utf8')
  const journal = JSON.parse(journalRaw)
  assert.equal(journal.version, 1)
  assert.equal(journal.phase, 'prepared')
  assert.match(journal.generation, /^[a-f0-9-]{36}$/u)
  return { journalRaw, journal }
}

function makeCase(name, setup, restart = false) {
  const originalRoots = roots(name, 'original')
  const goRoots = roots(name, 'go')
  setup(originalRoots)
  setup(goRoots)
  const before = {
    legacy: inventory(originalRoots.legacySource),
    canonical: inventory(originalRoots.canonicalTarget),
    userData: inventory(originalRoots.userData),
  }
  assert.deepEqual(inventory(goRoots.legacySource), before.legacy)
  assert.deepEqual(inventory(goRoots.canonicalTarget), before.canonical)
  assert.deepEqual(inventory(goRoots.userData), before.userData)
  const result = runGlobalDataMigration({ ...originalRoots, checkpoint: step => {
    if (step === 'prepared') throw new Error('synthetic prepared interruption')
  } })
  const { journalRaw, journal } = journalAt(originalRoots)
  if (journal) {
    const generations = path.join(originalRoots.canonicalTarget, 'generations')
    assert(fs.existsSync(generations))
    assert.deepEqual(fs.readdirSync(generations), [])
    assert(!fs.existsSync(path.join(originalRoots.canonicalTarget, '.migration', 'receipt.json')))
  }
  assert.deepEqual(inventory(originalRoots.legacySource), before.legacy)
  assert.deepEqual(inventory(originalRoots.userData), before.userData)
  let restarted
  if (restart) {
    assert(journalRaw)
    const control = path.join(goRoots.canonicalTarget, '.migration')
    fs.mkdirSync(control, { mode: 0o700 })
    fs.mkdirSync(path.join(goRoots.canonicalTarget, 'generations'), { mode: 0o700 })
    fs.writeFileSync(path.join(control, 'journal.json'), journalRaw, { flag: 'wx', mode: 0o600 })
    restarted = runGlobalDataMigration(originalRoots)
    assert.equal(restarted.state, 'ready')
    assert.equal(restarted.globalGeneration, journal.generation)
    assert.deepEqual(inventory(originalRoots.legacySource), before.legacy)
    assert.deepEqual(inventory(originalRoots.userData), before.userData)
  }
  return {
    name,
    goRoots: { legacySource: goRoots.legacySource, canonicalTarget: goRoots.canonicalTarget, userData: goRoots.userData },
    oracle: { ...result, journalRaw, journal, ...(restarted ? { restarted } : {}) },
    seedFiles: { legacy: inventory(goRoots.legacySource), canonical: inventory(goRoots.canonicalTarget),
      userData: inventory(goRoots.userData) },
  }
}

const sentinel = options => put(options.userData, 'sentinel.txt', 'Synthetic user data retained')
const cases = [
  makeCase('empty-prepared', sentinel),
  makeCase('legacy-prepared', options => { sentinel(options); populateLegacy(options.legacySource) }),
  makeCase('dual-root-conflict', options => {
    sentinel(options)
    populateLegacy(options.legacySource, 'synthetic-key-a')
    populateLegacy(options.canonicalTarget, 'synthetic-key-b')
  }),
  makeCase('unknown-partial', options => {
    sentinel(options)
    fs.mkdirSync(path.join(options.canonicalTarget, 'generations'), { mode: 0o700 })
  }),
  makeCase('legacy-prepared-restart', options => { sentinel(options); populateLegacy(options.legacySource) }, true),
]
assert.deepEqual(cases.map(item => item.oracle.code), [
  'GLOBAL_MIGRATION_IO_FAILED', 'GLOBAL_MIGRATION_IO_FAILED',
  'GLOBAL_MODEL_CREDENTIALS_DIFFER', 'GLOBAL_PARTIAL_TARGET_UNKNOWN',
  'GLOBAL_MIGRATION_IO_FAILED',
])
assert.deepEqual(cases.map(item => item.oracle.journal?.source ?? null), [
  'empty', 'legacy', null, null, 'legacy',
])
fs.writeFileSync(output, JSON.stringify({ formatVersion: 1, cases }) + '\n', { flag: 'wx', mode: 0o600 })
console.log(JSON.stringify({ formatVersion: 1, cases: cases.map(item => ({ name: item.name,
  code: item.oracle.code, journalSource: item.oracle.journal?.source ?? null })) }))
