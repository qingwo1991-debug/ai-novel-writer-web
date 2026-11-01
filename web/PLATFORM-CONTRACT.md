# Native application host contract

`electron/platform/index.ts` is the neutral main-process API. It imports only Node and neutral modules, never Electron. Browser workers must configure it **before dynamically importing controllers or modules that calculate data paths**:

```ts
configurePlatform({
  app: { getLocale(): string, getPath(name: string): string,
    isPackaged?: boolean, resourcesPath?: string, getVersion?(): string },
  dialogs?: {
    showOpenDialog(options, event?: InvokeEvent): Promise<{ canceled: boolean; filePaths: string[] }>,
    showSaveDialog(options, event?: InvokeEvent): Promise<{ canceled: boolean; filePath?: string }>,
    showMessageBox(options, event?: InvokeEvent): Promise<{ response: number; checkboxChecked?: boolean }>,
  },
  navigation?: { openExternal(url: string, event?: InvokeEvent): Promise<void> },
  secrets?: SecretStorage,
})
```

## Requests, lifetime and events

```ts
interface ClientSink {
  readonly id: number // stable, server-assigned; not a browser-supplied authority
  send(channel: string, ...args: unknown[]): void
  isDestroyed(): boolean
  once(event: 'destroyed', listener: () => void): unknown
}
interface InvokeEvent { readonly sender: ClientSink }
operations.handle(channel, (event, ...args) => result)
await operations.invoke(channel, { sender }, ...args)
operations.has(channel): boolean
operations.list(): string[] // sorted; channels() is an equivalent convenience
operations.removeHandler(channel): void
eventsFor(event): ClientSink
currentInvokeEvent(): InvokeEvent | undefined
```

`OperationRegistry` is exported for isolated registries/tests. Duplicate registrations throw instead of silently replacing security wrappers. `invoke` rejects unknown operations and disconnected/invalid clients. It establishes AsyncLocalStorage request context across async continuations. `dialogHost` and `navigationHost` automatically receive that invocation event even when called from helpers such as remembered dialogs. Explicit event arguments remain supported. The host is responsible for validating async browser dialog replies, binding replies to the initiating client, cancellation/timeouts, and publishing genuine disconnection exactly once. Do not supply fake BrowserWindow objects. LLM streams and generation subscribers send directly to ClientSink.

Dialogs use plain options/return types exported from the same module (`OpenDialogOptions`, `SaveDialogOptions`, `MessageBoxOptions`, corresponding ReturnValue types). Unconfigured dialogs/navigation fail with explicit errors rather than pretending a user canceled or opening a server-side browser.

## Credentials and image optimization

`secretStorage` lazily defaults to `createServerSecretStorage({rootPath: appHost.getPath('userData')})`, exported from `electron/platform/server-secret-storage.ts`. This is AES-256-GCM with authenticated versioned envelopes, fresh 96-bit nonces, and a persisted random 32-byte `server-credentials.key` created privately (0600). Metadata is `server-encrypted`, **not OS-backed**. Protect and preserve this key with the data volume; an administrator or attacker able to read the key can decrypt credentials. A desktop adapter may inject OS-backed SecretStorage. There is no plaintext persistence fallback; credential service may honestly fall back to session-only on unavailable encryption.

`configureAvatarCompressor(adapter | undefined)` in `electron/services/avatar-image.ts` injects optional synchronous image optimization. Node's default preserves the original validated bytes and extension; resizing is an optimization, not a feature requirement. A desktop adapter can inject nativeImage compression without putting Electron on the Node import graph.

## Native web endpoint egress

`electron/platform/endpoint-fetch.ts` is the shared **actual HTTP dispatch** boundary for both LLM providers (OpenAI-compatible and native Gemini, generate and stream), both discovery protocols, every embedding batch, and every WebDAV request (PROPFIND/MKCOL/GET/PUT). In `AI_NOVEL_RUNTIME=web`, `NOVEL_ALLOWED_ENDPOINTS` is a comma-separated list of exact HTTP(S) origins; the default is `http://192.168.1.15:8317`. Deployment configuration must contain origins only: no credentials, wildcards, query, fragment or path prefix. Explicitly empty or malformed lists fail closed. The final resolved request origin is checked on each dispatch, including injected transport seams, so old saved models or execution leases cannot bypass policy.

All native web requests through this boundary force `redirect: 'error'`, even if a caller supplies another redirect mode; redirects to the same origin are also forbidden. Rejected configuration/URLs use fixed errors without returning credential-bearing URLs or bodies. Desktop requests keep their prior endpoint and redirect behavior. The policy is not a global `fetch` shim and does not control unrelated GitHub skill/update integrations or subprocess networking; deployment network isolation remains an independent defense.

## Composition boundary

Register all business controllers in Node. `window-controller.ts` is desktop-only and must not be imported from Node. Update lifecycle is host-specific: the controller accepts an injected service and publish callback; a browser host must report deployment-managed update capability honestly, not load Electron updater. Global data mutations must be routed through the single global worker; project operations through their project worker. Platform configuration is per worker, not mutable per request. Browser event sinks and platform IPC transport do not themselves grant filesystem authority; existing project leases and external-file grant checks remain authoritative.
