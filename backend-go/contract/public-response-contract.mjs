import assert from 'node:assert/strict'
import { existsSync, mkdirSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { createPublicResponseSanitizer } from '../../web/public-response.mjs'

// Directly import the actual, unmodified JS output boundary. No TS loader, HTTP
// service, source rewriting, credentials, database, or author data is involved.
assert.equal(process.env.GITHUB_ACTIONS, 'true', 'contract generation is GitHub-only')
const base = process.env.RUNNER_TEMP
const target = process.env.GO_PUBLIC_RESPONSE_CONTRACT
assert(base && target && path.isAbsolute(base) && path.isAbsolute(target))
assert.equal(path.normalize(target), target, 'contract path must be canonical')
assert.equal(target, path.join(base, 'novel-go-public-response', 'contract.json'))
assert(!existsSync(target), 'do not overwrite an existing oracle')
const units = text => Array.from({ length: text.length }, (_, i) => text.charCodeAt(i))
const json = value => JSON.parse(JSON.stringify(value))
const cases = []
function add(name, secrets, kind, input, options = {}) {
  const sanitizer = createPublicResponseSanitizer()
  const registrations = secrets.map(secret => {
    try { return { retained: sanitizer.registerSecret(secret), error: '' } }
    catch (error) { assert.equal(error.message, 'PUBLIC_RESPONSE_SECRET_LIMIT'); return { retained: false, error: error.message } }
  })
  let value = input
  if (kind === 'cycle-map') { value = { message: 'synthetic-token' }; value.self = value }
  if (kind === 'cycle-array') { value = []; value.push(value) }
  if (kind === 'shared') { const shared = { text: 'synthetic-token', apiKey: 'private-value' }; value = [shared, shared] }
  if (kind === 'depth') { value = options.objectLeaf ? { leaf: 'synthetic-token' } : 'synthetic-token'; for (let i = 0; i < options.depth; i++) value = [value] }
  if (kind === 'unsupported') { value = { missing: undefined, callable: () => 'never', symbol: Symbol('synthetic-token'), array: [undefined, () => 'never', Symbol('synthetic-token'), NaN, Infinity], nil: null } }
  const before = ['json', 'string', 'error-string', 'raw-json'].includes(kind) ? JSON.stringify(value) : null
  if (kind === 'raw-json') value = JSON.parse(input)
  const result = sanitizer.sanitizePublicResponse(value)
  if (before !== null && kind !== 'raw-json') assert.equal(JSON.stringify(value), before, 'source was mutated')
  const fixture = { name, kind, secrets: secrets.map(units), registrations, ...options }
  if (kind === 'string' || kind === 'error-string') { fixture.inputUnits = units(input); fixture.expectedUnits = units(result) }
  else { if (kind === 'json' || kind === 'raw-json') fixture.input = input; fixture.expected = json(result) }
  cases.push(fixture)
}

const strings = [
  ['ordinary', ['synthetic-token'], 'prefix synthetic-token suffix'],
  ['self-overlap', ['aaaa'], 'xaaaaaay'],
  ['cross-overlap', ['abcde', 'defgh'], 'xabcdefghy'],
  ['cross-overlap-reverse', ['defgh', 'abcde'], 'xabcdefghy'],
  ['adjacent-union', ['abcd', 'efgh'], 'abcdefgh'],
  ['regex-is-literal', ['a.*$'], 'aZZ a.*$'],
  ['no-trimming', [' abc '], 'abc| abc |abc'],
  ['marker-substring', ['REDA'], 'xREDAy[REDACTED]z'],
  ['marker-whole', ['[REDACTED]'], '[REDACTED] x [REDACTED]'],
  ['marker-boundary', ['abcd', 'x[RE'], 'xabcdz'],
  ['fixed-point-join', ['abcd', 'REDA', 'xyzq'], 'xyabcdzq'],
  ['astral-minimum', ['😀😀'], 'a😀😀b😀c'],
  ['astral-short', ['😀a'], '😀a'],
  ['cjk-short', ['界界界'], '界界界'],
  ['lone-surrogate', ['a\ud800bc'], 'x a\ud800bc y \udfff'],
  ['half-astral-short', ['x\ud83d', '\ude00yz'], 'x😀yz'],
  ['half-astral-overlap', ['abc\ud83d', '\ude00def'], 'abc😀def'],
  ['surrogate-four-units', ['a\ud83d\ude00b'], 'before a😀b after'],
  ['short-values', ['', 'a', 'abc'], 'abc a'],
  ['nul-and-line-break', ['a\u0000b\nc'], 'before a\u0000b\nc after'],
  ['case-sensitive', ['ABCD'], 'abcd ABCD'],
  ['unicode-normalization-literal', ['éééé'], 'e\u0301e\u0301e\u0301e\u0301 éééé'],
]
for (const [name, secrets, input] of strings) add(name, secrets, 'string', input)
for (const [index, secret] of ['REDA', 'CTED', 'ACTE', '[RED', 'TED]', 'DACT', '[REDACTED]'].entries()) {
  add(`marker/${index}`, [secret, 'synthetic-token'], 'string', `synthetic-token/${secret}/[REDACTED]`)
}
for (const [index, input] of [
  { apiKey: 'synthetic-token', password: 'keep-private', ACCESStoken: 'synthetic-token', SECRET: 0, key: 'ordinary', false: false, zero: 0, nil: null },
  { 'synthetic-token/key': 'synthetic-token', 'synthetic-token': 'x', array: [false, 0, null, 'synthetic-token', { ApiKEY: 'private' }] },
  JSON.parse('{"__proto__":{"secret":"private"},"prototype":"bad","constructor":"bad","Constructor":"safe","APİKEY":"synthetic-token","APIKEY":"private"}'),
  { already: '[REDACTED]', unregistered: 'unregistered-value', api_key: 'ordinary', access_token: 'ordinary', secretThing: 'ordinary' },
].entries()) add(`json/${index}`, ['synthetic-token'], 'json', input)
add('json/redacted-unsafe-keys', ['REDA'], 'json', { conREDAstructor: 'must-not-copy', __proREDAto__: 'must-not-copy', safe: true })
add('json/redacted-key-collision', ['aaaa', 'bbbb'], 'json', { aaaa: 'first', bbbb: 'last' })
for (const kind of ['cycle-map', 'cycle-array', 'shared', 'unsupported']) {
  add(kind, ['synthetic-token'], kind, null)
  add(`${kind}/marker-secrets`, ['synthetic-token', 'Circular', 'Truncated'], kind, null)
}
for (const depth of [0, 1, 127, 128, 129, 130, 256]) for (const objectLeaf of [false, true]) {
  add(`depth/${depth}/${objectLeaf}`, ['synthetic-token'], 'depth', null, { depth, objectLeaf })
}
for (const error of [
  'Error: upstream rejected synthetic-token',
  'TypeError: synthetic-token in synthetic-token provider endpoint',
  'SyntaxError: Unexpected token in "synthetic-token"',
  'prefix synthetic-token suffix\nsecret=synthetic-token',
]) add(`error-string/${cases.length}`, ['synthetic-token'], 'error-string', error)
add('raw-json/object', ['synthetic-token'], 'raw-json', '{"apiKey":"synthetic-token","nested":{"text":"synthetic-token"},"key":"safe"}')
add('raw-json/array', ['synthetic-token'], 'raw-json', '["synthetic-token",false,0,null,{"password":"private"}]')
add('raw-json/number', [], 'raw-json', '42')
add('raw-json/lone-surrogate', ['a\ud800bc'], 'raw-json', '{"text":"a\\ud800bc"}')
add('raw-json/redacted-key-order', ['aaaa', 'bbbb'], 'raw-json', '{"bbbb":"first","aaaa":"last"}')
add('raw-json/duplicate-key-order', ['aaaa', 'bbbb'], 'raw-json', '{"bbbb":"first","aaaa":"last","bbbb":"replacement"}')
add('raw-json/numeric-key-order', ['1234', '5678'], 'raw-json', '{"5678":"higher","1234":"lower"}')

// Deterministic adversarial literal combinations broaden overlap and cleanup
// coverage without using the Go implementation to manufacture expected output.
let state = 0x12feab34
const next = () => { state = (Math.imul(state, 1664525) + 1013904223) >>> 0; return state }
const tokens = ['a', 'b', 'c', 'd', '[', ']', 'R', 'E', 'D', 'A', 'C', 'T', '😀', '\ud800', '\udfff']
for (let i = 0; i < 96; i++) {
  const input = Array.from({ length: 48 }, () => tokens[next() % tokens.length]).join('')
  const start = next() % Math.max(1, input.length - 8)
  const secrets = [input.slice(start, start + 4 + next() % 7), input.slice(start + 1, start + 8), 'REDA']
  add(`synthetic/${i}`, secrets, 'string', input)
}
add('capacity/count-no-eviction', [...Array.from({ length: 512 }, (_, i) => `synthetic-${String(i).padStart(4, '0')}`), 'synthetic-extra', 'synthetic-0000'], 'string', 'synthetic-0000 synthetic-extra')
add('capacity/utf16-per-secret', ['😀'.repeat(32768), '😀'.repeat(32768) + 'x', '界'.repeat(65536), '界'.repeat(65537)], 'string', 'unchanged')
const full = Array.from({ length: 16 }, (_, i) => String(i).padStart(4, '0') + '界'.repeat(65532))
add('capacity/utf16-total', [...full, 'next', full[0]], 'string', 'next')

// JS-only object hazards have no Go accessor/prototype equivalent, but verify
// the real oracle's boundary rather than projecting these away silently.
{
  const sanitizer = createPublicResponseSanitizer()
  sanitizer.registerSecret('synthetic-token')
  const proto = { inherited: 'must-not-copy' }
  const value = Object.create(proto)
  Object.defineProperty(value, 'accessor', { enumerable: true, get() { throw new Error('accessor invoked') } })
  value.own = 'synthetic-token'
  value.toJSON = () => { throw new Error('toJSON invoked') }
  const result = sanitizer.sanitizePublicResponse(value)
  assert.deepEqual(json(result), { own: '[REDACTED]' })
  assert.equal(Object.getPrototypeOf(result), Object.prototype)
  const error = new Error('synthetic-token')
  assert.deepEqual(json(sanitizer.sanitizePublicResponse(error)), {})
  assert.equal(error.message, 'synthetic-token')
}
mkdirSync(path.dirname(target), { recursive: true, mode: 0o700 })
writeFileSync(target, JSON.stringify({ formatVersion: 1, source: 'web/public-response.mjs', encoding: 'utf16-code-units', cases }) + '\n', { flag: 'wx', mode: 0o600 })
console.log(JSON.stringify({ formatVersion: 1, caseCount: cases.length, source: 'web/public-response.mjs' }))
