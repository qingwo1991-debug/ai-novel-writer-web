import type { FinalizationResult } from '../../electron/services/finalization-service'
import { getActiveProjectSessionContext, sameProjectSessionContext } from '../shared/project-session-context'
import type { ProjectSessionContext } from '../shared/ipc-channels'
import type { FinalizationSnapshot } from './finalization-snapshot'
import { useLocaleStore } from '../stores/locale-store'
import { getAPI, ipc } from './ipc-client'

function getFinalizationAPI() {
  // i18n-exempt: 仅规范桌面桥和显式配置的浏览器后端均不可用时触发。
  if (!ipc.hasBackend) throw new Error('后端接口未就绪，无法提交定稿')
  return getAPI()
}

/** 只接受已冻结快照；调用处不能把任意正文/目标路径塞给 retry。 */
export async function commitFinalizationSnapshot(
  snapshot: FinalizationSnapshot,
): Promise<FinalizationResult> {
  const currentSession = getActiveProjectSessionContext()
  if (!sameProjectSessionContext(snapshot.projectSession, currentSession)) {
    throw new Error(useLocaleStore.getState().text('项目会话已变化，已拒绝提交旧定稿快照', 'The book session changed, so the earlier finalization was not submitted.'))
  }
  return getFinalizationAPI().invoke(
    'finalization:commit',
    snapshot,
    snapshot.projectSession,
  ) as Promise<FinalizationResult>
}

/** 实体稿重试只带已提交的 finalizationId，正文和路径始终从 SQLite outbox 读取。 */
export async function retryFinalizationPublication(
  finalizationId: string,
  projectSession: ProjectSessionContext,
): Promise<FinalizationResult> {
  const currentSession = getActiveProjectSessionContext()
  if (!sameProjectSessionContext(projectSession, currentSession)) {
    throw new Error(useLocaleStore.getState().text('项目会话已变化，已拒绝实体稿重试', 'The book session changed, so the manuscript file was not written again.'))
  }
  return getFinalizationAPI().invoke(
    'finalization:retry',
    finalizationId,
    projectSession,
  ) as Promise<FinalizationResult>
}
