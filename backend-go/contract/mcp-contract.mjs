// GitHub-only differential oracle: imports the ACTUAL original manager and
// replaces only its filesystem locator and external-process transport boundary.
// There is no MCP SDK dependency, package install, external process or author data.
import assert from 'node:assert/strict'
import { EventEmitter } from 'node:events'
import { mkdirSync, writeFileSync } from 'node:fs'
import { registerHooks, stripTypeScriptTypes } from 'node:module'
import path from 'node:path'
import { readFileSync } from 'node:fs'

assert.equal(process.env.GITHUB_ACTIONS, 'true')
assert.equal(process.argv[2], 'generate')
const output = process.env.GO_MCP_CONTRACT
const base = process.env.RUNNER_TEMP
assert(output && base && path.isAbsolute(output))
assert.equal(path.resolve(output), path.join(path.resolve(base), 'novel-go-mcp', 'contract.json'))
const directory = path.join(path.dirname(output), 'original-synthetic')
mkdirSync(directory, { recursive: true, mode: 0o700 })
process.env.AI_NOVEL_RUNTIME = 'web'
const tools = [{ name: 'fixture', description: 'public synthetic', inputSchema: { type: 'object' } }]
const resources = [{ uri: 'fixture://public', name: 'fixture-resource', mimeType: 'text/plain' }]
const methods = []
globalThis.__novelMCPOracle = {
  home: directory,
  spawn(command, args, options) {
    assert.equal(command, 'PUBLIC_SYNTHETIC_TOOL_NEVER_EXECUTED')
    assert.deepEqual(args, [])
    assert.equal(options.env.PUBLIC_EXPLICIT_ENV, 'public-value')
    const child = new EventEmitter()
    child.stdout = new EventEmitter()
    child.stderr = new EventEmitter()
    child.stdin = Object.assign(new EventEmitter(), {
      destroyed: false,
      writableLength: 0,
      write(raw) {
        const request = JSON.parse(raw)
        methods.push(request.method)
        if (request.id === undefined) return true
        const result = request.method === 'tools/list' ? { tools }
          : request.method === 'resources/list' ? { resources }
            : request.method === 'tools/call' ? { content: [{ type: 'text', text: '你好，fixture' }, { type: 'image', data: 'ignored' }, { type: 'text', text: 'second' }] } : {}
        queueMicrotask(() => child.stdout.emit('data', Buffer.from(JSON.stringify({ jsonrpc: '2.0', id: request.id, result }) + '\n')))
        return true
      },
    })
    child.kill = () => { child.stdin.destroyed = true; queueMicrotask(() => child.emit('exit', 0)); return true }
    return child
  },
}
registerHooks({
  resolve(specifier, context, nextResolve) {
    if (context.parentURL?.endsWith('/electron/mcp/mcp-manager.ts')) {
      if (specifier === 'child_process') return { url: 'novel-mcp-oracle:spawn', shortCircuit: true }
      if (specifier === '../utils/config-utils') return { url: 'novel-mcp-oracle:config', shortCircuit: true }
      if (specifier === '../services/app-data-locator') return { url: 'novel-mcp-oracle:locator', shortCircuit: true }
    }
    return nextResolve(specifier, context)
  },
  load(url, context, nextLoad) {
    if (url === 'novel-mcp-oracle:spawn') return { format: 'module', source: 'export const spawn = (...args) => globalThis.__novelMCPOracle.spawn(...args)', shortCircuit: true }
    if (url === 'novel-mcp-oracle:config') return { format: 'module', source: 'export const VELA_HOME = globalThis.__novelMCPOracle.home', shortCircuit: true }
    if (url === 'novel-mcp-oracle:locator') return { format: 'module', source: 'export function assertGlobalDataReady() {}', shortCircuit: true }
    if (url.endsWith('.ts')) return { format: 'module', source: stripTypeScriptTypes(readFileSync(new URL(url), 'utf8'), { mode: 'transform', sourceUrl: url }), shortCircuit: true }
    return nextLoad(url, context)
  },
})
const { mcpManager: original } = await import('../../electron/mcp/mcp-manager.ts')
const valid = [
  { mcpServers: {} },
  { mcpServers: { a: { command: 'fixture', args: ['--public'], env: { TOKEN: 'public-fixture' } }, b: { url: 'https://example.invalid/sse' } } },
  { mcpServers: { z: { command: 'fixture' }, a: { url: 'http://127.0.0.1:12345/sse' } } },
]
const invalid = [null, [], {}, { mcpServers: null }, { mcpServers: [] }, { mcpServers: { a: null } }, { mcpServers: { a: {} } }, { mcpServers: { a: { command: 'x', url: 'https://example.invalid' } } }, { mcpServers: { a: { command: 'x', args: [1] } } }, { mcpServers: { a: { command: 'x', env: { K: 1 } } } }]
const configCases = []
for (const input of [...valid, ...invalid]) {
  const raw = JSON.stringify(input)
  writeFileSync(path.join(directory, 'mcp_config.json'), raw, { mode: 0o600 })
  const result = await original.loadConfig()
  result.servers.sort((a, b) => a.id.localeCompare(b.id))
  configCases.push({ raw, result })
}
writeFileSync(path.join(directory, 'mcp_config.json'), JSON.stringify({ mcpServers: { fixture: { command: 'PUBLIC_SYNTHETIC_TOOL_NEVER_EXECUTED', env: { PUBLIC_EXPLICIT_ENV: 'public-value' } } } }), { mode: 0o600 })
await original.loadConfig()
await original.connect('fixture')
const catalog = { tools: original.getAllTools(), resources: original.getAllResources(), statuses: original.getServersStatus() }
const calls = {
  known: await original.callTool('fixture', 'fixture', {}),
  unknown: await original.callTool('fixture', 'not-discovered', {}),
  disconnected: await original.callTool('missing', 'fixture', {}),
}
await original.disconnectAll()
const disconnected = { tools: original.getAllTools(), resources: original.getAllResources(), statuses: original.getServersStatus() }
writeFileSync(output, JSON.stringify({ formatVersion: 1, provenance: 'actual electron/mcp/mcp-manager.ts with synthetic spawn and locator only', originalSSE: 'unsupported; native SSE is extension', configCases, catalog, calls, disconnected, methods }) + '\n', { mode: 0o600, flag: 'wx' })
console.log('Generated actual original MCP config/catalog/lifecycle differential oracle (synthetic only)')
