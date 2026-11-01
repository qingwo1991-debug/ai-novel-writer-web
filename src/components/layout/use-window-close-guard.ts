import { useEffect, useState } from 'react'
import { useProjectStore } from '../../stores/project-store'
import { useWorkflowStore } from '../../stores/workflow-store'
import { saveDirtyEditorChangesForExit, useEditorStore } from '../../stores/editor-store'
import { countUnsavedEditorItems } from '../../stores/editor-unsaved'
import { discardAllEditorChanges } from '../../stores/editor-discard'
import { sameProjectPathKey } from '../../shared/project-session-context'
import { ipc } from '../../services/ipc-client'
import { useLocaleStore } from '../../stores/locale-store'
import { displayedErrorMessage } from '../../i18n/app-errors'
import { hasUnsavedSettings } from '../settings/unsaved-settings'

export interface ExitRequest {
  requestId: string
  workflowBlocked?: boolean
}

/**
 * 主进程关窗前发来 window:close-requested，渲染端必须回复，否则窗口关不掉。
 * App 在外壳之外调用一次，外壳渲染出错时也照样回复；外壳只负责呈现确认对话框。
 */
export function useWindowCloseGuard() {
  const text = useLocaleStore(s => s.text)
  const [exitRequest, setExitRequest] = useState<ExitRequest | null>(null)
  const [exitBusy, setExitBusy] = useState(false)
  const [exitError, setExitError] = useState<string | null>(null)

  useEffect(() => {
    if (ipc.isElectron) return
    const beforeUnload = (event: BeforeUnloadEvent) => {
      const editor = useEditorStore.getState()
      const projectPath = useProjectStore.getState().currentProject?.path
      const hasActiveWorkflow = !!projectPath && useWorkflowStore.getState().activeRuns.some(
        run => sameProjectPathKey(run.projectPath, projectPath),
      )
      if (countUnsavedEditorItems(editor.tabs, editor.draftLedgers) === 0
        && !hasUnsavedSettings() && !hasActiveWorkflow) return
      // Browsers own this synchronous, generic prompt and may suppress it (for
      // example without prior interaction). Never start an async save here or
      // claim that a close acknowledgement guarantees saved data.
      event.preventDefault()
      event.returnValue = ''
    }
    window.addEventListener('beforeunload', beforeUnload)
    return () => window.removeEventListener('beforeunload', beforeUnload)
  }, [])

  useEffect(() => {
    if (!ipc.isElectron) return
    return ipc.on('window:close-requested', ({ requestId }) => {
      const projectPath = useProjectStore.getState().currentProject?.path
      const hasActiveWorkflow = !!projectPath && useWorkflowStore.getState().activeRuns.some(
        run => sameProjectPathKey(run.projectPath, projectPath),
      )
      if (hasActiveWorkflow) {
        setExitError(null)
        setExitRequest({ requestId, workflowBlocked: true })
        return
      }
      const editor = useEditorStore.getState()
      if (countUnsavedEditorItems(editor.tabs, editor.draftLedgers) === 0) {
        void ipc.invoke('window:resolve-close', requestId, 'proceed').then(result => {
          if (!result.success) console.error('[WindowCloseGuard] 退出请求已失效')
        }).catch(error => console.error('[WindowCloseGuard] 退出请求失败:', error))
        return
      }
      setExitError(null)
      setExitRequest({ requestId })
    })
  }, [])

  const cancelExit = async () => {
    const request = exitRequest
    if (!request || exitBusy) return
    setExitBusy(true)
    try {
      const result = await ipc.invoke('window:resolve-close', request.requestId, 'cancel')
      if (!result.success) throw new Error(text('退出请求已失效，请重试', 'The exit request expired. Try again.'))
      setExitRequest(null)
      setExitError(null)
    } catch (error) {
      setExitError(displayedErrorMessage(useLocaleStore.getState().locale, error))
    } finally {
      setExitBusy(false)
    }
  }

  const discardAndExit = async () => {
    const request = exitRequest
    if (!request || request.workflowBlocked || exitBusy) return
    setExitBusy(true)
    setExitError(null)
    try {
      const result = await ipc.invoke('window:resolve-close', request.requestId, 'proceed')
      if (!result.success) throw new Error(text('退出请求已失效，请重试', 'The exit request expired. Try again.'))
      discardAllEditorChanges()
      setExitRequest(null)
    } catch (error) {
      setExitError(displayedErrorMessage(useLocaleStore.getState().locale, error))
    } finally {
      setExitBusy(false)
    }
  }

  const saveAndExit = async () => {
    const request = exitRequest
    if (!request || request.workflowBlocked || exitBusy) return
    setExitBusy(true)
    setExitError(null)
    try {
      await saveDirtyEditorChangesForExit(useProjectStore.getState().currentProject?.path)
      const result = await ipc.invoke('window:resolve-close', request.requestId, 'proceed')
      if (!result.success) throw new Error(text('退出请求已失效，请重试', 'The exit request expired. Try again.'))
    } catch (error) {
      setExitError(displayedErrorMessage(useLocaleStore.getState().locale, error))
    } finally {
      setExitBusy(false)
    }
  }

  return { exitRequest, exitBusy, exitError, cancelExit, discardAndExit, saveAndExit }
}

export type WindowCloseGuard = ReturnType<typeof useWindowCloseGuard>
