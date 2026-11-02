import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { existsSync, mkdirSync, readFileSync, realpathSync, writeFileSync } from 'node:fs'
import { registerHooks, stripTypeScriptTypes } from 'node:module'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

// CI-only differential oracle, not an application-time Node bridge. All inputs,
// credentials, transport bytes and mapping directives below are PUBLIC fixtures.
assert.equal(process.env.GITHUB_ACTIONS, 'true', 'OpenAI contract generation is GitHub-only')
const base = process.env.RUNNER_TEMP
const target = process.env.GO_OPENAI_CONTRACT
assert(base && target && path.isAbsolute(base) && path.isAbsolute(target))
const within = (root, candidate) => {
  const relative = path.relative(root, candidate)
  return relative !== '' && relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative)
}
assert(within(base, target), 'contract must use the disposable runner directory')
assert(process.argv[2] === undefined || process.argv[2] === 'generate', 'expected generate')
assert(!existsSync(target), 'never overwrite an existing contract')
mkdirSync(path.dirname(target), { recursive: true, mode: 0o700 })
assert(within(realpathSync(base), path.join(realpathSync(path.dirname(target)), path.basename(target))), 'no output-directory symlink escape')

const root = new URL('../../', import.meta.url)
const providerUrl = new URL('electron/llm/openai-provider.ts', root).href
const sourcePaths = [
  'electron/llm/openai-provider.ts',
  'electron/llm/provider.interface.ts',
  'electron/llm/openai-compatible-endpoint.ts',
  'electron/llm/visible-stream.ts',
  'electron/llm/transport-diagnostics.ts',
  'electron/llm/provider-context-length.ts',
  'src/shared/generation-contract.ts',
  'src/shared/source-ref.ts',
  'src/utils/sha256.ts',
]
const sources = new Map(sourcePaths.map(name => {
  const url = new URL(name, root).href
  return [url, { name, source: readFileSync(fileURLToPath(url), 'utf8') }]
}))
const version = JSON.parse(readFileSync(new URL('package.json', root), 'utf8')).version
assert.equal(typeof version, 'string')
const fetchStubUrl = 'openai-contract:endpoint-fetch'
const presetsStubUrl = 'openai-contract:provider-presets'
const packageStubUrl = 'openai-contract:package-json'
const transportKey = '__GO_OPENAI_CONTRACT_SYNTHETIC_FETCH__'
const stubSources = new Map([
  [fetchStubUrl, `export const endpointFetch = (...args) => globalThis[${JSON.stringify(transportKey)}](...args)`],
  // This deliberately DOES NOT validate real presets, inferred capabilities or
  // generation-parameter-policy. It tests provider serialization after mapping.
  [presetsStubUrl, 'export const resolveModelProfileReasoningMapping = model => model.syntheticMapping'],
  [packageStubUrl, `export const version = ${JSON.stringify(version)}`],
])
const runtimeImport = "import { InBandReasoningStream, ILLMProvider, LLMGenerateOptions, LLMResponse, LLMStreamOptions } from './provider.interface'"
assert(sources.get(providerUrl).source.includes(runtimeImport), 'review oracle loader after provider import changes')

// Install BEFORE importing the original provider. No package resolver, npm
// dependency, Electron module or endpoint implementation is loaded. Fail closed
// on unexpected transitive imports instead of accidentally accessing the host.
const hooks = registerHooks({
  resolve(specifier, context) {
    assert(context.parentURL, `unexpected root import: ${specifier}`)
    const url = new URL(specifier, context.parentURL).href
    const withoutExtension = url.endsWith('.ts') ? url.slice(0, -3) : url
    if (withoutExtension === new URL('electron/platform/endpoint-fetch', root).href) {
      return { url: fetchStubUrl, shortCircuit: true }
    }
    if (withoutExtension === new URL('src/shared/provider-presets', root).href) {
      return { url: presetsStubUrl, shortCircuit: true }
    }
    if (url === new URL('package.json', root).href) return { url: packageStubUrl, shortCircuit: true }
    const resolved = sources.has(url) ? url : `${url}.ts`
    assert(sources.has(resolved), `unexpected oracle dependency: ${specifier}`)
    return { url: resolved, shortCircuit: true }
  },
  load(url) {
    if (stubSources.has(url)) return { format: 'module', source: stubSources.get(url), shortCircuit: true }
    assert(sources.has(url), `unexpected oracle module: ${url}`)
    let source = sources.get(url).source
    // Node's syntax-only TS transform cannot infer these four imports are
    // interfaces. Drop ONLY the erased imports, never any runtime provider code.
    if (url === providerUrl) source = source.replace(runtimeImport, "import { InBandReasoningStream } from './provider.interface'")
    // The originals use TS constructor parameter properties, requiring transform
    // rather than strip mode. This is Node's built-in transform, not npm/tsx.
    return { format: 'module', source: stripTypeScriptTypes(source, { mode: 'transform', sourceUrl: url }), shortCircuit: true }
  },
})

// A stray fetch must fail closed as well; only the endpoint-fetch adapter can
// dispatch to the per-case controlled synthetic transport installed below.
const originalFetch = globalThis.fetch
const previousTransport = globalThis[transportKey]
globalThis.fetch = () => { throw new Error('real network is forbidden in the OpenAI contract oracle') }

const model = {
  id: 'public-synthetic-model', name: 'Public synthetic OpenAI fixture',
  provider: 'custom', protocol: 'openai', modelName: 'synthetic-model',
  baseUrl: 'https://example.invalid/v1', apiKey: 'public-synthetic-not-a-secret',
  temperature: 0.8, maxTokens: 1024, purposes: ['generation'],
}
const messages = [
  { role: 'system', content: 'PUBLIC synthetic fixture; preserve whitespace.' },
  { role: 'user', content: '合成 😀 <>&\u2028\u2029\npublic prompt' },
]
const defaults = { temperature: 0, maxTokens: 128 }
const usage = { prompt_tokens: 11, completion_tokens: 7, total_tokens: 18, completion_tokens_details: { reasoning_tokens: 3 } }
const delta = (content, extra = {}) => ({ choices: [{ delta: { content }, ...extra }] })
const finish = reason => ({ choices: [{ delta: {}, finish_reason: reason }] })
const frame = (payload, newline = '\n') => `data: ${typeof payload === 'string' ? payload : JSON.stringify(payload)}${newline}${newline}`
const done = frame('[DONE]')
const textResponse = (body, status = 200, contentType = 'text/event-stream') => ({
  status, headers: { 'content-type': contentType }, body, chunks: [],
})
const chunkBytes = (body, mode = 'whole') => {
  const bytes = Buffer.from(body)
  if (mode === 'whole') return bytes.length ? [Array.from(bytes)] : []
  const cuts = new Set([0, bytes.length])
  if (mode === 'bytes') for (let index = 1; index < bytes.length; index++) cuts.add(index)
  else {
    // Explicitly split CR/LF pairs, and each interior UTF-8 byte of the first
    // emoji. Add other arbitrary cuts across SSE field names and JSON tokens.
    for (let index = 7; index < bytes.length; index += 23) cuts.add(index)
    for (let index = 0; index < bytes.length; index++) if (bytes[index] === 13) cuts.add(index + 1)
    const emoji = bytes.indexOf(Buffer.from('😀'))
    if (emoji >= 0) for (let index = 1; index < 4; index++) cuts.add(emoji + index)
  }
  const ordered = [...cuts].sort((a, b) => a - b)
  return ordered.slice(1).map((end, index) => Array.from(bytes.subarray(ordered[index], end)))
}
const definitions = []
function stream(name, body, config = {}) {
  const { status = 200, split = 'whole', ...rest } = config
  const response = textResponse(body, status)
  response.chunks = chunkBytes(body, split)
  definitions.push({ name, mode: 'stream', model: { ...model, ...rest.model }, messages,
    opts: { ...defaults, visibleOnly: true, ...rest.opts }, response })
}
function generate(name, payload, config = {}) {
  const { status = 200, raw = false, ...rest } = config
  definitions.push({ name, mode: 'generate', model: { ...model, ...rest.model }, messages,
    opts: { ...defaults, ...rest.opts }, response: textResponse(raw ? payload : JSON.stringify(payload), status, 'application/json') })
}
const successPayload = (content = '  <think>hidden</think> visible 😀 \n', reason = 'stop') => ({
  choices: [{ message: { content, reasoning_content: 'never persisted' }, finish_reason: reason }], usage,
})

// Primary durable-owner path: exact callback ordering, whitespace, split tag
// boundaries, explicit reasoning, usage evidence, split CRLF and UTF-8 bytes.
const primaryBody = ': public heartbeat\r\nevent: message\r\nid: public-1\r\n\r\n'
  + frame({ choices: [{ delta: { reasoning_content: 'field thought 😀' } }] }, '\r\n')
  + frame(delta('  visible 😀 <thi'), '\r\n')
  + frame(delta('nk>hidden '), '\r\n')
  + frame(delta('<THINK>nested</THINK>thought</thi'), '\r\n')
  + frame(delta('nk>\n尾声 '), '\r\n')
  + frame({ choices: [], usage }, '\r\n')
  + frame(finish('stop'), '\r\n') + frame('[DONE]', '\r\n')
stream('visible-primary-split-crlf-utf8-reasoning-usage', primaryBody, { split: 'boundaries' })
stream('visible-primary-one-byte-transport', primaryBody, { split: 'bytes' })
stream('visible-unfinished-thought-and-pending-tag', frame(delta('public<think>hidden')) + frame(finish('stop')) + done)
stream('visible-pending-markup-not-flushed', frame(delta('public <thi')) + frame(finish('stop')) + done)
stream('visible-literal-markup-orphan-close', frame(delta(' <other>literal</other></think> tail ')) + frame(finish('stop')) + done)
stream('legacy-reasoning-wrapper-and-trimming', frame({ choices: [{ delta: { reasoning_content: 'private' } }] })
  + frame(delta('  visible  ')) + frame(finish('stop')) + done, { opts: { visibleOnly: false } })
stream('legacy-reasoning-only-closes-wrapper', frame({ choices: [{ delta: { reasoning_content: 'private' } }] }) + frame(finish('stop')) + done,
  { opts: { visibleOnly: false } })
for (const reason of ['stop', 'length', 'model_context_window_exceeded', 'content_filter', 'sensitive', 'network_error', 'tool_calls', null]) {
  stream(`stream-finish-${reason === null ? 'null' : reason}`, frame(delta('正文')) + frame(finish(reason)) + done)
}
stream('stream-done-without-finish', frame(delta('text')) + done)
stream('stream-no-done-preserves-partial-and-usage', frame(delta(' visible ')) + frame({ choices: [], usage }) + frame(finish('stop')))
stream('stream-empty-no-done', '')
stream('stream-ignore-after-done', frame(delta('text')) + frame(finish('stop')) + done + frame('{invalid ignored'))
stream('stream-final-event-without-newline', frame(delta('text')) + frame(finish('stop')) + 'data: [DONE]')
stream('stream-multiline-data-and-cr-only', ':comment\r\rdata: {"choices":\rdata: [{"delta":{"content":"text"},"finish_reason":"stop"}]}\r\rdata:[DONE]\r\r', { split: 'boundaries' })
stream('stream-empty-heartbeats-and-metadata', ':beat\n\nevent: token\nretry: 100\n\ndata:\n\n'
  + frame({ id: 'public-metadata' }) + frame({ choices: [] }) + frame(delta(null)) + frame(finish('stop')) + done)
stream('stream-partial-usage-evidence-preserves-complete-usage', frame({ choices: [], usage })
  + frame({ choices: [], usage: { prompt_tokens: 'bad', completion_tokens: 2, completion_tokens_details: { reasoning_tokens: 'bad' } } })
  + frame(delta('text')) + frame(finish('stop')) + done)
stream('stream-null-and-array-usage-ignored', frame({ choices: [], usage: null }) + frame({ choices: [], usage: [] }) + frame(finish('stop')) + done)
const invalid = [
  ['json', '{not-json'], ['null-payload', 'null'], ['array-payload', '[]'], ['number-payload', '5'],
  ['choices-object', { choices: {} }], ['choice-null', { choices: [null] }], ['choice-array', { choices: [[]] }],
  ['delta-null', { choices: [{ delta: null }] }], ['delta-array', { choices: [{ delta: [] }] }],
  ['finish-number', { choices: [{ finish_reason: 3 }] }],
  ['content-object', { choices: [{ delta: { content: {} } }] }],
  ['reasoning-number', { choices: [{ delta: { reasoning_content: 7 } }] }],
]
for (const [name, payload] of invalid) {
  stream(`stream-invalid-${name}-preserves-partial-usage`, frame(delta('partial ')) + frame({ choices: [], usage }) + frame(payload) + done)
}
stream('stream-provider-error-preserves-partial-usage', frame(delta('partial ')) + frame({ choices: [], usage })
  + frame({ error: { message: 'PUBLIC synthetic provider error' } }) + done)
stream('stream-error-property-null-still-provider-error', frame({ error: null }) + done)
for (const [name, status, body] of [
  ['openai-context', 400, '{"error":{"message":"maximum context length is 123 tokens"}}'],
  ['input-max-seq', 400, 'input tokens (777) has exceeded max_seq_len'],
  ['output-max-seq', 400, 'max_tokens (888) have exceeded max_seq_len'],
  ['input-count', 400, 'Input token count (999) exceeds the maximum number of tokens'],
  ['unrelated-400', 400, 'PUBLIC malformed input'],
  ['context-text-wrong-status', 429, 'maximum context length is 123 tokens'],
  ['server-failure', 503, 'PUBLIC synthetic upstream failure'],
]) stream(`stream-http-${name}`, body, { status })

// Nonstream responses distinguish semantic finish evidence from transport
// success and exercise the original regex-based thinking stripping path.
for (const reason of ['stop', 'length', 'model_context_window_exceeded', 'content_filter', 'sensitive', 'network_error', 'tool_calls', null]) {
  generate(`generate-finish-${reason === null ? 'null' : reason}`, successPayload(undefined, reason))
}
for (const [name, content] of [
  ['multiple-and-case-insensitive-thoughts', ' \n<THINK>a\nb</THINK> A <think>c</think> B  '],
  ['orphan-close', 'discarded prefix</think>  visible  '],
  ['unfinished-thought', 'visible <think>private forever'],
  ['plain-trim', ' \n visible 😀 \t '],
]) generate(`generate-strip-${name}`, successPayload(content))
generate('generate-empty-choices', { choices: [] })
generate('generate-missing-content-stop', { choices: [{ message: {}, finish_reason: 'stop' }] })
generate('generate-http-context-is-raw-http-failure', 'maximum context length is 123 tokens', { status: 400, raw: true })

// Request serialization uses resolved synthetic directives only; this is NOT a
// claim about real provider presets or the main-owner parameter policy.
generate('request-omit-temperature-use-model-token-default', successPayload(), { opts: { temperature: undefined, maxTokens: undefined } })
generate('request-completion-token-parameter-response-format-effort', successPayload(), {
  opts: { outputTokenParameter: 'max_completion_tokens', temperature: 0.45, maxTokens: 99,
    responseFormat: { type: 'json_object' }, reasoning: { adapter: 'openai-reasoning-effort', reasoningEffort: 'high' } },
})
for (const budget of [0, 64]) generate(`request-synthetic-budget-${budget}`, successPayload(), {
  model: { syntheticMapping: { adapter: 'openai-thinking-budget' } },
  opts: { reasoning: { adapter: 'openai-thinking-budget', thinkingBudget: budget } },
})
generate('request-synthetic-mapping-mismatch-omits-budget', successPayload(), {
  opts: { reasoning: { adapter: 'openai-thinking-budget', thinkingBudget: 64 } },
})
generate('request-synthetic-siliconflow-v4', successPayload(), {
  model: { syntheticMapping: { adapter: 'siliconflow-v4-thinking' } },
  opts: { reasoning: { adapter: 'siliconflow-v4-thinking', reasoningEffort: 'max' } },
})
for (const thinking of ['enabled', 'disabled']) generate(`request-synthetic-deepseek-v4-${thinking}`, successPayload(), {
  model: { syntheticMapping: { adapter: 'deepseek-v4-thinking' } },
  opts: { reasoning: { adapter: 'deepseek-v4-thinking', thinking, reasoningEffort: 'max' } },
})
stream('request-novelai-narrow-stream', frame(delta('text')) + frame(finish('stop')) + done, {
  model: { provider: 'novelai', baseUrl: 'https://example.invalid/oa' },
  opts: { responseFormat: { type: 'json_object' }, reasoning: { adapter: 'openai-reasoning-effort', reasoningEffort: 'high' } },
})
for (const [name, baseUrl] of [
  ['root', 'https://example.invalid'],
  ['chat-suffix', 'https://example.invalid/v1/chat/'],
  ['complete-suffix-query-fragment', ' https://example.invalid/v1/chat/completions/?public=1#fixture '],
]) generate(`request-endpoint-${name}`, successPayload(), { model: { baseUrl } })

try {
  const { OpenAIProvider } = await import(providerUrl)
  const cases = []
  for (const definition of definitions) {
    const captured = []
    const events = []
    globalThis[transportKey] = async (url, init) => {
      assert.equal(new URL(url).hostname, 'example.invalid', 'synthetic endpoint only')
      captured.push({ url: String(url), method: init.method, headers: Object.fromEntries(new Headers(init.headers)), body: JSON.parse(init.body) })
      const { response } = definition
      const body = definition.mode === 'stream'
        ? new ReadableStream({ start(controller) { for (const bytes of response.chunks) controller.enqueue(Uint8Array.from(bytes)); controller.close() } })
        : response.body
      return new Response(body, { status: response.status, headers: response.headers })
    }
    const controller = new AbortController()
    const opts = { ...definition.opts, signal: controller.signal }
    const provider = new OpenAIProvider()
    let result
    if (definition.mode === 'stream') {
      await provider.generateStream(definition.model, definition.messages, {
        ...opts,
        onChunk: text => events.push({ type: 'chunk', text }),
        onReasoning: text => events.push({ type: 'reasoning', text }),
        onUsageEvidence: evidence => events.push({ type: 'usage', evidence }),
        onDone: (content, usage, finishReason) => events.push({ type: 'done', content, usage, finishReason }),
        onError: (error, content, usage) => events.push({ type: 'error', error, content, usage }),
      })
      assert.equal(events.filter(event => event.type === 'done' || event.type === 'error').length, 1, `${definition.name}: exactly one terminal callback`)
    } else result = await provider.generate(definition.model, definition.messages, opts)
    assert.equal(captured.length, 1, `${definition.name}: exactly one synthetic request`)
    cases.push({ ...definition, request: captured[0], events, ...(result === undefined ? {} : { result }) })
  }
  const contract = {
    formatVersion: 1,
    oracle: {
      provider: 'electron/llm/openai-provider.ts',
      scope: 'original-provider-synthetic-transport-and-mapping-only',
      transport: 'in-memory Response / ReadableStream, never network',
      stubs: ['electron/platform/endpoint-fetch', 'src/shared/provider-presets: model.syntheticMapping only', 'package.json: named version export'],
      excludedClaims: ['real preset mapping', 'generation-parameter-policy', 'diagnostics event timing', 'network transport behavior', 'application runtime integration'],
      sources: [...sources.values()].map(({ name, source }) => ({ path: name, sha256: createHash('sha256').update(source).digest('hex') })),
    },
    cases,
  }
  writeFileSync(target, JSON.stringify(contract) + '\n', { flag: 'wx', mode: 0o600 })
  console.log(`Original OpenAIProvider emitted ${cases.length} public synthetic CI contracts to ${target}`)
} finally {
  hooks.deregister()
  globalThis.fetch = originalFetch
  if (previousTransport === undefined) delete globalThis[transportKey]
  else globalThis[transportKey] = previousTransport
}
