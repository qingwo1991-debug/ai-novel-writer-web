/* eslint-disable react-refresh/only-export-components */
import { useRef, useState } from 'react'
import { createRoot } from 'react-dom/client'
import { browserRequest } from '../services/browser-transport'
import { ipc } from '../services/ipc-client'
import type { ProjectOpenTarget } from '../shared/ipc-channels'
import { useLocaleStore } from '../stores/locale-store'
import { useProjectStore } from '../stores/project-store'
import { InkDialog } from './stage/InkDialog'
import { alertError } from './ui/AlertDialog'
import { Button } from './ui/Button'
import { createInkOverlayMount } from './ui/ink-overlay'

interface LibraryProject {
  name: string
  path: string
}

/**
 * 浏览器端没有原生文件夹选择器，`dialog:select-folder` 只回答书库根的授权，
 * 打不开任何具体作品（project:open 的字符串形态按目录清单键解析路径）。
 * 打开作品改走 /api/library 全量清单：列出每一部规范项目，选中后把它的
 * path 键交给 openProject——与书架、最近项目同一条已验证的打开路径。
 */
async function loadLibraryProjects(): Promise<LibraryProject[]> {
  const value = await browserRequest<{ projects?: unknown }>('/api/library')
  const projects = Array.isArray(value?.projects) ? value.projects : []
  return projects.filter((project): project is LibraryProject =>
    typeof project === 'object' && project !== null
    && typeof (project as LibraryProject).name === 'string'
    && typeof (project as LibraryProject).path === 'string'
    && (project as LibraryProject).path !== '')
}

function WebProjectPicker({ projects, portalContainer, onResolve }: {
  projects: LibraryProject[]
  portalContainer: HTMLElement
  onResolve: (target: string | null) => void
}) {
  const text = useLocaleStore(s => s.text)
  const currentPath = useProjectStore(s => s.currentProject?.path ?? null)
  const [open, setOpen] = useState(true)
  const [selected, setSelected] = useState(projects[0]?.path ?? '')
  const settled = useRef(false)

  const close = (target: string | null) => {
    if (settled.current) return
    settled.current = true
    setOpen(false)
    // 等 Radix 的退出动画走完再卸载，和 InkPrompt 的 onCloseAutoFocus 同一时机。
    setTimeout(() => onResolve(target))
  }

  return (
    <InkDialog
      open={open}
      onOpenChange={next => { if (!next) close(null) }}
      portalContainer={portalContainer}
      title={text('打开其他作品', 'Open another book')}
      description={text('选择服务器书库里的作品。', 'Choose a book from the server library.')}
      footer={(
        <>
          <Button variant="secondary" onClick={() => close(null)}>{text('取消', 'Cancel')}</Button>
          <Button variant="primary" disabled={!selected} onClick={() => close(selected)}>{text('打开', 'Open')}</Button>
        </>
      )}
    >
      {projects.length === 0
        ? <p className="pi-dialog-text">{text('服务器书库里还没有作品。', 'There are no books in the server library yet.')}</p>
        : (
          <label>
            {text('作品', 'Book')}
            <select className="pi-input" value={selected} onChange={event => setSelected(event.target.value)}>
              {projects.map(project => (
                <option key={project.path} value={project.path}>
                  {project.name}{project.path === currentPath ? text('（当前）', ' (current)') : ''}
                </option>
              ))}
            </select>
          </label>
        )}
    </InkDialog>
  )
}

function openWebProjectPicker(projects: LibraryProject[]): Promise<string | null> {
  return new Promise(resolve => {
    const layer = createInkOverlayMount()
    const root = createRoot(layer)
    root.render(<WebProjectPicker projects={projects} portalContainer={layer} onResolve={target => {
      setTimeout(() => {
        root.unmount()
        layer.remove()
        resolve(target)
      })
    }} />)
  })
}

/**
 * 「打开其他作品」的统一入口：桌面端照旧走原生文件夹授权；
 * 浏览器端列出服务器书库让作者选一部，返回 project:open 可直接消费的目标。
 */
export async function selectProjectOpenTarget(): Promise<ProjectOpenTarget | null> {
  if (ipc.runtimeKind !== 'web') {
    return ipc.invoke('dialog:select-folder', 'project-open')
  }
  let projects: LibraryProject[]
  try {
    projects = await loadLibraryProjects()
  } catch {
    const text = useLocaleStore.getState().text
    await alertError(text('无法读取服务器书库，请重试。', 'Could not read the server library. Please try again.'), {
      title: text('打开作品失败', 'Could not open a book'),
    })
    return null
  }
  return openWebProjectPicker(projects)
}
