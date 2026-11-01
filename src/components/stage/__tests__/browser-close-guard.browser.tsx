import { act } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ipc } from '../../../services/ipc-client'
import { useEditorStore } from '../../../stores/editor-store'
import { useProjectStore } from '../../../stores/project-store'
import { useWorkflowStore } from '../../../stores/workflow-store'
import { useWindowCloseGuard } from '../../layout/use-window-close-guard'
import { useUnsavedSettings } from '../../settings/unsaved-settings'

;(globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true

const editorState = useEditorStore.getState()
const projectState = useProjectStore.getState()
const workflowState = useWorkflowStore.getState()
let root: Root
let container: HTMLDivElement
let invoke: ReturnType<typeof vi.spyOn>
let on: ReturnType<typeof vi.spyOn>

function Guard({ settingsDirty = false }: { settingsDirty?: boolean }) {
  useUnsavedSettings(settingsDirty)
  useWindowCloseGuard()
  return null
}

function unload() {
  const event = new Event('beforeunload', { cancelable: true })
  window.dispatchEvent(event)
  return event.defaultPrevented
}

beforeEach(() => {
  vi.spyOn(ipc, 'isElectron', 'get').mockReturnValue(false)
  invoke = vi.spyOn(ipc, 'invoke')
  on = vi.spyOn(ipc, 'on')
  useEditorStore.setState({ tabs: [], draftLedgers: {} })
  useProjectStore.setState({ currentProject: null })
  useWorkflowStore.setState({ activeRuns: [] })
  container = document.createElement('div')
  document.body.append(container)
  root = createRoot(container)
})

afterEach(async () => {
  await act(async () => root.unmount())
  container.remove()
  useEditorStore.setState(editorState, true)
  useProjectStore.setState(projectState, true)
  useWorkflowStore.setState(workflowState, true)
  vi.restoreAllMocks()
})

describe('native browser unload guard', () => {
  it('uses a synchronous generic prompt only while editor state is dirty', async () => {
    await act(async () => root.render(<Guard />))
    expect(unload()).toBe(false)
    useEditorStore.setState({ tabs: [{ id: 'browser-draft', name: 'Draft', type: 'chapter', dirty: true }] })
    expect(unload()).toBe(true)
    expect(useEditorStore.getState().tabs[0]?.dirty).toBe(true)
    useEditorStore.setState({ tabs: [] })
    expect(unload()).toBe(false)
    expect(invoke).not.toHaveBeenCalled()
    expect(on).not.toHaveBeenCalled()
    expect(container.querySelector('[role="dialog"]')).toBeNull()
  })

  it('includes background draft ledgers, not only visible tabs', async () => {
    useEditorStore.setState({ draftLedgers: { config: JSON.stringify({ projects: [{ projectKey: '/books/first' }] }) } })
    await act(async () => root.render(<Guard />))
    expect(unload()).toBe(true)
    expect(invoke).not.toHaveBeenCalled()
  })

  it('shares settings dirty registration without consuming the existing navigation guard', async () => {
    await act(async () => root.render(<Guard settingsDirty />))
    expect(unload()).toBe(true)
    expect(invoke).not.toHaveBeenCalled()
    await act(async () => root.render(<Guard settingsDirty={false} />))
    expect(unload()).toBe(false)
  })

  it('removes its listener when the app unmounts', async () => {
    await act(async () => root.render(<Guard settingsDirty />))
    expect(unload()).toBe(true)
    await act(async () => root.render(<></>))
    expect(unload()).toBe(false)
  })
})
