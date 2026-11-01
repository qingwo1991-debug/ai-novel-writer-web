const COPY_FAILED = 'CLIPBOARD_COPY_FAILED'

/** Copy during the initiating user gesture, including ordinary insecure LAN HTTP. */
export async function copyText(text: string): Promise<void> {
  try {
    if (globalThis.isSecureContext && typeof navigator?.clipboard?.writeText === 'function') {
      await navigator.clipboard.writeText(text)
      return
    }
    // No await before execCommand: the HTTP fallback needs the click's activation.
    if (typeof document === 'undefined' || !document.body || typeof document.execCommand !== 'function') throw new Error(COPY_FAILED)
    const focused = document.activeElement as HTMLElement | null
    const input = focused instanceof HTMLInputElement || focused instanceof HTMLTextAreaElement ? focused : null
    const inputSelection = input && input.selectionStart !== null && input.selectionEnd !== null
      ? { start: input.selectionStart, end: input.selectionEnd, direction: input.selectionDirection ?? 'none' } : null
    const selection = document.getSelection()
    const ranges = selection ? Array.from({ length: selection.rangeCount }, (_, index) => selection.getRangeAt(index).cloneRange()) : []
    const temporary = document.createElement('textarea')
    temporary.value = text
    temporary.readOnly = true
    temporary.tabIndex = -1
    temporary.setAttribute('aria-hidden', 'true')
    temporary.style.cssText = 'position:fixed;left:-9999px;top:0;opacity:0;pointer-events:none'
    try {
      document.body.appendChild(temporary)
      temporary.focus({ preventScroll: true })
      temporary.select()
      temporary.setSelectionRange(0, text.length)
      if (!document.execCommand('copy')) throw new Error(COPY_FAILED)
    } finally {
      temporary.remove()
      focused?.focus?.({ preventScroll: true })
      if (input && inputSelection) input.setSelectionRange(inputSelection.start, inputSelection.end, inputSelection.direction)
      if (selection) {
        selection.removeAllRanges()
        for (const range of ranges) selection.addRange(range)
      }
    }
  } catch {
    // Never leak browser-specific details or silently report success on denial.
    throw new Error(COPY_FAILED)
  }
}
