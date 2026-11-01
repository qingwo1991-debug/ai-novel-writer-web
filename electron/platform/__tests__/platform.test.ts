import { afterEach, describe, expect, it, vi } from 'vitest'
import { EventEmitter } from 'node:events'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import {
  configurePlatform, currentInvokeEvent, dialogHost, eventsFor, navigationHost,
  OperationRegistry, type ClientSink, type InvokeEvent,
} from '../index'
import { createServerSecretStorage, loadOrCreatePrivateKey } from '../server-secret-storage'
import { loadApplicationImportSourceSecret } from '../../services/import-source-identity-secret'
import { CloudCredentialStore } from '../../services/cloud-credential-store'
import { compressAvatarImage, configureAvatarCompressor } from '../../services/avatar-image'

const temporary: string[] = []
function directory(): string { const root = fs.mkdtempSync(path.join(os.tmpdir(), 'anw-platform-')); temporary.push(root); return root }
class Client extends EventEmitter implements ClientSink {
  private destroyed = false
  readonly send = vi.fn()
  constructor(readonly id: number) { super() }
  isDestroyed(): boolean { return this.destroyed }
  destroy(): void { if (!this.destroyed) { this.destroyed = true; this.emit('destroyed') } }
}
afterEach(() => { configureAvatarCompressor(undefined); for (const root of temporary.splice(0)) fs.rmSync(root, { recursive: true, force: true }) })

describe('neutral operation and event platform', () => {
  it('dispatches registered business handlers with caller identity and preserves results', async () => {
    const registry = new OperationRegistry(), sender = new Client(12)
    registry.handle('novel:count', (event, left: number, right: number) => {
      expect(eventsFor(event)).toBe(sender)
      return { count: left + right }
    })
    expect(registry.has('novel:count')).toBe(true)
    expect(registry.list()).toEqual(['novel:count'])
    expect(await registry.invoke('novel:count', { sender }, 2, 3)).toEqual({ count: 5 })
    expect(() => registry.handle('novel:count', () => 0)).toThrow('OPERATION_ALREADY_REGISTERED')
    registry.removeHandler('novel:count')
    await expect(registry.invoke('novel:count', { sender })).rejects.toThrow('OPERATION_NOT_REGISTERED')
  })
  it('rejects disconnected clients without running a handler', async () => {
    const registry = new OperationRegistry(), sender = new Client(13), handler = vi.fn()
    registry.handle('write', handler)
    sender.destroy()
    await expect(registry.invoke('write', { sender })).rejects.toThrow('CLIENT_DISCONNECTED')
    expect(handler).not.toHaveBeenCalled()
  })
  it('keeps async dialog and navigation replies bound to concurrent invoking clients', async () => {
    const registry = new OperationRegistry(), first = new Client(14), second = new Client(15)
    const seen: number[] = []
    configurePlatform({ app: { getLocale: () => 'en-US', getPath: () => directory() }, dialogs: {
      async showOpenDialog(_options, event) { seen.push(event!.sender.id); return { canceled: false, filePaths: [`/client/${event!.sender.id}`] } },
      async showSaveDialog(_options, event) { return { canceled: false, filePath: `/save/${event!.sender.id}` } },
      async showMessageBox(_options, event) { return { response: event!.sender.id } },
    }, navigation: { async openExternal(_url, event) { seen.push(event!.sender.id) } } })
    let release!: () => void
    const gate = new Promise<void>(resolve => { release = resolve })
    registry.handle('choose', async (event: InvokeEvent) => {
      if (event.sender.id === first.id) await gate
      const opened = await dialogHost.showOpenDialog({ properties: ['openFile'] })
      expect(currentInvokeEvent()?.sender).toBe(event.sender)
      await navigationHost.openExternal('https://example.test')
      return opened
    })
    const pending = registry.invoke('choose', { sender: first })
    expect(await registry.invoke('choose', { sender: second })).toEqual({ canceled: false, filePaths: ['/client/15'] })
    release()
    expect(await pending).toEqual({ canceled: false, filePaths: ['/client/14'] })
    expect(seen).toEqual([15, 15, 14, 14])
    expect(currentInvokeEvent()).toBeUndefined()
  })
  it('supplies direct push events and exactly-once destroyed subscriptions without a window', () => {
    const sender = new Client(16), destroyed = vi.fn()
    sender.once('destroyed', destroyed)
    eventsFor({ sender }).send('llm:stream-chunk', { chunk: 'A' })
    expect(sender.send).toHaveBeenCalledWith('llm:stream-chunk', { chunk: 'A' })
    sender.destroy(); sender.destroy()
    expect(destroyed).toHaveBeenCalledOnce()
  })
})

describe('server credential protection', () => {
  it('persists one private key and decrypts authenticated ciphertext after restart', () => {
    const root = directory(), first = createServerSecretStorage({ rootPath: root })
    expect(first.persistence).toBe('server-encrypted')
    expect(first.isEncryptionAvailable()).toBe(true)
    const ciphertext = first.encryptString('private 密钥')
    const nextCiphertext = first.encryptString('private 密钥')
    expect(ciphertext.equals(nextCiphertext)).toBe(false)
    expect(ciphertext.includes(Buffer.from('private'))).toBe(false)
    expect(createServerSecretStorage({ rootPath: root }).decryptString(ciphertext)).toBe('private 密钥')
    if (process.platform !== 'win32') expect(fs.statSync(path.join(root, 'server-credentials.key')).mode & 0o777).toBe(0o600)
    expect(fs.readdirSync(root)).toEqual(['server-credentials.key'])
  })
  it('rejects tampered envelopes and ciphertext encrypted under another key', () => {
    const first = createServerSecretStorage({ rootPath: directory() })
    const ciphertext = first.encryptString('private')
    const tampered = Buffer.from(ciphertext); tampered[tampered.length - 1] ^= 1
    expect(() => first.decryptString(tampered)).toThrow('SERVER_SECRET_DECRYPT_FAILED')
    expect(() => first.decryptString(Buffer.from('plaintext'))).toThrow('SERVER_SECRET_CIPHERTEXT_INVALID')
    expect(() => createServerSecretStorage({ rootPath: directory() }).decryptString(ciphertext)).toThrow('SERVER_SECRET_DECRYPT_FAILED')
  })
  it('fails closed for damaged, permissive and symlinked keys without replacing them', () => {
    const root = directory(), keyPath = path.join(root, 'server-credentials.key')
    fs.writeFileSync(keyPath, 'damaged', { mode: 0o600 })
    expect(() => loadOrCreatePrivateKey(keyPath)).toThrow('SERVER_SECRET_KEY_INVALID')
    expect(fs.readFileSync(keyPath, 'utf8')).toBe('damaged')
    fs.unlinkSync(keyPath)
    if (process.platform !== 'win32') {
      fs.writeFileSync(keyPath, Buffer.alloc(32), { mode: 0o644 })
      expect(() => loadOrCreatePrivateKey(keyPath)).toThrow('SERVER_SECRET_KEY_INVALID')
      fs.unlinkSync(keyPath)
      const target = path.join(root, 'other'); fs.writeFileSync(target, Buffer.alloc(32), { mode: 0o600 })
      fs.symlinkSync(target, keyPath)
      expect(() => loadOrCreatePrivateKey(keyPath)).toThrow()
    }
  })
  it('keeps cloud credentials persistent on Linux with honest server-encrypted metadata', () => {
    const root = directory()
    const options = { rootPath: root, platform: 'linux' as const, safeStorage: createServerSecretStorage({ rootPath: root }) }
    const store = new CloudCredentialStore(options)
    const saved = store.save({ endpoint: 'https://dav.example.test/root/', username: 'writer', secret: 'sensitive-password' })
    expect(saved.metadata.persistence).toBe('server-encrypted')
    expect(saved.warning).toBeUndefined()
    const restarted = new CloudCredentialStore({ ...options, safeStorage: createServerSecretStorage({ rootPath: root }) })
    expect(restarted.resolveSecret(saved.metadata.accountId)).toEqual({ metadata: saved.metadata, secret: 'sensitive-password' })
    expect(fs.readFileSync(path.join(root, 'cloud-backup-credentials.json'), 'utf8')).not.toContain('sensitive-password')
  })
  it('keeps the import source identity secret stable, private and outside project storage', () => {
    const root = directory()
    configurePlatform({ app: { getLocale: () => 'en-US', getPath: name => { expect(name).toBe('userData'); return root } } })
    const first = loadApplicationImportSourceSecret()
    expect(first.length).toBe(32)
    expect(first.equals(loadApplicationImportSourceSecret())).toBe(true)
    expect(fs.readdirSync(root)).toEqual(['import-source-identity.secret'])
  })
})

describe('optional avatar optimization', () => {
  it('preserves validated original bytes by default and rejects broken adapters', () => {
    const bytes = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1])
    expect(compressAvatarImage(bytes, 'png')).toEqual({ bytes, extension: 'png' })
    configureAvatarCompressor(() => { throw new Error('codec unavailable') })
    expect(compressAvatarImage(bytes, 'png')).toEqual({ bytes, extension: 'png' })
    configureAvatarCompressor(() => ({ bytes: Buffer.from('invalid'), extension: 'png' }))
    expect(compressAvatarImage(bytes, 'png')).toEqual({ bytes, extension: 'png' })
  })
})
