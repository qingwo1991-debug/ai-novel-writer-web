import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { existsSync, mkdirSync, readFileSync, realpathSync, writeFileSync } from 'node:fs'
import { registerHooks, stripTypeScriptTypes } from 'node:module'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

// CI-only differential oracle, never an application-time Node bridge. Inputs,
// credentials, messages and transport bytes below are PUBLIC synthetic fixtures.
assert.equal(process.env.GITHUB_ACTIONS, 'true', 'Gemini contract generation is GitHub-only')
const base = process.env.RUNNER_TEMP
const target = process.env.GO_GEMINI_CONTRACT
assert(base && target && path.isAbsolute(base) && path.isAbsolute(target))
assert.equal(target, path.join(base, 'novel-go-gemini', 'contract.json'), 'use the disposable Gemini runner path')
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
const providerUrl = new URL('electron/llm/gemini-provider.ts', root).href
const sourcePaths = [
  'electron/llm/gemini-provider.ts',
  'electron/llm/provider.interface.ts',
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
const fetchStubUrl = 'gemini-contract:endpoint-fetch'
const transportKey = '__GO_GEMINI_CONTRACT_SYNTHETIC_FETCH__'
const runtimeImport = "import { InBandReasoningStream, ILLMProvider, LLMGenerateOptions, LLMResponse, LLMStreamOptions } from './provider.interface'"
assert.equal(sources.get(providerUrl).source.split('\n')[0], runtimeImport, 'review oracle loader after provider first-line import changes')

// Install before loading the original provider. Only endpoint-fetch is stubbed;
// all runtime provider/interface/visible/diagnostics/context/shared code is real.
// Fail closed on any unexpected transitive import: no npm, Electron or presets.
const hooks = registerHooks({
  resolve(specifier, context) {
    assert(context.parentURL, `unexpected root import: ${specifier}`)
    const url = new URL(specifier, context.parentURL).href
    const withoutExtension = url.endsWith('.ts') ? url.slice(0, -3) : url
    if (withoutExtension === new URL('electron/platform/endpoint-fetch', root).href) {
      return { url: fetchStubUrl, shortCircuit: true }
    }
    const resolved = sources.has(url) ? url : `${url}.ts`
    assert(sources.has(resolved), `unexpected oracle dependency: ${specifier}`)
    return { url: resolved, shortCircuit: true }
  },
  load(url) {
    if (url === fetchStubUrl) return { format: 'module', source: `export const endpointFetch = (...args) => globalThis[${JSON.stringify(transportKey)}](...args)`, shortCircuit: true }
    assert(sources.has(url), `unexpected oracle module: ${url}`)
    let source = sources.get(url).source
    // Syntax-only TS transform cannot identify these four imports as interfaces.
    // Remove ONLY those erased names on the exact original first import line.
    if (url === providerUrl) source = source.replace(runtimeImport, "import { InBandReasoningStream } from './provider.interface'")
    return { format: 'module', source: stripTypeScriptTypes(source, { mode: 'transform', sourceUrl: url }), shortCircuit: true }
  },
})
const originalFetch = globalThis.fetch
const previousTransport = globalThis[transportKey]
globalThis.fetch = () => { throw new Error('real network is forbidden in the Gemini contract oracle') }

const model = {
  id: 'public-synthetic-gemini', name: 'Public synthetic Gemini fixture',
  provider: 'custom', protocol: 'gemini', modelName: 'synthetic-gemini',
  baseUrl: 'https://example.invalid', apiKey: 'public-synthetic-not-a-secret',
  temperature: 0.8, maxTokens: 1024, purposes: ['generation'],
}
const messages = [
  { role: 'system', content: 'PUBLIC synthetic fixture; preserve whitespace.' },
  { role: 'user', content: '合成 😀 <>&\u2028\u2029\npublic prompt' },
]
const defaults = { temperature: 0, maxTokens: 128 }
const usage = { promptTokenCount: 11, candidatesTokenCount: 7, thoughtsTokenCount: 3, totalTokenCount: 21 }
const candidate = (parts = [], reason) => ({ content: { parts }, ...(reason === undefined ? {} : { finishReason: reason }) })
const text = (value, reason) => ({ candidates: [candidate([{ text: value }], reason)] })
const finish = reason => ({ candidates: [{ finishReason: reason }] })
const frame = (payload, newline = '\n') => `data: ${typeof payload === 'string' ? payload : JSON.stringify(payload)}${newline}${newline}`
const chunkBytes = (body, mode = 'whole') => {
  const bytes = Buffer.from(body)
  if (mode === 'whole') return bytes.length ? [Array.from(bytes)] : []
  const cuts = new Set([0, bytes.length])
  if (mode === 'bytes') for (let index = 1; index < bytes.length; index++) cuts.add(index)
  else {
    // Split SSE names/JSON, CRLF pairs, and every interior UTF-8 emoji byte.
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
  const { status = 200, split = 'whole', contentType = 'text/event-stream', ...rest } = config
  definitions.push({ name, mode: 'stream', model: { ...model, ...rest.model },
    opts: { ...defaults, visibleOnly: true, ...rest.opts }, messages: rest.messages ?? messages,
    response: { status, headers: { 'content-type': contentType }, body, chunks: chunkBytes(body, split) } })
}
function generate(name, payload, config = {}) {
  const { status = 200, raw = false, ...rest } = config
  definitions.push({ name, mode: 'generate', model: { ...model, ...rest.model },
    opts: { ...defaults, ...rest.opts }, messages: rest.messages ?? messages,
    response: { status, headers: { 'content-type': 'application/json' }, body: raw ? payload : JSON.stringify(payload), chunks: [] } })
}
const success = (value = '  public 😀 <think>in-band is NOT stripped by generate</think> \n', reason = 'STOP') => ({
  candidates: [candidate([{ text: value }], reason)], usageMetadata: usage,
})

// Visible and legacy paths are deliberately both observed. The current original
// always uses line-oriented SSE (exact "data: "), even in legacy mode. Raw JSON
// arrays are NOT parsed; the fixtures below freeze that behavior, not an imagined
// JSON-array decoder. Legacy mode tolerates malformed data and leaves in-band
// tags visible while still sending their contents to the reasoning callback.
const primary = ': public heartbeat\r\nevent: message\r\nid: public-1\r\n\r\n'
  + frame({ candidates: [candidate([{ text: 'flagged thought 😀', thought: true }, { text: '  visible 😀 <thi' }])] }, '\r\n')
  + frame(text('nk>hidden <THINK>nested</THINK>'), '\r\n')
  + frame(text('tail</thi'), '\r\n')
  + frame(text('nk>\n尾声 '), '\r\n')
  + frame({ usageMetadata: usage }, '\r\n') + frame(finish('STOP'), '\r\n')
stream('visible-primary-split-crlf-utf8-thought-usage', primary, { split: 'boundaries' })
stream('visible-primary-one-byte-transport', primary, { split: 'bytes' })
stream('legacy-primary-sse-inband-kept-thought-flag-excluded', primary, { split: 'boundaries', opts: { visibleOnly: false } })
for (const visibleOnly of [true, false]) {
  const prefix = visibleOnly ? 'visible' : 'legacy'
  stream(`${prefix}-json-array-whole-ignored`, JSON.stringify([text('array text'), { usageMetadata: usage }, finish('STOP')]),
    { contentType: 'application/json', opts: { visibleOnly } })
  stream(`${prefix}-json-array-multiline-byte-split-ignored`, JSON.stringify([text('array 😀'), finish('MAX_TOKENS')], null, 2),
    { split: 'bytes', contentType: 'application/json', opts: { visibleOnly } })
}
stream('visible-unfinished-thought', frame(text('public<think>private forever')) + frame(finish('STOP')))
stream('visible-pending-tag-not-flushed', frame(text('public <thi')) + frame(finish('STOP')))
stream('visible-literal-markup-orphan-close', frame(text(' <other>literal</other></think> tail ')) + frame(finish('STOP')))
stream('visible-all-parts-thought-flag-and-false', frame({ candidates: [candidate([
  { text: 'thought', thought: true }, { text: ' A ', thought: false }, { inlineData: { mimeType: 'public/fixture', data: '' } }, { text: ' B ' },
], 'STOP')] }))
stream('visible-thought-flag-does-not-enter-inband-state', frame({ candidates: [candidate([{ text: '<think>flag text', thought: true }])] })
  + frame(text('visible after flagged unfinished tag', 'STOP')))
const reasons = ['STOP', 'MAX_TOKENS', 'SAFETY', 'RECITATION', 'BLOCKLIST', 'PROHIBITED_CONTENT', 'OTHER', 'MALFORMED_FUNCTION_CALL', 'stop', '', null, undefined]
for (const reason of reasons) {
  const label = reason === undefined ? 'missing' : reason === null ? 'null' : reason === '' ? 'empty' : reason
  stream(`stream-finish-${label}`, frame(text(' 正文 😀 ', reason)) + frame({ usageMetadata: usage }))
  generate(`generate-finish-${label}`, { candidates: [candidate([{ text: ' 正文 😀 ' }], reason)], usageMetadata: usage })
}
stream('stream-empty-eof', '')
stream('stream-text-eof-without-finish', frame(text(' partial ')) + frame({ usageMetadata: usage }))
stream('stream-final-line-no-newline', frame(text('public')) + `data: ${JSON.stringify(finish('STOP'))}`)
stream('stream-finish-not-terminal-later-content-and-finish-win', frame(text('first', 'STOP')) + frame(text(' second', 'MAX_TOKENS')))
stream('stream-null-finish-overwrites-stop', frame(text('text', 'STOP')) + frame(finish(null)))
stream('stream-missing-finish-does-not-overwrite-stop', frame(text('text', 'STOP')) + frame(text(' tail')))
stream('stream-done-marker-is-invalid-preserves-partial', frame(text('partial', 'STOP')) + frame({ usageMetadata: usage }) + frame('[DONE]'))
stream('legacy-done-marker-ignored-not-terminal', frame(text('first', 'STOP')) + frame('[DONE]') + frame(text(' second', 'MAX_TOKENS')), { opts: { visibleOnly: false } })
stream('stream-data-without-space-ignored', `data:${JSON.stringify(text('ignored', 'MAX_TOKENS'))}\n\n` + frame(text('kept', 'STOP')))
stream('stream-cr-only-is-not-sse-framing', frame(text('not framed'), '\r') + frame(finish('STOP'), '\r'), { split: 'boundaries' })
stream('stream-multiline-data-is-invalid', 'data: {"candidates":\ndata: []}\n\n')
stream('legacy-multiline-data-is-ignored', 'data: {"candidates":\ndata: []}\n\n' + frame(text('kept', 'STOP')), { opts: { visibleOnly: false } })
stream('stream-heartbeats-metadata-empty-candidates', ':beat\n\nevent: token\nretry: 100\n\ndata: \n\n'
  + frame({ modelVersion: 'public-synthetic' }) + frame({ candidates: [] }) + frame({ candidates: [{ content: {} }] }) + frame(finish('STOP')))
stream('stream-multiple-candidates-only-first-output', frame({ candidates: [candidate([{ text: 'first' }], 'STOP'), candidate([{ text: 'ignored', thought: true }], 'MAX_TOKENS')] }))
stream('stream-empty-first-candidate-does-not-select-second', frame({ candidates: [{}, candidate([{ text: 'ignored' }], 'STOP')] }))

// Usage is normalized for terminal callbacks but evidence preserves raw reported
// fields. Missing is not zero, null is invalid, and candidates+thoughts are added.
const usageCases = [
  ['empty', {}], ['zero', { promptTokenCount: 0, candidatesTokenCount: 0, thoughtsTokenCount: 0, totalTokenCount: 0 }],
  ['candidates-only', { candidatesTokenCount: 7 }], ['thoughts-only', { thoughtsTokenCount: 3 }],
  ['prompt-total-only', { promptTokenCount: 11, totalTokenCount: 11 }],
  ['null-fields', { promptTokenCount: null, candidatesTokenCount: null, thoughtsTokenCount: null, totalTokenCount: null }],
  ['invalid-string', { promptTokenCount: '11', candidatesTokenCount: '7', thoughtsTokenCount: 3, totalTokenCount: '21' }],
  ['negative', { promptTokenCount: -1, candidatesTokenCount: -2, thoughtsTokenCount: 3, totalTokenCount: -4 }],
  ['fraction', { promptTokenCount: 1.5, candidatesTokenCount: 2.5, thoughtsTokenCount: 0, totalTokenCount: 4.5 }],
  ['unsafe', { promptTokenCount: 9007199254740992, candidatesTokenCount: 9007199254740992, thoughtsTokenCount: 1, totalTokenCount: 9007199254740992 }],
  ['safe-boundary-and-sum', { promptTokenCount: Number.MAX_SAFE_INTEGER, candidatesTokenCount: Number.MAX_SAFE_INTEGER, thoughtsTokenCount: 1, totalTokenCount: Number.MAX_SAFE_INTEGER }],
  ['sum-not-safe-checked', { candidatesTokenCount: Number.MAX_SAFE_INTEGER, thoughtsTokenCount: Number.MAX_SAFE_INTEGER }],
  ['null-candidate-valid-thought', { candidatesTokenCount: null, thoughtsTokenCount: 3 }],
  ['object-and-boolean', { promptTokenCount: {}, candidatesTokenCount: false, thoughtsTokenCount: [], totalTokenCount: true }],
]
for (const [name, metadata] of usageCases) {
  stream(`stream-usage-${name}`, frame(text('text', 'STOP')) + frame({ usageMetadata: metadata }))
  generate(`generate-usage-${name}`, { candidates: [candidate([{ text: 'text' }], 'STOP')], usageMetadata: metadata })
}
stream('stream-null-usage-does-not-overwrite', frame({ usageMetadata: usage }) + frame({ usageMetadata: null }) + frame(text('text', 'STOP')))
stream('stream-partial-usage-overwrites-complete', frame({ usageMetadata: usage }) + frame({ usageMetadata: { candidatesTokenCount: 2 } }) + frame(finish('STOP')))
stream('stream-usage-array-is-truthy', frame({ usageMetadata: [] }) + frame(finish('STOP')))
generate('generate-null-usage-omitted', { candidates: [candidate([], 'STOP')], usageMetadata: null })

// Validation walks ALL candidates/parts, not merely the selected first one.
const invalid = [
  ['json', '{not-json'], ['null-payload', 'null'], ['array-payload', '[]'], ['number-payload', '5'],
  ['candidates-null', { candidates: null }], ['candidates-object', { candidates: {} }],
  ['candidate-null', { candidates: [null] }], ['candidate-array', { candidates: [[]] }],
  ['finish-number', { candidates: [{ finishReason: 3 }] }],
  ['content-null', { candidates: [{ content: null }] }], ['content-array', { candidates: [{ content: [] }] }],
  ['parts-null', { candidates: [{ content: { parts: null } }] }], ['parts-object', { candidates: [{ content: { parts: {} } }] }],
  ['part-null', { candidates: [candidate([null])] }], ['part-array', { candidates: [candidate([[]])] }],
  ['text-null', { candidates: [candidate([{ text: null }])] }], ['text-number', { candidates: [candidate([{ text: 7 }])] }],
  ['thought-null', { candidates: [candidate([{ text: 'text', thought: null }])] }],
  ['thought-string', { candidates: [candidate([{ text: 'text', thought: 'true' }])] }],
  ['second-candidate-invalid', { candidates: [candidate([{ text: 'must not emit' }], 'STOP'), null] }],
  ['second-part-invalid', { candidates: [candidate([{ text: 'must not emit' }, { text: {} }], 'STOP')] }],
]
for (const [name, payload] of invalid) {
  stream(`stream-invalid-${name}-preserves-partial-usage`, frame(text('partial ')) + frame({ usageMetadata: usage }) + frame(payload))
}
stream('stream-provider-error-preserves-partial-usage', frame(text('partial ')) + frame({ usageMetadata: usage }) + frame({ error: { message: 'PUBLIC synthetic error' } }))
stream('stream-error-property-null-still-provider-error', frame({ error: null }))
stream('legacy-malformed-and-provider-error-tolerated', frame('{not-json') + frame('null') + frame({ error: { message: 'PUBLIC ignored' } }) + frame(text(' kept ', 'STOP')), { opts: { visibleOnly: false } })
stream('legacy-invalid-second-candidate-not-validated', frame({ candidates: [candidate([{ text: 'kept' }], 'STOP'), null] }), { opts: { visibleOnly: false } })

for (const [name, status, body] of [
  ['gemini-context', 400, '{"error":{"message":"Input token count (999) exceeds the maximum number of tokens"}}'],
  ['openai-context', 400, 'maximum context length is 123 tokens'],
  ['input-max-seq', 400, 'input tokens (777) has exceeded max_seq_len'],
  ['output-max-seq', 400, 'max_tokens (888) have exceeded max_seq_len'],
  ['unrelated-400', 400, 'PUBLIC malformed input'],
  ['context-wrong-status', 429, 'Input token count (999) exceeds the maximum number of tokens'],
  ['forbidden', 403, 'PUBLIC synthetic permission failure'],
  ['server-failure', 503, 'PUBLIC synthetic upstream failure'],
]) {
  stream(`stream-http-${name}`, body, { status })
  generate(`generate-http-${name}-raw-failure`, body, { status, raw: true })
}

// Nonstream intentionally selects only the first candidate's first part, without
// thought-flag filtering, trimming or in-band tag removal. Freeze those quirks.
generate('generate-first-part-only-even-thought-flag', { candidates: [candidate([{ text: '  flagged thought  ', thought: true }, { text: 'ignored' }], 'STOP'), candidate([{ text: 'ignored candidate' }], 'MAX_TOKENS')], usageMetadata: usage })
generate('generate-empty-first-part-no-fallback', { candidates: [candidate([{}, { text: 'ignored' }], 'STOP')] })
generate('generate-empty-candidates', { candidates: [] })
generate('generate-missing-candidates', {})
generate('generate-empty-content-stop', { candidates: [{ finishReason: 'STOP' }] })
generate('generate-unfinished-think-kept', success('visible <think>private forever'))
generate('generate-provider-error-object-not-special-cased', { error: { message: 'PUBLIC synthetic error' } })

const requestMessages = [
  { role: 'system', content: 'PUBLIC first system ignored' }, { role: 'user', content: 'first user' },
  { role: 'assistant', content: 'prior assistant' }, { role: 'tool', content: 'other roles become user' },
  { role: 'system', content: 'PUBLIC last system wins 😀' }, { role: 'user', content: 'last user' },
]
generate('request-system-last-wins-assistant-to-model-other-to-user', success(), { messages: requestMessages })
stream('request-stream-system-mapping-json-thinking-level', frame(text('text', 'STOP')), { messages: requestMessages,
  opts: { responseFormat: { type: 'json_object' }, reasoning: { adapter: 'gemini-thinking-level', thinkingLevel: 'high' } } })
generate('request-empty-last-system-omitted', success(), { messages: [...requestMessages, { role: 'system', content: '' }] })
generate('request-no-system', success(), { messages: [{ role: 'assistant', content: ' model ' }] })
generate('request-omit-temperature-model-token-default', success(), { opts: { temperature: undefined, maxTokens: undefined } })
generate('request-null-token-falls-back-temperature-null-kept', success(), { opts: { temperature: null, maxTokens: null } })
generate('request-no-model-token-default-omitted', success(), { model: { maxTokens: undefined }, opts: { temperature: undefined, maxTokens: undefined } })
generate('request-explicit-zero-tokens-temperature', success(), { opts: { temperature: 0, maxTokens: 0 } })
generate('request-json-object-format', success(), { opts: { responseFormat: { type: 'json_object' }, temperature: 0.45, maxTokens: 99 } })
generate('request-other-format-and-openai-token-parameter-ignored', success(), { opts: { responseFormat: { type: 'json_schema' }, outputTokenParameter: 'max_completion_tokens' } })
for (const budget of [-1, 0, 64]) generate(`request-thinking-budget-${budget}`, success(), { opts: { reasoning: { adapter: 'gemini-thinking-budget', thinkingBudget: budget } } })
for (const level of ['minimal', 'low', 'medium', 'high']) generate(`request-thinking-level-${level}`, success(), { opts: { reasoning: { adapter: 'gemini-thinking-level', thinkingLevel: level } } })
generate('request-budget-adapter-ignores-level', success(), { opts: { reasoning: { adapter: 'gemini-thinking-budget', thinkingBudget: 64, thinkingLevel: 'high' } } })
generate('request-level-adapter-ignores-budget', success(), { opts: { reasoning: { adapter: 'gemini-thinking-level', thinkingLevel: 'low', thinkingBudget: 64 } } })
generate('request-nongemini-reasoning-ignored', success(), { opts: { reasoning: { adapter: 'openai-reasoning-effort', reasoningEffort: 'high' } } })
generate('request-missing-budget-leaves-empty-thinking-config', success(), { opts: { reasoning: { adapter: 'gemini-thinking-budget' } } })
generate('request-single-trailing-slash-removed', success(), { model: { baseUrl: 'https://example.invalid/public/' } })
generate('request-double-trailing-slash-only-one-removed', success(), { model: { baseUrl: 'https://example.invalid/public//' } })

try {
  const { GeminiProvider } = await import(providerUrl)
  const cases = []
  assert(definitions.length > 50, 'keep broad Gemini protocol coverage')
  assert.equal(new Set(definitions.map(item => item.name)).size, definitions.length, 'unique case names')
  for (const definition of definitions) {
    const captured = []
    const events = []
    globalThis[transportKey] = async (url, init) => {
      const endpoint = new URL(url)
      assert.equal(endpoint.protocol, 'https:', 'synthetic HTTPS fixtures only')
      assert.equal(endpoint.hostname, 'example.invalid', 'synthetic endpoint only')
      assert.equal(new Headers(init.headers).get('x-goog-api-key'), 'public-synthetic-not-a-secret', 'public fixture key only')
      captured.push({ url: String(url), method: init.method, headers: Object.fromEntries(new Headers(init.headers)), body: JSON.parse(init.body) })
      const { response } = definition
      const body = definition.mode === 'stream'
        ? new ReadableStream({ start(controller) { for (const bytes of response.chunks) controller.enqueue(Uint8Array.from(bytes)); controller.close() } })
        : response.body
      return new Response(body, { status: response.status, headers: response.headers })
    }
    const controller = new AbortController()
    const opts = { ...definition.opts, signal: controller.signal }
    const provider = new GeminiProvider()
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
      provider: 'electron/llm/gemini-provider.ts',
      scope: 'original-provider-synthetic-transport-only',
      transport: 'in-memory Response / ReadableStream, never network',
      stubs: ['electron/platform/endpoint-fetch'],
      excludedClaims: ['real preset mapping', 'generation-parameter-policy', 'diagnostics event timing', 'network transport behavior', 'application runtime integration'],
      sources: [...sources.values()].map(({ name, source }) => ({ path: name, sha256: createHash('sha256').update(source).digest('hex') })),
    },
    cases,
  }
  writeFileSync(target, JSON.stringify(contract) + '\n', { flag: 'wx', mode: 0o600 })
  console.log(`Original GeminiProvider emitted ${cases.length} public synthetic CI contracts to ${target}`)
} finally {
  hooks.deregister()
  globalThis.fetch = originalFetch
  if (previousTransport === undefined) delete globalThis[transportKey]
  else globalThis[transportKey] = previousTransport
}
