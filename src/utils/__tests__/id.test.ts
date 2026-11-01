import { afterEach, describe, expect, it, vi } from 'vitest'
import { randomUUID } from '../id'

const uuidV4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/
afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks() })

describe('browser-compatible UUID generation', () => {
  it('prefers the native UUID method when it is available', () => {
    const native = vi.fn(() => 'native-id')
    const values = vi.fn()
    vi.stubGlobal('crypto', { randomUUID: native, getRandomValues: values })
    expect(randomUUID()).toBe('native-id')
    expect(native).toHaveBeenCalledOnce()
    expect(values).not.toHaveBeenCalled()
  })

  it('uses Web Crypto entropy on ordinary LAN HTTP with no randomUUID', () => {
    const values = vi.fn((bytes: Uint8Array) => {
      bytes.set(Array.from({ length: 16 }, (_, index) => index))
      return bytes
    })
    vi.stubGlobal('crypto', { getRandomValues: values })
    const weak = vi.spyOn(Math, 'random').mockImplementation(() => { throw new Error('Weak entropy must not be used') })
    expect(randomUUID()).toBe('00010203-0405-4607-8809-0a0b0c0d0e0f')
    expect(values).toHaveBeenCalledOnce()
    expect(weak).not.toHaveBeenCalled()
  })

  it('sets the version and variant bits without discarding the remaining entropy', () => {
    vi.stubGlobal('crypto', { getRandomValues: (bytes: Uint8Array) => bytes.fill(255) })
    expect(randomUUID()).toBe('ffffffff-ffff-4fff-bfff-ffffffffffff')
  })

  it.each([undefined, {}])('retains the legacy format if Web Crypto is absent (%s)', cryptoValue => {
    vi.stubGlobal('crypto', cryptoValue)
    vi.spyOn(Math, 'random').mockReturnValue(0)
    expect(randomUUID()).toMatch(uuidV4)
    expect(randomUUID()).toBe('00000000-0000-4000-8000-000000000000')
  })

  it('does not hide a Web Crypto failure behind weak entropy', () => {
    vi.stubGlobal('crypto', { getRandomValues: () => { throw new Error('entropy unavailable') } })
    expect(() => randomUUID()).toThrow('entropy unavailable')
  })
})
