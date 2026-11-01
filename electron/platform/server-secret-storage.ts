import { createCipheriv, createDecipheriv, randomBytes } from 'node:crypto'
import fs from 'node:fs'
import path from 'node:path'

export interface SecretStorage {
  readonly persistence?: 'os-backed' | 'server-encrypted'
  isEncryptionAvailable(): boolean
  encryptString(plainText: string): Buffer
  decryptString(encrypted: Buffer): string
  getSelectedStorageBackend?(): string
}

const KEY_BYTES = 32
const MAGIC = Buffer.from('ANWSEC01', 'ascii')
const NONCE_BYTES = 12
const TAG_BYTES = 16
const AAD = Buffer.from('AI-Novel-Writer/server-secret/v1', 'utf8')
const MAX_SECRET_BYTES = 64 * 1024

/** Reject links, permissive files and foreign ownership instead of silently weakening key protection. */
export function readPrivateKey(filePath: string, expectedBytes = KEY_BYTES): Buffer {
  const fd = fs.openSync(filePath, fs.constants.O_RDONLY | (fs.constants.O_NOFOLLOW ?? 0))
  try {
    const info = fs.fstatSync(fd)
    if (!info.isFile() || info.size !== expectedBytes
      || (process.platform !== 'win32' && ((info.mode & 0o077) !== 0
        || (typeof process.getuid === 'function' && info.uid !== process.getuid())))) {
      throw new Error('SERVER_SECRET_KEY_INVALID')
    }
    const key = fs.readFileSync(fd)
    if (key.byteLength !== expectedBytes) throw new Error('SERVER_SECRET_KEY_INVALID')
    return key
  } finally { fs.closeSync(fd) }
}

/** Atomic publish means racing processes never observe a partially written key. */
export function loadOrCreatePrivateKey(filePath: string, bytes = KEY_BYTES): Buffer {
  try { return readPrivateKey(filePath, bytes) } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error
  }
  fs.mkdirSync(path.dirname(filePath), { recursive: true, mode: 0o700 })
  const key = randomBytes(bytes)
  const temporaryPath = `${filePath}.${process.pid}.${randomBytes(12).toString('hex')}.tmp`
  let fd: number | undefined
  try {
    fd = fs.openSync(temporaryPath, fs.constants.O_WRONLY | fs.constants.O_CREAT | fs.constants.O_EXCL, 0o600)
    fs.writeFileSync(fd, key)
    fs.fsyncSync(fd)
    fs.closeSync(fd)
    fd = undefined
    try { fs.linkSync(temporaryPath, filePath) } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== 'EEXIST') throw error
    }
  } finally {
    if (fd !== undefined) fs.closeSync(fd)
    fs.rmSync(temporaryPath, { force: true })
  }
  return readPrivateKey(filePath, bytes)
}

export interface ServerSecretStorageOptions { rootPath: string; keyFileName?: string }
/**
 * Encrypts credentials at rest using AES-256-GCM and a local 0600 key. This does
 * not protect against an administrator or a process that can read that key.
 * Deployments must preserve/back up the key alongside their private data volume.
 */
export function createServerSecretStorage(options: ServerSecretStorageOptions): SecretStorage {
  const name = options.keyFileName ?? 'server-credentials.key'
  if (!name || path.basename(name) !== name || name === '.' || name === '..') throw new Error('SERVER_SECRET_KEY_INVALID')
  const keyPath = path.join(options.rootPath, name)
  let cachedKey: Buffer | undefined
  const key = () => cachedKey ??= loadOrCreatePrivateKey(keyPath)
  return {
    persistence: 'server-encrypted',
    getSelectedStorageBackend: () => 'server_aes_256_gcm',
    isEncryptionAvailable() { try { key(); return true } catch { return false } },
    encryptString(plainText) {
      if (typeof plainText !== 'string' || Buffer.byteLength(plainText, 'utf8') > MAX_SECRET_BYTES) {
        throw new Error('SERVER_SECRET_INPUT_INVALID')
      }
      const nonce = randomBytes(NONCE_BYTES)
      const cipher = createCipheriv('aes-256-gcm', key(), nonce)
      cipher.setAAD(AAD)
      const ciphertext = Buffer.concat([cipher.update(plainText, 'utf8'), cipher.final()])
      return Buffer.concat([MAGIC, nonce, cipher.getAuthTag(), ciphertext])
    },
    decryptString(encrypted) {
      if (!Buffer.isBuffer(encrypted) || encrypted.length < MAGIC.length + NONCE_BYTES + TAG_BYTES
        || encrypted.length > MAGIC.length + NONCE_BYTES + TAG_BYTES + MAX_SECRET_BYTES
        || !encrypted.subarray(0, MAGIC.length).equals(MAGIC)) throw new Error('SERVER_SECRET_CIPHERTEXT_INVALID')
      const nonceEnd = MAGIC.length + NONCE_BYTES
      const tagEnd = nonceEnd + TAG_BYTES
      try {
        const decipher = createDecipheriv('aes-256-gcm', key(), encrypted.subarray(MAGIC.length, nonceEnd))
        decipher.setAAD(AAD)
        decipher.setAuthTag(encrypted.subarray(nonceEnd, tagEnd))
        return Buffer.concat([decipher.update(encrypted.subarray(tagEnd)), decipher.final()]).toString('utf8')
      } catch { throw new Error('SERVER_SECRET_DECRYPT_FAILED') }
    },
  }
}
