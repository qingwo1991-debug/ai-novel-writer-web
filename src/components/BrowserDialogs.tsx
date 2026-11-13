import { useEffect, useLayoutEffect, useState } from 'react'
import { createInkOverlayMount } from './ui/ink-overlay'
import { browserRequest, getBrowserTransport } from '../services/browser-transport'
import { ipc } from '../services/ipc-client'
import { Button } from './ui/Button'
import { InkDialog } from './stage/InkDialog'

type DialogRequest = { id: string; kind: 'open' | 'save' | 'message'; options: {
  title?: string; message?: string; detail?: string; buttons?: string[]; cancelId?: number; defaultName?: string;
  directoryKind?: string; properties?: string[]; filters?: Array<{ extensions: string[] }>
} }
type ProjectChoice = { path: string; name: string }

/** User gestures occur here in the real browser, never on a shared desktop. */
export default function BrowserDialogs() {
  const [queue, setQueue] = useState<DialogRequest[]>([])
  const [projects, setProjects] = useState<ProjectChoice[]>([])
  const [selected, setSelected] = useState('')
  const [files, setFiles] = useState<File[]>([])
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [downloads, setDownloads] = useState<Array<{ url: string; name: string }>>([])
  const current = queue[0]
  const [promptLayer, setPromptLayer] = useState<HTMLElement | null>(null)
  useLayoutEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect
    if (!current) { setPromptLayer(null); return }
    const layer = createInkOverlayMount()
    setPromptLayer(layer)
    return () => layer.remove()
  }, [current])
  useEffect(() => {
    if (ipc.runtimeKind !== 'web') return
    const transport = getBrowserTransport()
    const off = transport.on('browser:dialog', payload => {
      const request = payload as DialogRequest
      if (request && typeof request.id === 'string') setQueue(previous => previous.some(item => item.id === request.id) ? previous : [...previous, request])
    })
    const offExternal = transport.on('browser:open-external', payload => {
      const url = (payload as { url?: unknown })?.url
      if (typeof url === 'string' && url.startsWith('https://')) {
        // Browsers may block asynchronous popups. A visible link remains usable.
        const opened = window.open(url, '_blank', 'noopener,noreferrer')
        if (!opened) setQueue(previous => [...previous, { id: `external:${url}`, kind: 'message', options: { title: '打开外部网页', message: url, buttons: ['关闭'] } }])
      }
    })
    const offDownloads = transport.on('browser:download-ready', payload => {
      const download = payload as { url: string; name: string }
      if (download && typeof download.url === 'string' && download.url.startsWith('/api/downloads/')) {
        setDownloads(previous => [...previous.filter(item => item.url !== download.url), download])
      }
    })
    return () => { off(); offExternal(); offDownloads() }
  }, [])
  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setFiles([]); setSelected(''); setName(current?.options.defaultName ?? ''); setError(''); setBusy(false)
    if (current?.options.properties?.includes('openDirectory')) {
      void browserRequest<{ projects: ProjectChoice[] }>('/api/library').then(value => setProjects(value.projects)).catch(() => setProjects([]))
    }
  }, [current])
  if (!current) return downloads.length ? <aside className="paper-ink" aria-label="下载文件" style={{ position: 'fixed', right: 20, bottom: 20, zIndex: 100, padding: 16, borderRadius: 12, background: 'var(--surface)', border: '1px solid var(--rule-strong)', maxWidth: 'min(420px, 90vw)' }}>
    <strong>导出文件已生成</strong>{downloads.map(download => <div key={download.url} style={{ display: 'flex', gap: 12, marginTop: 10 }}>
      <a href={download.url} download={download.name}>{download.name} · 点击下载</a>
      <button type="button" aria-label="关闭下载提示" onClick={() => setDownloads(items => items.filter(item => item.url !== download.url))}>×</button>
    </div>)}
  </aside> : null
  const directory = current.options.properties?.includes('openDirectory')
  const title = current.options.title ?? '选择文件'
  const automaticDirectory = directory && ['export-directory', 'project-create', 'project-restore-target'].includes(current.options.directoryKind ?? '')
  const uploadDirectory = directory && !automaticDirectory && current.options.directoryKind !== 'project-open'
  const accept = current.options.filters?.flatMap(filter => filter.extensions.filter(ext => ext !== '*').map(ext => `.${ext}`)).join(',')
  const finish = async (result: Record<string, unknown>) => {
    setError(''); setBusy(true)
    try {
      if (!current.id.startsWith('external:')) await browserRequest(`/api/dialogs/${encodeURIComponent(current.id)}`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(result),
      })
      setQueue(previous => previous.filter(item => item.id !== current.id))
    } catch (cause) { setError(cause instanceof Error ? cause.message : '操作失败，请重试'); setBusy(false) }
  }
  const submitFiles = async () => {
    setError(''); setBusy(true)
    try {
      const uploadIds: string[] = []
      let folderUploadId: string | undefined
      if (uploadDirectory) {
        const begin = await browserRequest<{ folderUploadId: string }>('/api/upload-folders', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' })
        folderUploadId = begin.folderUploadId
      }
      for (const file of files) {
        const relative = file.webkitRelativePath?.split('/').slice(1).join('/') || file.name
        const upload = await browserRequest<{ uploadId: string }>('/api/uploads', {
          method: 'POST', headers: { 'Content-Type': 'application/octet-stream', 'X-Upload-Name': encodeURIComponent(file.name),
            ...(folderUploadId ? { 'X-Upload-Folder': folderUploadId, 'X-Upload-Relative': encodeURIComponent(relative) } : {}) }, body: file,
        })
        uploadIds.push(upload.uploadId)
      }
      await finish(folderUploadId ? { folderUploadId } : { uploadIds })
    } catch (cause) { setError(cause instanceof Error ? cause.message : '上传失败'); setBusy(false) }
  }
  const cancel = () => { if (!busy) void finish(current.kind === 'message' ? { response: current.options.cancelId ?? 0 } : { canceled: true }) }
  if (!promptLayer) return null
  return <div className="paper-ink pi-overlay-host"><InkDialog open portalContainer={promptLayer} onOpenChange={open => { if (!open) cancel() }} title={title}
    description={current.options.message ?? (current.kind === 'save' ? '文件将在生成后通过浏览器下载。' : automaticDirectory ? '作品保存在服务器书库；导出文件通过浏览器下载，不需要远程桌面。' : '从本机选择文件并上传到服务器。')}
    footer={current.kind === 'message' ? <>{(current.options.buttons ?? ['关闭']).map((button, response) => <Button key={response} disabled={busy} onClick={() => void finish({ response })}>{button}</Button>)}</> : <>
      <Button variant="secondary" disabled={busy} onClick={cancel}>取消</Button>
      <Button disabled={busy || (!automaticDirectory && current.kind !== 'save' && !(directory && !uploadDirectory ? selected : files.length))}
        onClick={() => {
          if (automaticDirectory) void finish({ useLibrary: true })
          else if (current.kind === 'save') void finish({ name })
          else if (directory && !uploadDirectory) void finish({ projectPath: selected })
          else void submitFiles()
        }}>{busy ? '处理中…' : automaticDirectory ? '继续' : current.kind === 'save' ? '生成下载文件' : '确认'}</Button>
    </>}>
    {current.options.detail && <p style={{ whiteSpace: 'pre-wrap' }}>{current.options.detail}</p>}
    {current.id.startsWith('external:') && <a href={current.options.message} target="_blank" rel="noopener noreferrer">点击打开网页</a>}
    {current.kind === 'save' && <label>下载文件名<input className="pi-input" value={name} onChange={event => setName(event.target.value)} /></label>}
    {current.kind === 'open' && !automaticDirectory && (directory && !uploadDirectory
      ? <label>服务器书库<select className="pi-input" value={selected} onChange={event => setSelected(event.target.value)}><option value="">选择作品</option>{projects.map(project => <option key={project.path} value={project.path}>{project.name}</option>)}</select></label>
      : <div><input type="file" accept={uploadDirectory ? undefined : accept || undefined} multiple={uploadDirectory || current.options.properties?.includes('multiSelections')}
          {...(uploadDirectory ? { webkitdirectory: '', directory: '' } : {})}
          onChange={event => setFiles(Array.from(event.target.files ?? []))} />
        {files.length > 0 && <p>{files.length} 个文件 · {(files.reduce((sum, file) => sum + file.size, 0) / 1024).toFixed(1)} KB</p>}</div>)}
    {error && <p role="alert" className="stage-dialog-error">{error}</p>}
  </InkDialog></div>
}
