import { createHash, webcrypto } from 'node:crypto'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { sha256Hex } from '../sha256'
import { hashAuthorText } from '../../shared/source-ref'

const reference = (text: string) => createHash('sha256').update(text, 'utf8').digest('hex')
afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks() })

describe('SHA-256 on HTTPS and ordinary LAN HTTP', () => {
  it.each([
    ['', 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'],
    ['abc', 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad'],
    ['abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq', '248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1'],
  ])('matches a published SHA-256 vector (%s)', async (text, expected) => {
    vi.stubGlobal('crypto', {})
    expect(await sha256Hex(text)).toBe(expected)
  })

  it.each([0, 1, 55, 56, 63, 64, 65, 119, 120, 127, 128, 129, 1024, 1000000])('matches Node SHA-256 at %i input bytes without SubtleCrypto', async length => {
    vi.stubGlobal('crypto', {})
    const text = 'a'.repeat(length)
    expect(await sha256Hex(text)).toBe(reference(text))
  })

  it.each(['林岚在雨夜找到铜钥匙。\r\n', '😀𝄞🧑🏽‍💻', '\u0000a\u0000', 'é', 'e\u0301', '\ud800', '\udc00', '\ud800x\udc00'])('preserves exact UTF-8 semantics for %j', async text => {
    vi.stubGlobal('crypto', undefined)
    expect(await sha256Hex(text)).toBe(reference(text))
    expect(await hashAuthorText(text)).toBe(reference(text))
  })

  it('does not normalize author prose', async () => {
    vi.stubGlobal('crypto', {})
    expect(await sha256Hex('é')).not.toBe(await sha256Hex('e\u0301'))
    expect(await sha256Hex('word\n')).not.toBe(await sha256Hex('word\r\n'))
  })

  it('matches native Web Crypto across arbitrary multi-block Unicode input', async () => {
    let state = 0x98765432
    const next = () => { state = (Math.imul(state, 1664525) + 1013904223) >>> 0; return state }
    for (let sample = 0; sample < 80; sample++) {
      const text = String.fromCharCode(...Array.from({ length: next() % 2000 }, () => next() & 0xffff))
      vi.stubGlobal('crypto', {})
      const portable = await sha256Hex(text)
      vi.stubGlobal('crypto', webcrypto)
      expect(await sha256Hex(text)).toBe(portable)
      expect(portable).toBe(reference(text))
    }
  })

  it('prefers native SubtleCrypto and does not mask native errors', async () => {
    const digest = vi.fn(async () => new Uint8Array([1, 255]).buffer)
    vi.stubGlobal('crypto', { subtle: { digest } })
    expect(await sha256Hex('abc')).toBe('01ff')
    expect(digest).toHaveBeenCalledWith('SHA-256', new TextEncoder().encode('abc'))
    digest.mockRejectedValueOnce(new Error('native digest failed'))
    await expect(sha256Hex('abc')).rejects.toThrow('native digest failed')
  })
})
