import crypto from 'node:crypto'
import fs from 'node:fs'
import path from 'node:path'

export class HttpError extends Error {
  constructor(status, code, message = code) { super(message); this.status = status; this.code = code }
}
export function inside(root, value) {
  const rel = path.relative(path.resolve(root), path.resolve(value))
  return rel === '' || (!rel.startsWith(`..${path.sep}`) && rel !== '..' && !path.isAbsolute(rel))
}
export function safePath(root, relative, { missing = false } = {}) {
  if (typeof relative !== 'string' || relative.includes('\0') || relative.includes('\\') || path.isAbsolute(relative)
    || relative.split('/').some(s => s === '..' || s === '.')) throw new HttpError(400, 'INVALID_PATH')
  const target = path.resolve(root, relative)
  if (!inside(root, target)) throw new HttpError(403, 'PATH_OUTSIDE_STORAGE')
  let cursor = path.resolve(root)
  const rootInfo = fs.lstatSync(cursor)
  if (!rootInfo.isDirectory() || rootInfo.isSymbolicLink() || fs.realpathSync(cursor) !== cursor) throw new HttpError(403, 'UNSAFE_STORAGE_ROOT')
  for (const part of path.relative(cursor, target).split(path.sep).filter(Boolean)) {
    cursor = path.join(cursor, part)
    let info
    try { info = fs.lstatSync(cursor) } catch (error) {
      if (missing && error.code === 'ENOENT') continue
      throw new HttpError(404, 'FILE_NOT_FOUND')
    }
    if (info.isSymbolicLink()) throw new HttpError(403, 'SYMLINK_DENIED')
  }
  return target
}
export function safeFilename(value, fallback = 'document') {
  if (typeof value !== 'string') return fallback
  const name = value.normalize('NFC').replace(/[\x00-\x1f\x7f<>:"/\\|?*]/g, '_').replace(/^[. ]+|[. ]+$/g, '').slice(0, 160)
  return name || fallback
}
export async function body(req, limit = 8 * 1024 * 1024) {
  const chunks = []; let size = 0
  for await (const chunk of req) {
    size += chunk.length
    if (size > limit) throw new HttpError(413, 'BODY_TOO_LARGE')
    chunks.push(chunk)
  }
  return Buffer.concat(chunks)
}
export async function jsonBody(req, limit) {
  if (String(req.headers['content-type']).split(';')[0].trim().toLowerCase() !== 'application/json') throw new HttpError(415, 'JSON_REQUIRED')
  try { return JSON.parse((await body(req, limit)).toString('utf8')) }
  catch (error) { if (error instanceof HttpError) throw error; throw new HttpError(400, 'INVALID_JSON') }
}
export function checkOrigin(req, allowedOrigins) {
  const origin = req.headers.origin
  if (typeof origin !== 'string' || !allowedOrigins.has(origin)) throw new HttpError(403, 'ORIGIN_DENIED')
  if (req.headers['sec-fetch-site'] === 'cross-site') throw new HttpError(403, 'CROSS_SITE_DENIED')
}
export function json(res, status, value) {
  res.writeHead(status, { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store', 'X-Content-Type-Options': 'nosniff' })
  res.end(JSON.stringify(value))
}
export function constantEqual(a, b) {
  if (typeof a !== 'string' || typeof b !== 'string') return false
  const left = Buffer.from(a), right = Buffer.from(b)
  return left.length === right.length && crypto.timingSafeEqual(left, right)
}
export class Sessions {
  #sessions = new Map()
  get(req, res, principal) {
    const cookie = String(req.headers.cookie ?? '').split(';').map(x => x.trim()).find(x => x.startsWith('novel_session='))?.slice(14)
    let session = cookie && this.#sessions.get(cookie)
    if (session && (session.principal !== principal || Date.now() - session.touched > 24 * 60 * 60 * 1000)) session = undefined
    if (!session) {
      if (this.#sessions.size >= 256) {
        for (const [key, value] of this.#sessions) if (Date.now() - value.touched > 60 * 60 * 1000) this.#sessions.delete(key)
        if (this.#sessions.size >= 256) throw new HttpError(503, 'SESSION_LIMIT')
      }
      session = { id: crypto.randomBytes(32).toString('hex'), csrf: crypto.randomBytes(32).toString('hex'), principal, touched: Date.now(), clients: new Map() }
      this.#sessions.set(session.id, session)
      const secure = req.headers['x-forwarded-proto'] === 'https' ? '; Secure' : ''
      res.setHeader('Set-Cookie', `novel_session=${session.id}; HttpOnly; SameSite=Strict; Path=/${secure}`)
    }
    session.touched = Date.now()
    return session
  }
  verify(req, session, allowedOrigins) {
    checkOrigin(req, allowedOrigins)
    if (!constantEqual(req.headers['x-csrf-token'], session.csrf)) throw new HttpError(403, 'CSRF_DENIED')
  }
}
export function validateArguments(args) {
  if (!Array.isArray(args) || args.length > 16) throw new HttpError(400, 'INVALID_ARGUMENTS')
  let nodes = 0
  function visit(value, depth) {
    if (++nodes > 100000 || depth > 32) throw new HttpError(400, 'ARGUMENT_COMPLEXITY')
    if (typeof value === 'number' && !Number.isFinite(value)) throw new HttpError(400, 'INVALID_NUMBER')
    if (value && typeof value === 'object') for (const [key, item] of Object.entries(value)) {
      if (['__proto__', 'prototype', 'constructor'].includes(key)) throw new HttpError(400, 'INVALID_OBJECT_KEY')
      visit(item, depth + 1)
    }
  }
  visit(args, 0)
}
