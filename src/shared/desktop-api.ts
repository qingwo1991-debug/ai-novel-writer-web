/** Explicit renderer runtime; a browser backend is not an Electron preload bridge. */
export type RuntimeKind = 'electron' | 'web' | 'unavailable'

/** Shared renderer transport contract. Project authorization remains on the backend. */
export interface RendererAPI {
  invoke: (channel: string, ...args: unknown[]) => Promise<unknown>
  on: (channel: string, callback: (...args: unknown[]) => void) => () => void
  once: (channel: string, callback: (...args: unknown[]) => void) => void
  send: (channel: string, ...args: unknown[]) => void
  setZoomLevel: (level: number) => void
  setZoomFactor: (factor: number) => void
  getZoomLevel: () => number
}

/** The canonical desktop bridge exposed by Electron preload, never synthesized in a browser. */
export type AiNovelAPI = RendererAPI
