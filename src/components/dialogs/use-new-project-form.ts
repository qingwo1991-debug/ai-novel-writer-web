import { useCallback, useEffect, useRef, useState } from 'react'
import type { ProjectDirectoryGrant } from '../../shared/ipc-channels'
import { useProjectStore } from '../../stores/project-store'
import { ipc } from '../../services/ipc-client'
import { useLocaleStore } from '../../stores/locale-store'

/** 新建作品：名称与保存位置，创建后其余设定在设定页填写。 */
export function useNewProjectForm(open: boolean, onClose: () => void) {
  const createProject = useProjectStore((s) => s.createProject)
  const locale = useLocaleStore(s => s.locale)
  const [name, setName] = useState('')
  const [path, setPath] = useState<ProjectDirectoryGrant | null>(null)
  const [creating, setCreating] = useState(false)
  const [locationLoading, setLocationLoading] = useState(false)
  const [locationError, setLocationError] = useState<string | null>(null)
  const locationRequest = useRef(0)
  const dialogOpen = useRef(false)
  const isWeb = ipc.runtimeKind === 'web'

  const selectFolder = useCallback(async () => {
    if (!dialogOpen.current) return
    const request = ++locationRequest.current
    const english = useLocaleStore.getState().locale === 'en-US'
    setLocationLoading(true)
    setLocationError(null)
    const isCurrent = () => dialogOpen.current && request === locationRequest.current
    try {
      const selected = await ipc.invoke('dialog:select-folder', 'project-create')
      if (!isCurrent()) return
      if (selected) setPath(selected)
      else if (isWeb) {
        setPath(null)
        setLocationError(english ? 'Server library is unavailable. Please retry.' : '服务器书库暂不可用，请重试。')
      }
    } catch {
      if (!isCurrent()) return
      // Do not surface backend errors: they may contain private server paths.
      if (isWeb) setPath(null)
      setLocationError(isWeb
        ? english ? 'Unable to prepare the server library. Please retry.' : '无法准备服务器书库，请重试。'
        : english ? 'Unable to select a folder. Please retry.' : '无法选择文件夹，请重试。')
    } finally {
      if (isCurrent()) setLocationLoading(false)
    }
  }, [isWeb])

  useEffect(() => {
    dialogOpen.current = open
    if (!open) return
    let mounted = true
    Promise.resolve().then(() => {
      if (!mounted) return
      setName('')
      setPath(null)
      setLocationError(null)
      setLocationLoading(false)
      if (isWeb) void selectFolder()
    })
    return () => {
      mounted = false
      dialogOpen.current = false
      // eslint-disable-next-line react-hooks/exhaustive-deps
      ++locationRequest.current
    }
  }, [open, isWeb, selectFolder])

  /** 类型与目标读者留空，在设定页填写。 */
  const create = async () => {
    if (!name.trim() || !path || locationLoading) return
    setCreating(true)
    let success = false
    try {
      success = await createProject({
        name: name.trim(),
        parentGrantId: path.grantId,
        genre: '',
        targetAudience: '',
        writingLanguage: locale,
      })
    } catch {
      // Keep rejected creation requests handled; never display raw server errors.
    } finally {
      setPath(null)
      setCreating(false)
    }
    if (success) onClose()
    else if (isWeb) setLocationError(locale === 'en-US' ? 'Please prepare the server library again before retrying.' : '请重新准备服务器书库后重试。')
  }

  return { name, setName, path, selectFolder, locationLoading, locationError, creating, canCreate: !creating && !locationLoading && Boolean(name.trim()) && path !== null, create }
}
