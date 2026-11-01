import { afterEach, describe, expect, it, vi } from 'vitest'
import { copyText } from '../clipboard'

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks() })

function fallback(allowed = true) {
  class Input {
    value = ''
    readOnly = false
    tabIndex = 0
    style = { cssText: '' }
    selectionStart = 2
    selectionEnd = 4
    selectionDirection: 'forward' | 'backward' | 'none' = 'backward'
    focus = vi.fn()
    select = vi.fn()
    setSelectionRange = vi.fn()
    setAttribute = vi.fn()
    remove = vi.fn()
  }
  const active = new Input(), temporary = new Input()
  const cloned = {}, range = { cloneRange: () => cloned }
  const selection = { rangeCount: 1, getRangeAt: vi.fn(() => range), removeAllRanges: vi.fn(), addRange: vi.fn() }
  const execCommand = vi.fn(() => allowed)
  vi.stubGlobal('HTMLInputElement', Input)
  vi.stubGlobal('HTMLTextAreaElement', Input)
  vi.stubGlobal('isSecureContext', false)
  vi.stubGlobal('navigator', {})
  vi.stubGlobal('document', { activeElement: active, body: { appendChild: vi.fn() }, getSelection: () => selection, createElement: () => temporary, execCommand })
  return { active, temporary, selection, cloned, execCommand }
}

describe('clipboard on HTTPS and ordinary LAN HTTP', () => {
  it('prefers native clipboard only in a secure context', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal('isSecureContext', true)
    vi.stubGlobal('navigator', { clipboard: { writeText } })
    await copyText('中文\nexact text')
    expect(writeText).toHaveBeenCalledWith('中文\nexact text')
  })

  it('runs HTTP fallback synchronously before returning its promise', async () => {
    const fixture = fallback()
    const native = vi.fn()
    vi.stubGlobal('navigator', { clipboard: { writeText: native } })
    const completion = copyText('正文 😀\n')
    expect(fixture.execCommand).toHaveBeenCalledWith('copy')
    expect(native).not.toHaveBeenCalled()
    expect(fixture.temporary.value).toBe('正文 😀\n')
    expect(fixture.temporary.readOnly).toBe(true)
    expect(fixture.temporary.style.cssText).toContain('position:fixed')
    await completion
    expect(fixture.temporary.remove).toHaveBeenCalledOnce()
    expect(fixture.active.focus).toHaveBeenCalledWith({ preventScroll: true })
    expect(fixture.active.setSelectionRange).toHaveBeenCalledWith(2, 4, 'backward')
    expect(fixture.selection.addRange).toHaveBeenCalledWith(fixture.cloned)
  })

  it('uses fallback when secure clipboard is absent', async () => {
    const fixture = fallback()
    vi.stubGlobal('isSecureContext', true)
    await copyText('fallback')
    expect(fixture.execCommand).toHaveBeenCalledOnce()
  })

  it('rejects denied copy with a fixed error and still restores focus/selection', async () => {
    const fixture = fallback(false)
    await expect(copyText('private prose')).rejects.toThrow('CLIPBOARD_COPY_FAILED')
    expect(fixture.temporary.remove).toHaveBeenCalledOnce()
    expect(fixture.active.focus).toHaveBeenCalledOnce()
    expect(fixture.selection.addRange).toHaveBeenCalledWith(fixture.cloned)
  })

  it('cleans temporary elements when browser copy throws', async () => {
    const fixture = fallback()
    fixture.execCommand.mockImplementation(() => { throw new Error('browser-specific private detail') })
    await expect(copyText('private prose')).rejects.toThrow(/^CLIPBOARD_COPY_FAILED$/)
    expect(fixture.temporary.remove).toHaveBeenCalledOnce()
    expect(fixture.active.focus).toHaveBeenCalledOnce()
  })

  it('does not silently bypass native permission denial', async () => {
    vi.stubGlobal('isSecureContext', true)
    vi.stubGlobal('navigator', { clipboard: { writeText: vi.fn().mockRejectedValue(new Error('denied')) } })
    await expect(copyText('text')).rejects.toThrow(/^CLIPBOARD_COPY_FAILED$/)
  })

  it('rejects unavailable document copy without leaking an implementation TypeError', async () => {
    vi.stubGlobal('isSecureContext', false)
    vi.stubGlobal('document', undefined)
    await expect(copyText('text')).rejects.toThrow(/^CLIPBOARD_COPY_FAILED$/)
  })
})
