/**
 * Browser-output boundary only: use immediately before JSON HTTP/SSE encoding,
 * never on private worker results used for credentials, grants, or routing.
 *
 * Register known runtime credentials explicitly, BEFORE an operation can emit
 * provider errors/events. In particular, register every saved model.apiKey from
 * the first PRIVATE models() read at startup, plus newly supplied credentials.
 * This is literal redaction, NOT arbitrary credential detection: unregistered,
 * short (< 4 UTF-16 code units), encoded, or split-across-events secrets are not
 * recognized. This module neither reads storage nor discovers/decrypts secrets.
 */

const CREDENTIAL_FIELDS = new Set(['apikey', 'password', 'accesstoken', 'secret'])
const UNSAFE_FIELDS = new Set(['__proto__', 'prototype', 'constructor'])
const MIN_SECRET_LENGTH = 4
const MAX_SECRETS = 512
const MAX_SECRET_LENGTH = 64 * 1024
const MAX_SECRET_CHARACTERS = 1024 * 1024
const MAX_DEPTH = 128
const REDACTED = '[REDACTED]'
const CIRCULAR = '[Circular]'
const TRUNCATED = '[Truncated]'

// Merge already-sorted ranges as a union. Overlap (including partial overlap
// between different credentials) must not leave a visible tail of either one.
function mergeRanges(left, right) {
  const merged = []
  let a = 0, b = 0
  while (a < left.length || b < right.length) {
    const next = b >= right.length || (a < left.length && left[a][0] <= right[b][0]) ? left[a++] : right[b++]
    const last = merged.at(-1)
    if (last && next[0] <= last[1]) last[1] = Math.max(last[1], next[1])
    else merged.push([...next])
  }
  return merged
}

function redactPass(value, secrets, marker) {
  let ranges = []
  for (const secret of secrets) {
    const matches = []
    let offset = value.indexOf(secret)
    while (offset !== -1) {
      const end = offset + secret.length
      const last = matches.at(-1)
      if (last && offset <= last[1]) last[1] = end
      else matches.push([offset, end])
      // +1, not +secret.length: matches of one literal can overlap, too.
      offset = value.indexOf(secret, offset + 1)
    }
    if (matches.length) ranges = mergeRanges(ranges, matches)
  }
  if (!ranges.length) return value
  const parts = []
  let offset = 0
  for (const [start, end] of ranges) {
    parts.push(value.slice(offset, start), marker)
    offset = end
  }
  parts.push(value.slice(offset))
  return parts.join('')
}

function redactString(value, secrets) {
  // A credential can itself equal or be part of the usual marker. Never emit
  // such a marker verbatim; removing that match is safer than recreating it.
  const marker = [...secrets].some(secret => REDACTED.includes(secret)) ? '' : REDACTED
  let output = redactPass(value, secrets, marker)
  // Markers or joins can create another known literal across a replacement
  // boundary. Cleanup only deletes: each changed pass is strictly shorter, so
  // this reaches a fixed point without cascading marker substitution loops.
  while (true) {
    const clean = redactPass(output, secrets, '')
    if (clean === output) return output
    output = clean
  }
}

function ownValue(object, key) {
  const descriptor = Object.getOwnPropertyDescriptor(object, key)
  return descriptor && 'value' in descriptor ? descriptor.value : undefined
}

function put(object, key, value) {
  Object.defineProperty(object, key, { value, enumerable: true, writable: true, configurable: true })
}

/** Separate instances are useful for isolated tests and independent gateways. */
export function createPublicResponseSanitizer() {
  const secrets = new Set()
  let secretCharacters = 0

  /**
   * Returns false for nonstrings/short values, true for retained credentials.
   * No trimming/normalization/regex interpretation is performed. Storage is
   * bounded; capacity failure throws a fixed, credential-free error rather than
   * silently evicting an active secret. Call BEFORE using a new credential and
   * reject the operation on failure. Old credentials remain protected until the
   * process/instance is replaced; there is intentionally no browser-driven reset.
   */
  function registerSecret(value) {
    if (typeof value !== 'string' || value.length < MIN_SECRET_LENGTH) return false
    if (secrets.has(value)) return true
    if (value.length > MAX_SECRET_LENGTH || secrets.size >= MAX_SECRETS
      || secretCharacters + value.length > MAX_SECRET_CHARACTERS) {
      throw new Error('PUBLIC_RESPONSE_SECRET_LIMIT')
    }
    secrets.add(value)
    secretCharacters += value.length
    return true
  }

  /**
   * Immutable JSON-data clone. Credential-named fields are always blanked;
   * ordinary `key` fields and false/0/null/array ordering are preserved. Strings
   * include property names. Dangerous prototype keys and accessors are omitted,
   * and no inherited fields, custom prototypes, or toJSON functions are copied.
   * Back-edges/depth overflow become safe markers rather than loops. Shared but
   * non-cyclic objects are independently cloned, not mistaken for cycles.
   * Non-JSON primitives become undefined, as JSON would omit them in objects.
   * Registration is deliberately NOT inferred from arbitrary response fields.
   */
  function sanitizePublicResponse(value) {
    const active = new WeakSet()
    function visit(item, depth) {
      if (typeof item === 'string') return redactString(item, secrets)
      if (item === null || typeof item === 'boolean' || typeof item === 'number') return item
      if (typeof item !== 'object') return undefined
      if (active.has(item)) return redactString(CIRCULAR, secrets)
      if (depth > MAX_DEPTH) return redactString(TRUNCATED, secrets)
      active.add(item)
      try {
        if (Array.isArray(item)) {
          const output = new Array(item.length)
          for (let index = 0; index < item.length; index++) {
            const descriptor = Object.getOwnPropertyDescriptor(item, String(index))
            if (descriptor && 'value' in descriptor) output[index] = visit(descriptor.value, depth + 1)
          }
          return output
        }
        const output = {}
        for (const key of Object.keys(item)) {
          if (UNSAFE_FIELDS.has(key)) continue
          const descriptor = Object.getOwnPropertyDescriptor(item, key)
          if (!descriptor || !('value' in descriptor)) continue
          const publicKey = redactString(key, secrets)
          if (UNSAFE_FIELDS.has(publicKey)) continue
          put(output, publicKey, CREDENTIAL_FIELDS.has(key.toLowerCase()) ? '' : visit(descriptor.value, depth + 1))
        }
        return output
      } finally {
        active.delete(item)
      }
    }
    return visit(value, 0)
  }

  /**
   * Explicit model-list projection of PRIVATE model records. This does not
   * register secrets: callers must register each private apiKey first. The
   * readiness hint derives only from an own string credential, never a supplied
   * hasCredential hint. Internal model records are neither mutated nor reused.
   */
  function publicModels(items) {
    if (!Array.isArray(items)) throw new TypeError('PUBLIC_MODELS_INVALID')
    return items.map(model => {
      if (!model || typeof model !== 'object' || Array.isArray(model)) throw new TypeError('PUBLIC_MODELS_INVALID')
      const credential = ownValue(model, 'apiKey')
      const output = sanitizePublicResponse(model)
      put(output, 'apiKey', '')
      put(output, 'hasCredential', typeof credential === 'string' && credential.length > 0)
      return output
    })
  }

  return Object.freeze({ registerSecret, sanitizePublicResponse, publicModels })
}

// A single gateway-wide registry covers HTTP responses, live SSE, and replay.
const shared = createPublicResponseSanitizer()
export const registerSecret = shared.registerSecret
export const sanitizePublicResponse = shared.sanitizePublicResponse
export const publicModels = shared.publicModels
