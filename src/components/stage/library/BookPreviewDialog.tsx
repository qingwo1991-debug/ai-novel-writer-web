import { useRef, type ReactNode } from 'react'
import * as DialogPrimitive from '@radix-ui/react-dialog'
import { keepToastStackClickable } from '../../ui/ToastStack'
import { usePaperInkRoot } from '../../ui/use-paper-ink-root'

interface BookPreviewDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** 仅作无障碍标题；卡片本身已显示书名。 */
  title: string
  children: ReactNode
}

/**
 * 窄屏书房的作品预览：点书封弹出的底部抽屉，内容是右侧概览卡的同一份。
 * 桌面端不用它——预览直接落在书架右侧。
 */
export default function BookPreviewDialog({ open, onOpenChange, title, children }: BookPreviewDialogProps) {
  const sentinelRef = useRef<HTMLSpanElement>(null)
  const root = usePaperInkRoot(sentinelRef)
  return (
    <DialogPrimitive.Root open={open} onOpenChange={onOpenChange}>
      <span ref={sentinelRef} hidden />
      {root !== undefined && (
        <DialogPrimitive.Portal container={root}>
          <DialogPrimitive.Overlay className="pi-scrim" />
          <DialogPrimitive.Content
            className="pi-dialog stage-book-dialog"
            onPointerDownOutside={event => keepToastStackClickable(event)}
            {...{ 'aria-describedby': undefined }}
          >
            <DialogPrimitive.Title className="stage-visually-hidden">{title}</DialogPrimitive.Title>
            {children}
          </DialogPrimitive.Content>
        </DialogPrimitive.Portal>
      )}
    </DialogPrimitive.Root>
  )
}
