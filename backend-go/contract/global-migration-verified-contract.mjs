import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'

import { runGlobalDataMigration } from '../../electron/services/global-data-migration.ts'

// Original TypeScript S03 owns the durable boundary oracle. Every input here
// is synthetic and confined to disposable siblings beneath /tmp.
assert.equal(process.env.GITHUB_ACTIONS, 'true')
assert.equal(process.platform, 'linux')
const output = process.env.GO_GLOBAL_VERIFIED_CONTRACT
const runnerTemp = process.env.RUNNER_TEMP
assert(output && runnerTemp && path.isAbsolute(output) && path.isAbsolute(runnerTemp))
const relativeOutput = path.relative(runnerTemp, output)
assert(relativeOutput && relativeOutput !== '..' && !relativeOutput.startsWith(`..${path.sep}`) && !path.isAbsolute(relativeOutput))
fs.mkdirSync(path.dirname(output), { recursive: true, mode: 0o700 })

const caseRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'novel-go-global-verified-'))
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

function populate(root) {
  put(root, 'models.json', [{ id: 'synthetic-model', name: 'Synthetic model', provider: 'openai', apiKey: 'synthetic-key' }])
  put(root, 'config.json', { defaultModelId: 'synthetic-model' })
  put(root, 'recent-projects.json', [])
  put(root, 'mcp_config.json', { mcpServers: {} })
  put(root, 'prompts/author.json', { content: 'Synthetic author prompt' })
  put(root, 'prompts/nested/more.json', { content: 'Synthetic nested prompt' })
  put(root, 'skills/author/SKILL.md', '# Synthetic skill\n')
  put(root, 'skins/manifest.json', { version: 1 })
  put(root, 'logs/retained.txt', 'Synthetic retained file')
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

function journalAt(options, phase) {
  const file = path.join(options.canonicalTarget, '.migration', 'journal.json')
  const journalRaw = fs.readFileSync(file, 'utf8')
  const journal = JSON.parse(journalRaw)
  assert.equal(journal.version, 1)
  assert.equal(journal.phase, phase)
  assert.match(journal.generation, /^[a-f0-9-]{36}$/u)
  assert(['empty', 'legacy', 'canonical'].includes(journal.source))
  return { journalRaw, journal }
}

function copyPreparedState(from, to, journalRaw) {
  const control = path.join(to.canonicalTarget, '.migration')
  fs.mkdirSync(control, { mode: 0o700 })
  fs.mkdirSync(path.join(to.canonicalTarget, 'generations'), { mode: 0o700 })
  fs.writeFileSync(path.join(control, 'journal.json'), journalRaw, { flag: 'wx', mode: 0o600 })
  assert.deepEqual(fs.readdirSync(path.join(from.canonicalTarget, 'generations')), [])
}

function makeCase(name, setup, expectedSource, checkpointStep) {
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

  const prepared = runGlobalDataMigration({ ...originalRoots, checkpoint: step => {
    if (step === 'prepared') throw new Error('synthetic prepared interruption')
  } })
  assert.deepEqual(prepared, { state: 'blocked', code: 'GLOBAL_MIGRATION_IO_FAILED' })
  const { journalRaw: preparedJournalRaw, journal: preparedJournal } = journalAt(originalRoots, 'prepared')
  assert.equal(preparedJournal.source, expectedSource)
  copyPreparedState(originalRoots, goRoots, preparedJournalRaw)

  const checkpointResult = runGlobalDataMigration({ ...originalRoots, checkpoint: step => {
    if (step === checkpointStep) throw new Error(`synthetic ${checkpointStep} interruption`)
  } })
  assert.deepEqual(checkpointResult, { state: 'blocked', code: 'GLOBAL_MIGRATION_IO_FAILED' })
  const expectedPhase = checkpointStep === 'verified' ? 'verified' : 'prepared'
  const { journalRaw, journal } = journalAt(originalRoots, expectedPhase)
  assert.equal(journal.generation, preparedJournal.generation)
  const staging = path.join(originalRoots.canonicalTarget, 'generations', `${journal.generation}.staging`)
  assert(fs.statSync(staging).isDirectory())
  const stagingFiles = inventory(staging)
  assert(!fs.existsSync(path.join(originalRoots.canonicalTarget, 'generations', journal.generation)))
  assert(!fs.existsSync(path.join(originalRoots.canonicalTarget, '.migration', 'receipt.json')))
  assert.deepEqual(inventory(originalRoots.legacySource), before.legacy)
  assert.deepEqual(inventory(originalRoots.userData), before.userData)

  if (checkpointStep.startsWith('object:')) {
    fs.cpSync(staging, path.join(goRoots.canonicalTarget, 'generations', `${journal.generation}.staging`), { recursive: true, errorOnExist: true, force: false })
    assert.deepEqual(inventory(path.join(goRoots.canonicalTarget, 'generations', `${journal.generation}.staging`)), stagingFiles)
  }
  const seedFiles = { legacy: inventory(goRoots.legacySource), canonical: inventory(goRoots.canonicalTarget),
    userData: inventory(goRoots.userData) }
  const restarted = runGlobalDataMigration(originalRoots)
  assert.equal(restarted.state, 'ready')
  assert.equal(restarted.globalGeneration, journal.generation)
  assert.deepEqual(inventory(originalRoots.legacySource), before.legacy)
  assert.deepEqual(inventory(originalRoots.userData), before.userData)
  assert.deepEqual(inventory(path.join(originalRoots.canonicalTarget, 'generations', journal.generation)),
    checkpointStep === 'verified' ? stagingFiles : inventory(expectedSource === 'legacy' ? originalRoots.legacySource : originalRoots.canonicalTarget)
      .filter(item => !item.path.startsWith('logs/') && !item.path.startsWith('.migration/') && !item.path.startsWith('generations/')))
  return {
    name,
    goRoots: { legacySource: goRoots.legacySource, canonicalTarget: goRoots.canonicalTarget, userData: goRoots.userData },
    oracle: { checkpointStep, checkpointResult, preparedJournalRaw, preparedJournal, journalRaw, journal,
      stagingFiles, sourceFiles: inventory(expectedSource === 'legacy' ? originalRoots.legacySource : originalRoots.canonicalTarget), restarted },
    seedFiles,
  }
}

const sentinel = options => put(options.userData, 'sentinel.txt', 'Synthetic user data retained')
const cases = [
  makeCase('empty-verified', sentinel, 'empty', 'verified'),
  makeCase('legacy-verified', options => { sentinel(options); populate(options.legacySource) }, 'legacy', 'verified'),
  makeCase('canonical-verified', options => { sentinel(options); populate(options.canonicalTarget) }, 'canonical', 'verified'),
  makeCase('legacy-object-interrupted', options => { sentinel(options); populate(options.legacySource) }, 'legacy', 'object:config.json'),
]
assert.deepEqual(cases.map(item => item.oracle.journal.phase), ['verified', 'verified', 'verified', 'prepared'])
assert.deepEqual(cases.map(item => item.oracle.stagingFiles.length), [0, 8, 8, 1])
fs.writeFileSync(output, JSON.stringify({ formatVersion: 1, cases }) + '\n', { flag: 'wx', mode: 0o600 })
console.log(JSON.stringify({ formatVersion: 1, cases: cases.map(item => ({ name: item.name,
  checkpointStep: item.oracle.checkpointStep, code: item.oracle.checkpointResult.code,
  phase: item.oracle.journal.phase, source: item.oracle.journal.source,
  stagedFiles: item.oracle.stagingFiles.length, restarted: item.oracle.restarted.state })) }))
