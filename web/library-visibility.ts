import fs from 'node:fs'
import path from 'node:path'
import { randomUUID } from 'node:crypto'
import { parseCanonicalProjectManifest } from '../src/shared/project-format'

type HiddenProject = { path: string; projectId: string }
type VisibilityState = { version: 1; hidden: HiddenProject[] }
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i

function canonicalAbsolute(value: string): boolean {
  return typeof value === 'string' && !value.includes('\0') && path.isAbsolute(value) && path.resolve(value) === value
}
function physicalDirectory(directory: string): void {
  const info = fs.lstatSync(directory)
  if (!info.isDirectory() || info.isSymbolicLink() || fs.realpathSync(directory) !== directory) {
    throw new Error('LIBRARY_VISIBILITY_UNSAFE_DIRECTORY')
  }
}
function regularFile(file: string): void {
  const info = fs.lstatSync(file)
  if (!info.isFile() || info.isSymbolicLink() || info.nlink !== 1) throw new Error('LIBRARY_VISIBILITY_UNSAFE_FILE')
}
function keysAre(value: Record<string, unknown>, keys: string[]): boolean {
  return Object.keys(value).length === keys.length && keys.every(key => Object.hasOwn(value, key))
}

/** UI visibility only: never opens SQLite or writes to a project. The caller supplies
 * a statePath in its canonical global data root (whose parent must already exist).
 * State is reread on every call; malformed or unsafe state always throws.
 */
export function createLibraryVisibility(options: { libraryRoot: string; statePath: string }): {
  isHidden(projectPath: string, projectId: string): boolean
  hide(projectPath: string): void
  show(projectPath: string): void
} {
  const { libraryRoot, statePath } = options
  if (!canonicalAbsolute(libraryRoot) || !canonicalAbsolute(statePath)) throw new Error('LIBRARY_VISIBILITY_INVALID_PATH')
  physicalDirectory(libraryRoot)
  const stateDirectory = path.dirname(statePath)
  physicalDirectory(stateDirectory)
  const relativeState = path.relative(libraryRoot, statePath)
  if (relativeState === '' || (!relativeState.startsWith(`..${path.sep}`) && relativeState !== '..' && !path.isAbsolute(relativeState))) {
    throw new Error('LIBRARY_VISIBILITY_STATE_INSIDE_LIBRARY')
  }
  const isChild = (candidate: string) => canonicalAbsolute(candidate) && path.dirname(candidate) === libraryRoot && candidate !== libraryRoot
  const identity = (projectPath: string): HiddenProject => {
    if (!isChild(projectPath)) throw new Error('LIBRARY_VISIBILITY_INVALID_PROJECT_PATH')
    physicalDirectory(libraryRoot)
    physicalDirectory(projectPath)
    const storage = path.join(projectPath, '.ai-novel')
    physicalDirectory(storage)
    const manifestFile = path.join(storage, 'project.json')
    regularFile(manifestFile)
    regularFile(path.join(storage, 'project.db'))
    const manifest = parseCanonicalProjectManifest(JSON.parse(fs.readFileSync(manifestFile, 'utf8')))
    return { path: projectPath, projectId: manifest.projectId }
  }
  const readState = (): VisibilityState => {
    physicalDirectory(stateDirectory)
    try { regularFile(statePath) } catch (error) {
      if ((error as NodeJS.ErrnoException).code === 'ENOENT') return { version: 1, hidden: [] }
      throw error
    }
    const value: unknown = JSON.parse(fs.readFileSync(statePath, 'utf8'))
    if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('LIBRARY_VISIBILITY_STATE_INVALID')
    const state = value as Record<string, unknown>
    if (!keysAre(state, ['version', 'hidden']) || state.version !== 1 || !Array.isArray(state.hidden)) throw new Error('LIBRARY_VISIBILITY_STATE_INVALID')
    const seen = new Set<string>()
    for (const entry of state.hidden) {
      if (!entry || typeof entry !== 'object' || Array.isArray(entry) || !keysAre(entry, ['path', 'projectId']) ||
          typeof entry.path !== 'string' || !isChild(entry.path) || typeof entry.projectId !== 'string' || !UUID.test(entry.projectId)) {
        throw new Error('LIBRARY_VISIBILITY_STATE_INVALID')
      }
      const key = JSON.stringify([entry.path, entry.projectId])
      if (seen.has(key)) throw new Error('LIBRARY_VISIBILITY_STATE_INVALID')
      seen.add(key)
    }
    return state as VisibilityState
  }
  const persist = (state: VisibilityState): void => {
    // Validate existing bytes again before replacement; never recover corruption by overwriting it.
    readState()
    const temporary = path.join(stateDirectory, `.${path.basename(statePath)}.${randomUUID()}.tmp`)
    let fd: number | undefined
    try {
      fd = fs.openSync(temporary, 'wx', 0o600)
      fs.fchmodSync(fd, 0o600)
      fs.writeFileSync(fd, `${JSON.stringify(state, null, 2)}\n`, 'utf8')
      fs.fsyncSync(fd)
      fs.closeSync(fd); fd = undefined
      fs.renameSync(temporary, statePath)
      const directoryFd = fs.openSync(stateDirectory, 'r')
      try { fs.fsyncSync(directoryFd) } finally { fs.closeSync(directoryFd) }
    } finally {
      if (fd !== undefined) fs.closeSync(fd)
      fs.rmSync(temporary, { force: true })
    }
  }
  readState()
  return {
    isHidden(projectPath, projectId) {
      const state = readState()
      let current: HiddenProject
      try { current = identity(projectPath) } catch { return false }
      return current.projectId === projectId && state.hidden.some(entry => entry.path === current.path && entry.projectId === current.projectId)
    },
    hide(projectPath) {
      const state = readState(), current = identity(projectPath)
      if (!state.hidden.some(entry => entry.path === current.path && entry.projectId === current.projectId)) {
        persist({ version: 1, hidden: [...state.hidden, current] })
      }
    },
    show(projectPath) {
      const state = readState(), current = identity(projectPath)
      const hidden = state.hidden.filter(entry => entry.path !== current.path || entry.projectId !== current.projectId)
      if (hidden.length !== state.hidden.length) persist({ version: 1, hidden })
    },
  }
}
