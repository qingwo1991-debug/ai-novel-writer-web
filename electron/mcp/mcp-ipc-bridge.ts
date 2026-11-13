/**
 * MCP IPC 桥接
 *
 * 在 Electron 主进程注册 MCP 相关的 IPC 处理器，
 * 让渲染进程能够通过 IPC 管理和调用 MCP 服务器。
 */

import { operations } from '../platform'
import { mcpManager } from './mcp-manager'

/**
 * 注册所有 MCP IPC 处理器
 * 在 main.ts 中调用
 */
export function registerMCPHandlers(): void {
  // 加载配置文件
  operations.handle('mcp:load-config', async () => {
    try {
      const result = await mcpManager.loadConfig()
      if (result.status === 'error') return { success: false, ...result }
      return { success: true, ...result }
    } catch {
      return {
        success: false,
        status: 'error' as const,
        servers: [],
        error: 'MCP_CONFIG_INVALID',
      }
    }
  })

  // 连接服务器
  operations.handle('mcp:connect', async (_event, serverId: string) => {
    try {
      await mcpManager.connect(serverId)
      return { success: true }
    } catch {
      return { success: false, error: 'MCP_CONNECT_FAILED' }
    }
  })

  // 断开服务器
  operations.handle('mcp:disconnect', async (_event, serverId: string) => {
    try {
      await mcpManager.disconnect(serverId)
      return { success: true }
    } catch (error) {
      return { success: false, error: String(error) }
    }
  })

  // 断开所有
  operations.handle('mcp:disconnect-all', async () => {
    try {
      await mcpManager.disconnectAll()
      return { success: true }
    } catch (error) {
      return { success: false, error: String(error) }
    }
  })

  // 获取所有可用 Tool
  operations.handle('mcp:list-tools', async () => {
    return mcpManager.getAllTools()
  })

  // 获取所有可用资源
  operations.handle('mcp:list-resources', async () => {
    return mcpManager.getAllResources()
  })

  // 调用 MCP Tool
  operations.handle('mcp:call-tool', async (_event, serverId: string, toolName: string, args: Record<string, unknown>) => {
    return await mcpManager.callTool(serverId, toolName, args)
  })

  // 获取服务器状态
  operations.handle('mcp:get-servers-status', async () => {
    return mcpManager.getServersStatus()
  })

  // 获取默认配置文件路径
  operations.handle('mcp:get-config-path', async () => {
    return mcpManager.getDefaultConfigPath()
  })
}
