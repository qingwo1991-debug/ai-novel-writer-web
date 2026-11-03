import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'

import { GLOBAL_OBJECTS, runGlobalDataMigration } from '../../electron/services/global-data-migration.ts'

// The original TypeScript S03 supplies durable-boundary fixtures. All roots and
// bytes are synthetic and confined to disposable siblings beneath /tmp.
assert.equal(process.env.GITHUB_ACTIONS, 'true')
assert.equal(process.platform, 'linux')
const output = process.env.GO_GLOBAL_INSTALL_CONTRACT
const runnerTemp = process.env.RUNNER_TEMP
assert(output && runnerTemp && path.isAbsolute(output) && path.isAbsolute(runnerTemp))
const relativeOutput = path.relative(runnerTemp, output)
assert(relativeOutput && relativeOutput !== '..' && !relativeOutput.startsWith(`..${path.sep}`) && !path.isAbsolute(relativeOutput))
fs.mkdirSync(path.dirname(output), { recursive: true, mode: 0o700 })

const caseRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'novel-go-global-install-'))
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
  put(root, 'skills/author/SKILL.md', '# Synthetic skill\n')
  put(root, 'skins/manifest.json', { version: 1 })
}

function setupEmpty(options) {
  put(options.userData, 'sentinel.txt', 'Synthetic user data retained')
}

function setupLegacy(options) {
  setupEmpty(options)
  populate(options.legacySource)
  put(options.legacySource, 'logs/retained.txt', 'Synthetic retained log')
  put(options.legacySource, 'metadata/retained.json', { state: 'retained' })
  put(options.legacySource, 'unknown-tool/retained.txt', 'Synthetic unknown bytes')
  put(options.canonicalTarget, 'logs/target-retained.txt', 'Synthetic target log')
}

function setupCanonical(options) {
  setupEmpty(options)
  populate(options.canonicalTarget)
  put(options.canonicalTarget, 'logs/retained.txt', 'Synthetic retained log')
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

function selectedInventory(root) {
  return inventory(root).filter(item => GLOBAL_OBJECTS.includes(item.path.split('/')[0]))
}

function journalAt(options) {
  const raw = fs.readFileSync(path.join(options.canonicalTarget, '.migration', 'journal.json'), 'utf8')
  const value = JSON.parse(raw)
  assert.deepEqual(Object.keys(value), ['version', 'generation', 'phase', 'source'])
  assert.equal(value.version, 1)
  assert.equal(value.phase, 'verified')
  assert.match(value.generation, /^[a-f0-9-]{36}$/u)
  assert(['empty', 'legacy', 'canonical'].includes(value.source))
  return { raw, value }
}

function retainedCount(options) {
  const count = root => fs.existsSync(root) ? fs.readdirSync(root)
    .filter(name => !GLOBAL_OBJECTS.includes(name) && !['.migration', 'generations'].includes(name)).length : 0
  return count(options.legacySource) + count(options.canonicalTarget)
}

function copyVerifiedState(from, to, journalRaw, generation) {
  const control = path.join(to.canonicalTarget, '.migration')
  const generations = path.join(to.canonicalTarget, 'generations')
  fs.mkdirSync(control, { mode: 0o700 })
  fs.mkdirSync(generations, { mode: 0o700 })
  fs.writeFileSync(path.join(control, 'journal.json'), journalRaw, { flag: 'wx', mode: 0o600 })
  const stagingName = `${generation}.staging`
  fs.cpSync(path.join(from.canonicalTarget, 'generations', stagingName), path.join(generations, stagingName),
    { recursive: true, errorOnExist: true, force: false })
}

function makeCase(name, setup, source, checkpoint, changeSourceAfterReceipt = false) {
  const original = roots(name, 'original')
  const go = roots(name, 'go')
  setup(original)
  setup(go)
  const before = { legacy: inventory(original.legacySource), canonical: inventory(original.canonicalTarget),
    userData: inventory(original.userData) }
  const sourceBefore = selectedInventory(source === 'legacy' ? original.legacySource : original.canonicalTarget)
  assert.deepEqual(inventory(go.legacySource), before.legacy)
  assert.deepEqual(inventory(go.canonicalTarget), before.canonical)
  assert.deepEqual(inventory(go.userData), before.userData)

  const verifiedResult = runGlobalDataMigration({ ...original, checkpoint: step => {
    if (step === 'verified') throw new Error('synthetic verified interruption')
  } })
  assert.deepEqual(verifiedResult, { state: 'blocked', code: 'GLOBAL_MIGRATION_IO_FAILED' })
  const { raw: verifiedJournalRaw, value: verifiedJournal } = journalAt(original)
  assert.equal(verifiedJournal.source, source)
  const generation = verifiedJournal.generation
  const generations = path.join(original.canonicalTarget, 'generations')
  const staging = path.join(generations, `${generation}.staging`)
  const installed = path.join(generations, generation)
  const receiptFile = path.join(original.canonicalTarget, '.migration', 'receipt.json')
  const stagedFiles = inventory(staging)
  assert(!fs.existsSync(installed))
  assert(!fs.existsSync(receiptFile))
  copyVerifiedState(original, go, verifiedJournalRaw, generation)
  const seedFiles = { legacy: inventory(go.legacySource), canonical: inventory(go.canonicalTarget),
    userData: inventory(go.userData) }
  assert.deepEqual(inventory(path.join(go.canonicalTarget, 'generations', `${generation}.staging`)), stagedFiles)

  let result = verifiedResult
  if (checkpoint !== 'verified') {
    result = runGlobalDataMigration({ ...original, checkpoint: step => {
      if (step === checkpoint) throw new Error(`synthetic ${checkpoint} interruption`)
    } })
    assert.deepEqual(result, { state: 'blocked', code: 'GLOBAL_MIGRATION_IO_FAILED' })
  }
  const journal = journalAt(original)
  assert.equal(journal.raw, verifiedJournalRaw)
  assert.equal(journal.value.generation, generation)
  const expectedStaging = checkpoint === 'verified' ? stagedFiles : []
  const expectedInstalled = checkpoint === 'verified' ? [] : stagedFiles
  assert.deepEqual(inventory(staging), expectedStaging)
  assert.deepEqual(inventory(installed), expectedInstalled)
  const receiptRaw = fs.existsSync(receiptFile) ? fs.readFileSync(receiptFile, 'utf8') : null
  const receipt = receiptRaw === null ? null : JSON.parse(receiptRaw)
  assert.equal(receipt !== null, checkpoint === 'receipt')
  if (receipt) {
    assert.deepEqual(Object.keys(receipt), ['version', 'generation', 'completed', 'preservedUnknownCount', 'requiredObjects'])
    assert.equal(receipt.version, 1)
    assert.equal(receipt.generation, generation)
    assert.equal(receipt.completed, true)
    assert.equal(receipt.preservedUnknownCount, retainedCount(original))
    assert.deepEqual(receipt.requiredObjects, GLOBAL_OBJECTS.filter(object => fs.existsSync(path.join(installed, object))))
  }
  assert.deepEqual(inventory(original.legacySource), before.legacy)
  assert.deepEqual(inventory(original.userData), before.userData)
  assert.deepEqual(selectedInventory(source === 'legacy' ? original.legacySource : original.canonicalTarget), sourceBefore)

  const resumed = runGlobalDataMigration(original)
  assert.equal(resumed.state, 'ready')
  assert.equal(resumed.globalGeneration, generation)
  assert.deepEqual(inventory(installed), stagedFiles)
  assert.deepEqual(inventory(original.legacySource), before.legacy)
  assert.deepEqual(inventory(original.userData), before.userData)
  assert.deepEqual(selectedInventory(source === 'legacy' ? original.legacySource : original.canonicalTarget), sourceBefore)
  const finalReceiptRaw = fs.readFileSync(receiptFile, 'utf8')
  const finalReceipt = JSON.parse(finalReceiptRaw)
  assert.equal(finalReceipt.generation, generation)
  assert.equal(finalReceipt.preservedUnknownCount, retainedCount(original))
  assert.deepEqual(finalReceipt.requiredObjects, GLOBAL_OBJECTS.filter(object => fs.existsSync(path.join(installed, object))))
  if (receiptRaw !== null) assert.equal(finalReceiptRaw, receiptRaw)

  let afterSourceChange = null
  if (changeSourceAfterReceipt) {
    assert.equal(source, 'legacy')
    fs.writeFileSync(path.join(original.legacySource, 'config.json'), JSON.stringify({ defaultModelId: 'changed-after-receipt' }))
    afterSourceChange = runGlobalDataMigration(original)
    assert.equal(afterSourceChange.state, 'ready')
    assert.equal(afterSourceChange.globalGeneration, generation)
    assert.equal(fs.readFileSync(receiptFile, 'utf8'), finalReceiptRaw)
    assert.deepEqual(inventory(installed), stagedFiles)
    assert.deepEqual(inventory(original.userData), before.userData)
  }
  return { name, goRoots: { legacySource: go.legacySource, canonicalTarget: go.canonicalTarget, userData: go.userData },
    seedFiles, oracle: { checkpoint, result, verifiedJournalRaw, verifiedJournal, generation, source,
      stagedFiles, expectedStaging, expectedInstalled, receiptRaw, receipt, resumed,
      finalReceiptRaw, finalReceipt, preservedUnknownCount: retainedCount(original), afterSourceChange,
      sourceBefore, userDataBefore: before.userData } }
}

const cases = [
  makeCase('empty-verified', setupEmpty, 'empty', 'verified'),
  makeCase('legacy-installed', setupLegacy, 'legacy', 'installed'),
  makeCase('canonical-installed', setupCanonical, 'canonical', 'installed'),
  makeCase('legacy-receipt', setupLegacy, 'legacy', 'receipt'),
  makeCase('legacy-receipt-source-changed', setupLegacy, 'legacy', 'receipt', true),
]
assert.deepEqual(cases.map(item => item.oracle.checkpoint), ['verified', 'installed', 'installed', 'receipt', 'receipt'])
assert.deepEqual(cases.map(item => item.oracle.preservedUnknownCount), [0, 4, 1, 4, 4])
fs.writeFileSync(output, `${JSON.stringify({ formatVersion: 1, cases })}\n`, { flag: 'wx', mode: 0o600 })
console.log(JSON.stringify({ formatVersion: 1, cases: cases.map(item => ({ name: item.name,
  checkpoint: item.oracle.checkpoint, source: item.oracle.source, generation: item.oracle.generation,
  stagedFiles: item.oracle.stagedFiles.length, receipt: item.oracle.receipt !== null,
  unknown: item.oracle.preservedUnknownCount, resumed: item.oracle.resumed.state,
  sourceChangedReady: item.oracle.afterSourceChange?.state ?? null })) }))
