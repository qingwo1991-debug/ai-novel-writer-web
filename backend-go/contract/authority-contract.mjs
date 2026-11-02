import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { readFileSync, writeFileSync } from 'node:fs'
import { stripTypeScriptTypes } from 'node:module'
import path from 'node:path'
import { runInNewContext } from 'node:vm'

// CI-only original-source oracle. Do not import restore/transfer modules: their
// transitive dependencies load SQLite/vector runtimes. Extract only literal
// constants and the two original pure path functions; no author tree is read.
assert.equal(process.env.GITHUB_ACTIONS, 'true')
assert.equal(process.platform, 'linux')
const destination = process.env.GO_AUTHORITY_CONTRACT
const base = process.env.RUNNER_TEMP
assert(destination && base && path.isAbsolute(destination) && path.isAbsolute(base))
const relative = path.relative(base, destination)
assert(relative && relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative))
const source = name => readFileSync(new URL(`../../${name}`, import.meta.url), 'utf8')
const freeze = source('electron/services/portable-runtime-freeze.ts')
const transfer = source('electron/services/portable-transfer-authority.ts')
const restore = source('electron/services/project-restore-service.ts')
const current = source('electron/services/portable-current-authority.ts')
const shared = source('src/shared/project-format.ts')

function literal(text, name) {
  const pattern = new RegExp(`^(?:export )?const ${name} = '([^'\\r\\n]+)'`, 'gm')
  const matches = [...text.matchAll(pattern)]
  assert.equal(matches.length, 1, `original constant missing or ambiguous: ${name}`)
  return matches[0][1]
}

function pureFunction(text, name) {
  const start = text.indexOf(`\nfunction ${name}(`)
  assert(start >= 0, `original function missing: ${name}`)
  const end = text.indexOf('\n}', start + 1)
  assert(end > start, `original function terminator missing: ${name}`)
  return text.slice(start + 1, end + 2)
}

const storageDirectory = literal(shared, 'CANONICAL_PROJECT_DIRECTORY')
const runtimeFreezeName = literal(freeze, 'FREEZE_FILE')
const transferAuthorityName = literal(freeze, 'AUTHORITY_FILE')
const transferAuthorityPath = literal(transfer, 'PORTABLE_TRANSFER_AUTHORITY_PATH')
const reservationName = literal(restore, 'INSTALL_MARKER_NAME')
assert.equal(transferAuthorityName, literal(current, 'AUTHORITY_FILE'))
assert.equal(runtimeFreezeName, literal(restore, 'FREEZE_ARCHIVE_PATH'))
assert.equal(transferAuthorityPath, `${storageDirectory}/${transferAuthorityName}`)
const originalFunctions = [pureFunction(restore, 'normalized'), pureFunction(restore, 'installJournalPath')].join('\n')
const javascript = stripTypeScriptTypes(originalFunctions, { mode: 'transform' })
const originalJournalPath = runInNewContext(`${javascript}\ninstallJournalPath`, {
  path,
  createHash,
  process: Object.freeze({ platform: 'linux' }),
}, { timeout: 1000 })
const projectPaths = [
  '/synthetic-library/Synthetic project',
  '/synthetic-library/中文项目 😀',
  '/synthetic-library/MiXeD.Case-@_+',
  '/synthetic-library/dot.name',
]
const journalCases = projectPaths.map(projectPath => {
  const journalPath = originalJournalPath(projectPath)
  assert.equal(path.dirname(journalPath), path.dirname(projectPath))
  return { projectPath, journalName: path.basename(journalPath) }
})
writeFileSync(destination, JSON.stringify({
  formatVersion: 1,
  storageDirectory,
  runtimeFreezeName,
  transferAuthorityName,
  transferAuthorityPath,
  reservationName,
  journalCases,
}) + '\n', { flag: 'wx', mode: 0o600 })
console.log('Original TypeScript constants and pure restore-journal function emitted synthetic authority contract')
