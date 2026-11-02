import assert from 'node:assert/strict'
import { mkdirSync, readFileSync, writeFileSync, chmodSync } from 'node:fs'
import path from 'node:path'
import { createServerSecretStorage } from '../../electron/platform/server-secret-storage.ts'

// Synthetic PUBLIC test key. Never reads application configuration or host keys.
assert.equal(process.env.GITHUB_ACTIONS, 'true')
const contractPath = process.env.GO_SECRET_CONTRACT
const base = process.env.RUNNER_TEMP
assert(contractPath && base && path.isAbsolute(contractPath))
const relative = path.relative(base, contractPath)
assert(relative && !relative.startsWith('..') && !path.isAbsolute(relative))
const directory = path.dirname(contractPath)
const key = Buffer.alloc(32, 0x47)
const keyFileName = 'public-synthetic-fixture.key'
const plain = ['', 'Go / Node 凭据兼容 😀 <>&\u2028\u2029\u0000', 'x'.repeat(65536)]
if (process.argv[2] === 'generate') {
  mkdirSync(directory, { recursive: true, mode: 0o700 })
  writeFileSync(path.join(directory, keyFileName), key, { flag: 'wx', mode: 0o600 })
  chmodSync(path.join(directory, keyFileName), 0o600)
  const storage = createServerSecretStorage({ rootPath: directory, keyFileName })
  const cases = plain.map(plaintext => ({ plaintext, encrypted: storage.encryptString(plaintext).toString('base64') }))
  writeFileSync(contractPath, JSON.stringify({ formatVersion: 1, publicTestKeyHex: key.toString('hex'), cases }) + '\n', { flag: 'wx', mode: 0o600 })
} else if (process.argv[2] === 'verify') {
  const storage = createServerSecretStorage({ rootPath: directory, keyFileName })
  const generated = JSON.parse(readFileSync(contractPath + '.go.json', 'utf8'))
  assert.equal(generated.length, plain.length)
  generated.forEach((entry, index) => {
    assert.equal(entry.plaintext, plain[index])
    assert.equal(storage.decryptString(Buffer.from(entry.encrypted, 'base64')), plain[index])
  })
  console.log('Original TypeScript decrypted all Go-generated synthetic envelopes')
} else {
  throw new Error('expected generate or verify')
}
