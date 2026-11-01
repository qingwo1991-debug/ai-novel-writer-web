import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawn, type ChildProcess } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import { fileURLToPath } from 'node:url'
import { performance } from 'node:perf_hooks'
import { buildSync } from 'esbuild'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { withCloudMetadataLock } from '../cloud-metadata-lock'
import { CloudCredentialStore } from '../cloud-credential-store'
import { CloudProjectBindingStore } from '../cloud-project-binding-store'

const roots: string[] = []
const children = new Set<ChildProcess>()
let bundleRoot: string
let workerFile: string
const id = (n: number) => `${n.toString(16).padStart(8, '0')}-1111-4111-8111-111111111111`
function root(): string {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'cloud-metadata-lock-'))
  roots.push(directory)
  return directory
}
function marker(file: string): string { return path.join(path.dirname(file), `.${path.basename(file)}.lock`) }

interface Result { result?: unknown; error?: string }
function start(directory: string, operation: string, input: unknown) {
  const child = spawn(process.execPath, [workerFile, directory, operation, JSON.stringify(input)], { stdio: ['pipe', 'pipe', 'pipe'] })
  children.add(child)
  let stdout = '', stderr = ''
  let readyResolve!: () => void, heldResolve!: () => void
  let readyReject!: (error: Error) => void, heldReject!: (error: Error) => void
  const ready = new Promise<void>((resolve, reject) => { readyResolve = resolve; readyReject = reject })
  const held = new Promise<void>((resolve, reject) => { heldResolve = resolve; heldReject = reject })
  // Some operations do not acquire a long-held lock; avoid unused rejection.
  void held.catch(() => {})
  child.stdout!.on('data', bytes => {
    stdout += bytes.toString()
    if (stdout.includes('READY\n')) readyResolve()
    if (stdout.includes('HELD\n')) heldResolve()
  })
  child.stderr!.on('data', bytes => { stderr += bytes.toString() })
  const done = new Promise<Result>((resolve, reject) => {
    child.once('error', error => { readyReject(error); heldReject(error); reject(error) })
    child.once('close', (code, signal) => {
      children.delete(child)
      const error = new Error(`Worker exit ${code}/${signal}: ${stderr}\n${stdout}`)
      if (!stdout.includes('READY\n')) readyReject(error)
      if (!stdout.includes('HELD\n')) heldReject(error)
      if (signal === 'SIGKILL') { resolve({ error: 'SIGKILL' }); return }
      const result = stdout.split('\n').find(line => line.startsWith('RESULT '))
      if (code !== 0 || !result) reject(error)
      else resolve(JSON.parse(result.slice(7)) as Result)
    })
  })
  return { child, ready, held, done, go: () => { child.stdin!.write('go\n') } }
}
async function together(directory: string, operation: string, inputs: unknown[]): Promise<Result[]> {
  const jobs = inputs.map(input => start(directory, operation, input))
  await Promise.all(jobs.map(job => job.ready))
  for (const job of jobs) job.go()
  return Promise.all(jobs.map(job => job.done))
}
const binding = (n: number, expectedRevision: number | null = null) => ({
  localProjectId: id(n), cloudBookId: `book-${n}`, localEndpointAccountId: 'account-a',
  lastSelectedParentGenerationIds: ['generation-a'], expectedRevision,
})
const credential = (n: number) => ({ accountId: id(n), endpoint: 'https://dav.example.test/books', username: `author-${n}`, secret: `secret-${n}` })

beforeAll(() => {
  bundleRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'cloud-metadata-test-worker-'))
  workerFile = path.join(bundleRoot, 'worker.cjs')
  buildSync({ entryPoints: [fileURLToPath(new URL('./fixtures/cloud-metadata-worker.ts', import.meta.url))],
    bundle: true, platform: 'node', format: 'cjs', outfile: workerFile, logLevel: 'silent' })
})
afterEach(async () => {
  await Promise.all([...children].map(child => new Promise<void>(resolve => {
    child.once('close', () => resolve()); child.kill('SIGKILL')
  })))
  for (const directory of roots.splice(0)) fs.rmSync(directory, { recursive: true, force: true })
})
afterAll(() => { fs.rmSync(bundleRoot, { recursive: true, force: true }) })

describe('withCloudMetadataLock', () => {
  it('releases on exception, rejects reentry, and leaves no lock after success', () => {
    const file = path.join(root(), 'counter.json')
    expect(() => withCloudMetadataLock(file, () => { throw new Error('transaction failed') })).toThrow('transaction failed')
    expect(fs.existsSync(marker(file))).toBe(false)
    withCloudMetadataLock(file, () => {
      expect(() => withCloudMetadataLock(file, () => 0)).toThrow('CLOUD_METADATA_LOCK_REENTRANT')
    })
    expect(withCloudMetadataLock(file, () => 42)).toBe(42)
    expect(fs.existsSync(marker(file))).toBe(false)
  })

  it('times out without age-stealing a live holder, then acquires after release', async () => {
    const directory = root(), file = path.join(directory, 'counter.json')
    const job = start(directory, 'hold', { milliseconds: 500 })
    await job.ready; job.go(); await job.held
    const owner = path.join(marker(file), fs.readdirSync(marker(file))[0]!)
    fs.utimesSync(owner, new Date(0), new Date(0))
    const started = performance.now()
    expect(() => withCloudMetadataLock(file, () => 0, { timeoutMs: 60 })).toThrow('CLOUD_METADATA_LOCK_TIMEOUT')
    expect(performance.now() - started).toBeLessThan(400)
    expect(fs.existsSync(owner)).toBe(true)
    expect(withCloudMetadataLock(file, () => 'acquired', { timeoutMs: 2_000 })).toBe('acquired')
    expect(await job.done).toEqual({ result: true })
  })

  it.skipIf(process.platform !== 'linux')('recovers a positively dead same-namespace owner, not merely an old lock', async () => {
    const directory = root(), file = path.join(directory, 'counter.json')
    const job = start(directory, 'hold', { milliseconds: 60_000 })
    await job.ready; job.go(); await job.held
    job.child.kill('SIGKILL'); await job.done
    const owner = path.join(marker(file), fs.readdirSync(marker(file))[0]!)
    // Death alone does not waive the minimum stale age either.
    fs.utimesSync(owner, new Date(), new Date())
    expect(() => withCloudMetadataLock(file, () => 0, { timeoutMs: 30 })).toThrow('CLOUD_METADATA_LOCK_TIMEOUT')
    fs.utimesSync(owner, new Date(0), new Date(0))
    const record = JSON.parse(fs.readFileSync(owner, 'utf8'))
    const original = JSON.stringify(record)
    record.linux.namespace = 'pid:[foreign-test-namespace]'
    fs.writeFileSync(owner, JSON.stringify(record)); fs.utimesSync(owner, new Date(0), new Date(0))
    expect(() => withCloudMetadataLock(file, () => 0, { timeoutMs: 30 })).toThrow('CLOUD_METADATA_LOCK_TIMEOUT')
    expect(fs.existsSync(owner)).toBe(true)
    fs.writeFileSync(owner, original); fs.utimesSync(owner, new Date(0), new Date(0))
    expect(withCloudMetadataLock(file, () => 'recovered', { timeoutMs: 500 })).toBe('recovered')
    expect(fs.existsSync(marker(file))).toBe(false)
  })

  it('fails closed for malformed/partial lock state and rejects a symlink lock', () => {
    const directory = root(), file = path.join(directory, 'counter.json')
    fs.mkdirSync(marker(file))
    expect(() => withCloudMetadataLock(file, () => 0, { timeoutMs: 30 })).toThrow('CLOUD_METADATA_LOCK_TIMEOUT')
    const owner = path.join(marker(file), `owner-${randomUUID()}.json`)
    fs.writeFileSync(owner, '{not json'); fs.utimesSync(owner, new Date(0), new Date(0))
    expect(() => withCloudMetadataLock(file, () => 0, { timeoutMs: 30 })).toThrow('CLOUD_METADATA_LOCK_TIMEOUT')
    expect(fs.readFileSync(owner, 'utf8')).toBe('{not json')
    fs.rmSync(marker(file), { recursive: true })
    if (process.platform !== 'win32') {
      fs.symlinkSync(directory, marker(file), 'dir')
      expect(() => withCloudMetadataLock(file, () => 0)).toThrow('CLOUD_METADATA_LOCK_INVALID')
    }
  })
})

describe('cross-process cloud document transactions', () => {
  it('preserves every concurrent credential save and independent clear', async () => {
    const directory = root()
    expect((await together(directory, 'credential-save', Array.from({ length: 8 }, (_, index) => credential(index + 1)))).every(result => !result.error)).toBe(true)
    const credentials = new CloudCredentialStore({ rootPath: directory, safeStorage: { isEncryptionAvailable: () => false, encryptString: value => Buffer.from(value), decryptString: value => value.toString() } })
    expect(credentials.list()).toHaveLength(8)
    const results = await together(directory, 'credential-clear', Array.from({ length: 4 }, (_, index) => ({ accountId: id(index + 1) })))
    expect(results).toEqual(Array.from({ length: 4 }, () => ({ result: true })))
    expect(credentials.list().map(item => item.accountId).sort()).toEqual([5, 6, 7, 8].map(id))
  }, 15_000)

  it('preserves concurrent different-project bindings and makes exactly one same-revision CAS win', async () => {
    const directory = root(), store = new CloudProjectBindingStore(directory)
    expect((await together(directory, 'binding-save', [1, 2, 3, 4, 5, 6].map(n => binding(n)))).every(result => !result.error)).toBe(true)
    expect(store.list()).toHaveLength(6)
    const results = await together(directory, 'binding-save', Array.from({ length: 5 }, (_, index) => ({ ...binding(1, 1), cloudBookId: `contender-${index}` })))
    expect(results.filter(result => !result.error)).toHaveLength(1)
    expect(results.filter(result => result.error === 'CLOUD_BINDING_REVISION_CONFLICT')).toHaveLength(4)
    expect(store.get(id(1))?.revision).toBe(2)
    expect(store.list()).toHaveLength(6)
  }, 15_000)

  it('serializes readonly creation, account invalidation, and deletion mutations too', async () => {
    const directory = root(), store = new CloudProjectBindingStore(directory)
    expect((await together(directory, 'binding-origin', [1, 2, 3, 4].map(n => ({ ...binding(n), localEndpointAccountId: `account-${n}` })))).every(result => !result.error)).toBe(true)
    expect(store.list()).toHaveLength(4)
    expect(await together(directory, 'binding-unconfigure', [1, 2, 3, 4].map(n => ({ accountId: `account-${n}` })))).toEqual([1, 2, 3, 4].map(() => ({ result: 1 })))
    expect(store.list().every(item => item.mode === 'unconfigured' && item.revision === 2)).toBe(true)
    expect(await together(directory, 'binding-remove', [1, 2, 3, 4].map(n => ({ projectId: id(n) })))).toEqual([1, 2, 3, 4].map(() => ({ result: true })))
    expect(store.list()).toEqual([])
  }, 15_000)
})
