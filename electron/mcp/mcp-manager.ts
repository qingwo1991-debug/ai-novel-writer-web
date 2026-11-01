/**
 * MCP（Model Context Protocol）连接管理器
 *
 * 运行在 Electron 主进程，通过 stdio/SSE 管理与外部 MCP Server 的连接。
 * 兼容 Claude Desktop 的配置格式 (claude_desktop_config.json)。
 *
 * 架构：
 * - 主进程负责 MCP 连接的生命周期（启动子进程 / 建立 SSE）
 * - 通过 IPC 将可用 Tool 列表暴露给渲染进程
 * - 渲染进程通过 IPC 调用 MCP Tool
 */

import { spawn, type ChildProcess } from 'child_process'
import { mkdir, readFile } from 'fs/promises'
import { join } from 'path'
import { TextDecoder } from 'node:util'
import { isDeepStrictEqual } from 'node:util'
import { VELA_HOME } from '../utils/config-utils'
import { assertGlobalDataReady } from '../services/app-data-locator'
import type {
  MCPConfigLoadResult,
  MCPConnectionStatus,
  MCPResourceDescription,
  MCPServerStatus,
  MCPServerSummary,
  MCPToolDescription,
} from '../../src/shared/ipc-channels'

// Native HTTP workers invoke only administrator-configured servers. These
// transport limits are not an OS sandbox: children retain the worker identity.
export const NATIVE_MCP_LIMITS = Object.freeze({
  requestBytes: 1024 * 1024,
  pendingRequests: 32,
  stdoutBytes: 4 * 1024 * 1024,
  queuedInputBytes: 4 * 1024 * 1024,
  discoveredEntries: 512,
})

const NATIVE_INHERITED_ENV = [
  'PATH', 'LANG', 'LC_ALL', 'LC_CTYPE', 'TZ',
  'SystemRoot', 'WINDIR', 'PATHEXT', 'COMSPEC',
] as const
const NATIVE_ERROR_CODES = new Set([
  'MCP_CONNECT_FAILED', 'MCP_SERVER_NOT_CONNECTED', 'MCP_REQUEST_FAILED',
  'MCP_REQUEST_TIMEOUT', 'MCP_REQUEST_TOO_LARGE', 'MCP_REQUEST_INVALID',
  'MCP_PENDING_LIMIT_EXCEEDED', 'MCP_INPUT_LIMIT_EXCEEDED',
  'MCP_STDOUT_LIMIT_EXCEEDED', 'MCP_STDOUT_INVALID', 'MCP_STDIO_WRITE_FAILED',
])

// ===== 类型定义 =====

/** MCP 服务器配置（兼容 Claude Desktop 格式） */
export interface MCPServerConfig {
  /** 服务器唯一 ID */
  id: string
  /** 显示名称 */
  name: string
  /** 传输协议 */
  transport: 'stdio' | 'sse'
  /** stdio 模式：要执行的命令 */
  command?: string
  /** stdio 模式：命令参数 */
  args?: string[]
  /** stdio 模式：环境变量 */
  env?: Record<string, string>
  /** SSE 模式：服务器 URL */
  url?: string
}

/** MCP 配置文件格式（兼容 Claude Desktop） */
export interface MCPConfig {
  mcpServers: Record<string, {
    command?: string
    args?: string[]
    env?: Record<string, string>
    url?: string
  }>
}

/** MCP Tool 描述 */
export interface MCPToolDesc extends MCPToolDescription {
  /** 工具名（MCP 原始名称） */
  name: string
  /** 描述 */
  description: string
  /** 输入 JSON Schema */
  inputSchema: Record<string, unknown>
  /** 所属服务器 ID */
  serverId: string
}

/** MCP 资源描述 */
export interface MCPResourceDesc extends MCPResourceDescription {
  uri: string
  name: string
  description?: string
  mimeType?: string
  serverId: string
}

/** 服务器连接状态 */
/** 服务器运行时状态 */
interface MCPServerRuntime {
  config: MCPServerConfig
  status: MCPConnectionStatus
  process?: ChildProcess
  tools: MCPToolDesc[]
  resources: MCPResourceDesc[]
  error?: string
  /** 消息缓冲区（用于解析 JSON-RPC） */
  buffer: string
  /** Native HTTP safeguards must not change the desktop stdio contract. */
  native: boolean
  decoder: TextDecoder
  /** 请求 ID 计数器 */
  nextRequestId: number
  /** 待响应的请求回调 */
  pendingRequests: Map<number, {
    resolve: (result: unknown) => void
    reject: (error: Error) => void
  }>
}

// ===== MCP Manager 实现 =====

class MCPManagerImpl {
  private servers: Map<string, MCPServerRuntime> = new Map()
  private loadedConfigs: Map<string, MCPServerConfig> = new Map()

  /** 状态变更通知回调（通知渲染进程） */
  private onStatusChange?: (serverId: string, status: MCPConnectionStatus, error?: string) => void
  /** Tool 列表变更回调 */
  private onToolsChange?: (tools: MCPToolDesc[]) => void

  /** 设置状态变更回调 */
  setCallbacks(callbacks: {
    onStatusChange?: (serverId: string, status: MCPConnectionStatus, error?: string) => void
    onToolsChange?: (tools: MCPToolDesc[]) => void
  }) {
    this.onStatusChange = callbacks.onStatusChange
    this.onToolsChange = callbacks.onToolsChange
  }

  /** 获取 MCP 配置文件默认路径 */
  getDefaultConfigPath(): string {
    assertGlobalDataReady()
    return join(VELA_HOME, 'mcp_config.json')
  }

  /**
   * 加载 MCP 配置文件
   * 兼容 Claude Desktop 格式
   */
  async loadConfig(): Promise<MCPConfigLoadResult> {
    const path = this.getDefaultConfigPath()
    this.loadedConfigs.clear()
    const revoke = async (result: MCPConfigLoadResult): Promise<MCPConfigLoadResult> => {
      await this.disconnectAll()
      return result
    }
    try {
      const raw = await readFile(path, 'utf-8')
      const parsed: unknown = JSON.parse(raw)

      if (
        !parsed
        || typeof parsed !== 'object'
        || !('mcpServers' in parsed)
        || !parsed.mcpServers
        || typeof parsed.mcpServers !== 'object'
        || Array.isArray(parsed.mcpServers)
      ) {
        return revoke({ status: 'error', servers: [], error: 'MCP_CONFIG_INVALID' })
      }

      const servers: MCPServerSummary[] = []
      const trustedConfigs = new Map<string, MCPServerConfig>()
      for (const [id, value] of Object.entries(parsed.mcpServers)) {
        if (!value || typeof value !== 'object' || Array.isArray(value)) {
          return revoke({ status: 'error', servers: [], error: 'MCP_CONFIG_INVALID' })
        }
        const cfg = value as MCPConfig['mcpServers'][string]
        if (
          (cfg.command !== undefined && typeof cfg.command !== 'string')
          || (cfg.args !== undefined && (!Array.isArray(cfg.args) || cfg.args.some(arg => typeof arg !== 'string')))
          || (cfg.env !== undefined && (
            !cfg.env
            || typeof cfg.env !== 'object'
            || Array.isArray(cfg.env)
            || Object.values(cfg.env).some(entry => typeof entry !== 'string')
          ))
          || (cfg.url !== undefined && typeof cfg.url !== 'string')
        ) {
          return revoke({ status: 'error', servers: [], error: 'MCP_CONFIG_INVALID' })
        }
        const hasCommand = typeof cfg.command === 'string' && cfg.command.trim().length > 0
        const hasUrl = typeof cfg.url === 'string' && cfg.url.trim().length > 0
        if (hasCommand === hasUrl) {
          return revoke({ status: 'error', servers: [], error: 'MCP_CONFIG_INVALID' })
        }
        const trustedConfig: MCPServerConfig = {
          id,
          name: id,
          transport: hasUrl ? 'sse' : 'stdio',
          command: cfg.command,
          args: cfg.args,
          env: cfg.env,
          url: cfg.url,
        }
        trustedConfigs.set(id, trustedConfig)
        servers.push({ id, name: id, transport: trustedConfig.transport })
      }
      for (const [id, runtime] of this.servers) {
        const trustedConfig = trustedConfigs.get(id)
        if (!trustedConfig || !isDeepStrictEqual(runtime.config, trustedConfig)) {
          await this.disconnect(id)
        }
      }
      this.loadedConfigs = trustedConfigs
      return { status: 'loaded', servers }
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === 'ENOENT') {
        return revoke({ status: 'missing', servers: [] })
      }
      return revoke({ status: 'error', servers: [], error: 'MCP_CONFIG_INVALID' })
    }
  }

  /**
   * 连接到 MCP 服务器
   */
  async connect(serverId: string): Promise<void> {
    const config = this.loadedConfigs.get(serverId)
    if (!config) throw new Error('MCP_SERVER_NOT_CONFIGURED')
    if (this.servers.has(config.id)) {
      await this.disconnect(config.id)
    }

    const runtime: MCPServerRuntime = {
      config,
      status: 'connecting',
      tools: [],
      resources: [],
      buffer: '',
      native: process.env.AI_NOVEL_RUNTIME === 'web',
      decoder: new TextDecoder('utf-8', { fatal: true }),
      nextRequestId: 1,
      pendingRequests: new Map(),
    }
    this.servers.set(config.id, runtime)
    this.notifyStatusChange(config.id, 'connecting')

    try {
      if (config.transport === 'stdio') {
        await this.connectStdio(runtime)
      } else {
        throw new Error('MCP_SSE_UNSUPPORTED')
      }

      // 初始化 MCP 会话
      await this.initializeSession(runtime)

      // 发现工具
      await this.discoverTools(runtime)

      // 发现资源
      await this.discoverResources(runtime)
      // A response and malformed trailing frame can arrive in one stdout tick;
      // never overwrite a fatal native transport error after the await resumes.
      if (runtime.native && runtime.error) throw new Error(runtime.error)

      runtime.status = 'connected'
      this.notifyStatusChange(config.id, 'connected')
      this.notifyToolsChange()
    } catch {
      const code = runtime.native && runtime.error ? runtime.error : 'MCP_CONNECT_FAILED'
      this.failRuntime(runtime, code)
      throw new Error(code)
    }
  }

  private failRuntime(runtime: MCPServerRuntime, code: string): void {
    if (runtime.status === 'error') return
    runtime.status = 'error'
    runtime.error = code
    runtime.buffer = ''
    runtime.tools = []
    runtime.resources = []
    runtime.pendingRequests.forEach(p => p.reject(new Error(code)))
    runtime.pendingRequests.clear()
    runtime.process?.kill()
    this.notifyStatusChange(runtime.config.id, 'error', code)
    this.notifyToolsChange()
  }

  /** stdio 模式连接 */
  private async connectStdio(runtime: MCPServerRuntime): Promise<void> {
    const { command, args = [], env } = runtime.config
    if (!command) {
      throw new Error('MCP_STDIO_COMMAND_REQUIRED')
    }

    let childEnv: Record<string, string | undefined> = { ...process.env, ...env }
    let nativeCwd: string | undefined
    if (runtime.native) {
      nativeCwd = join(VELA_HOME, 'mcp-runtime')
      await mkdir(nativeCwd, { recursive: true, mode: 0o700 })
      const inherited: Record<string, string | undefined> = {}
      for (const key of NATIVE_INHERITED_ENV) {
        if (process.env[key] !== undefined) inherited[key] = process.env[key]
      }
      // Never implicitly expose the author's HOME, app secrets, injection flags,
      // proxy credentials or worker bootstrap paths. Explicit administrator env
      // remains compatible, including credentials intentionally given a server.
      childEnv = { ...inherited, HOME: nativeCwd, ...env }
    }
    const proc = spawn(command, args, {
      // Vite augments ProcessEnv with renderer-only required keys. They are
      // intentionally absent from native children, not inherited to satisfy TS.
      env: childEnv as NodeJS.ProcessEnv,
      ...(nativeCwd ? { cwd: nativeCwd } : {}),
      stdio: ['pipe', 'pipe', 'pipe'],
      windowsHide: true,
    })

    runtime.process = proc

    // Native framing is byte-bounded before decoding/concatenation. A fatal
    // streaming decoder also preserves UTF-8 code points split across chunks.
    proc.stdout?.on('data', (data: Buffer) => {
      if (runtime.status === 'error' || runtime.status === 'disconnected') return
      if (runtime.native) {
        if (data.byteLength + Buffer.byteLength(runtime.buffer, 'utf8') > NATIVE_MCP_LIMITS.stdoutBytes) {
          this.failRuntime(runtime, 'MCP_STDOUT_LIMIT_EXCEEDED')
          return
        }
        try {
          runtime.buffer += runtime.decoder.decode(data, { stream: true })
        } catch {
          this.failRuntime(runtime, 'MCP_STDOUT_INVALID')
          return
        }
      } else {
        runtime.buffer += data.toString()
      }
      this.processBuffer(runtime)
    })
    proc.stdout?.on('end', () => {
      if (!runtime.native || runtime.status === 'error' || runtime.status === 'disconnected') return
      try {
        runtime.buffer += runtime.decoder.decode()
        if (runtime.buffer.trim()) this.failRuntime(runtime, 'MCP_STDOUT_INVALID')
      } catch {
        this.failRuntime(runtime, 'MCP_STDOUT_INVALID')
      }
    })

    // Drain native server stderr without persisting credentials, prompts, or
    // emitting unbounded attacker-controlled logs. Desktop diagnostics unchanged.
    proc.stderr?.on('data', (data: Buffer) => {
      if (!runtime.native) console.warn(`[MCP:${runtime.config.id}] stderr:`, data.toString())
    })

    // 监听进程退出
    proc.on('exit', (code) => {
      console.log(`[MCP:${runtime.config.id}] 进程退出，code=${code}`)
      if (runtime.status === 'error') return
      runtime.status = 'disconnected'
      runtime.buffer = ''
      if (runtime.native) {
        runtime.pendingRequests.forEach(p => p.reject(new Error('MCP_SERVER_NOT_CONNECTED')))
        runtime.pendingRequests.clear()
      }
      this.notifyStatusChange(runtime.config.id, 'disconnected')
      this.notifyToolsChange()
    })

    proc.stdin?.on?.('error', () => {
      if (runtime.native) this.failRuntime(runtime, 'MCP_STDIO_WRITE_FAILED')
    })
    proc.on('error', () => {
      if (runtime.native) {
        this.failRuntime(runtime, 'MCP_CONNECT_FAILED')
        return
      }
      if (runtime.status === 'connecting') {
        runtime.pendingRequests.forEach(p => p.reject(new Error('MCP_CONNECT_FAILED')))
        runtime.pendingRequests.clear()
        return
      }
      runtime.status = 'error'
      runtime.error = 'MCP_CONNECT_FAILED'
      this.notifyStatusChange(runtime.config.id, 'error', runtime.error)
    })
  }

  /** 处理 JSON-RPC 消息缓冲区 */
  private processBuffer(runtime: MCPServerRuntime): void {
    // MCP 使用 \n 分隔的 JSON-RPC 消息
    const lines = runtime.buffer.split('\n')
    runtime.buffer = lines.pop() ?? '' // 保留最后一行（可能不完整）

    for (const line of lines) {
      const trimmed = line.trim()
      if (!trimmed) continue

      try {
        const msg: unknown = JSON.parse(trimmed)
        if (runtime.native && (!msg || typeof msg !== 'object' || Array.isArray(msg)
          || (msg as Record<string, unknown>).jsonrpc !== '2.0')) {
          throw new Error('MCP_STDOUT_INVALID')
        }
        this.handleMessage(runtime, msg as Record<string, unknown>)
      } catch {
        if (runtime.native) {
          this.failRuntime(runtime, 'MCP_STDOUT_INVALID')
          return
        }
        // Desktop servers historically tolerate non-JSON stdout diagnostics.
      }
    }
  }

  /** 处理收到的 JSON-RPC 消息 */
  private handleMessage(runtime: MCPServerRuntime, msg: Record<string, unknown>): void {
    // 响应消息（有 id）
    if ('id' in msg && msg.id != null) {
      const pending = runtime.pendingRequests.get(msg.id as number)
      if (pending) {
        runtime.pendingRequests.delete(msg.id as number)
        if ('error' in msg) {
          const err = msg.error as { message?: string }
          pending.reject(new Error(runtime.native ? 'MCP_REQUEST_FAILED' : err?.message ?? 'MCP error'))
        } else {
          pending.resolve(msg.result)
        }
      }
    }
    // 通知消息（无 id）— 暂时只记录日志
  }

  private writeMessage(runtime: MCPServerRuntime, message: string): void {
    const stdin = runtime.process?.stdin
    if (runtime.native) {
      if (!stdin || stdin.destroyed || runtime.status === 'error' || runtime.status === 'disconnected') {
        throw new Error('MCP_SERVER_NOT_CONNECTED')
      }
      const bytes = Buffer.byteLength(message, 'utf8')
      if (bytes > NATIVE_MCP_LIMITS.requestBytes) throw new Error('MCP_REQUEST_TOO_LARGE')
      if ((stdin.writableLength ?? 0) + bytes > NATIVE_MCP_LIMITS.queuedInputBytes) {
        throw new Error('MCP_INPUT_LIMIT_EXCEEDED')
      }
    }
    try {
      stdin?.write(message)
    } catch (error) {
      if (runtime.native) throw new Error('MCP_STDIO_WRITE_FAILED')
      throw error
    }
  }

  /** 发送 JSON-RPC 请求 */
  private sendRequest(runtime: MCPServerRuntime, method: string, params?: unknown): Promise<unknown> {
    if (runtime.native && runtime.pendingRequests.size >= NATIVE_MCP_LIMITS.pendingRequests) {
      return Promise.reject(new Error('MCP_PENDING_LIMIT_EXCEEDED'))
    }
    const id = runtime.nextRequestId++
    let message: string
    try {
      message = JSON.stringify({ jsonrpc: '2.0', id, method, params: params ?? {} }) + '\n'
      if (runtime.native && Buffer.byteLength(message, 'utf8') > NATIVE_MCP_LIMITS.requestBytes) {
        return Promise.reject(new Error('MCP_REQUEST_TOO_LARGE'))
      }
    } catch {
      return Promise.reject(new Error('MCP_REQUEST_INVALID'))
    }
    return new Promise((resolve, reject) => {
      // Clear on every settlement, not only timeouts: fast responses must not
      // accumulate live timers, and disconnect/fatal stdout settles immediately.
      const timer = setTimeout(() => {
        const pending = runtime.pendingRequests.get(id)
        if (pending) {
          runtime.pendingRequests.delete(id)
          console.warn(`[MCP:${runtime.config.id}] request timed out`, { method })
          pending.reject(new Error('MCP_REQUEST_TIMEOUT'))
        }
      }, 10000)
      runtime.pendingRequests.set(id, {
        resolve: result => { clearTimeout(timer); resolve(result) },
        reject: error => { clearTimeout(timer); reject(error) },
      })
      try {
        this.writeMessage(runtime, message)
      } catch (error) {
        runtime.pendingRequests.delete(id)
        clearTimeout(timer)
        reject(error)
      }
    })
  }

  /** 初始化 MCP 会话 */
  private async initializeSession(runtime: MCPServerRuntime): Promise<void> {
    await this.sendRequest(runtime, 'initialize', {
      protocolVersion: '2024-11-05',
      capabilities: {},
      clientInfo: {
        name: 'vela',
        version: '1.0.0',
      },
    })

    // 发送 initialized 通知
    const msg = JSON.stringify({
      jsonrpc: '2.0',
      method: 'notifications/initialized',
    })
    this.writeMessage(runtime, msg + '\n')
  }

  /** 发现可用工具 */
  private async discoverTools(runtime: MCPServerRuntime): Promise<void> {
    try {
      const result = await this.sendRequest(runtime, 'tools/list') as { tools?: MCPToolDesc[] }
      if (runtime.native && (result?.tools !== undefined && (!Array.isArray(result.tools)
        || result.tools.length > NATIVE_MCP_LIMITS.discoveredEntries
        || result.tools.some(t => !t || typeof t.name !== 'string' || !t.name.trim())))) {
        this.failRuntime(runtime, 'MCP_STDOUT_INVALID')
        throw new Error('MCP_STDOUT_INVALID')
      }
      runtime.tools = (result?.tools ?? []).map(t => ({
        ...t,
        serverId: runtime.config.id,
      }))
    } catch (error) {
      runtime.tools = []
      if (runtime.native && runtime.status === 'error') throw error
    }
  }

  /** 发现可用资源 */
  private async discoverResources(runtime: MCPServerRuntime): Promise<void> {
    try {
      const result = await this.sendRequest(runtime, 'resources/list') as { resources?: MCPResourceDesc[] }
      if (runtime.native && (result?.resources !== undefined && (!Array.isArray(result.resources)
        || result.resources.length > NATIVE_MCP_LIMITS.discoveredEntries))) {
        this.failRuntime(runtime, 'MCP_STDOUT_INVALID')
        throw new Error('MCP_STDOUT_INVALID')
      }
      runtime.resources = (result?.resources ?? []).map(r => ({
        ...r,
        serverId: runtime.config.id,
      }))
    } catch (error) {
      runtime.resources = []
      if (runtime.native && runtime.status === 'error') throw error
    }
  }

  /**
   * 调用 MCP Tool
   */
  async callTool(serverId: string, toolName: string, args: Record<string, unknown>): Promise<{
    success: boolean
    content: string
    error?: string
  }> {
    const runtime = this.servers.get(serverId)
    if (!runtime || runtime.status !== 'connected') {
      return { success: false, content: '', error: 'MCP_SERVER_NOT_CONNECTED' }
    }

    if (runtime.native && !runtime.tools.some(tool => tool.name === toolName)) {
      return { success: false, content: '', error: 'MCP_TOOL_NOT_DISCOVERED' }
    }
    try {
      const result = await this.sendRequest(runtime, 'tools/call', {
        name: toolName,
        arguments: args,
      }) as { content?: Array<{ type: string; text?: string }> }

      const textParts = (result?.content ?? [])
        .filter(c => c.type === 'text')
        .map(c => c.text ?? '')
        .join('\n')

      return { success: true, content: textParts }
    } catch (error) {
      const code = error instanceof Error && NATIVE_ERROR_CODES.has(error.message)
        ? error.message : 'MCP_REQUEST_FAILED'
      return { success: false, content: '', error: runtime.native ? code : String(error) }
    }
  }

  /** 断开服务器连接 */
  async disconnect(serverId: string): Promise<void> {
    const runtime = this.servers.get(serverId)
    if (!runtime) return

    runtime.process?.kill()
    runtime.pendingRequests.forEach(p => p.reject(new Error('MCP_SERVER_NOT_CONNECTED')))
    runtime.pendingRequests.clear()
    this.servers.delete(serverId)
    this.notifyStatusChange(serverId, 'disconnected')
    this.notifyToolsChange()
  }

  /** 断开所有连接 */
  async disconnectAll(): Promise<void> {
    for (const id of this.servers.keys()) {
      await this.disconnect(id)
    }
  }

  /** 获取所有 MCP Tool */
  getAllTools(): MCPToolDesc[] {
    const tools: MCPToolDesc[] = []
    for (const runtime of this.servers.values()) {
      if (runtime.status === 'connected') {
        tools.push(...runtime.tools)
      }
    }
    return tools
  }

  /** 获取所有 MCP 资源 */
  getAllResources(): MCPResourceDesc[] {
    const resources: MCPResourceDesc[] = []
    for (const runtime of this.servers.values()) {
      if (runtime.status === 'connected') {
        resources.push(...runtime.resources)
      }
    }
    return resources
  }

  /** 获取所有服务器状态 */
  getServersStatus(): MCPServerStatus[] {
    return Array.from(this.servers.values()).map(r => ({
      id: r.config.id,
      name: r.config.name,
      status: r.status,
      toolCount: r.tools.length,
      error: r.error,
    }))
  }

  private notifyStatusChange(serverId: string, status: MCPConnectionStatus, error?: string) {
    this.onStatusChange?.(serverId, status, error)
  }

  private notifyToolsChange() {
    this.onToolsChange?.(this.getAllTools())
  }
}

/** 全局单例 MCP Manager */
export const mcpManager = new MCPManagerImpl()
