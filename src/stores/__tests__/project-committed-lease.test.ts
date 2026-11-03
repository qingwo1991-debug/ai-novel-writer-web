import { afterEach, describe, expect, it } from 'vitest'

import { getActiveProjectSessionContext } from '../../shared/project-session-context'
import { useProjectStore } from '../project-store'

const oldSession = { projectId: 'book', projectPath: 'C:\\novels\\book', leaseId: 'old-lease' }
const nextSession = { ...oldSession, leaseId: 'new-lease' }

afterEach(() => {
  useProjectStore.setState({ currentProject: null })
})

describe('acknowledged draft save lease', () => {
  it('adopts only the current project lease and invalidates stale work', () => {
    useProjectStore.setState({
      currentProject: { id: 'book', name: 'book', path: oldSession.projectPath,
        sessionLease: oldSession.leaseId, novelConfig: {} } as never,
      projectSessionEpoch: 7,
    })
    expect(useProjectStore.getState().adoptCommittedProjectSession(oldSession, nextSession)).toBe(true)
    expect(useProjectStore.getState().currentProject?.sessionLease).toBe('new-lease')
    expect(useProjectStore.getState().projectSessionEpoch).toBe(8)
    expect(getActiveProjectSessionContext()).toEqual(nextSession)
    expect(useProjectStore.getState().adoptCommittedProjectSession(oldSession, { ...nextSession, leaseId: 'third' })).toBe(false)
    expect(useProjectStore.getState().adoptCommittedProjectSession(nextSession,
      { ...nextSession, projectId: 'other', leaseId: 'foreign' })).toBe(false)
    expect(useProjectStore.getState().currentProject?.sessionLease).toBe('new-lease')
  })
})
