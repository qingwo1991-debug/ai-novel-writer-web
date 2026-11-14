import { ipc } from '../ipc-client'
import { BrowserTransportError } from '../browser-transport'
import { errorCodeOf } from '../../i18n/app-errors'
import { getActiveProjectSessionContext } from '../../shared/project-session-context'
import type { ProjectSessionContext } from '../../shared/ipc-channels'
import type { BeginGenerationRequest } from '../../shared/generation-owner-contract'
import type { MainGenerationExecuteReceipt, MainGenerationRunHandle, MainGenerationRunView, MainGenerationTransport } from './generation-runtime'
import { PLOT_OUTLINE_PROTOCOL } from '../../shared/plot-outline-contract'

const handleKey = (handle: MainGenerationRunHandle) => JSON.stringify([
  // A failure result that escaped the DTO guard can be undefined here; render
  // a stable sentinel instead of dereferencing it into a TypeError.
  handle?.projectId ?? '', handle?.epoch ?? '', handle?.rootActionId ?? '', handle?.runId ?? '',
])
// The generation family answers a mutation refusal as a BUSINESS DTO
// {success:false,error:"Error: CODE"} inside a 200 result, not as an HTTP error.
// Returning it as if it succeeded lets callers dereference receipt.run and throw
// a bare TypeError instead of the stable code. Turn that into the symbolic error
// the workflow's resume retry and the batch failure receipt both understand.
function rejectBusinessFailure<T>(result: T): T {
  // A failure DTO carries an error string. It may or may not also carry a
  // success:false flag; either way it has no handle, so passing it to bindView
  // would produce "Cannot read properties of undefined (reading 'projectId')".
  if (result && typeof result === 'object' && !Array.isArray(result)
    && typeof (result as Record<string, unknown>).error === 'string') {
    throw new Error((result as Record<string, unknown>).error as string)
  }
  return result
}
function invokeOperation<T>(call: () => Promise<T>): Promise<T> {
  return call().then(result => rejectBusinessFailure(result))
}
function freezeSession(session: ProjectSessionContext | null): ProjectSessionContext {
  if (!session || ![session.projectId, session.leaseId, session.projectPath].every(value => typeof value === 'string' && Boolean(value.trim()))) throw new Error('GENERATION_PROJECT_SESSION_REQUIRED')
  return Object.freeze({ ...session })
}
function assertHandle(session: ProjectSessionContext, handle: MainGenerationRunHandle, historical = false): void {
  if (!handle) {
    // Native TypeError source: a caller passed undefined as a handle. Log the
    // originating path and emit a coded refusal instead of crashing.
    if (typeof console !== 'undefined') console.trace('ASSERT_HANDLE_CALLED_WITH_UNDEFINED', { session })
    throw new Error('GENERATION_PROJECT_SESSION_MISMATCH: handle=undefined')
  }
  if (![handle.projectId, handle.epoch, handle.rootActionId, handle.runId].every(value => typeof value === 'string' && Boolean(value.trim()))
    || handle.projectId !== session.projectId || !historical && handle.epoch !== session.leaseId) {
    throw new Error('GENERATION_PROJECT_SESSION_MISMATCH: handle=' + JSON.stringify(handle).slice(0, 200) + ' session.projectId=' + String(session.projectId))
  }
}
/** Construction is inert: neither current session nor IPC is read until a call. */
export function createMainGenerationTransport(captureSession = getActiveProjectSessionContext) {
  const sessions = new Map<string, ProjectSessionContext>()
  function sessionFor(handle: MainGenerationRunHandle, historical = false) {
    const key = handleKey(handle)
    const session = sessions.get(key) ?? freezeSession(captureSession())
    assertHandle(session, handle, historical)
    sessions.set(key, session)
    return session
  }
  function bindView(view: MainGenerationRunView, session: ProjectSessionContext, historical = false): MainGenerationRunView {
    // A view must always carry a handle. When the lane answered something else,
    // show the raw payload instead of dereferencing view.handle.projectId into a
    // TypeError nobody can diagnose.
    if (!view || typeof view.handle !== 'object' || view.handle === null)
      throw new Error('INVALID_MAIN_VIEW_WITHOUT_HANDLE: ' + JSON.stringify(view).slice(0, 400))
    assertHandle(session, view.handle, historical)
    sessions.set(handleKey(view.handle), session)
    return view
  }
  // Long executes can outlive a proxy or tunnel hop between the browser and the
  // backend. The server detaches dispatch from the request lifecycle and records
  // every attempt in the durable ledger, so a dropped POST is recoverable:
  // generation:execute-receipt polls that ledger for the same
  // (runId, invocationNonce) and returns the settled { outcome, run } DTO, or
  // reports the invocation absent so the same nonce can be resent safely.
  const EXECUTE_RECEIPT_POLL_MS = 2000
  const EXECUTE_RECEIPT_DEADLINE_MS = 20 * 60 * 1000
  const EXECUTE_RECEIPT_NOT_FOUND_STREAK = 3
  const sleep = (ms: number) => new Promise(resolve => setTimeout(resolve, ms))
  function isTransportFailure(error: unknown): boolean {
    if (!(error instanceof BrowserTransportError)) return false
    if (error.code === 'BROWSER_NETWORK_ERROR') return true
    // A 5xx means the server may still be settling the attempt; a 4xx is a
    // refusal that polling cannot fix, so only retry server-side failures.
    return error.code === 'BROWSER_REQUEST_FAILED' && (error.status ?? 0) >= 500
  }
  const transport: MainGenerationTransport = {
    async execute(request) {
      const frozen = structuredClone(request)
      const session = sessionFor(frozen.handle)
      const deadline = Date.now() + EXECUTE_RECEIPT_DEADLINE_MS
      const accept = <T extends { run: MainGenerationRunView }>(receipt: T): T => {
        if (handleKey(receipt.run.handle) !== handleKey(frozen.handle)) throw new Error('GENERATION_RESPONSE_IDENTITY_MISMATCH')
        bindView(receipt.run, session)
        return receipt
      }
      const pollReceipt = async (): Promise<'retry' | MainGenerationExecuteReceipt> => {
        let notFoundStreak = 0
        for (;;) {
          if (Date.now() >= deadline) throw new Error('GENERATION_EXECUTE_RECEIPT_TIMEOUT')
          let result: Awaited<ReturnType<typeof invokeReceipt>>
          try {
            result = await invokeReceipt()
          } catch (error) {
            if (errorCodeOf(error) === 'GENERATION_INVOCATION_NOT_FOUND') {
              // The execute may still be in its pre-dispatch admission window;
              // only resend after several consecutive misses.
              notFoundStreak += 1
              if (notFoundStreak >= EXECUTE_RECEIPT_NOT_FOUND_STREAK) return 'retry'
              await sleep(EXECUTE_RECEIPT_POLL_MS)
              continue
            }
            if (isTransportFailure(error)) {
              await sleep(EXECUTE_RECEIPT_POLL_MS)
              continue
            }
            throw error
          }
          notFoundStreak = 0
          if (result.state === 'pending') {
            await sleep(EXECUTE_RECEIPT_POLL_MS)
            continue
          }
          return accept(result)
        }
      }
      const invokeReceipt = () => invokeOperation(() => ipc.invokeWithProjectSession(session, 'generation:execute-receipt', {
        handle: frozen.handle,
        invocationNonce: frozen.invocationNonce,
      }))
      for (;;) {
        try {
          const receipt = await invokeOperation(() => ipc.invokeWithProjectSession(session, 'generation:execute', frozen))
          return accept(receipt)
        } catch (error) {
          if (!isTransportFailure(error) || Date.now() >= deadline) throw error
        }
        const recovered = await pollReceipt()
        if (recovered !== 'retry') return recovered
      }
    },
    async read(handle) {
      const frozen = Object.freeze({ ...handle }), session = sessionFor(frozen, true)
      const view = await invokeOperation(() => ipc.invokeWithProjectSession(session, 'generation:read', frozen))
      if (handleKey(view.handle) !== handleKey(frozen)) throw new Error('GENERATION_RESPONSE_IDENTITY_MISMATCH')
      return bindView(view, session, true)
    },
    async cancel(handle) {
      const frozen = Object.freeze({ ...handle }), session = sessionFor(frozen)
      const view = await invokeOperation(() => ipc.invokeWithProjectSession(session, 'generation:cancel', frozen))
      if (handleKey(view.handle) !== handleKey(frozen)) throw new Error('GENERATION_RESPONSE_IDENTITY_MISMATCH')
      return bindView(view, session)
    },
    subscribe(handle, listener) {
      const frozen = Object.freeze({ ...handle }); sessionFor(frozen, true)
      let closed = false
      const unsubscribe = ipc.on('generation:snapshot', snapshot => {
        if (!closed && handleKey(snapshot) === handleKey(frozen)) listener(snapshot)
      })
      return () => { if (!closed) { closed = true; unsubscribe() } }
    },
    subscribeReasoning(handle, listener) {
      const frozen = Object.freeze({ ...handle }); sessionFor(frozen, true)
      let closed = false
      const unsubscribe = ipc.on('generation:reasoning', event => {
        if (!closed && handleKey(event) === handleKey(frozen)) listener(event)
      })
      return () => { if (!closed) { closed = true; unsubscribe() } }
    },
  }
  return Object.assign(transport, {
    async begin(sessionInput: ProjectSessionContext, request: BeginGenerationRequest) {
      const session = freezeSession(sessionInput), frozen = structuredClone(request)
      return bindView(await invokeOperation(() => ipc.invokeWithProjectSession(session, 'generation:begin', frozen)), session)
    },
    async pause(handle: MainGenerationRunHandle) {
      const frozen = { ...handle }, session = sessionFor(frozen)
      const view = await invokeOperation(() => ipc.invokeWithProjectSession(session, 'generation:pause', frozen))
      if (handleKey(view.handle) !== handleKey(frozen)) throw new Error('GENERATION_RESPONSE_IDENTITY_MISMATCH')
      return bindView(view, session)
    },
    async resume(sessionInput: ProjectSessionContext, handle: MainGenerationRunHandle) {
      const session = freezeSession(sessionInput), frozen = { ...handle }
      assertHandle(session, frozen, true)
      const view = await invokeOperation(() => ipc.invokeWithProjectSession(session, 'generation:resume', frozen))
      if (view.handle.runId !== frozen.runId || view.handle.rootActionId !== frozen.rootActionId) throw new Error('GENERATION_RESPONSE_IDENTITY_MISMATCH')
      const readOnlyOutline = view.nonReplayable && view.plotOutline?.protocol === PLOT_OUTLINE_PROTOCOL && view.plotOutline.cursor.kind === 'complete'
      if (readOnlyOutline && view.handle.epoch !== frozen.epoch) throw new Error('GENERATION_RESPONSE_IDENTITY_MISMATCH')
      return bindView(view, session, readOnlyOutline)
    },
    async restart(sessionInput: ProjectSessionContext, handle: MainGenerationRunHandle, request: BeginGenerationRequest) {
      const session = freezeSession(sessionInput), frozen = { ...handle }, intent = structuredClone(request)
      assertHandle(session, frozen, true)
      return bindView(await invokeOperation(() => ipc.invokeWithProjectSession(session, 'generation:restart', frozen, intent)), session, true)
    },
    async discard(sessionInput: ProjectSessionContext, handle: MainGenerationRunHandle, artifactId: string) {
      const session = freezeSession(sessionInput), frozen = { ...handle }
      assertHandle(session, frozen, true)
      const view = await invokeOperation(() => ipc.invokeWithProjectSession(session, 'generation:discard-candidate', frozen, artifactId))
      if (handleKey(view.handle) !== handleKey(frozen)) throw new Error('GENERATION_RESPONSE_IDENTITY_MISMATCH')
      return bindView(view, session, true)
    },
  })
}
export const mainGenerationTransport = createMainGenerationTransport()
export const beginMainGeneration = mainGenerationTransport.begin
export const pauseMainGeneration = mainGenerationTransport.pause
export const resumeMainGeneration = mainGenerationTransport.resume
export const restartMainGeneration = mainGenerationTransport.restart
export const discardMainGenerationCandidate = mainGenerationTransport.discard
