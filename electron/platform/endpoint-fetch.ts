const DEFAULT_ALLOWED_ENDPOINTS = 'http://192.168.1.15:8317'

export type EndpointPolicyErrorCode =
  | 'WEB_ENDPOINT_NOT_ALLOWED'
  | 'WEB_ENDPOINT_POLICY_INVALID'
  | 'WEB_ENDPOINT_REDIRECT_FORBIDDEN'

/** Errors contain only fixed codes, never credential-bearing URLs or request bodies. */
export class EndpointPolicyError extends Error {
  constructor(readonly code: EndpointPolicyErrorCode) { super(code); this.name = 'EndpointPolicyError' }
}

function fail(code: EndpointPolicyErrorCode): never { throw new EndpointPolicyError(code) }
function parseHttpUrl(value: string, code: EndpointPolicyErrorCode): URL {
  let url: URL
  try { url = new URL(value) } catch { return fail(code) }
  if ((url.protocol !== 'http:' && url.protocol !== 'https:') || url.username || url.password
    || !url.hostname || url.hostname.includes('*')) fail(code)
  return url
}

/** Deployment configuration accepts exact HTTP(S) origins, never wildcard hosts or path prefixes. */
export function allowedEndpointOrigins(environment: Readonly<Record<string, string | undefined>> = process.env): ReadonlySet<string> {
  const configured = environment.NOVEL_ALLOWED_ENDPOINTS ?? DEFAULT_ALLOWED_ENDPOINTS
  const values = configured.split(',').map(value => value.trim())
  if (values.length === 0 || values.some(value => !value)) fail('WEB_ENDPOINT_POLICY_INVALID')
  const origins = new Set<string>()
  for (const value of values) {
    const url = parseHttpUrl(value, 'WEB_ENDPOINT_POLICY_INVALID')
    if (url.pathname !== '/' || url.search || url.hash) fail('WEB_ENDPOINT_POLICY_INVALID')
    origins.add(url.origin)
  }
  return origins
}

/** Validate the fully resolved dispatch URL, not merely a model's saved Base URL. */
export function assertAllowedEndpoint(url: URL, environment: Readonly<Record<string, string | undefined>> = process.env): void {
  if (environment.AI_NOVEL_RUNTIME !== 'web') return
  if (url.username || url.password || (url.protocol !== 'http:' && url.protocol !== 'https:')
    || !allowedEndpointOrigins(environment).has(url.origin)) fail('WEB_ENDPOINT_NOT_ALLOWED')
}

/**
 * Shared dispatch boundary for generation, streaming, discovery, embeddings and
 * WebDAV. Injected fetch implementations still pass through this policy. Native
 * web requests never follow redirects, including same-origin redirects; desktop
 * retains its previous fetch behavior. No global fetch replacement is installed.
 */
export async function endpointFetch(
  input: RequestInfo | URL,
  init?: RequestInit,
  implementation: typeof fetch = globalThis.fetch,
): Promise<Response> {
  if (process.env.AI_NOVEL_RUNTIME !== 'web') return implementation(input, init)
  let target: URL
  let dispatchInput: RequestInfo | URL
  if (typeof input === 'string' || input instanceof URL) {
    target = parseHttpUrl(String(input), 'WEB_ENDPOINT_NOT_ALLOWED')
    // Snapshot mutable URL objects and use the same canonical URL that was checked.
    dispatchInput = typeof input === 'string' ? target.href : target
  } else if (typeof Request !== 'undefined' && input instanceof Request) {
    target = parseHttpUrl(input.url, 'WEB_ENDPOINT_NOT_ALLOWED')
    dispatchInput = input
  } else return fail('WEB_ENDPOINT_NOT_ALLOWED')
  assertAllowedEndpoint(target)
  const response = await implementation(dispatchInput, { ...init, redirect: 'error' })
  // Native fetch rejects followed redirects already. Keep custom transports from
  // exposing redirect responses as normal provider output as well.
  if (response.redirected || response.status >= 300 && response.status < 400) {
    await response.body?.cancel().catch(() => {})
    fail('WEB_ENDPOINT_REDIRECT_FORBIDDEN')
  }
  return response
}
