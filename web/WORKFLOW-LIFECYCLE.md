# Native-Web workflow lifecycle and recovery boundary

## Supported product contract

The Electron application already schedules multi-step writing workflows in its live renderer. Native Web may preserve that architecture: **keep the application tab open while a workflow runs**. A new always-on server workflow scheduler is not required merely to preserve the original business features.

Do not describe this as background batch execution, automatic workflow continuation after refresh, or guaranteed unload protection. The server owns individual generation requests and durable effects; the browser still owns the workflow loop. Closing or refreshing a tab discards that loop. A browser may suppress `beforeunload`, terminate a tab, or suspend its JavaScript without a useful teardown callback.

This document separates implemented protections from follow-up gaps. Deterministic domain/unit tests exercise the request fence and read projections; that is not a claim that every browser termination boundary has been end-to-end tested.

## Implemented refresh protections

- A transactional SQLite reservation fence rejects `GENERATION_DISPATCH_IN_FLIGHT` when a different nonce would overlap a live attempt on the **same run**. Exact nonce/hash replay remains idempotent; the owner joins its existing execute promise. Independent sibling runs are not serialized, and settled attempts do not prevent an explicit bounded retry.
- Run/context/review recovery reads expose `inFlight` from `reserved`/`dispatch-marked` attempts rather than guessing from renderer state. Review recovery is not resumable while live; draft recovery includes pending requests even before visible text exists. Batch recovery exposes live attempts under its original root.
- The Run recovery list shows a wait/pause-original-request card after reload instead of offering another generation. Pause preserves the existing candidate/root rather than permanently cancelling it. A 2.5-second read-only status refresh while orphaned work is pending, plus online/visibility refresh, prevents a missed terminal SSE event from leaving the card stuck forever. The workflow recovery runtime also refuses to reopen a live run.
- Pending review-cycle rechecks are projected from durable state and displayed as **explicit** “Start required recheck” actions. Matching recoverable review runs and active local chapter workflows are deduplicated. Launch rereads the pending cycle/hash and uses the committed revised draft; loading/reconnecting does not call a model.
- Auto-finalize batch progress now waits for **every declared** post-processing step to be `ok`, including a noncritical `character_cards` step if present, while retaining the required critical KB/notes proof. A failed/pending character step cannot disappear from recovery or advance the chapter merely because `allCriticalPassed` is true. No absent optional step is invented.
- The finalization-specific `kb:import-finalized-text` path uses a deterministic document ID bound to the verified immutable finalization/source identity, checks complete canonical/chunk/vector/copy evidence on replay, and relinks the same document rather than re-embedding it. Damaged or edited evidence returns an explicit repair-required result, not false success. Ordinary generic text imports remain a separate API.
- Worker/app restart still conservatively marks dispatched work unknown and pauses roots. This fence does not automatically turn an unknown provider outcome into success or a retry.
- Browser `pagehide` sends a best-effort same-origin, CSRF-header-bound keepalive departure hint. After a 1.5-second grace the gateway releases only a disconnected, idle client: active operation promises/worker requests/streams remain protected. A same-tab SSE reconnect cancels the hint; a late old-document hint while the replacement remains connected is discarded. Missing teardown callbacks retain the existing two-minute disconnect grace. Back/forward-cache restoration reloads the read model rather than retaining a stale project lease. This is not reliable delivery or background workflow continuation.

The dispatch fence is intentionally per-run. It is not an authoritative cross-root resource lock, does not continue a whole workflow, and does not by itself close every non-idempotent domain-effect/unknown-response window below.

## Original versus native-Web behavior

| Event | Original Electron renderer | Native Web |
| --- | --- | --- |
| Renderer/tab stays alive | Runs workflow executors, bounded continuation, commits, post-processing, and next steps. | Same frontend business orchestration over HTTP/SSE. |
| Ordinary close while work is active | Desktop close protocol can keep the window open and show the app's workflow-blocked dialog. | Synchronous browser-owned `beforeunload` warning is best effort; the app cannot veto every close. |
| Network/SSE disconnect, page remains alive | No equivalent browser-network hop to local IPC. | Worker generation can continue. EventSource reconnects; snapshot reads repair missed display events. A lost HTTP response remains an unknown result and is not automatically resent. |
| Renderer/tab is destroyed or refreshed | Renderer executor/confirmation state disappears; shutting down the entire app also ends main-process work. | Renderer executor/confirmation state disappears, **but an already dispatched request can still be running in the project worker**. Later frontend stages are not thereby scheduled. |
| Reopen the project | Durable candidates/effect receipts support explicit, source-checked recovery where implemented. | Same domain recovery, after reconnecting and reopening the project; do not replay a new author action to imitate the old loop. |
| Server/app process restarts | Durable generation recovery marks interrupted dispatches unknown and pauses roots. | Same conservative recovery. No promise to resume a paid provider request or the complete workflow automatically. |

The native-Web project worker opens its canonical project once and serves cached `project:open` results. Reopening a tab therefore does not rotate that worker's project lease. Subscriber removal does not itself cancel generation. These are useful existing properties, not a workflow scheduler.

## Existing durable seams

- `src/stores/workflow-store.ts` owns executor closures, `activeContexts`, pause/confirmation promises, mutable `context.data`, resource claims, and in-memory workflow history. These do not survive renderer loss.
- `electron/services/main-generation-owner.ts`, `generation-run-service.ts`, and `electron/repositories/generation-run-repository.ts` own generation roots, frozen bindings, budgets, invocation nonces, attempts, artifacts, and source-checked recovery. Same nonce plus same request hash reuses the invocation; a new nonce is not the same request.
- `GenerationRunRepository.recoverInterrupted()` marks `dispatch-marked` attempts `unknown` and pauses roots. Durable `status: running` alone is not proof of a currently live request: some renderer-side failures leave that status unchanged.
- `generation:read-batch` derives saved chapter identities, pending/published finalization identities, and the next chapter from durable receipts. `src/services/workflows/batch-chapter-workflow.ts` still schedules guards, drafting, finalization, post-processing, and subsequent chapters.
- `draft_review` means editable saved drafts awaiting human review. It does **not** automatically run the review/revision/finalization pipeline. `auto_finalize` does draft → finalization/publication → post-processing, stopping on any failed step. The shared batch bound is 1–10 chapters.
- Draft, review/revision, and planning recovery factories reuse selected durable identities. Saved review/revision receipts can be opened without another model call; invalid/stale sources can be copy-only. Incomplete output is not automatically a valid saved result.
- Finalization commits the authoritative SQLite snapshot and publication outbox together. Publication repair and finalized-generation slot receipts should be reused, never replaced with browser filesystem writes.
- Import already has a durable stage/batch/effect ledger, execution owner/epoch, lease renewal, and safe-boundary cancellation. Its DI-shaped `ImportRunOrchestrator` is reusable, but its current stage scheduling and heartbeat still live in the frontend.
- `src/services/browser-transport.ts` keeps one client ID per browsing context in `window.name` (session storage only records the witness a duplicated tab or `window.open` copy inherits, so a copy takes a fresh ID instead of sharing the session owner and project lease), does not retry writes, and refreshes generation artifacts on an established EventSource reconnect. This does not restore workflow steps, approvals, or a lost HTTP completion. The gateway event buffer is bounded and volatile; it is not an execution ledger.

## Finite minimum recovery hardening; no full supervisor

The protections above cover the highest-risk same-run reload overlap. The remaining items below are bounded follow-ups for a stronger **safe explicit recovery** contract; the full list is not implemented. Documentation and an unload warning alone do not establish zero duplicate requests/effects after refresh.

### 1. Fence genuinely in-flight work on the backend

The same-run pending projection and transactional reservation fence are implemented as described above. Keep those checks at reservation/admission and preserve legitimate domain concurrency rather than imposing an unreviewed global lock. Domain effect execution state is a separate concern.

Also prevent a freshly started, conflicting action from bypassing the fence by creating a new root for the same chapter/resource while the old worker request is active. Existing frontend resource claims disappear on refresh and are not a backend conflict authority.

Recovery must show “request still running; wait or explicitly stop” instead of “review again” while that request is alive. Do not determine this solely from local `activeRuns`, lease equality, or durable run status. Previously `run-recovery.ts` inferred interrupted work from local active workflows and `ReviewRevisionGeneration.readRecovery()` did not exclude live requests from `canResume`. The implemented pending projection/fence closes that same-run overlap: refreshed renderers wait rather than minting another physical request. `workflow-main-generation.ts` still normally chooses a fresh invocation nonce for a legitimate next request, so nonce idempotency alone would not have been sufficient.

An explicit stop must retain the current domain's recoverability semantics: pausing to retain candidates differs from permanently cancelling an author action. Never mark an actually live provider call unknown merely because one SSE connection closed.

### 2. Refresh durable recovery projections, not just streamed text

After successful project reattachment and after transport reconnection, reread `generation:list-recovery`, current batch progress, publication/post-processing state, required review-cycle state, and relevant import state. Gate all application of results by the captured project identity/session.

Reconstruct a recovery/status view, not executor closures. In-flight completion must invalidate the view even if no new local workflow was created. Add a bounded status refresh fallback for missed terminal events. Never restart generation from an EventSource `onopen` callback.

### 3. Retain action identity across unknown HTTP outcomes

Before sending a non-idempotent author action, retain a small nonsecret pending-action reference: project ID, original author nonce, selected run/batch handle when known, operation, and expected source/hash. Resolve an unknown outcome through receipt/read APIs; clear the reference only after authoritative reconciliation. Provide a nonce-based lookup for the response-lost-before-handle case if existing read projections cannot resolve it.

Do not persist executable workflow definitions, model credentials, or a broad “retry every POST” queue. An explicit *new* paid attempt must be distinguishable from receipt lookup. Keep the existing source-CAS, frozen model/template/skill bindings, and original root budget.

### 4. Close finalization recovery gaps

- Keep finalization knowledge retries on the added `kb:import-finalized-text` stable finalization/source path; generic `kb:import-text` mints random document IDs and is not a safe substitute. Replays verify existing integrity and relink the original document. Incomplete/corrupt/edited canonical or vector evidence must remain repair-required rather than silently re-embedding or reporting success. Reference-corpus authority is not reused for finalized prose.
- Resume the exact unfinished post-processing run and skip confirmed effects. Do not create a fresh run and repeat `kb_import` by default when recovering an interrupted finalization. Reuse existing publication and finalized-generation receipts.
- Preserve the stricter `auto_finalize` stop-on-**any**-failure behavior. The durable `postProcessComplete`/`nextChapterNumber` gate now requires every declared step to pass, while still requiring critical `kb_import` and `chapter_notes`. A noncritical `character_cards` failure/pending result blocks advance if that step was declared, including on the last requested chapter; an absent optional step is not synthesized.

### 5. Keep manual obligations discoverable

- A revision merge can durably require a review-cycle recheck, while the existing normal path launches it only after the merge response. The added pending projection and explicit recovery entry close discoverability of the merge-to-launch gap, keyed by cycle ID/merged hash. No autonomous scheduler is used; a required recheck is never reported as already run.
- Reopen pending character proposals and review/merge choices from durable domain records. Lost workflow confirmation promises are not approval. Never auto-adopt proposals, merge a revision, or finalize a human-review draft during reconnect.
- Show import execution lease expiry/busy state and allow only existing lease-fenced resume. Do not bypass a still-valid old executor lease just to make a refreshed button work. Cancellation remains checkpointed at the documented safe boundary.
- Unsaved editor/configuration form contents and callback-only generated previews are not covered by generation-ledger durability. Keep dirty-state warnings honest and distinguish these from committed domain results.

## Focused acceptance checks

Use a deterministic provider/request counter and durable DB/effect assertions, not only screenshots:

1. Refresh during a slow review/draft request: the worker may finish it; recovery cannot dispatch a second request concurrently, and there is no automatic next workflow stage.
2. Drop a successful HTTP response before the UI receives its handle/receipt: read/reconcile identifies the original action; reopening recovery does not create another root, model request, or saved draft.
3. Drop SSE, exceed its replay buffer, then reconnect: durable state rebuilds the recovery view without replaying writes. Reopening the same project preserves the worker lease.
4. Refresh after draft save, finalization commit, manuscript publication, KB import, and post-process effect commit: each stored identity/effect remains single, source-checked, and recoverable or explicitly blocked.
5. Fail only `character_cards` in auto-finalize: batch stops and remains a visible repair obligation; recovery does not quietly generate the following chapter.
6. Refresh after revision merge but before recheck launch, and while awaiting proposal adoption: required review remains visible; no human confirmation is fabricated.
7. Kill/restart a worker/server: dispatched-but-unsettled accounting becomes unknown, candidates remain inspectable, stale sources cannot be overwritten, and no paid request or entire workflow silently restarts.
8. Reopen import before and after its lease expires; exercise cancel/resume and changed-source rejection without duplicate effect receipts.

## Non-goals for this parity boundary

No automatic complete-workflow continuation after tab loss, server restart, or browser suspension is promised. Adding that capability later requires a typed durable workflow state machine, execution fencing, persisted human-wait states, and server-owned stage scheduling. Merely moving streaming to the worker or persisting Zustand state does not supply it.
