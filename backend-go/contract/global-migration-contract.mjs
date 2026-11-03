import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import fs from 'node:fs'
import path from 'node:path'

import { GLOBAL_OBJECTS, runGlobalDataMigration } from '../../electron/services/global-data-migration.ts'

// Original TypeScript migration oracle. All roots and bytes are disposable and
// synthetic; the Go binary never imports this script or an Electron module.
assert.equal(process.env.GITHUB_ACTIONS, 'true')
const output = process.env.GO_GLOBAL_MIGRATION_CONTRACT
const runnerTemp = process.env.RUNNER_TEMP
assert(output && runnerTemp && path.isAbsolute(output) && path.isAbsolute(runnerTemp))
const relativeOutput = path.relative(runnerTemp, output)
assert(relativeOutput && relativeOutput !== '..' && !relativeOutput.startsWith(`..${path.sep}`) && !path.isAbsolute(relativeOutput))
assert.equal(process.platform, 'linux')
assert.equal(typeof runGlobalDataMigration, 'function')

fs.mkdirSync(path.dirname(output), { recursive: true, mode: 0o700 })
const caseRoot = fs.mkdtempSync(path.join(path.dirname(output), 'case-'))
fs.chmodSync(caseRoot, 0o700)

function roots(name) {
  const root = path.join(caseRoot, name)
  fs.mkdirSync(root, { mode: 0o700 })
  const options = {
    legacySource: path.join(root, 'legacy'),
    canonicalTarget: path.join(root, 'canonical'),
    userData: path.join(root, 'user-data'),
    exclusiveAccess: true,
  }
  fs.mkdirSync(options.userData, { mode: 0o700 })
  fs.writeFileSync(path.join(options.userData, 'sentinel'), 'synthetic user data stays untouched', { flag: 'wx', mode: 0o600 })
  return options
}

function put(root, name, value) {
  const file = path.join(root, name)
  fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 })
  const bytes = typeof value === 'string' ? value : JSON.stringify(value)
  fs.writeFileSync(file, bytes, { flag: 'wx', mode: 0o600 })
}

function populateLegacy(root) {
  put(root, 'models.json', [{ id: 'model-a', name: 'Synthetic Model', provider: 'openai', modelName: 'synthetic-only' }])
  put(root, 'config.json', { defaultModelId: 'model-a', defaultEmbeddingModelId: null,
    updatePreferences: { lastAutomaticCheckDate: '2026-01-01' } })
  put(root, 'recent-projects.json', [{ name: 'Synthetic', path: '/synthetic/nonexistent/project' }])
  put(root, 'mcp_config.json', { mcpServers: { synthetic: { command: 'never-executed' } } })
  put(root, 'prompts/author.json', { key: 'author', content: 'Synthetic prompt 😀' })
  put(root, 'skills/author/SKILL.md', '# Synthetic skill\nNo execution')
  put(root, 'skins/manifest.json', { version: 1, activeSkin: 'synthetic' })
  put(root, 'logs/synthetic.log', 'retained synthetic log')
  put(root, 'metadata/synthetic.json', { state: 'retained' })
  put(root, 'update/synthetic.json', { state: 'retained' })
  put(root, 'unknown-tool/untouched.txt', 'retained unknown synthetic bytes')
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
  return found.sort((a, b) => a.path < b.path ? -1 : a.path > b.path ? 1 : 0)
}

function makeCase(name, setup, mutate) {
  const options = roots(name)
  setup?.(options)
  const first = runGlobalDataMigration(options)
  assert.equal(first.state, 'ready', `${name}: initial migration must commit`)
  mutate?.(first, options)
  const expected = runGlobalDataMigration(options) // Original completed-receipt reopen contract.
  const receiptFile = path.join(options.canonicalTarget, '.migration', 'receipt.json')
  const receiptRaw = fs.readFileSync(receiptFile, 'utf8')
  const receipt = JSON.parse(receiptRaw)
  assert.equal(receipt.version, 1)
  assert.equal(receipt.completed, true)
  assert(Array.isArray(receipt.requiredObjects))
  assert(receipt.requiredObjects.every(item => GLOBAL_OBJECTS.includes(item)))
  const generationRoot = path.join(options.canonicalTarget, 'generations', receipt.generation)
  return { name, legacySource: options.legacySource, canonicalTarget: options.canonicalTarget,
    userData: options.userData, expected, receiptRaw, requiredObjects: receipt.requiredObjects,
    generationFiles: inventory(generationRoot), legacyFiles: inventory(options.legacySource),
    userDataFiles: inventory(options.userData) }
}

const cases = [
  makeCase('empty-committed'),
  makeCase('legacy-committed', options => populateLegacy(options.legacySource)),
  makeCase('missing-required', options => populateLegacy(options.legacySource), first => {
    fs.unlinkSync(path.join(first.dataRoot, 'models.json'))
  }),
  makeCase('bad-model-ref', options => populateLegacy(options.legacySource), first => {
    fs.writeFileSync(path.join(first.dataRoot, 'config.json'), JSON.stringify({ defaultModelId: 'missing-model' }))
  }),
]
assert.deepEqual(cases.map(item => item.expected.state), ['ready', 'ready', 'blocked', 'blocked'])
assert.equal(cases[2].expected.code, 'GLOBAL_COMMITTED_OBJECT_MISSING')
assert.equal(cases[3].expected.code, 'GLOBAL_DEFAULT_MODEL_MISSING')
fs.writeFileSync(output, `${JSON.stringify({ formatVersion: 1, cases })}\n`, { flag: 'wx', mode: 0o600 })
console.log(JSON.stringify({ formatVersion: 1, cases: cases.map(item => ({ name: item.name, state: item.expected.state,
  code: item.expected.code ?? null, fileCount: item.generationFiles.length })) }))
