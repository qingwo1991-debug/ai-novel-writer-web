import { EventEmitter } from 'node:events'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const spawnMock = vi.hoisted(() => vi.fn())
vi.mock('child_process', () => ({ spawn: spawnMock }))

type Manager = typeof import('../mcp-manager')['mcpManager']
let manager: Manager
let root: string
let home: string
let limits: typeof import('../mcp-manager')['NATIVE_MCP_LIMITS']

function fixtureProcess(options: { hold?: boolean; error?: string; tools?: number; badFinalFrame?: boolean } = {}) {
  const child = new EventEmitter()
  const stdout = new EventEmitter()
  const stderr = new EventEmitter()
  const stdin = Object.assign(new EventEmitter(), {
    destroyed: false,
    writableLength: 0,
    write: vi.fn((raw: string) => {
      const request = JSON.parse(raw)
      if (request.id === undefined || (request.method === 'tools/call' && options.hold)) return true
      const result = request.method === 'tools/list'
        ? { tools: Array.from({ length: options.tools ?? 1 }, (_, index) => ({ name: index ? `fixture-${index}` : 'fixture', description: 'test only', inputSchema: {} })) }
        : request.method === 'resources/list' ? { resources: [] }
          : request.method === 'tools/call' ? { content: [{ type: 'text', text: '你好，fixture' }] } : {}
      queueMicrotask(() => stdout.emit('data', Buffer.from(JSON.stringify({
        jsonrpc: '2.0', id: request.id,
        ...(request.method === 'tools/call' && options.error ? { error: { message: options.error } } : { result }),
      }) + '\n' + (request.method === 'resources/list' && options.badFinalFrame ? 'SECRET_BAD_FRAME\n' : ''))))
      return true
    }),
  })
  return Object.assign(child, { stdout, stderr, stdin, kill: vi.fn() })
}

async function connect(options: Parameters<typeof fixtureProcess>[0] = {}, env?: Record<string, string>) {
  fs.writeFileSync(path.join(home, 'mcp_config.json'), JSON.stringify({ mcpServers: { configured: { command: 'fixture-command', args: ['fixture.js'], ...(env ? { env } : {}) } } }))
  const child = fixtureProcess(options)
  spawnMock.mockReturnValue(child)
  await manager.loadConfig()
  await manager.connect('configured')
  return child
}

beforeEach(async () => {
  vi.resetModules()
  vi.stubEnv('AI_NOVEL_RUNTIME', 'web')
  spawnMock.mockReset()
  root = fs.mkdtempSync(path.join(os.tmpdir(), 'native-mcp-'))
  home = await (await import('../../services/__tests__/global-data-fixture')).prepareGlobalDataFixture(root)
  const module = await import('../mcp-manager')
  manager = module.mcpManager
  limits = module.NATIVE_MCP_LIMITS
})

afterEach(async () => {
  await manager.disconnectAll()
  vi.useRealTimers()
  vi.unstubAllEnvs()
  vi.restoreAllMocks()
  delete process.env.AI_NOVEL_VELA_HOME
  delete process.env.AI_NOVEL_APP_DATA_HOME
  fs.rmSync(root, { recursive: true, force: true })
})

describe('native MCP transport safeguards (configured servers remain trusted processes)', () => {
  it('only inherits executable/locale essentials, controls HOME/cwd and accepts explicit admin env', async () => {
    vi.stubEnv('HOME', '/SECRET_AUTHOR_HOME')
    vi.stubEnv('OPENAI_API_KEY', 'SECRET_APP_KEY')
    vi.stubEnv('AI_NOVEL_WORKER_PROJECT', '/SECRET_AUTHOR_PROJECT')
    vi.stubEnv('NODE_OPTIONS', '--require SECRET_NODE_INJECTION')
    vi.stubEnv('LD_PRELOAD', 'SECRET_LIBRARY_INJECTION')
    vi.stubEnv('HTTPS_PROXY', 'http://user:SECRET_PROXY_PASSWORD@example.invalid')
    vi.stubEnv('PATH', '/fixture/bin')
    vi.stubEnv('LANG', 'zh_CN.UTF-8')
    await connect({}, { SERVER_API_KEY: 'EXPLICIT_SERVER_KEY' })
    const options = spawnMock.mock.calls[0]![2]
    expect(options.cwd).toBe(path.join(home, 'mcp-runtime'))
    expect(fs.statSync(options.cwd).isDirectory()).toBe(true)
    expect(options.env).toEqual(expect.objectContaining({ PATH: '/fixture/bin', LANG: 'zh_CN.UTF-8', HOME: options.cwd, SERVER_API_KEY: 'EXPLICIT_SERVER_KEY' }))
    expect(JSON.stringify(options.env)).not.toMatch(/SECRET_|AI_NOVEL_RUNTIME/u)
    expect(options.env).not.toHaveProperty('NODE_OPTIONS')
    expect(options.env).not.toHaveProperty('LD_PRELOAD')
    expect(options.env).not.toHaveProperty('AI_NOVEL_APP_DATA_HOME')
  })

  it('leaves desktop inheritance, cwd, tolerant stdout, unknown tool and stderr diagnostics compatible', async () => {
    vi.stubEnv('AI_NOVEL_RUNTIME', 'desktop')
    vi.stubEnv('HOME', '/DESKTOP_HOME')
    vi.stubEnv('NODE_OPTIONS', '--no-warnings')
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const child = await connect()
    expect(spawnMock.mock.calls[0]![2].env).toEqual(expect.objectContaining({ HOME: '/DESKTOP_HOME', NODE_OPTIONS: '--no-warnings' }))
    expect(spawnMock.mock.calls[0]![2]).not.toHaveProperty('cwd')
    child.stdout.emit('data', Buffer.from('legacy desktop debug output\n'))
    expect(child.kill).not.toHaveBeenCalled()
    child.stderr.emit('data', Buffer.from('desktop diagnostic'))
    expect(warn).toHaveBeenCalledWith('[MCP:configured] stderr:', 'desktop diagnostic')
    await expect(manager.callTool('configured', 'legacy-undiscovered-name', {})).resolves.toMatchObject({ success: true })
  })

  it('preserves intentional administrator environment overrides instead of treating config as browser input', async () => {
    await connect({}, { HOME: '/explicit-admin-home', NODE_OPTIONS: '--no-warnings', LD_PRELOAD: '/explicit-admin-library' })
    expect(spawnMock.mock.calls[0]![2].env).toEqual(expect.objectContaining({ HOME: '/explicit-admin-home', NODE_OPTIONS: '--no-warnings', LD_PRELOAD: '/explicit-admin-library' }))
  })

  it('gates tool calls to names returned by discovery, without writing unknown calls', async () => {
    const child = await connect()
    const writes = child.stdin.write.mock.calls.length
    await expect(manager.callTool('configured', 'not-discovered', {})).resolves.toEqual({ success: false, content: '', error: 'MCP_TOOL_NOT_DISCOVERED' })
    expect(child.stdin.write).toHaveBeenCalledTimes(writes)
    await expect(manager.callTool('configured', 'fixture', {})).resolves.toMatchObject({ success: true, content: '你好，fixture' })
  })

  it('caps serialized UTF-8 input, rejects cyclic input, and does not leave timeout timers', async () => {
    vi.useFakeTimers()
    const child = await connect()
    expect(vi.getTimerCount()).toBe(0)
    const writes = child.stdin.write.mock.calls.length
    await expect(manager.callTool('configured', 'fixture', { text: '中'.repeat(Math.ceil(limits.requestBytes / 3)) })).resolves.toMatchObject({ success: false, error: 'MCP_REQUEST_TOO_LARGE' })
    const cyclic: Record<string, unknown> = {}; cyclic.self = cyclic
    await expect(manager.callTool('configured', 'fixture', cyclic)).resolves.toMatchObject({ success: false, error: 'MCP_REQUEST_INVALID' })
    expect(child.stdin.write).toHaveBeenCalledTimes(writes)
    expect(vi.getTimerCount()).toBe(0)
  })

  it('caps pending requests and clears all pending timers on disconnect', async () => {
    vi.useFakeTimers()
    const child = await connect({ hold: true })
    const pending = Array.from({ length: limits.pendingRequests }, () => manager.callTool('configured', 'fixture', {}))
    const writes = child.stdin.write.mock.calls.length
    await expect(manager.callTool('configured', 'fixture', {})).resolves.toMatchObject({ success: false, error: 'MCP_PENDING_LIMIT_EXCEEDED' })
    expect(child.stdin.write).toHaveBeenCalledTimes(writes)
    expect(vi.getTimerCount()).toBe(limits.pendingRequests)
    await manager.disconnect('configured')
    expect((await Promise.all(pending)).every(result => result.error === 'MCP_SERVER_NOT_CONNECTED')).toBe(true)
    expect(vi.getTimerCount()).toBe(0)
  })

  it('rejects writes when the stdin queue would exceed its bounded budget', async () => {
    const child = await connect()
    child.stdin.writableLength = limits.queuedInputBytes
    const writes = child.stdin.write.mock.calls.length
    await expect(manager.callTool('configured', 'fixture', {})).resolves.toMatchObject({ success: false, error: 'MCP_INPUT_LIMIT_EXCEEDED' })
    expect(child.stdin.write).toHaveBeenCalledTimes(writes)
  })

  it.each([
    { name: 'invalid JSON', data: Buffer.from('SECRET_STDOUT_NOT_JSON\n'), code: 'MCP_STDOUT_INVALID' },
    { name: 'invalid JSON-RPC envelope', data: Buffer.from('[]\n'), code: 'MCP_STDOUT_INVALID' },
    { name: 'invalid UTF-8', data: Buffer.from([0xff, 10]), code: 'MCP_STDOUT_INVALID' },
  ])('fails closed on $name and settles outstanding requests without exposing server text', async ({ data, code }) => {
    const child = await connect({ hold: true })
    const pending = manager.callTool('configured', 'fixture', {})
    child.stdout.emit('data', data)
    await expect(pending).resolves.toEqual({ success: false, content: '', error: code })
    expect(child.kill).toHaveBeenCalledOnce()
    expect(manager.getServersStatus()).toMatchObject([{ status: 'error', error: code, toolCount: 0 }])
    expect(manager.getAllTools()).toEqual([])
  })

  it('bounds unterminated stdout across chunks before concatenation and ignores output after failure', async () => {
    const child = await connect()
    child.stdout.emit('data', Buffer.alloc(limits.stdoutBytes, 32))
    expect(child.kill).not.toHaveBeenCalled()
    child.stdout.emit('data', Buffer.from('x'))
    expect(child.kill).toHaveBeenCalledOnce()
    expect(manager.getServersStatus()).toMatchObject([{ status: 'error', error: 'MCP_STDOUT_LIMIT_EXCEEDED' }])
    child.stdout.emit('data', Buffer.alloc(limits.stdoutBytes, 32))
    expect(child.kill).toHaveBeenCalledOnce()
  })

  it('preserves UTF-8 split across chunks and uses safe response errors', async () => {
    const child = await connect({ hold: true })
    const pending = manager.callTool('configured', 'fixture', {})
    const request = JSON.parse(child.stdin.write.mock.calls.at(-1)![0])
    const response = Buffer.from(JSON.stringify({ jsonrpc: '2.0', id: request.id, result: { content: [{ type: 'text', text: '中' }] } }) + '\n')
    const split = response.indexOf(Buffer.from('中')) + 1
    child.stdout.emit('data', response.subarray(0, split))
    child.stdout.emit('data', response.subarray(split))
    await expect(pending).resolves.toMatchObject({ success: true, content: '中' })
    await manager.disconnectAll()
    await connect({ error: 'SECRET_SERVER_ERROR' })
    await expect(manager.callTool('configured', 'fixture', {})).resolves.toEqual({ success: false, content: '', error: 'MCP_REQUEST_FAILED' })
  })

  it('drains but never logs raw stderr in native runtime', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const error = vi.spyOn(console, 'error').mockImplementation(() => {})
    const child = await connect()
    child.stderr.emit('data', Buffer.from('SECRET_STDERR_API_KEY'))
    expect(warn).not.toHaveBeenCalled()
    expect(error).not.toHaveBeenCalled()
  })

  it('does not revive a transport failed by a malformed frame beside the last discovery response', async () => {
    await expect(connect({ badFinalFrame: true })).rejects.toThrow('MCP_STDOUT_INVALID')
    expect(manager.getServersStatus()).toMatchObject([{ status: 'error', error: 'MCP_STDOUT_INVALID', toolCount: 0 }])
    expect(manager.getAllTools()).toEqual([])
  })

  it('bounds discovered tool entries and leaves remote SSE unsupported', async () => {
    await expect(connect({ tools: limits.discoveredEntries + 1 })).rejects.toThrow('MCP_STDOUT_INVALID')
    fs.writeFileSync(path.join(home, 'mcp_config.json'), JSON.stringify({ mcpServers: { remote: { url: 'https://fixture.invalid/sse' } } }))
    await manager.loadConfig()
    spawnMock.mockClear()
    await expect(manager.connect('remote')).rejects.toThrow('MCP_CONNECT_FAILED')
    expect(spawnMock).not.toHaveBeenCalled()
  })
})
