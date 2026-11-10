import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  autoSaveDirtyChapterTabs,
  registerEditorExitSaveHandler,
  saveDirtyEditorChangesForExit,
  useEditorStore,
} from '../editor-store'
import { useLocaleStore } from '../locale-store'

const PROJECT_A = 'C:\\novels\\exit-a'
const PROJECT_B = 'C:\\novels\\exit-b'

beforeEach(() => {
  useEditorStore.getState().clearTabs()
  useEditorStore.setState({ tabs: [], activeTabId: null, draftLedgers: {} })
})

describe('editor exit save settlement', () => {
  it('lets a clean workspace exit without a registered save handler', async () => {
    await expect(saveDirtyEditorChangesForExit(PROJECT_A)).resolves.toBeUndefined()
  })

  it('settles every dirty tab through its existing save handler', async () => {
    useEditorStore.setState({
      tabs: [
        { id: 'chapter-a', name: 'A', type: 'chapter', projectKey: PROJECT_A, content: 'A', contentRevision: 1, dirty: true },
        { id: 'arch-a', name: 'B', type: 'arch-file', projectKey: PROJECT_A, content: 'B', contentRevision: 2, dirty: true },
      ],
    })
    const saveChapter = vi.fn(async () => {
      useEditorStore.getState().settleTabSave('chapter-a', { content: 'A', contentRevision: 1 })
    })
    const saveArch = vi.fn(async () => {
      useEditorStore.getState().settleTabSave('arch-a', { content: 'B', contentRevision: 2 })
    })
    registerEditorExitSaveHandler({ tabId: 'chapter-a', type: 'chapter', projectKey: PROJECT_A, save: saveChapter })
    registerEditorExitSaveHandler({ tabId: 'arch-a', type: 'arch-file', projectKey: PROJECT_A, save: saveArch })

    await saveDirtyEditorChangesForExit(PROJECT_A)

    expect(saveChapter).toHaveBeenCalledOnce()
    expect(saveArch).toHaveBeenCalledOnce()
    expect(useEditorStore.getState().tabs.every(tab => !tab.dirty)).toBe(true)
  })

  it('explains a blocked exit in the English interface (#474)', async () => {
    const original = useLocaleStore.getState()
    useLocaleStore.setState({ locale: 'en-US' })
    try {
      useEditorStore.setState({
        tabs: [{ id: 'chapter-b', name: 'Chapter 2', type: 'chapter', projectKey: PROJECT_B, content: 'B', contentRevision: 1, dirty: true }],
      })
      await expect(saveDirtyEditorChangesForExit(PROJECT_A)).rejects.toThrow(/^Another book has unsaved changes/u)
    } finally {
      useLocaleStore.setState(original, true)
    }
  })

  it('keeps the exit blocked when saving fails', async () => {
    useEditorStore.setState({
      tabs: [{ id: 'chapter-a', name: 'A', type: 'chapter', projectKey: PROJECT_A, content: 'A', contentRevision: 1, dirty: true }],
    })
    registerEditorExitSaveHandler({
      tabId: 'chapter-a',
      type: 'chapter',
      projectKey: PROJECT_A,
      save: async () => { throw new Error('disk full') },
    })

    await expect(saveDirtyEditorChangesForExit(PROJECT_A)).rejects.toThrow('disk full')
    expect(useEditorStore.getState().tabs[0]?.dirty).toBe(true)
  })

  it('keeps the exit blocked when the author types again while save is pending', async () => {
    useEditorStore.setState({
      tabs: [{ id: 'chapter-a', name: 'A', type: 'chapter', projectKey: PROJECT_A, content: 'A', contentRevision: 1, dirty: true }],
    })
    registerEditorExitSaveHandler({
      tabId: 'chapter-a',
      type: 'chapter',
      projectKey: PROJECT_A,
      save: async () => {
        useEditorStore.getState().updateTabContent('chapter-a', 'AB')
        useEditorStore.getState().settleTabSave('chapter-a', { content: 'A', contentRevision: 1 })
      },
    })

    await expect(saveDirtyEditorChangesForExit(PROJECT_A)).rejects.toThrow('仍有未保存')
    expect(useEditorStore.getState().tabs[0]).toMatchObject({ content: 'AB', dirty: true })
  })

  it('never saves another project through the current project identity', async () => {
    useEditorStore.setState({
      tabs: [{ id: 'chapter-b', name: 'B', type: 'chapter', projectKey: PROJECT_B, content: 'B', dirty: true }],
    })
    const save = vi.fn(async () => undefined)
    registerEditorExitSaveHandler({ tabId: 'chapter-b', type: 'chapter', projectKey: PROJECT_B, save })

    await expect(saveDirtyEditorChangesForExit(PROJECT_A)).rejects.toThrow('另一个项目')
    expect(save).not.toHaveBeenCalled()
  })
})

describe('chapter autosave round', () => {
  const autoSave = (save: () => Promise<void>, inFlight: () => boolean = () => false) => ({ inFlight, save })
  const noopSave = async () => undefined

  it('saves dirty writable chapter tabs and skips clean, locked and non-chapter tabs', async () => {
    useEditorStore.setState({
      tabs: [
        { id: 'chapter-dirty', name: 'A', type: 'chapter', projectKey: PROJECT_A, content: 'A', contentRevision: 1, dirty: true, draftStatus: 'draft' },
        { id: 'chapter-clean', name: 'B', type: 'chapter', projectKey: PROJECT_A, content: 'B', contentRevision: 1, dirty: false, draftStatus: 'draft' },
        { id: 'chapter-locked', name: 'C', type: 'chapter', projectKey: PROJECT_A, content: 'C', contentRevision: 1, dirty: true, draftStatus: 'finalized' },
        { id: 'arch-dirty', name: 'D', type: 'arch-file', projectKey: PROJECT_A, content: 'D', contentRevision: 1, dirty: true },
      ],
    })
    const dirtyAutoSave = vi.fn(noopSave)
    const cleanAutoSave = vi.fn(noopSave)
    const lockedAutoSave = vi.fn(noopSave)
    const archAutoSave = vi.fn(noopSave)
    registerEditorExitSaveHandler({ tabId: 'chapter-dirty', type: 'chapter', projectKey: PROJECT_A, save: noopSave, autoSave: autoSave(dirtyAutoSave) })
    registerEditorExitSaveHandler({ tabId: 'chapter-clean', type: 'chapter', projectKey: PROJECT_A, save: noopSave, autoSave: autoSave(cleanAutoSave) })
    registerEditorExitSaveHandler({ tabId: 'chapter-locked', type: 'chapter', projectKey: PROJECT_A, save: noopSave, autoSave: autoSave(lockedAutoSave) })
    // 非正文标签即使登记了自动保存入口也不进定时轮。
    registerEditorExitSaveHandler({ tabId: 'arch-dirty', type: 'arch-file', projectKey: PROJECT_A, save: vi.fn(noopSave), autoSave: autoSave(archAutoSave) })

    await autoSaveDirtyChapterTabs()

    expect(dirtyAutoSave).toHaveBeenCalledOnce()
    expect(cleanAutoSave).not.toHaveBeenCalled()
    expect(lockedAutoSave).not.toHaveBeenCalled()
    expect(archAutoSave).not.toHaveBeenCalled()
  })

  it('skips a tab whose save is already in flight', async () => {
    useEditorStore.setState({
      tabs: [{ id: 'chapter-a', name: 'A', type: 'chapter', projectKey: PROJECT_A, content: 'A', contentRevision: 1, dirty: true }],
    })
    const save = vi.fn(noopSave)
    registerEditorExitSaveHandler({
      tabId: 'chapter-a',
      type: 'chapter',
      projectKey: PROJECT_A,
      save: noopSave,
      autoSave: autoSave(save, () => true),
    })

    await autoSaveDirtyChapterTabs()

    expect(save).not.toHaveBeenCalled()
  })

  it('keeps saving the rest of the round after one tab fails', async () => {
    useEditorStore.setState({
      tabs: [
        { id: 'chapter-a', name: 'A', type: 'chapter', projectKey: PROJECT_A, content: 'A', contentRevision: 1, dirty: true },
        { id: 'chapter-b', name: 'B', type: 'chapter', projectKey: PROJECT_A, content: 'B', contentRevision: 1, dirty: true },
      ],
    })
    const failing = vi.fn(async () => { throw new Error('disk full') })
    const healthy = vi.fn(noopSave)
    registerEditorExitSaveHandler({ tabId: 'chapter-a', type: 'chapter', projectKey: PROJECT_A, save: noopSave, autoSave: autoSave(failing) })
    registerEditorExitSaveHandler({ tabId: 'chapter-b', type: 'chapter', projectKey: PROJECT_A, save: noopSave, autoSave: autoSave(healthy) })

    await expect(autoSaveDirtyChapterTabs()).resolves.toBeUndefined()
    expect(failing).toHaveBeenCalledOnce()
    expect(healthy).toHaveBeenCalledOnce()
  })
})
