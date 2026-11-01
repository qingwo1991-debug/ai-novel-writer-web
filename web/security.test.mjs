import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { Readable } from 'node:stream'
import {
  HttpError, Sessions, body, checkOrigin, constantEqual, inside, json, jsonBody,
  safeFilename, safePath, validateArguments,
} from './security.mjs'
import { directoryZip } from './downloads.mjs'

const origin = 'https://writer.example.test'
const origins = new Set([origin])
const rejects = (status, code) => error => error instanceof HttpError && error.status === status && error.code === code

function temporary(t) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'novel-web-security-'))
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }))
  return directory
}
function file(root, relative, contents = '') {
  const target = path.join(root, relative)
  fs.mkdirSync(path.dirname(target), { recursive: true })
  fs.writeFileSync(target, contents)
  return target
}
function response() {
  return {
    headers: {}, setHeader(name, value) { this.headers[name.toLowerCase()] = value },
    writeHead(status, headers) { this.status = status; Object.assign(this.headers, headers) },
    end(value) { this.value = value },
  }
}
function request(headers = {}) { return { headers } }
function payload(chunks, contentType = 'application/json') {
  return Object.assign(Readable.from(chunks.map(chunk => Buffer.from(chunk))), { headers: { 'content-type': contentType } })
}
function cookie(session) { return `novel_session=${session.id}` }

// An independent small ZIP reader checks both directory records and local file records.
// It is deliberately not an extractor and never writes archive-supplied paths to disk.
function parseZip(bytes) {
  assert.ok(Buffer.isBuffer(bytes))
  assert.ok(bytes.length >= 22)
  const end = bytes.length - 22
  assert.equal(bytes.readUInt32LE(end), 0x06054b50)
  assert.equal(bytes.readUInt16LE(end + 4), 0, 'one disk only')
  assert.equal(bytes.readUInt16LE(end + 6), 0)
  const count = bytes.readUInt16LE(end + 10)
  assert.equal(bytes.readUInt16LE(end + 8), count)
  const centralSize = bytes.readUInt32LE(end + 12)
  const centralOffset = bytes.readUInt32LE(end + 16)
  assert.equal(centralOffset + centralSize, end)
  assert.equal(bytes.readUInt16LE(end + 20), 0, 'no trailing archive comment')
  const entries = []
  let cursor = centralOffset
  for (let index = 0; index < count; index += 1) {
    assert.equal(bytes.readUInt32LE(cursor), 0x02014b50)
    const flags = bytes.readUInt16LE(cursor + 8)
    assert.equal(flags, 0x800, 'UTF-8 flag set, encryption and data-descriptor flags unset')
    assert.equal(bytes.readUInt16LE(cursor + 10), 0, 'store method, not compression')
    const crc = bytes.readUInt32LE(cursor + 16)
    const size = bytes.readUInt32LE(cursor + 20)
    assert.equal(bytes.readUInt32LE(cursor + 24), size)
    const nameLength = bytes.readUInt16LE(cursor + 28)
    const extraLength = bytes.readUInt16LE(cursor + 30)
    const commentLength = bytes.readUInt16LE(cursor + 32)
    const localOffset = bytes.readUInt32LE(cursor + 42)
    const name = bytes.subarray(cursor + 46, cursor + 46 + nameLength)
    assert.equal(bytes.readUInt32LE(localOffset), 0x04034b50)
    assert.equal(bytes.readUInt16LE(localOffset + 6), flags)
    assert.equal(bytes.readUInt16LE(localOffset + 8), 0)
    assert.equal(bytes.readUInt32LE(localOffset + 14), crc)
    assert.equal(bytes.readUInt32LE(localOffset + 18), size)
    assert.equal(bytes.readUInt32LE(localOffset + 22), size)
    assert.equal(bytes.readUInt16LE(localOffset + 26), nameLength)
    const localExtraLength = bytes.readUInt16LE(localOffset + 28)
    assert.deepEqual(bytes.subarray(localOffset + 30, localOffset + 30 + nameLength), name)
    const start = localOffset + 30 + nameLength + localExtraLength
    assert.ok(start + size <= centralOffset, 'file bytes precede the central directory')
    const contents = bytes.subarray(start, start + size)
    entries.push({ name: name.toString('utf8'), contents, crc, size, localOffset })
    cursor += 46 + nameLength + extraLength + commentLength
  }
  assert.equal(cursor, centralOffset + centralSize)
  return entries
}
function independentCrc32(bytes) {
  let value = 0xffffffff
  for (const byte of bytes) {
    value ^= byte
    for (let bit = 0; bit < 8; bit += 1) value = value & 1 ? (value >>> 1) ^ 0xedb88320 : value >>> 1
  }
  return (value ^ 0xffffffff) >>> 0
}

test('inside uses directory boundaries rather than vulnerable string prefixes', t => {
  const root = temporary(t)
  assert.equal(inside(root, root), true)
  assert.equal(inside(root, path.join(root, 'nested', 'book.txt')), true)
  assert.equal(inside(root, `${root}-sibling/book.txt`), false)
  assert.equal(inside(root, path.dirname(root)), false)
  assert.equal(inside(root, path.join(root, 'nested', '..', 'book.txt')), true)
})

test('safePath accepts contained existing files and explicitly permitted missing destinations', t => {
  const root = temporary(t)
  const target = file(root, '作品/正文.txt', '正文')
  assert.equal(safePath(root, ''), root)
  assert.equal(safePath(root, '作品/正文.txt'), target)
  assert.equal(safePath(root, '新作品/未创建.txt', { missing: true }), path.join(root, '新作品/未创建.txt'))
  assert.throws(() => safePath(root, 'missing.txt'), rejects(404, 'FILE_NOT_FOUND'))
})

test('safePath denies traversal, absolute paths, backslashes, NUL and non-string input', t => {
  const root = temporary(t)
  for (const candidate of ['..', '../escape', 'a/../../escape', '.', './book', 'a/./book', '/etc/passwd', 'a\\..\\secret', '\\server\\share', 'bad\0name', null, {}, 7]) {
    assert.throws(() => safePath(root, candidate, { missing: true }), rejects(400, 'INVALID_PATH'), String(candidate))
  }
})

test('safePath treats URL-encoded traversal as literal filenames, never a second decode', t => {
  const root = temporary(t)
  const literal = file(root, '%2e%2e/%2fsecret', 'literal')
  assert.equal(safePath(root, '%2e%2e/%2fsecret'), literal)
})

test('safePath denies file, directory and dangling child symlinks even for missing destinations', t => {
  const root = temporary(t)
  const outside = temporary(t)
  const target = file(outside, 'secret.txt', 'secret')
  fs.symlinkSync(target, path.join(root, 'linked-file'))
  fs.symlinkSync(outside, path.join(root, 'linked-directory'), 'dir')
  fs.symlinkSync(path.join(outside, 'missing'), path.join(root, 'dangling'))
  for (const candidate of ['linked-file', 'linked-directory/secret.txt', 'linked-directory/new/file.txt', 'dangling']) {
    assert.throws(() => safePath(root, candidate, { missing: true }), rejects(403, 'SYMLINK_DENIED'))
  }
})

test('safePath denies a symlink storage root and roots reached through symlink ancestors', t => {
  const base = temporary(t)
  const actual = path.join(base, 'actual')
  file(actual, 'nested/book.txt', 'book')
  const link = path.join(base, 'linked-root')
  fs.symlinkSync(actual, link, 'dir')
  for (const [root, relative] of [[link, 'nested/book.txt'], [path.join(link, 'nested'), 'book.txt']]) {
    assert.throws(() => safePath(root, relative), rejects(403, 'UNSAFE_STORAGE_ROOT'))
  }
})

test('safeFilename normalizes Unicode and strips controls, separators and unsafe edge dots', () => {
  assert.equal(safeFilename(' ..小説/章节\\一<>:"|?*\r\n\0\x7f.txt... '), '小説_章节_一___________.txt')
  assert.equal(safeFilename('e\u0301.txt'), 'é.txt')
  assert.equal(safeFilename('... '), 'document')
  assert.equal(safeFilename(undefined, 'fallback.txt'), 'fallback.txt')
  assert.equal(safeFilename('a'.repeat(200)).length, 160)
  assert.ok(!/[<>:"/\\|?*\x00-\x1f\x7f]/u.test(safeFilename('../../危险\\文件.txt')))
})

test('body accepts an exact byte limit and rejects oversize before consuming the remainder', async () => {
  assert.deepEqual(await body(payload(['ab', 'cd']), 4), Buffer.from('abcd'))
  assert.equal((await body(payload([]), 0)).length, 0)
  let consumed = 0
  async function* stream() { for (const value of ['ab', 'cd', 'never-read']) { consumed += 1; yield Buffer.from(value) } }
  await assert.rejects(body(stream(), 3), rejects(413, 'BODY_TOO_LARGE'))
  assert.equal(consumed, 2)
})

test('jsonBody accepts valid JSON media type, charset and scalar values without altering text', async () => {
  assert.deepEqual(await jsonBody(payload(['{"text":"章', '节"}'], 'application/json; charset=utf-8')), { text: '章节' })
  assert.equal(await jsonBody(payload(['null'])), null)
  assert.equal(await jsonBody(payload(['false'])), false)
})

test('jsonBody rejects invalid JSON, non-JSON lookalike types and oversized payloads', async () => {
  await assert.rejects(jsonBody(payload(['{broken}'])), rejects(400, 'INVALID_JSON'))
  for (const type of [undefined, 'text/plain', 'application/jsonp', 'application/json-malicious']) {
    const req = payload(['{}'], type)
    if (type === undefined) delete req.headers['content-type']
    await assert.rejects(jsonBody(req), rejects(415, 'JSON_REQUIRED'))
  }
  await assert.rejects(jsonBody(payload(['{"long":true}']), 4), rejects(413, 'BODY_TOO_LARGE'))
})

test('json responses disable caching and MIME sniffing', () => {
  const res = response()
  json(res, 403, { error: { code: 'DENIED' } })
  assert.equal(res.status, 403)
  assert.equal(res.headers['Content-Type'], 'application/json; charset=utf-8')
  assert.equal(res.headers['Cache-Control'], 'no-store')
  assert.equal(res.headers['X-Content-Type-Options'], 'nosniff')
  assert.deepEqual(JSON.parse(res.value), { error: { code: 'DENIED' } })
})

test('constantEqual compares strings without accepting non-string secrets', () => {
  assert.equal(constantEqual('abc', 'abc'), true)
  assert.equal(constantEqual('abc', 'abd'), false)
  assert.equal(constantEqual('abc', 'ab'), false)
  assert.equal(constantEqual('', ''), true)
  for (const value of [undefined, null, 17, [], Buffer.from('abc')]) {
    assert.equal(constantEqual(value, 'abc'), false)
    assert.equal(constantEqual('abc', value), false)
  }
})

test('constantEqual returns false rather than throwing for equal JS length but unequal UTF-8 byte length', () => {
  assert.equal(constantEqual('é', 'a'), false)
  assert.equal(constantEqual('é'.repeat(64), 'a'.repeat(64)), false)
  assert.equal(constantEqual('😀', 'ab'), false)
  assert.equal(constantEqual('秘密', '秘密'), true)
  assert.equal(constantEqual('秘密', '机密'), false)
})

test('checkOrigin requires an exact allowlisted origin and denies explicit cross-site requests', () => {
  assert.doesNotThrow(() => checkOrigin(request({ origin, 'sec-fetch-site': 'same-origin' }), origins))
  assert.doesNotThrow(() => checkOrigin(request({ origin }), origins))
  for (const value of [undefined, null, 'null', '*', 'http://writer.example.test', `${origin}:443`, `${origin}.evil.test`, `${origin}/`, [origin]]) {
    assert.throws(() => checkOrigin(request({ origin: value }), origins), rejects(403, 'ORIGIN_DENIED'))
  }
  assert.throws(() => checkOrigin(request({ origin, 'sec-fetch-site': 'cross-site' }), origins), rejects(403, 'CROSS_SITE_DENIED'))
})

test('Sessions issues random HttpOnly SameSite cookie and a separate CSRF secret', () => {
  const sessions = new Sessions()
  const res = response()
  const created = sessions.get(request(), res, 'author-a')
  assert.match(created.id, /^[a-f0-9]{64}$/u)
  assert.match(created.csrf, /^[a-f0-9]{64}$/u)
  assert.notEqual(created.id, created.csrf)
  assert.equal(created.principal, 'author-a')
  assert.ok(created.clients instanceof Map)
  assert.equal(res.headers['set-cookie'], `${cookie(created)}; HttpOnly; SameSite=Strict; Path=/`)
  assert.ok(!res.headers['set-cookie'].includes(created.csrf))
  const secureRes = response()
  sessions.get(request({ 'x-forwarded-proto': 'https' }), secureRes, 'author-a')
  assert.match(secureRes.headers['set-cookie'], /; Secure$/u)
})

test('Sessions parses exact cookie key and reuses only the same principal session', () => {
  const sessions = new Sessions()
  const original = sessions.get(request(), response(), 'author-a')
  original.clients.set('tab-1', { id: 'tab-1' })
  const res = response()
  const reused = sessions.get(request({ cookie: `unrelated=x; ${cookie(original)}; another=y` }), res, 'author-a')
  assert.equal(reused, original)
  assert.equal(res.headers['set-cookie'], undefined)
  const otherPrincipal = sessions.get(request({ cookie: cookie(original) }), response(), 'author-b')
  assert.notEqual(otherPrincipal.id, original.id)
  assert.equal(otherPrincipal.clients.size, 0)
  assert.equal(original.principal, 'author-a')
  for (const wrong of [`not_${cookie(original)}`, 'novel_session=forged', `novel_session=${original.id}trailing`]) {
    const fresh = sessions.get(request({ cookie: wrong }), response(), 'author-a')
    assert.notEqual(fresh.id, original.id)
  }
})

test('Sessions expires old cookies and refreshes last-touch on valid reuse', () => {
  const sessions = new Sessions()
  const original = sessions.get(request(), response(), 'author-a')
  original.touched = Date.now() - 25 * 60 * 60 * 1000
  const fresh = sessions.get(request({ cookie: cookie(original) }), response(), 'author-a')
  assert.notEqual(fresh.id, original.id)
  fresh.touched -= 5000
  const before = fresh.touched
  assert.equal(sessions.get(request({ cookie: cookie(fresh) }), response(), 'author-a'), fresh)
  assert.ok(fresh.touched > before)
})

test('Sessions enforces a bounded session count and prunes stale entries before refusing new sessions', () => {
  const sessions = new Sessions()
  const entries = Array.from({ length: 256 }, () => sessions.get(request(), response(), 'author-a'))
  assert.throws(() => sessions.get(request(), response(), 'author-a'), rejects(503, 'SESSION_LIMIT'))
  entries[0].touched = Date.now() - 2 * 60 * 60 * 1000
  const replacement = sessions.get(request(), response(), 'author-a')
  assert.notEqual(replacement.id, entries[0].id)
  assert.throws(() => sessions.get(request(), response(), 'author-a'), rejects(503, 'SESSION_LIMIT'))
})

test('Sessions.verify binds CSRF to session and origin and denies Unicode mismatch safely', () => {
  const sessions = new Sessions()
  const a = sessions.get(request(), response(), 'author-a')
  const b = sessions.get(request(), response(), 'author-b')
  assert.doesNotThrow(() => sessions.verify(request({ origin, 'x-csrf-token': a.csrf }), a, origins))
  for (const token of [undefined, null, '', b.csrf, [a.csrf], 'é'.repeat(a.csrf.length)]) {
    assert.throws(() => sessions.verify(request({ origin, 'x-csrf-token': token }), a, origins), rejects(403, 'CSRF_DENIED'))
  }
  assert.throws(() => sessions.verify(request({ 'x-csrf-token': a.csrf }), a, origins), rejects(403, 'ORIGIN_DENIED'))
  assert.throws(() => sessions.verify(request({ origin, 'x-csrf-token': a.csrf, 'sec-fetch-site': 'cross-site' }), a, origins), rejects(403, 'CROSS_SITE_DENIED'))
})

test('validateArguments permits ordinary JSON while denying non-array and excessive positional args', () => {
  assert.doesNotThrow(() => validateArguments([]))
  assert.doesNotThrow(() => validateArguments([null, false, 42, '正文', { nested: ['内容', { ok: true }] }]))
  assert.doesNotThrow(() => validateArguments(Array(16).fill(null)))
  for (const value of [null, undefined, {}, 'args', Array(17).fill(null)]) {
    assert.throws(() => validateArguments(value), rejects(400, 'INVALID_ARGUMENTS'))
  }
})

test('validateArguments denies dangerous prototype keys at every nesting level', () => {
  for (const key of ['__proto__', 'prototype', 'constructor']) {
    assert.throws(() => validateArguments([JSON.parse(`{"nested":{"${key}":{"polluted":true}}}`)]), rejects(400, 'INVALID_OBJECT_KEY'))
  }
  assert.equal(Object.prototype.polluted, undefined)
})

test('validateArguments denies nonfinite numeric values and bounds recursion and node count', () => {
  for (const value of [Number.NaN, Infinity, -Infinity]) {
    assert.throws(() => validateArguments([{ nested: value }]), rejects(400, 'INVALID_NUMBER'))
  }
  let nested = null
  for (let level = 0; level < 34; level += 1) nested = { nested }
  assert.throws(() => validateArguments([nested]), rejects(400, 'ARGUMENT_COMPLEXITY'))
  const cyclic = {}; cyclic.self = cyclic
  assert.throws(() => validateArguments([cyclic]), rejects(400, 'ARGUMENT_COMPLEXITY'))
  assert.throws(() => validateArguments([Array(100000).fill(null)]), rejects(400, 'ARGUMENT_COMPLEXITY'))
  assert.doesNotThrow(() => validateArguments([Array(1000).fill(null)]))
})

test('directoryZip encodes the standard CRC32 test vector into local and central records', t => {
  const root = temporary(t)
  file(root, 'crc.txt', '123456789')
  const entries = parseZip(directoryZip(root))
  assert.equal(entries.length, 1)
  assert.equal(entries[0].name, 'crc.txt')
  assert.equal(entries[0].crc, 0xcbf43926)
  assert.equal(entries[0].size, 9)
  assert.equal(entries[0].localOffset, 0)
  assert.equal(entries[0].contents.toString(), '123456789')
})

test('directoryZip preserves Unicode names, nested/multiple files, empty files and binary content', t => {
  const root = temporary(t)
  const expected = new Map([
    ['正文/第一章😀.txt', Buffer.from('第一章\r\n中文内容\n')],
    ['角色/e\u0301.txt', Buffer.from('人物')],
    ['empty.txt', Buffer.alloc(0)],
    ['binary.bin', Buffer.from([0, 255, 128, 1, 2, 3])],
  ])
  for (const [name, contents] of expected) file(root, name, contents)
  fs.mkdirSync(path.join(root, 'empty-directory'))
  const entries = parseZip(directoryZip(root))
  assert.equal(entries.length, expected.size)
  for (const entry of entries) {
    assert.ok(expected.has(entry.name), `Unexpected archive entry ${entry.name}`)
    assert.deepEqual(entry.contents, expected.get(entry.name))
    assert.equal(entry.crc, independentCrc32(entry.contents))
    assert.equal(entry.size, entry.contents.length)
    assert.ok(!entry.name.startsWith('/') && !entry.name.split('/').includes('..'))
  }
})

test('directoryZip creates a valid empty ZIP for a directory with no files', t => {
  const root = temporary(t)
  fs.mkdirSync(path.join(root, 'nested-empty'))
  const bytes = directoryZip(root)
  assert.equal(bytes.length, 22)
  assert.deepEqual(parseZip(bytes), [])
})

test('directoryZip denies file, directory and dangling symlinks without packaging their targets', t => {
  for (const kind of ['file', 'directory', 'dangling']) {
    const root = temporary(t)
    const outside = temporary(t)
    const target = file(outside, 'secret.txt', 'do not export')
    fs.symlinkSync(kind === 'file' ? target : kind === 'directory' ? outside : path.join(outside, 'missing'), path.join(root, 'linked'), kind === 'file' ? 'file' : 'dir')
    assert.throws(() => directoryZip(root), rejects(403, 'EXPORT_SYMLINK_DENIED'))
  }
})

test('directoryZip refuses symlink export roots instead of packaging data outside storage', t => {
  const base = temporary(t)
  const outside = temporary(t)
  file(outside, 'secret.txt', 'do not export')
  const link = path.join(base, 'export-link')
  fs.symlinkSync(outside, link, 'dir')
  assert.throws(() => directoryZip(link), rejects(403, 'UNSAFE_STORAGE_ROOT'))
})

test('directoryZip rejects oversized sparse files before reading or allocating their bytes', t => {
  const root = temporary(t)
  const target = file(root, 'too-large.bin')
  fs.truncateSync(target, 256 * 1024 * 1024 + 1)
  const reader = t.mock.method(fs, 'readFileSync', () => assert.fail('Oversized exports must be rejected before reading file bytes'))
  assert.throws(() => directoryZip(root), rejects(413, 'EXPORT_TOO_LARGE'))
  assert.equal(reader.mock.callCount(), 0)
})

test('directoryZip applies the size cap to the aggregate export rather than each file separately', t => {
  const root = temporary(t)
  const a = file(root, 'a.bin'), b = file(root, 'b.bin')
  fs.truncateSync(a, 128 * 1024 * 1024)
  fs.truncateSync(b, 128 * 1024 * 1024 + 1)
  const reader = t.mock.method(fs, 'readFileSync', () => assert.fail('Over-limit aggregates must not be read'))
  assert.throws(() => directoryZip(root), rejects(413, 'EXPORT_TOO_LARGE'))
  assert.equal(reader.mock.callCount(), 0)
})

test('directoryZip bounds entry count before file reads without creating thousands of fixture files', t => {
  const root = temporary(t)
  const directoryInfo = fs.lstatSync(root)
  const children = Array.from({ length: 20001 }, (_, index) => ({
    name: `${index}.txt`, isSymbolicLink: () => false, isDirectory: () => false, isFile: () => true,
  }))
  t.mock.method(fs, 'readdirSync', () => children)
  t.mock.method(fs, 'lstatSync', target => path.resolve(target) === root ? directoryInfo : { isSymbolicLink: () => false })
  t.mock.method(fs, 'statSync', () => ({ size: 0 }))
  const reader = t.mock.method(fs, 'readFileSync', () => assert.fail('Over-limit entry catalogs must not be read'))
  assert.throws(() => directoryZip(root), rejects(413, 'EXPORT_ENTRY_LIMIT'))
  assert.equal(reader.mock.callCount(), 0)
})
