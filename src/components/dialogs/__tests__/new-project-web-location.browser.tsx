import { act, useEffect } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { page } from 'vitest/browser'
import type { ProjectDirectoryGrant } from '../../../shared/ipc-channels'
import { useLayoutStore } from '../../../stores/layout-store'
import { useLocaleStore } from '../../../stores/locale-store'
import { useNewProjectForm } from '../use-new-project-form'
import NewWorkDialog from '../../stage/library/NewWorkDialog'

const transport = vi.hoisted(() => ({ kind: 'web', invoke: vi.fn() }))
vi.mock('../../../services/ipc-client', async importOriginal => {
  const actual = await importOriginal<typeof import('../../../services/ipc-client')>()
  return {
    ...actual,
    ipc: { ...actual.ipc, get runtimeKind() { return transport.kind }, invoke: transport.invoke },
  }
})

;(globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true

const originalLayout = useLayoutStore.getState()
const originalLocale = useLocaleStore.getState()
let root: Root
let container: HTMLDivElement
let latest: ReturnType<typeof useNewProjectForm>
const grant = (id: string): ProjectDirectoryGrant => ({ grantId: id, displayName: '/private/server-library' })
function deferred() {
  let resolve!: (value: ProjectDirectoryGrant | null) => void
  const promise = new Promise<ProjectDirectoryGrant | null>(done => { resolve = done })
  return { promise, resolve }
}
// eslint-disable-next-line react-refresh/only-export-components
function Probe({ open }: { open: boolean }) {
  const value = useNewProjectForm(open, () => {})
  useEffect(() => { latest = value })
  return null
}

beforeEach(() => {
  transport.kind = 'web'
  transport.invoke.mockReset()
  useLocaleStore.setState({ locale: 'en-US' })
  container = document.createElement('div')
  document.body.append(container)
  root = createRoot(container)
})
afterEach(async () => {
  await act(async () => root.unmount())
  container.remove()
  useLayoutStore.setState(originalLayout, true)
  useLocaleStore.setState(originalLocale, true)
})

describe('new book Web server-library location', () => {
  it('automatically requests a real grant and exposes only the fixed server-library label', async () => {
    const pending = deferred()
    transport.invoke.mockReturnValue(pending.promise)
    useLayoutStore.setState({ newProjectOpen: true })
    await act(async () => root.render(<NewWorkDialog />))
    expect(transport.invoke).toHaveBeenCalledExactlyOnceWith('dialog:select-folder', 'project-create')
    await expect.element(page.getByRole('textbox', { name: 'Location' })).toHaveValue('Server library')
    await expect.element(page.getByText('Preparing the server library…', { exact: true })).toBeVisible()
    expect(document.querySelector('[role="dialog"]')?.textContent).not.toContain('Choose')
    await expect.element(page.getByRole('button', { name: 'Create book', exact: true })).toBeDisabled()
    await act(async () => pending.resolve(grant('ready')))
    expect(document.querySelector('[role="dialog"]')?.textContent).not.toContain('/private/')
    await expect.element(page.getByRole('textbox', { name: 'Location' })).toHaveValue('Server library')
  })

  it('handles rejection without leaking errors and offers an explicit retry', async () => {
    transport.invoke.mockRejectedValueOnce(new Error('/private/secret-path'))
    transport.invoke.mockResolvedValueOnce(grant('retry'))
    useLayoutStore.setState({ newProjectOpen: true })
    await act(async () => root.render(<NewWorkDialog />))
    await expect.element(page.getByRole('alert')).toHaveTextContent('Unable to prepare the server library. Please retry.')
    expect(document.querySelector('[role="dialog"]')?.textContent).not.toContain('/private/')
    await act(async () => page.getByRole('button', { name: 'Retry', exact: true }).click())
    expect(transport.invoke).toHaveBeenCalledTimes(2)
    await expect.element(page.getByRole('button', { name: 'Retry', exact: true })).not.toBeInTheDocument()
  })

  it('ignores a grant from the previous open and cannot create while the new grant is pending', async () => {
    const oldRequest = deferred()
    const newRequest = deferred()
    transport.invoke.mockReturnValueOnce(oldRequest.promise).mockReturnValueOnce(newRequest.promise)
    await act(async () => root.render(<Probe open />))
    await act(async () => root.render(<Probe open={false} />))
    await act(async () => root.render(<Probe open />))
    await act(async () => latest.setName('Book'))
    await act(async () => oldRequest.resolve(grant('stale')))
    expect(latest.path).toBeNull()
    expect(latest.locationLoading).toBe(true)
    expect(latest.canCreate).toBe(false)
    await act(async () => newRequest.resolve(grant('current')))
    expect(latest.path?.grantId).toBe('current')
    expect(latest.canCreate).toBe(true)
  })

  it('keeps desktop folder selection manual', async () => {
    transport.kind = 'electron'
    transport.invoke.mockResolvedValue(grant('desktop'))
    await act(async () => root.render(<Probe open />))
    expect(transport.invoke).not.toHaveBeenCalled()
    await act(async () => latest.selectFolder())
    expect(latest.path?.grantId).toBe('desktop')
  })
})
