import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import http from 'node:http'
import type { AddressInfo } from 'node:net'
import type { ModelProfile } from '../../../src/shared/ipc-channels'
import { LLMFactory } from '../../llm/llm-factory'
import { embedGemini, embedOpenAI, generateEmbeddings } from '../../embedding'
import { ModelDiscoveryService } from '../../services/model-discovery-service'
import { WebDavBackupService } from '../../services/webdav-backup-service'
import { allowedEndpointOrigins, endpointFetch } from '../endpoint-fetch'

const DEFAULT_ORIGIN = 'http://192.168.1.15:8317'
const model = (provider: ModelProfile['provider'] = 'custom', protocol: ModelProfile['protocol'] = 'openai', baseUrl = DEFAULT_ORIGIN): ModelProfile => ({
  id: 'egress-test', name: 'Egress test', provider, protocol, baseUrl, apiKey: 'secret-test-key',
  modelName: 'test-model', maxTokens: 128, temperature: 0.7, purposes: ['generation'],
})
const generationOptions = { maxTokens: 128, temperature: 0.7 }
const streamOptions = () => ({ ...generationOptions, signal: new AbortController().signal, onChunk: vi.fn(), onDone: vi.fn(), onError: vi.fn() })
const response = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })

beforeEach(() => { vi.stubEnv('AI_NOVEL_RUNTIME', 'web'); vi.stubEnv('NOVEL_ALLOWED_ENDPOINTS', DEFAULT_ORIGIN) })
afterEach(() => { vi.unstubAllEnvs(); vi.unstubAllGlobals() })

describe('native web endpoint dispatch policy', () => {
  it('uses the private gateway default and canonicalizes exact configured origins', async () => {
    expect([...allowedEndpointOrigins({})]).toEqual([DEFAULT_ORIGIN])
    expect([...allowedEndpointOrigins({ NOVEL_ALLOWED_ENDPOINTS: ' https://Gateway.Example:443, http://127.0.0.1:8317/ ' })])
      .toEqual(['https://gateway.example', 'http://127.0.0.1:8317'])
    const dispatch = vi.fn<typeof fetch>().mockResolvedValue(response({}))
    await endpointFetch(`${DEFAULT_ORIGIN}/v1/chat/completions`, { redirect: 'follow' }, dispatch)
    expect(dispatch).toHaveBeenCalledWith(`${DEFAULT_ORIGIN}/v1/chat/completions`, { redirect: 'error' })
  })
  it.each([
    '', 'https://gateway.example/path', 'https://gateway.example?key=private', 'https://gateway.example#fragment',
    'https://user:password@gateway.example', 'ftp://gateway.example', '*.example', 'https://*.example',
    'https://gateway.example,',
  ])('rejects invalid allowlist configuration without network traffic: %s', async configuration => {
    vi.stubEnv('NOVEL_ALLOWED_ENDPOINTS', configuration)
    const dispatch = vi.fn<typeof fetch>()
    await expect(endpointFetch(`${DEFAULT_ORIGIN}/v1/models`, undefined, dispatch)).rejects.toThrow('WEB_ENDPOINT_POLICY_INVALID')
    expect(dispatch).not.toHaveBeenCalled()
  })
  it.each([
    'https://api.openai.com/v1/chat/completions', 'http://192.168.1.15:8318/v1/models',
    'http://192.168.1.15.evil.test:8317/', 'http://192.168.1.15:8317@evil.test/',
    'http://user:secret@192.168.1.15:8317/', 'file:///etc/passwd', 'data:text/plain,secret',
  ])('rejects disallowed actual URLs and does not leak them into errors: %s', async url => {
    const dispatch = vi.fn<typeof fetch>()
    await expect(endpointFetch(url, undefined, dispatch)).rejects.toThrow('WEB_ENDPOINT_NOT_ALLOWED')
    expect(dispatch).not.toHaveBeenCalled()
  })
  it('checks URL and Request objects and prevents a caller override of redirect policy', async () => {
    const dispatch = vi.fn<typeof fetch>().mockImplementation(async () => response({}))
    await endpointFetch(new URL(`${DEFAULT_ORIGIN}/v1/models`), { redirect: 'manual' }, dispatch)
    await endpointFetch(new Request(`${DEFAULT_ORIGIN}/v1/models`, { redirect: 'follow' }), undefined, dispatch)
    for (const [, init] of dispatch.mock.calls) expect(init?.redirect).toBe('error')
    await expect(endpointFetch(new Request('https://evil.test/'), undefined, dispatch)).rejects.toThrow('WEB_ENDPOINT_NOT_ALLOWED')
    expect(dispatch).toHaveBeenCalledTimes(2)
  })
  it('revalidates deployment policy at actual dispatch instead of caching model admission', async () => {
    const dispatch = vi.fn<typeof fetch>().mockImplementation(async () => response({}))
    await endpointFetch(`${DEFAULT_ORIGIN}/v1/models`, undefined, dispatch)
    vi.stubEnv('NOVEL_ALLOWED_ENDPOINTS', 'https://replacement.test')
    await expect(endpointFetch(`${DEFAULT_ORIGIN}/v1/models`, undefined, dispatch)).rejects.toThrow('WEB_ENDPOINT_NOT_ALLOWED')
    expect(dispatch).toHaveBeenCalledOnce()
  })
  it('leaves desktop fetch options and endpoint freedom unchanged', async () => {
    vi.stubEnv('AI_NOVEL_RUNTIME', 'desktop')
    vi.stubEnv('NOVEL_ALLOWED_ENDPOINTS', 'invalid configuration deliberately ignored')
    const dispatch = vi.fn<typeof fetch>().mockResolvedValue(response({}))
    const options: RequestInit = { method: 'POST', redirect: 'follow', body: 'private' }
    await endpointFetch('https://any-provider.test/v1/chat/completions', options, dispatch)
    expect(dispatch).toHaveBeenCalledWith('https://any-provider.test/v1/chat/completions', options)
  })
  it('blocks redirect responses from custom fetch transports too', async () => {
    const dispatch = vi.fn<typeof fetch>().mockResolvedValue(new Response(null, { status: 307, headers: { Location: 'https://evil.test/' } }))
    await expect(endpointFetch(`${DEFAULT_ORIGIN}/v1/models`, undefined, dispatch)).rejects.toThrow('WEB_ENDPOINT_REDIRECT_FORBIDDEN')
  })
  it('never contacts the Location destination for an actual native-fetch redirect', async () => {
    let destinationRequests = 0
    const destination = http.createServer((_request, reply) => { destinationRequests++; reply.end('forbidden') })
    const origin = http.createServer((_request, reply) => { reply.writeHead(307, { Location: `http://127.0.0.1:${(destination.address() as AddressInfo).port}/private` }); reply.end() })
    await new Promise<void>(resolve => destination.listen(0, '127.0.0.1', resolve))
    await new Promise<void>(resolve => origin.listen(0, '127.0.0.1', resolve))
    const allowed = `http://127.0.0.1:${(origin.address() as AddressInfo).port}`
    vi.stubEnv('NOVEL_ALLOWED_ENDPOINTS', allowed)
    try {
      await expect(endpointFetch(`${allowed}/redirect`, { redirect: 'follow' })).rejects.toThrow()
      expect(destinationRequests).toBe(0)
    } finally {
      origin.closeAllConnections(); destination.closeAllConnections()
      await Promise.all([new Promise<void>(resolve => origin.close(() => resolve())), new Promise<void>(resolve => destination.close(() => resolve()))])
    }
  })
})

describe('cross-provider actual dispatch coverage', () => {
  it.each<ModelProfile['provider']>(['openai', 'deepseek', 'ollama', 'bigmodel', 'novelai', 'xai', 'siliconflow', 'custom'])('covers OpenAI-compatible %s generation and streaming without restricting desktop provider mappings', async provider => {
    const dispatch = vi.fn<typeof fetch>().mockImplementation(async () => response({ choices: [{ message: { content: 'ok' }, finish_reason: 'stop' }] }))
    vi.stubGlobal('fetch', dispatch)
    const profile = model(provider), instance = LLMFactory.getProvider(profile)
    await instance.generate(profile, [{ role: 'user', content: 'private manuscript' }], generationOptions)
    await instance.generateStream(profile, [], streamOptions())
    expect(dispatch).toHaveBeenCalledTimes(2)
    for (const [url, options] of dispatch.mock.calls) {
      expect(new URL(String(url)).origin).toBe(DEFAULT_ORIGIN)
      expect(options?.redirect).toBe('error')
    }
    dispatch.mockClear()
    const denied = { ...profile, baseUrl: 'https://denied.example/v1' }
    await instance.generate(denied, [], generationOptions)
    const stream = streamOptions(); await instance.generateStream(denied, [], stream)
    expect(dispatch).not.toHaveBeenCalled()
    expect(stream.onError).toHaveBeenCalled()
  })
  it('covers native Gemini generation and streaming', async () => {
    const dispatch = vi.fn<typeof fetch>().mockImplementation(async () => response({ candidates: [{ content: { parts: [{ text: 'ok' }] }, finishReason: 'STOP' }] }))
    vi.stubGlobal('fetch', dispatch)
    const profile = model('gemini', 'gemini'), instance = LLMFactory.getProvider(profile)
    await instance.generate(profile, [], generationOptions)
    await instance.generateStream(profile, [], streamOptions())
    expect(dispatch).toHaveBeenCalledTimes(2)
    for (const [, options] of dispatch.mock.calls) expect(options?.redirect).toBe('error')
    dispatch.mockClear()
    const denied = { ...profile, baseUrl: 'https://denied.example' }
    await instance.generate(denied, [], generationOptions)
    await instance.generateStream(denied, [], streamOptions())
    expect(dispatch).not.toHaveBeenCalled()
  })
  it.each(['openai', 'gemini'] as const)('covers %s discovery including its injected transport', async protocol => {
    const payload = protocol === 'openai' ? { data: [{ id: 'test-model' }] } : { models: [{ name: 'models/test-model' }] }
    const dispatch = vi.fn<typeof fetch>().mockImplementation(async () => response(payload))
    const service = new ModelDiscoveryService({ fetchImpl: dispatch })
    expect((await service.discoverModels(model(protocol === 'gemini' ? 'gemini' : 'custom', protocol))).success).toBe(true)
    expect(dispatch).toHaveBeenCalledOnce()
    expect(dispatch.mock.calls[0][1]?.redirect).toBe('error')
    dispatch.mockClear()
    await service.discoverModels(model('custom', protocol, 'https://denied.example'))
    expect(dispatch).not.toHaveBeenCalled()
  })
  it.each(['openai', 'gemini'] as const)('covers %s direct embeddings and every batch', async protocol => {
    const dispatch = vi.fn<typeof fetch>().mockImplementation(async () => response(protocol === 'openai'
      ? { data: [{ index: 0, embedding: [0.1, 0.2] }] } : { embeddings: [{ values: [0.1, 0.2] }] }))
    vi.stubGlobal('fetch', dispatch)
    const profile = model('custom', protocol)
    const embed = protocol === 'openai' ? embedOpenAI : embedGemini
    await embed(['private'], profile)
    await generateEmbeddings(['first', 'second'], protocol, profile, 1)
    expect(dispatch).toHaveBeenCalledTimes(3)
    for (const [, options] of dispatch.mock.calls) expect(options?.redirect).toBe('error')
    dispatch.mockClear()
    await expect(embed(['private'], { ...profile, baseUrl: 'https://denied.example' })).rejects.toThrow('WEB_ENDPOINT_NOT_ALLOWED')
    expect(dispatch).not.toHaveBeenCalled()
  })
  it('covers the common WebDAV request path even with an injected transport', async () => {
    vi.stubEnv('NOVEL_ALLOWED_ENDPOINTS', 'https://dav.allowed.test')
    const dispatch = vi.fn<typeof fetch>().mockImplementation(async () => new Response('<multistatus/>', { status: 207 }))
    const service = new WebDavBackupService({ fetchImpl: dispatch })
    await service.checkConnection({ endpoint: 'https://dav.allowed.test/books/', username: 'writer', secret: 'private' })
    expect(dispatch).toHaveBeenCalledOnce()
    expect(dispatch.mock.calls[0][1]?.redirect).toBe('error')
    dispatch.mockClear()
    await expect(service.checkConnection({ endpoint: 'https://dav.denied.test/books/', username: 'writer', secret: 'private' })).rejects.toThrow('WEBDAV_NETWORK')
    expect(dispatch).not.toHaveBeenCalled()
  })
})
