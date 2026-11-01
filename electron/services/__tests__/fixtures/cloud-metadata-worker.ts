import fs from 'node:fs'
import path from 'node:path'

import { CloudCredentialStore } from '../../cloud-credential-store'
import { CloudProjectBindingStore } from '../../cloud-project-binding-store'
import { withCloudMetadataLock } from '../../cloud-metadata-lock'

const [root, operation, encoded] = process.argv.slice(2)
const input = JSON.parse(encoded!)
const pause = () => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 45)
const credentials = new CloudCredentialStore({ rootPath: root!, safeStorage: {
  persistence: 'server-encrypted',
  isEncryptionAvailable: () => true,
  encryptString(value) { pause(); return Buffer.from(value) },
  decryptString: value => value.toString(),
} })
const bindings = new CloudProjectBindingStore(root!)
// Widen the read/write race deterministically. Without the store transaction
// lock, simultaneous children read the same old bytes and lose updates/CAS.
const readFileSync = fs.readFileSync.bind(fs)
fs.readFileSync = ((...args: Parameters<typeof fs.readFileSync>) => {
  const bytes = readFileSync(...args)
  if (typeof args[0] === 'string' && /cloud-backup-(?:bindings|credentials)\.json$/u.test(args[0])) pause()
  return bytes
}) as typeof fs.readFileSync
process.stdout.write('READY\n')
process.stdin.once('data', () => {
  try {
    let result: unknown
    switch (operation) {
      case 'credential-save': result = credentials.save(input); break
      case 'credential-clear': result = credentials.clear(input.accountId); break
      case 'binding-save': result = bindings.saveWritable(input); break
      case 'binding-origin': result = bindings.saveOriginReadonly(input); break
      case 'binding-unconfigure': result = bindings.markAccountUnconfigured(input.accountId); break
      case 'binding-remove': result = bindings.removeDeletedProject(input.projectId); break
      case 'hold':
        result = withCloudMetadataLock(path.join(root!, 'counter.json'), () => {
          fs.writeFileSync(path.join(root!, 'held'), 'held')
          process.stdout.write('HELD\n')
          Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, input.milliseconds)
          return true
        })
        break
      default: throw new Error('UNKNOWN_TEST_OPERATION')
    }
    process.stdout.write(`RESULT ${JSON.stringify({ result })}\n`)
  } catch (error) {
    process.stdout.write(`RESULT ${JSON.stringify({ error: error instanceof Error ? error.message : 'unknown' })}\n`)
  }
  process.stdin.destroy()
})
