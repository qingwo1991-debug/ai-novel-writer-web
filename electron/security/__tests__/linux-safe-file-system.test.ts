import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { afterEach, describe, expect, it } from 'vitest'
import { createSecureFileCapability, createSecureFileSystem } from '../windows-safe-file-system'

const roots: string[] = []
function fixture() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'novel-linux-secure-'))
  roots.push(root)
  return root
}
afterEach(() => {
  for (const root of roots.splice(0)) fs.rmSync(root, { recursive: true, force: true })
})

describe.runIf(process.platform === 'linux')('Linux secure filesystem adapter', () => {
  it('uses the Python helper for mkdir, list, Unicode writes, bytes and exists', async () => {
    const root = fixture()
    const io = createSecureFileSystem()
    const cap = (name: string) => createSecureFileCapability(root, path.join(root, name))
    await io.mkdir(cap('正文'))
    const file = cap('正文/第1章.txt')
    let callbacks = 0
    await io.writeTextAtomically(file, '雨夜，列车驶入车站。', () => { callbacks++ })
    expect(callbacks).toBe(1)
    expect(await io.exists(file)).toBe(true)
    expect(await io.readText(file)).toBe('雨夜，列车驶入车站。')
    expect(await io.listDirectory(cap('正文'))).toEqual([{name: '第1章.txt', isDirectory: false}])
    await io.writeTextAtomically(file, '修订正文', undefined, {mustAlreadyExist: true})
    expect(await io.readBytes(file)).toEqual(Buffer.from('修订正文'))
    await expect(io.readText(file, 2)).rejects.toThrow('SECURE_FS_FILE_TOO_LARGE')
    expect(await io.exists(cap('missing'))).toBe(false)
  })

  it('preserves content when the lease callback rejects', async () => {
    const root = fixture()
    fs.writeFileSync(path.join(root, 'draft.txt'), 'original')
    const io = createSecureFileSystem()
    const cap = createSecureFileCapability(root, path.join(root, 'draft.txt'))
    await expect(io.writeTextAtomically(cap, 'new', () => { throw new Error('LEASE_CHANGED') }))
      .rejects.toThrow('LEASE_CHANGED')
    expect(fs.readFileSync(path.join(root, 'draft.txt'), 'utf8')).toBe('original')
    expect(fs.readdirSync(root)).toEqual(['draft.txt'])
  })

  it('rejects a parent switched to a symlink at the commit boundary', async () => {
    const root = fixture(), outside = fixture()
    fs.mkdirSync(path.join(root, 'chapters'))
    fs.writeFileSync(path.join(root, 'chapters', 'draft.txt'), 'original')
    const io = createSecureFileSystem()
    const cap = createSecureFileCapability(root, path.join(root, 'chapters', 'draft.txt'))
    await expect(io.writeTextAtomically(cap, 'new', () => {
      fs.renameSync(path.join(root, 'chapters'), path.join(root, 'moved'))
      fs.symlinkSync(outside, path.join(root, 'chapters'))
    })).rejects.toThrow(/SECURE_FS_(ROOT_CHANGED|REPARSE_POINT)/)
    expect(fs.readdirSync(outside)).toEqual([])
    expect(fs.readFileSync(path.join(root, 'moved', 'draft.txt'), 'utf8')).toBe('original')
  })

  it('does not recreate a required existing target removed during lease validation', async () => {
    const root = fixture(), file = path.join(root, 'draft.txt')
    fs.writeFileSync(file, 'original')
    const io = createSecureFileSystem()
    const cap = createSecureFileCapability(root, file)
    await expect(io.writeTextAtomically(cap, 'new', () => fs.unlinkSync(file), {mustAlreadyExist: true}))
      .rejects.toThrow('SECURE_FS_WRITE_FAILED')
    expect(fs.existsSync(file)).toBe(false)
  })
})
