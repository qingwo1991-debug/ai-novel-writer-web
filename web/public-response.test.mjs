import test from 'node:test'
import assert from 'node:assert/strict'
import {
  createPublicResponseSanitizer,
  registerSecret,
  sanitizePublicResponse,
  publicModels,
} from './public-response.mjs'

const REDACTED = '[REDACTED]'

function registered(...values) {
  const sanitizer = createPublicResponseSanitizer()
  for (const value of values) sanitizer.registerSecret(value)
  return sanitizer
}

function deepFreeze(value) {
  if (value && typeof value === 'object') {
    for (const item of Object.values(value)) deepFreeze(item)
    Object.freeze(value)
  }
  return value
}

test('malicious provider failure and SSE event/log payloads redact the stored model credential', () => {
  const credential = 'unit-only-stored-provider-credential'
  const boundary = registered(credential)
  const result = deepFreeze({
    success: false,
    content: '',
    finishReason: 'error',
    httpFailure: { status: 401, body: `Rejected Authorization: Bearer ${credential}. Repeated: ${credential}` },
    error: `Provider response quoted api_key=${credential}`,
  })
  const output = boundary.sanitizePublicResponse({ result })
  assert.equal(output.result.success, false)
  assert.equal(output.result.finishReason, 'error')
  assert.equal(output.result.httpFailure.status, 401)
  assert.equal(output.result.httpFailure.body, `Rejected Authorization: Bearer ${REDACTED}. Repeated: ${REDACTED}`)
  assert.equal(JSON.stringify(output).includes(credential), false)
  assert.equal(result.error.includes(credential), true, 'private result remains available internally')

  for (const channel of ['llm:stream-error', 'agent:log', 'llm:stream-done']) {
    const event = { channel, args: [{ error: `${credential} was rejected`, fullText: `echo ${credential}`, complete: false }] }
    const wire = JSON.stringify(boundary.sanitizePublicResponse(event))
    assert.equal(wire.includes(credential), false)
    assert.equal(JSON.parse(wire).args[0].complete, false)
  }
})

test('nested arrays, objects, numbers, null and explicit false flags are immutable', () => {
  const credential = 'unit-nested-secret'
  const boundary = registered(credential)
  const input = deepFreeze({
    success: false,
    complete: false,
    usage: { promptTokens: 0, completionTokens: 12, ratio: 1.5 },
    values: [null, false, 0, `${credential}!`, { nested: ['author prose', credential] }],
  })
  const output = boundary.sanitizePublicResponse(input)
  assert.deepEqual(output, {
    success: false,
    complete: false,
    usage: { promptTokens: 0, completionTokens: 12, ratio: 1.5 },
    values: [null, false, 0, `${REDACTED}!`, { nested: ['author prose', REDACTED] }],
  })
  assert.notEqual(output, input)
  assert.notEqual(output.values, input.values)
  assert.notEqual(output.usage, input.usage)
  assert.equal(input.values[3], `${credential}!`)
})

test('only explicit credential fields are blanked, never arbitrary author key properties', () => {
  const boundary = createPublicResponseSanitizer()
  const input = deepFreeze({
    key: 'The brass key unlocks the tower.',
    nested: [{ apiKey: 'unregistered-api-value', password: 'unregistered-password', accessToken: 'unregistered-token', secret: { nested: 'unregistered-secret' } }],
    APIKEY: 42,
    Password: false,
    AccessToken: null,
    SECRET: ['sensitive'],
    keyboard: 'instrument',
    apiKeyHint: 'not a credential field',
    passwordResetRequired: false,
  })
  assert.deepEqual(boundary.sanitizePublicResponse(input), {
    key: 'The brass key unlocks the tower.',
    nested: [{ apiKey: '', password: '', accessToken: '', secret: '' }],
    APIKEY: '', Password: '', AccessToken: '', SECRET: '',
    keyboard: 'instrument', apiKeyHint: 'not a credential field', passwordResetRequired: false,
  })
  assert.equal(input.nested[0].password, 'unregistered-password')
})

test('exact literals containing Unicode and regular-expression metacharacters are safe', () => {
  const credentials = ['密钥🗝️.[a-z]+?($)^|\\/尾', 'line\nsecret\u0000😀', 'e\u0301cho-token', '  padded token  ']
  const boundary = registered(...credentials)
  for (const credential of credentials) {
    assert.equal(boundary.sanitizePublicResponse(`前${credential}中${credential}后`), `前${REDACTED}中${REDACTED}后`)
  }
  assert.equal(boundary.sanitizePublicResponse('écho-token'), 'écho-token', 'no Unicode normalization')
  assert.equal(boundary.sanitizePublicResponse('padded token'), 'padded token', 'no trimming')
})

test('all many occurrences are replaced without regex substitution or replacement limits', () => {
  const credential = 'unit-many-occurrences'
  const boundary = registered(credential)
  const input = Array.from({ length: 3000 }, (_, index) => `${index}:${credential}`).join('|')
  const output = boundary.sanitizePublicResponse(input)
  assert.equal(output.includes(credential), false)
  assert.equal(output.split(REDACTED).length - 1, 3000)
  assert.equal(output, input.split(credential).join(REDACTED))
})

test('long, contained, partial-overlap and self-overlap credentials redact the full union', () => {
  const cases = [
    { secrets: ['short', 'short-long-credential'], input: 'short-long-credential + short', expected: `${REDACTED} + ${REDACTED}` },
    { secrets: ['abcde', 'defgh'], input: 'abcdefgh', expected: REDACTED },
    { secrets: ['aaaa'], input: 'aaaaaa', expected: REDACTED },
    { secrets: ['abcd'], input: 'abcdabcd', expected: REDACTED },
  ]
  for (const { secrets, input, expected } of cases) {
    for (const order of [secrets, [...secrets].reverse()]) {
      assert.equal(registered(...order).sanitizePublicResponse(input), expected)
    }
  }
  const long = `long-${'密钥😀'.repeat(4000)}-tail`
  const boundary = registered(long, '密钥😀')
  assert.equal(boundary.sanitizePublicResponse(`before ${long} after`), `before ${REDACTED} after`)
})

test('known credentials are redacted even under ordinary key fields and string property names', () => {
  const credential = 'unit-property-name-secret'
  const boundary = registered(credential)
  const input = { key: `Author wrote ${credential}`, [`prefix-${credential}-suffix`]: `value ${credential}` }
  const output = boundary.sanitizePublicResponse(input)
  assert.deepEqual(output, { key: `Author wrote ${REDACTED}`, [`prefix-${REDACTED}-suffix`]: `value ${REDACTED}` })
  assert.equal(JSON.stringify(output).includes(credential), false)
})

test('registration is explicit and does not claim arbitrary or encoded credential detection', () => {
  const boundary = createPublicResponseSanitizer()
  for (const value of [undefined, null, false, 1234, {}, '', 'abc', '😀']) {
    assert.equal(boundary.registerSecret(value), false)
  }
  const credential = 'unit-explicit:credential/?'
  const payload = { secret: credential, error: credential }
  assert.equal(boundary.sanitizePublicResponse(payload).error, credential, 'credential fields do not implicitly register values')
  assert.equal(boundary.registerSecret(credential), true)
  assert.equal(boundary.registerSecret(credential), true, 'duplicate registration is idempotent')
  assert.equal(boundary.sanitizePublicResponse(payload).error, REDACTED)
  assert.equal(boundary.sanitizePublicResponse('abc'), 'abc', 'short values are not globally redacted')
  const encoded = encodeURIComponent(credential)
  assert.equal(boundary.sanitizePublicResponse(encoded), encoded, 'URL encoded variants require explicit registration')
  assert.equal(createPublicResponseSanitizer().sanitizePublicResponse(credential), credential, 'instances have isolated registries')
})

test('public models expose only blank credential fields and truthful hasCredential hints', () => {
  const credential = 'unit-private-model-credential'
  const boundary = registered(credential)
  const input = deepFreeze([
    { id: 'saved', name: 'Saved model', apiKey: credential, hasCredential: false, note: `returned ${credential}`, password: 'password-value', reasoning: { key: 'normal author setting' } },
    { id: 'empty', apiKey: '', hasCredential: true },
    { id: 'absent', hasCredential: true },
    { id: 'invalid', apiKey: false, hasCredential: true },
  ])
  assert.deepEqual(boundary.publicModels(input), [
    { id: 'saved', name: 'Saved model', apiKey: '', hasCredential: true, note: `returned ${REDACTED}`, password: '', reasoning: { key: 'normal author setting' } },
    { id: 'empty', apiKey: '', hasCredential: false },
    { id: 'absent', apiKey: '', hasCredential: false },
    { id: 'invalid', apiKey: '', hasCredential: false },
  ])
  assert.equal(input[0].apiKey, credential)
  assert.equal(input[0].hasCredential, false)
  assert.throws(() => boundary.publicModels({}), { message: 'PUBLIC_MODELS_INVALID' })
  assert.throws(() => boundary.publicModels([null]), { message: 'PUBLIC_MODELS_INVALID' })
})

test('prototype-pollution fields and inherited data do not cross the browser boundary', () => {
  const boundary = createPublicResponseSanitizer()
  const input = JSON.parse('{"__proto__":{"polluted":true},"constructor":{"prototype":{"polluted":true}},"prototype":"unsafe","nested":{"safe":1,"__proto__":"unsafe"},"key":"author key"}')
  const output = boundary.sanitizePublicResponse(input)
  assert.deepEqual(output, { nested: { safe: 1 }, key: 'author key' })
  assert.equal(Object.prototype.polluted, undefined)
  assert.equal(Object.getPrototypeOf(output), Object.prototype)
  assert.equal(Object.hasOwn(output, '__proto__'), false)
  const inherited = Object.create({ inherited: 'not public', apiKey: 'inherited-credential' })
  inherited.own = false
  assert.deepEqual(boundary.sanitizePublicResponse(inherited), { own: false })
})

test('accessors and custom toJSON cannot bypass sanitization or execute during encoding', () => {
  const boundary = createPublicResponseSanitizer()
  let calls = 0
  const input = { safe: 'author text', toJSON() { calls++; return 'unsanitized' } }
  Object.defineProperty(input, 'getter', { enumerable: true, get() { calls++; return 'unsanitized' } })
  Object.defineProperty(input, 'password', { enumerable: true, get() { calls++; throw new Error('must not execute') } })
  const output = boundary.sanitizePublicResponse(input)
  assert.equal(JSON.stringify(output), '{"safe":"author text"}')
  assert.equal(calls, 0)
})

test('cycles terminate safely, but repeated non-cyclic references retain data', () => {
  const boundary = createPublicResponseSanitizer()
  const shared = { complete: false, key: 'author text' }
  const input = { shared, repeated: shared }
  input.self = input
  input.array = [input]
  const output = boundary.sanitizePublicResponse(input)
  assert.equal(output.self, '[Circular]')
  assert.equal(output.array[0], '[Circular]')
  assert.deepEqual(output.shared, shared)
  assert.deepEqual(output.repeated, shared)
  assert.notEqual(output.shared, output.repeated)
  assert.doesNotThrow(() => JSON.stringify(output))
  assert.equal(input.self, input)

  const deep = {}
  let cursor = deep
  for (let index = 0; index < 2000; index++) cursor = cursor.child = {}
  assert.equal(JSON.stringify(boundary.sanitizePublicResponse(deep)).includes('[Truncated]'), true)
})

test('fixed markers never directly disclose a registered marker credential', () => {
  const boundary = registered('[REDACTED]', 'Circular', 'Truncated', 'unit-marker-test')
  assert.equal(boundary.sanitizePublicResponse('before unit-marker-test after'), 'before  after')
  const cycle = {}
  cycle.self = cycle
  assert.equal(JSON.stringify(boundary.sanitizePublicResponse(cycle)).includes('Circular'), false)
})

test('replacement markers and deletion joins cannot recreate another registered literal', () => {
  const markerBoundary = registered('unit-boundary-source', 'prefix[REDACTED]')
  const output = markerBoundary.sanitizePublicResponse('prefixunit-boundary-source tail')
  assert.equal(output.includes('prefix[REDACTED]'), false)
  assert.equal(output.includes('unit-boundary-source'), false)

  const joinBoundary = registered('[RED', 'abcd', 'unit-join-source')
  const joined = joinBoundary.sanitizePublicResponse('abunit-join-sourcecd')
  assert.equal(joined, '')
})

test('secret registry is bounded and refuses overflow without silently evicting credentials', () => {
  const boundary = createPublicResponseSanitizer()
  for (let index = 0; index < 512; index++) assert.equal(boundary.registerSecret(`unit-capacity-${index}`), true)
  assert.equal(boundary.registerSecret('unit-capacity-0'), true)
  assert.throws(() => boundary.registerSecret('unit-capacity-overflow'), { message: 'PUBLIC_RESPONSE_SECRET_LIMIT' })
  assert.equal(boundary.sanitizePublicResponse('unit-capacity-0 + unit-capacity-511'), `${REDACTED} + ${REDACTED}`)

  const lengthBoundary = createPublicResponseSanitizer()
  assert.throws(() => lengthBoundary.registerSecret('x'.repeat(64 * 1024 + 1)), { message: 'PUBLIC_RESPONSE_SECRET_LIMIT' })
  for (let index = 0; index < 16; index++) lengthBoundary.registerSecret(`${String(index).padStart(2, '0')}${'x'.repeat(64 * 1024 - 2)}`)
  assert.throws(() => lengthBoundary.registerSecret('unit-total-character-overflow'), { message: 'PUBLIC_RESPONSE_SECRET_LIMIT' })
})

test('shared named exports provide the same public boundary API', () => {
  const credential = 'unit-only-shared-export-credential'
  assert.equal(registerSecret(credential), true)
  assert.equal(sanitizePublicResponse(`error ${credential}`), `error ${REDACTED}`)
  assert.deepEqual(publicModels([{ id: 'shared', apiKey: credential }]), [{ id: 'shared', apiKey: '', hasCredential: true }])
})
