import { useSyncExternalStore } from 'react'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogFooter,
  DialogTitle,
  DialogDescription,
} from '../ui/Dialog'
import { Button } from '../ui/Button'
import { Textarea } from '../ui/Textarea'
import { useLocaleStore } from '../../stores/locale-store'
import { toast } from '../ui/Toast'

export type DraftConflictResolution =
  | { action: 'overwrite' }
  | { action: 'merge' }
  | { action: 'cancel' }

export interface DraftConflictPrompt {
  /** 服务器当前正文（冲突时刚读出的版本）。 */
  serverContent: string
  /** 用户本地未保存的正文。 */
  localContent: string
}

interface ActiveDraftConflict extends DraftConflictPrompt {
  resolve: (resolution: DraftConflictResolution) => void
}

let activeConflict: ActiveDraftConflict | null = null
const conflictSubscribers = new Set<() => void>()

function emitConflictChange() {
  for (const listener of [...conflictSubscribers]) listener()
}

function subscribeConflict(listener: () => void): () => void {
  conflictSubscribers.add(listener)
  return () => { conflictSubscribers.delete(listener) }
}

function getConflictSnapshot(): ActiveDraftConflict | null {
  return activeConflict
}

/**
 * Binder-free 回调注册表：`use-draft-actions.ts` 调它打开对话框，并 await 作者的选择。
 * 对话框组件订阅同一个模块级状态渲染。一次只挂一个冲突，覆盖写入失败重试也是先 await 再重开。
 */
export function openDraftConflictDialog(
  prompt: DraftConflictPrompt,
): Promise<DraftConflictResolution> {
  return new Promise(resolve => {
    const request: ActiveDraftConflict = {
      serverContent: prompt.serverContent,
      localContent: prompt.localContent,
      resolve: (resolution) => {
        resolve(resolution)
        if (activeConflict === request) {
          activeConflict = null
          emitConflictChange()
        }
      },
    }
    activeConflict = request
    emitConflictChange()
  })
}

function useActiveDraftConflict(): ActiveDraftConflict | null {
  return useSyncExternalStore(subscribeConflict, getConflictSnapshot, getConflictSnapshot)
}

/** 自动复制失败时退回旧式临时 textarea，保证作者仍能拿到两份正文。 */
function copyTextFallback(value: string): boolean {
  try {
    const el = document.createElement('textarea')
    el.value = value
    el.setAttribute('readonly', '')
    el.style.position = 'fixed'
    el.style.opacity = '0'
    document.body.appendChild(el)
    el.select()
    const ok = document.execCommand('copy')
    document.body.removeChild(el)
    return ok
  } catch {
    return false
  }
}

async function copyBothVersions(block: string): Promise<boolean> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(block)
      return true
    }
  } catch {
    // 落到旧式复制。
  }
  return copyTextFallback(block)
}

/**
 * 保存冲突对话框：左栏服务器当前版、右栏本地版，两侧都是可选中复制的只读文本。
 * 「覆盖并写入」= resolve({action:'overwrite'})；「手动合并」= 先把两版复制到剪贴板再 resolve({action:'merge'})；
 * 「取消」或关闭 = resolve({action:'cancel'})。
 */
export default function DraftConflictDialog() {
  const conflict = useActiveDraftConflict()
  const text = useLocaleStore(s => s.text)
  const open = conflict !== null
  const serverContent = conflict?.serverContent ?? ''
  const localContent = conflict?.localContent ?? ''

  const close = (resolution: DraftConflictResolution) => {
    conflict?.resolve(resolution)
  }

  const handleMerge = () => {
    if (!conflict) return
    const block = `【${text('服务器当前版', 'Server version (current)')}】\n\n${serverContent}\n\n【${text('你的本地版', 'Your local version')}】\n\n${localContent}`
    void (async () => {
      const ok = await copyBothVersions(block)
      if (ok) {
        toast.success(text('两个版本已复制到剪贴板，请在正文中粘贴合并。', 'Both versions were copied to the clipboard. Paste them into the manuscript to merge.'))
      } else {
        toast.warning(text('无法自动复制，请手动选中并复制两侧文本。', 'Automatic copy failed. Select and copy the two panes manually.'))
      }
      close({ action: 'merge' })
    })()
  }

  return (
    <Dialog open={open} onOpenChange={next => { if (!next) close({ action: 'cancel' }) }}>
      <DialogContent className="max-w-3xl">
        <DialogHeader>
          <DialogTitle>{text('保存冲突', 'Save conflict')}</DialogTitle>
          <DialogDescription>
            {text('服务器正文已被修改。请选择覆盖服务器、手动合并，或取消以保留本地修改。', 'The server text changed. Overwrite the server, merge manually, or cancel to keep your local edits.')}
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-3 px-5 py-4 sm:grid-cols-2">
          <label className="flex flex-col gap-1">
            <span className="text-xs font-medium text-[var(--color-text-muted)]">{text('服务器当前版', 'Server version (current)')}</span>
            <Textarea readOnly value={serverContent} className="min-h-[280px] font-mono text-xs" />
          </label>
          <label className="flex flex-col gap-1">
            <span className="text-xs font-medium text-[var(--color-text-muted)]">{text('你的本地版', 'Your local version')}</span>
            <Textarea readOnly value={localContent} className="min-h-[280px] font-mono text-xs" />
          </label>
        </div>
        <DialogFooter>
          <Button variant="text" onClick={() => close({ action: 'cancel' })}>{text('取消', 'Cancel')}</Button>
          <div className="flex gap-2">
            <Button variant="secondary" onClick={handleMerge}>{text('手动合并', 'Merge manually')}</Button>
            <Button variant="primary" onClick={() => close({ action: 'overwrite' })}>{text('覆盖并写入', 'Overwrite and save')}</Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
