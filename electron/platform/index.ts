import { AsyncLocalStorage } from 'node:async_hooks'
import { createServerSecretStorage, type SecretStorage } from './server-secret-storage'

/** A connected application client, not a window or a DOM object. */
export interface ClientSink {
  readonly id: number
  send(channel: string, ...args: unknown[]): void
  isDestroyed(): boolean
  once(event: 'destroyed', listener: () => void): unknown
}

export interface InvokeEvent { readonly sender: ClientSink }
export interface AppHost {
  getLocale(): string
  getPath(name: string): string
  readonly isPackaged?: boolean
  readonly resourcesPath?: string
  getVersion?(): string
}
export interface FileFilter { name: string; extensions: string[] }
export interface OpenDialogOptions {
  /** Explicit browser capability intent; desktop hosts ignore this metadata. */
  directoryKind?: string
  title?: string; defaultPath?: string; buttonLabel?: string; filters?: FileFilter[]
  properties?: Array<'openFile' | 'openDirectory' | 'multiSelections' | 'showHiddenFiles' | 'createDirectory' | 'promptToCreate' | 'noResolveAliases' | 'treatPackageAsDirectory' | 'dontAddToRecent'>
  message?: string; securityScopedBookmarks?: boolean
}
export interface SaveDialogOptions {
  /** Explicit browser capability intent; desktop hosts ignore this metadata. */
  directoryKind?: string
  title?: string; defaultPath?: string; buttonLabel?: string; filters?: FileFilter[]
  message?: string; nameFieldLabel?: string; showsTagField?: boolean; securityScopedBookmarks?: boolean
  properties?: Array<'showHiddenFiles' | 'createDirectory' | 'treatPackageAsDirectory' | 'showOverwriteConfirmation' | 'dontAddToRecent'>
}
export interface OpenDialogReturnValue { canceled: boolean; filePaths: string[]; bookmarks?: string[] }
export interface SaveDialogReturnValue { canceled: boolean; filePath?: string; bookmark?: string }
export interface MessageBoxOptions {
  type?: 'none' | 'info' | 'error' | 'question' | 'warning'
  title?: string; message: string; detail?: string; buttons?: string[]
  defaultId?: number; cancelId?: number; noLink?: boolean
  checkboxLabel?: string; checkboxChecked?: boolean
}
export interface MessageBoxReturnValue { response: number; checkboxChecked?: boolean }
export interface DialogHost {
  showOpenDialog(options: OpenDialogOptions, event?: InvokeEvent): Promise<OpenDialogReturnValue>
  showSaveDialog(options: SaveDialogOptions, event?: InvokeEvent): Promise<SaveDialogReturnValue>
  showMessageBox(options: MessageBoxOptions, event?: InvokeEvent): Promise<MessageBoxReturnValue>
}
export interface NavigationHost { openExternal(url: string, event?: InvokeEvent): Promise<void> }
export interface PlatformConfiguration {
  app: AppHost
  dialogs?: DialogHost
  navigation?: NavigationHost
  secrets?: SecretStorage
}

const requests = new AsyncLocalStorage<InvokeEvent>()
let configuration: PlatformConfiguration | undefined
let defaultSecrets: SecretStorage | undefined
function configured(): PlatformConfiguration {
  if (!configuration) throw new Error('PLATFORM_NOT_CONFIGURED')
  return configuration
}
/** Call before importing modules that derive application data paths. */
export function configurePlatform(value: PlatformConfiguration): void {
  configuration = value
  defaultSecrets = undefined
}
export const appHost = {
  getLocale: (): string => configured().app.getLocale(),
  getPath: (name: string): string => configured().app.getPath(name),
  getVersion: (): string => configured().app.getVersion?.() ?? '0.0.0',
  get isPackaged(): boolean { return configuration?.app.isPackaged === true },
  get resourcesPath(): string | undefined { return configuration?.app.resourcesPath },
}
export function currentInvokeEvent(): InvokeEvent | undefined { return requests.getStore() }
export function eventsFor(event: InvokeEvent): ClientSink { return event.sender }

export class OperationRegistry {
  private readonly handlers = new Map<string, (event: InvokeEvent, ...args: never[]) => unknown>()
  handle<Args extends unknown[], Result>(channel: string, handler: (event: InvokeEvent, ...args: Args) => Result): void {
    if (!channel || typeof handler !== 'function') throw new Error('OPERATION_INVALID')
    if (this.handlers.has(channel)) throw new Error(`OPERATION_ALREADY_REGISTERED: ${channel}`)
    this.handlers.set(channel, handler as unknown as (event: InvokeEvent, ...args: never[]) => unknown)
  }
  removeHandler(channel: string): void { this.handlers.delete(channel) }
  has(channel: string): boolean { return this.handlers.has(channel) }
  list(): string[] { return [...this.handlers.keys()].sort() }
  channels(): string[] { return this.list() }
  async invoke(channel: string, event: InvokeEvent, ...args: unknown[]): Promise<unknown> {
    const handler = this.handlers.get(channel)
    if (!handler) throw new Error(`OPERATION_NOT_REGISTERED: ${channel}`)
    if (!event?.sender || !Number.isSafeInteger(event.sender.id) || event.sender.isDestroyed()) {
      throw new Error('CLIENT_DISCONNECTED')
    }
    return requests.run(event, () => handler(event, ...args as never[]))
  }
}
export const operations = new OperationRegistry()
export const dialogHost: DialogHost = {
  showOpenDialog(options, event = currentInvokeEvent()) {
    const host = configured().dialogs
    if (!host) throw new Error('DIALOG_HOST_UNAVAILABLE')
    return host.showOpenDialog(options, event)
  },
  showSaveDialog(options, event = currentInvokeEvent()) {
    const host = configured().dialogs
    if (!host) throw new Error('DIALOG_HOST_UNAVAILABLE')
    return host.showSaveDialog(options, event)
  },
  showMessageBox(options, event = currentInvokeEvent()) {
    const host = configured().dialogs
    if (!host) throw new Error('DIALOG_HOST_UNAVAILABLE')
    return host.showMessageBox(options, event)
  },
}
export const navigationHost: NavigationHost = {
  openExternal(url, event = currentInvokeEvent()) {
    const host = configured().navigation
    if (!host) throw new Error('NAVIGATION_HOST_UNAVAILABLE')
    return host.openExternal(url, event)
  },
}
function secretAdapter(): SecretStorage {
  return configured().secrets ?? (defaultSecrets ??= createServerSecretStorage({ rootPath: appHost.getPath('userData') }))
}
/** Node defaults to server-key encryption; it never advertises OS keychain protection. */
export const secretStorage: SecretStorage = {
  get persistence() { return secretAdapter().persistence },
  isEncryptionAvailable: () => secretAdapter().isEncryptionAvailable(),
  encryptString: value => secretAdapter().encryptString(value),
  decryptString: value => secretAdapter().decryptString(value),
  getSelectedStorageBackend: () => secretAdapter().getSelectedStorageBackend?.() ?? 'unknown',
}
export type { SecretStorage } from './server-secret-storage'
