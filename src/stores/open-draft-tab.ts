import type { ProjectSessionContext } from '../shared/ipc-channels'
import { getActiveProjectSessionContext, sameProjectSessionContext } from '../shared/project-session-context'
import { readDraftBody, type DraftMeta } from './draft-store'
import { useEditorStore } from './editor-store'
import { useLocaleStore } from './locale-store'
import { confirm } from '../components/ui/Confirm'
import {
  clearManuscriptSnapshot,
  readManuscriptSnapshot,
} from '../components/editor/manuscript-snapshot'

/**
 * 读出一稿正文，打开它的标签或切到已开的标签。章节列表与导航都走这里。
 * 读取期间项目会话变了，或调用方已不再需要这次打开（stillWanted 返回 false）时放弃，返回 false。
 * 若本地有未保存的正文快照且与服务器当前正文不同，先请作者选择恢复本地草稿还是使用服务器版。
 */
export async function openDraftTab(
  draft: DraftMeta,
  name: string,
  session: ProjectSessionContext,
  stillWanted: () => boolean = () => true,
): Promise<boolean> {
  const content = await readDraftBody(draft.filePath, session.projectPath, session)
  if (!stillWanted() || !sameProjectSessionContext(session, getActiveProjectSessionContext())) return false

  const snapshot = readManuscriptSnapshot(session.projectPath, draft.filePath)
  let tabContent = content
  let tabSavedContent = content
  let dirty = false

  if (snapshot && snapshot.content !== content) {
    const text = useLocaleStore.getState().text
    const restore = await confirm(
      text('发现未保存的正文修改，是否恢复本地草稿？', 'Unsaved manuscript changes were found. Restore your local draft?'),
      {
        title: text('恢复未保存的正文', 'Restore unsaved manuscript'),
        confirmText: text('恢复本地草稿', 'Restore local draft'),
        cancelText: text('使用服务器版', 'Use server version'),
      },
    )
    if (!stillWanted() || !sameProjectSessionContext(session, getActiveProjectSessionContext())) return false
    if (restore) {
      tabContent = snapshot.content
      // 保留快照时的保存基准：若服务器在快照后又变了，保存时会走冲突对话框，而不是静默覆盖。
      tabSavedContent = snapshot.savedContent
      dirty = true
    } else {
      clearManuscriptSnapshot(session.projectPath, draft.filePath)
    }
  } else if (snapshot) {
    // 快照与服务器一致，清掉避免下次再提示。
    clearManuscriptSnapshot(session.projectPath, draft.filePath)
  }

  useEditorStore.getState().openFile({
    id: draft.filePath,
    name,
    type: 'chapter',
    filePath: draft.filePath,
    content: tabContent,
    savedContent: tabSavedContent,
    dirty,
    draftId: draft.id,
    chapterNumber: draft.chapterNumber,
    draftStatus: draft.status,
    projectKey: session.projectPath,
    projectSessionLease: session.leaseId,
  })
  return true
}
