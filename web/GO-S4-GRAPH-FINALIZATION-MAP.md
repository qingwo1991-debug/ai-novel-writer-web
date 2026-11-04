# S4 binding map — the five `graph-generation:*` and five `finalization-generation:*` operations

Read-only analysis. Sources: `web/operations.json`, `src/shared/graph-generation.ts`,
`src/shared/finalization-generation.ts`, `src/services/graph-generation.ts`,
`src/services/finalization-generation.ts`, `src/services/workflows/commands/finalize-chapter.command.ts`,
`electron/controllers/generation-controller.ts`, `electron/services/main-generation-owner.ts`,
`electron/services/graph-generation{,-source}.ts`, `electron/services/finalization-generation{,-source}.ts`,
`backend-go/internal/**`, `backend-go/cmd/novel-server/**`.

Repository state at time of writing: `git log --oneline -1` → `1f1ecd87 feat(go): bind the nine
remaining review and revision operations`.

---

## 0. Shared facts (apply to all ten)

**Inventory.** `web/operations.json` — all ten exist exactly once, scope `project`:

```
finalization-generation:read    => project   (operations.json:194)
finalization-generation:begin   => project   (operations.json:195)
finalization-generation:execute => project   (operations.json:196)
finalization-generation:commit  => project   (operations.json:197)
finalization-generation:cancel  => project   (operations.json:198)
graph-generation:begin          => project   (operations.json:237)
graph-generation:read           => project   (operations.json:238)
graph-generation:execute        => project   (operations.json:239)
graph-generation:confirm        => project   (operations.json:240)
graph-generation:cancel         => project   (operations.json:241)
```

Status column per `web/GO-GAP-MATRIX.md:99-100` (`graph-generation | 5 | 0 | 0 | 5`,
`finalization-generation | 5 | 0 | 0 | 5`) and `web/GO-MIGRATION-MATRIX.md:277-281`, `:320-324`
(all `NOT STARTED`).

**Wire envelope.** Every one of the ten is invoked through `ipc.invokeWithProjectSession(...)`
(`src/services/ipc-client.ts:105-116`); `finalization-generation:` and `graph-generation:` are both
project-scoped by `isProjectScopedChannel` (`src/services/ipc-client.ts:49-70`, lines 54-55). The
browser therefore sends `args = [<request>, <ProjectSessionContext{projectId, leaseId, projectPath}>]`,
so **all ten have wire arity 2** — identical to the S1 lane convention enforced at
`internal/transport/generation.go:119-137`.

**Current Go behaviour.** Zero of the ten is bound. `grep -rn "graph-generation" backend-go/` and
`grep -rn "finalization-generation" backend-go/` return hits in exactly **one** file — the S1
acceptance test `cmd/novel-server/synthetic_generation_http_linux_test.go:450-453`, which lists all
ten in its `unmigrated` slice and asserts `501 GO_OPERATION_NOT_MIGRATED` for each
(`:456-464`). Unbound ⇒ `security.NewHTTPError(501,"GO_OPERATION_NOT_MIGRATED")`
(`internal/transport/server.go:100`).

**Consequence every batch must handle:** that slice is a hard `len(unmigrated) != 32` assertion
(`:456-458`) plus a per-op 501 loop (`:459-464`). Binding *any* of the ten breaks the existing S1
test. Each batch must move its ops out of the slice, decrement the `32`, and assert the real
envelope. Do not duplicate this test — reuse it.

**Authoritative behaviour lives in six Node modules** (the port targets):

| Module | Role |
|---|---|
| `electron/controllers/generation-controller.ts:223-232` | route table (`register(name, 1, …)`) |
| `electron/services/main-generation-owner.ts:1069-1121` | `finalizationRecovery` / `graphRecovery` projections |
| `electron/services/main-generation-owner.ts:1189-1411` | the ten owner methods |
| `electron/services/graph-generation{,-source}.ts` | context capture/read, task, artifact proof, confirm effects |
| `electron/services/finalization-generation{,-source}.ts` | slot key, context capture/read, task, commit effects |
| `src/shared/{plot-tree-generation-pure,narrative-thread-generation-pure,finalized-continuity-facts}.ts` | pure result parsers / fact builder |

**Substrate that already exists in Go.**

| Building block | Location |
|---|---|
| Admission + ledger + author `*sql.DB` in one release | `internal/authorruntime/generation_db_linux.go:26` (`BeginGenerationLedgerDB`) |
| Reusable one-per-call admission closure | `cmd/novel-server/synthetic_generation_linux.go:318` (`syntheticGeneration.admit`) |
| Model lease / plan / dispatch / durable execute | `internal/generationowner/{lease,plan,dispatch,ordinary}.go` |
| Handle parse (`projectId/epoch/rootActionId/runId`) | `cmd/novel-server/synthetic_generation_linux.go:346-371` |
| Run read-back + `viewOf` / `snapshotOf` projection | `cmd/novel-server/synthetic_generation_linux.go:1594`, `:1721` |
| Cancel / pause on a root | `internal/generationowner/ordinary.go:181`, `:173` |
| Plot snapshot store (validate + CAS save) | `internal/store/plottree.go:70` (`PlotTree.Save`), `:296` (`plotValidate`) |
| Narrative plan/event store | `internal/store/narrative.go:75` (`CreatePlan`), `:131` (`ConfirmEvent`) |
| Blueprint read | `internal/store/blueprints.go:69` (`All`), `:82` (`Get`) |
| Finalization outbox read | `internal/store/finalization_outbox.go:136` (`Get`), `:141` (`GetByDraftID`) |
| Finalized source + frozen character snapshot read | `internal/store/summary.go:446` (`ReadFinalizedSource`) |
| Project core (`writing_language`, `total_chapters`) | `internal/store/projectcore.go:57` (`Get`) |

**The five hard gates that stop all ten today.** Ordered by where the request dies.

| Gate | Location | Error |
|---|---|---|
| **G1 dispatch** — no handler registered; `GenerationProvider` has exactly 11 fields and `BindGeneration` exactly 11 names, none of them graph/finalization | `internal/transport/generation.go:18-30`, `:40-112`; `internal/transport/server.go:100` | `501 GO_OPERATION_NOT_MIGRATED` |
| **G2 ledger method allowlist** — no method writes a named effect key into `usage_receipt_json` | `internal/store/generation_ledger.go:267-270` (`read`/`write` sets), `:799` (`call`) | `GENERATION_OPERATION_UNSUPPORTED` |
| **G3 usage-receipt key allowlist** — 8 permitted top-level keys only; `graphEffects` / `finalizationEffect` are rejected, and this runs inside `mutableRoot` so it also poisons later `settle`/`pause` on the root | `internal/store/generation_ledger.go:495-501` | `GENERATION_EFFECT_UNSUPPORTED` |
| **G4 manifest quarantine** — `sourceManifest` must not carry the family key; checked at `open` **and** in `mutableRoot` for *every* run under the root | `internal/store/generation_ledger.go:434-438`, called at `:479` and `:596` | `GENERATION_OPERATION_UNSUPPORTED` |
| **G5 request-shape allowlist** — the S1 entry-point parser rejects `input`, `slot`, `artifact`, `index`, and requires non-empty `operation`/`uiActionNonce`/`modelId`, none of which the graph/finalization `begin` requests carry | `cmd/novel-server/synthetic_generation_linux.go:391-416` (`allowed` map at `:396`, required-field check at `:413`) | `GENERATION_BEGIN_INVALID` |

G4 is the reason no partial wiring can work: a finalization run's own manifest carries
`finalizationGenerationSlotKey`, so `mutableRoot` refuses `reserve`/`settle`/`pause` on the very root
the run lives in. Since both finalization steps deliberately share one root
(`electron/services/main-generation-owner.ts:1248-1250`), the quarantine is not an edge case — it is
the first statement of the family.

---

## 1. `graph-generation:begin`

### 1.1 Request args

`src/shared/graph-generation.ts:41` — `args: [{ input: GraphGenerationInput; modelId: string; uiActionNonce: string }]`
(wire arity 2). Call site: `src/services/graph-generation.ts:53` (and `:51` for the read variant when
`'handle' in request`).

`GraphGenerationInput` (`src/shared/graph-generation.ts:9`) is a closed union:

| `kind` | additional required fields |
|---|---|
| `'plot'` | — (`Object.keys` must be exactly `['kind']`) |
| `'plan'` | `chapterNumber: number` (> 0, safe integer) |
| `'event'` | `planId: number`, `draftId: number` (both > 0) |

Validated by `validateGraphGenerationInput` (`electron/services/graph-generation-source.ts:16-23`);
any extra key ⇒ `GENERATION_GRAPH_INPUT_INVALID`.

### 1.2 Response DTO — `GraphGenerationRecovery`

`src/shared/graph-generation.ts:30-39`:

```
view:          MainGenerationRunView
modelId:       string
context:       GraphGenerationContext
attemptCount:  number
sourceStatus:  'current' | 'conflict'
artifact?:     GraphGenerationArtifact  { artifactId, revision, textHash }
result?:       GraphGenerationResult
effects:       GraphGenerationEffect[]          // always present, may be []
```

`MainGenerationRunView` (`src/services/generation/generation-runtime.ts:100-115`) carries
`handle{projectId,epoch,rootActionId,runId}`, `status`, `operation?`, `inFlight?`, `nonReplayable`,
`artifacts[]`, `candidates?[]`, `budget{maxAttempts,maxRequestedOutputTokens,
maxRequestedOutputTokensPerAttempt,deadlineAt}`, `budgetDiagnostics?`, `ledger?`, `plotOutline?`,
`unsavedTails?`. A `MainGenerationSnapshot` (`:83-94`) carries `artifactId, attemptId, revision,
durableRevision, text, textHash, status, compositionEligible?`.

`GraphGenerationContext` (`src/shared/graph-generation.ts:11-21`):

```
projectId, originEpoch, key, createdAt, input, writingLanguage
+ kind:'plot'  → sources: PlotTreeSourceBundle, targetBaselineHash
+ kind:'plan'  → blueprint: BlueprintData, totalChapters
+ kind:'event' → plan: NarrativeThreadView, source: FinalizedSourceIdentity,
                 content: string, contentRevision, projectionGeneration
```

Materialised context rules: `captureGraphGenerationContext`
(`electron/services/graph-generation-source.ts:26-76`) reads `project_core.writing_language /
total_chapters / plot_tree_snapshot`, `PlotTreeRepository.read` (with `snapshot` forced to `null` and
`storedSnapshotInvalid` deleted, `:41-44`), `BlueprintRepository.getAll` (`:47`), or
`NarrativeThreadRepository.list` + a joined `drafts⨝contents⨝finalization_outbox⨝
continuity_projection_meta` row (`:51-75`).

### 1.3 Go implementation status

Nothing. Closest reusable pieces: `syntheticGeneration.admit`
(`cmd/novel-server/synthetic_generation_linux.go:318`), `registry.Begin` + `BindSources`
(`internal/generationowner/lease.go:145`, `binding.go:50`), and `ledger.CallJSON(ctx,"open",…)` as
used at `cmd/novel-server/synthetic_generation_linux.go:876`. `syntheticManifest` (`:704-731`) has no
graph keys.

### 1.4 Blockers

1. **G1** — unbound ⇒ 501.
2. **G5** — `input` is not in the `allowed` set (`synthetic_generation_linux.go:396-402`);
   `operation`/`uiActionNonce`/`modelId` requirement at `:413` cannot be met by
   `{input, modelId, uiActionNonce}`. The S1 entry point cannot be reused at all.
3. **G4** — the run must open with `sourceManifest.graphGenerationContext` plus the five companion
   keys (`graphGenerationContextHash`, `graphGenerationOriginEpoch`, `graphGenerationKey`,
   `graphGenerationInput`, `graphGenerationTask`, `graphGenerationTaskHash` — see
   `graph-generation-source.ts:86-100`), which `glOrdinaryBinding`
   (`internal/store/generation_ledger.go:434-438`) rejects with
   `GENERATION_OPERATION_UNSUPPORTED`.
4. **Missing manifest keys** — `syntheticManifest` (`:704-731`) has no slot for them.
5. **Missing context builder** — no Go port of `captureGraphGenerationContext`; in particular no Go
   function returning `PlotTreeSourceBundle` in the *captured* shape (`plotRead`
   `internal/store/plottree.go:53-68` returns extra `snapshot`/`storedSnapshotInvalid` keys and a
   `sourceRevision` that the Node capture deliberately drops from the hash-excluded bundle).
6. **Nonce lookup** — Node resolves a repeat `uiActionNonce` with
   `SELECT r.run_id … json_extract(binding_json,'$.sourceManifest.graphGenerationContext') IS NOT NULL`
   (`main-generation-owner.ts:1196-1198`). The Go ledger has no manifest-keyed run lookup; only
   `ListRuns` (`internal/store/generation_ledger.go:313-365`), which returns handle+status+operation
   only.
7. **Missing DTO fields** — `viewOf` (`synthetic_generation_linux.go:1594-1720`) emits no
   `context`, `modelId`, `attemptCount`, `sourceStatus`, `artifact`, `result`, `effects`.

### 1.5 Provider shape

**Same provider type, separate binder.** `syntheticGeneration` already holds exactly what is needed
(`lifecycle`, `registry`, `executor`, `loadModel`, `server`) and `admit` is family-agnostic. But
`transport.GenerationProvider` (`internal/transport/generation.go:18-30`) is a closed 11-field struct
and `BindGeneration` (`:40-112`) hardcodes 11 names; adding fields there would make `BindGeneration`
own names it does not implement. A separate `GraphGenerationProvider` + `Server.BindGraphGeneration`
is required. Go method names must differ from the S1 set (`Begin`/`Read`/`Execute`/`Cancel` are
already taken at `synthetic_generation_linux.go:815`, `:1262`, `:1015`, `:1315`) — use
`GraphBegin`/`GraphRead`/`GraphExecute`/`GraphConfirm`/`GraphCancel`.

---

## 2. `graph-generation:read`

### 2.1 Request args

`src/shared/graph-generation.ts:42` — `{ handle: MainGenerationRunHandle }` (arity 2). Handle shape
`{projectId, epoch, rootActionId, runId}` (`src/services/generation/generation-runtime.ts:77-82`).
Call sites: `src/services/graph-generation.ts:34`, `:52`; `src/components/stage/plot/use-plot-threads.ts:305`, `:322`.

### 2.2 Response DTO

`GraphGenerationRecovery` — identical to §1.2. Read is not nullable: an unresolvable handle throws
(`main-generation-owner.ts:1206-1210` → `GENERATION_GRAPH_REQUEST_INVALID` / `GENERATION_RUN_IDENTITY_MISMATCH`).

### 2.3 Go implementation status

`internal/store/generation_ledger.go:368` (`get`), `:381` (`budget`), `:519` (`receipt`), `:515`
(`inFlight`) already give every input `viewOf` needs; `cmd/novel-server/synthetic_generation_linux.go:1262`
(`Read`) and `:899` (`syntheticRequireRun`) are the reusable skeleton.

### 2.4 Blockers

1. **G1** — unbound.
2. **G4** — `Read` itself does not call `mutableRoot` (`generation_ledger.go:452` comment: "Reads do
   not call this guard"), so a plain handle read of a graph run **would already succeed** at the
   ledger level. The blocker is purely that nothing is bound and that the projection is missing.
3. **Missing projection** — six DTO fields (see §1.4 item 7). `graphRecovery`
   (`main-generation-owner.ts:1092-1106`) additionally **throws** `GENERATION_GRAPH_ATTEMPT_CONFLICT`
   when the run has more than one attempt row — a rule with no Go counterpart.
4. **Missing result derivation** — `result` requires `deriveGraphGenerationResult`
   (`electron/services/graph-generation.ts:16-24`) over the three pure parsers
   (`derivePlotTreeSnapshot`, `parseNarrativeThreadPlanCandidates`,
   `parseNarrativeThreadEventCandidates`). None exist in Go.
5. **`sourceStatus`** requires re-deriving the source binding and comparing it to the frozen one —
   Node: `assertFinalizationSources` (`main-generation-owner.ts:1087-1091`). Go's
   `Binding.validate` (`internal/generationowner/binding.go:58-60`) is the right primitive and already
   exists, but is unexported and takes a live `*Binding`.

### 2.5 Provider shape

Same as §1.5 (`GraphRead`).

---

## 3. `graph-generation:execute`

### 3.1 Request args

`src/shared/graph-generation.ts:43` — `{ handle }` (arity 2). **No task is sent.** Call site:
`src/services/graph-generation.ts:68`.

### 3.2 Response DTO — `MainGenerationExecuteReceipt`

`src/services/generation/generation-runtime.ts:118-121` = `{ outcome: GenerationOutcome; run: MainGenerationRunView }`.

`GenerationOutcome` (`src/services/generation/generation-harness.ts:215-225`) is
`{status:'completed', content, finishReason:'stop', receipt: GenerationAttemptReceipt}` or
`{status:'incomplete', content, finishReason, receipt: GenerationAttemptReceipt}`.

`GenerationAttemptReceipt` (`:163-186`): `diagnostics?`, `visibleArtifact?
{artifactId,attemptId,revision,textHash}`, `purpose?`, `model`, `capabilities`,
`budget{attempt,maxAttempts,requestedOutputTokens,cumulativeRequestedOutputTokens,
maxRequestedOutputTokens,maxRequestedOutputTokensPerAttempt,deadlineAt}`, `finishReason`,
`failureCode?`, `usage?`, `promptBudget?`.

### 3.3 Go implementation status

`cmd/novel-server/synthetic_generation_linux.go:1015` (`Execute`) + `:924` (`newService`) +
`:1086` (`syntheticOutcome`) are the working skeleton — but they take `request["task"]` from the
browser (`:1055-1060`). `OrdinaryService.Execute` (`internal/generationowner/ordinary.go:114`) and
`Executor.Prepare` (`internal/generationowner/dispatch.go:56`) are complete.

### 3.4 Blockers

1. **G1** — unbound.
2. **G4** — `reserve` calls `mutableRun` → `mutableRoot` → `glOrdinaryBinding`
   (`generation_ledger.go:845-848`, `:452-507`) ⇒ `GENERATION_OPERATION_UNSUPPORTED` before any
   dispatch.
3. **No task source.** The server must build the task itself from the frozen manifest. Node:
   `graphs.task(run)` (`electron/services/graph-generation.ts:152-156`) replays the frozen
   `sourceManifest.graphGenerationTask` after re-verifying `graphGenerationTaskHash`. Go's
   `syntheticCanonicalPlan` (`:666-688`) constructs a placeholder prompt from `promptKeys` — it is
   not a task builder and would produce the wrong prompt. The **real** task builders are
   `buildPlotTreeTask` / `buildNarrativeThreadPlanTask` / `buildNarrativeThreadEventTask`
   (`src/shared/plot-tree-generation-pure.ts`, `src/shared/narrative-thread-generation-pure.ts`,
   reached via `graph-generation-source.ts:78-83`); **no Go counterpart**.
4. **Invocation nonce & request hash divergence.** Node pins `invocationNonce='graph:0'` and hashes
   `JSON.stringify([task, binding.fingerprint.modelLeaseRevision, binding.fingerprint.policyHash])`
   (`main-generation-owner.ts:1214-1216`). Go's `OrdinaryService.Execute` hashes its own tuple
   (`ordinary.go:137-149`). The two hashes will never match, so `findInvocation`
   (`generation_ledger.go:572`) replay only works for attempts written by Go. Acceptable within one
   lane, but it means the Go lane must **never** be pointed at a DB a Node build already executed
   against, and it must be recorded as a cross-version parity gap.
5. **Missing `visibleArtifact`.** `grep -rn "visibleArtifact" backend-go/` ⇒ **zero hits**.
   `syntheticOutcome` (`:1086-1113`) writes `outcome.receipt = <raw ledger settle receipt>`, which has
   `run/attempt/artifact/budget/result` (`generation_ledger.go:519-566`) — not a
   `GenerationAttemptReceipt`. `graph-generation` itself does not read `receipt.visibleArtifact`, but
   the sibling `finalization-generation` flow does (§8.4 item 6), so the same projection must be
   fixed once for both.
6. **Attempt-count law.** Node refuses to execute a graph run that already has an attempt
   (`GENERATION_GRAPH_ATTEMPT_CONFLICT`, `main-generation-owner.ts:1221`), i.e. exactly one paid
   dispatch per graph run. Not enforceable in Go yet.

### 3.5 Provider shape

Same as §1.5 (`GraphExecute`).

---

## 4. `graph-generation:confirm`

### 4.1 Request args

`src/shared/graph-generation.ts:44` — `{ handle, artifact: GraphGenerationArtifact, index: number }`
(arity 2). `GraphGenerationArtifact` (`:25`) = `{artifactId, revision, textHash}`. Client-side
pre-checks at `src/services/graph-generation.ts:74-82`: an already-saved index returns the stored
effect; `index` must be a safe integer in `[0, 1)` for `plot` or `[0, candidates.length)` otherwise.

### 4.2 Response DTO — `GraphGenerationEffect`

`src/shared/graph-generation.ts:26-29`:

```
{ success: true; index: number } & (
  { kind:'plot';  snapshot: PlotTreeSnapshot; derivation: 'model'|'normalized'|'deterministic' }
| { kind:'plan';  plan: NarrativeThreadPlanRecord }     // {title,type,targetStartChapter,targetEndChapter,authorIntent} + id,createdAt,updatedAt  (src/shared/narrative-thread.ts:51-55)
| { kind:'event'; event: NarrativeThreadEvent })         // + id, chapterNumber, chapterTitle, createdAt  (src/shared/narrative-thread.ts:34-39)
```

### 4.3 Go implementation status

Half built. `internal/store/plottree.go:70` (`PlotTree.Save`) does exactly what the plot effect needs
(validate against live sources, CAS on `sourceRevision`, `UPDATE project_core SET
plot_tree_snapshot`). `internal/store/narrative.go:75` (`CreatePlan`) and `:131` (`ConfirmEvent`) do
exactly what the plan/event effects need. **The effect is persistable; only the effect record is
not.**

### 4.4 Blockers

1. **G1** — unbound.
2. **G3 (deliberate)** — the effect must be stored as
   `generation_attempts.usage_receipt_json.graphEffects[]` (Node:
   `electron/services/graph-generation.ts:128-131`; read back at `:80-96`). `glOrdinaryBinding`'s
   usage-key allowlist (`generation_ledger.go:495-501`) contains no `graphEffects` ⇒
   `GENERATION_EFFECT_UNSUPPORTED`. Worse, once such a row exists, **every** mutating op on the root
   fails, because `mutableRoot` re-scans all attempt rows (`:481-505`).
3. **G2** — no ledger `write` method appends an effect. `settle`
   (`generation_ledger.go:1017-1065`) writes only `{"result": …}`; `snapshot` (`:973`) writes the
   artifact; `discardCandidate` (`:1104`) flips artifact status. None can add a named top-level key.
4. **G4** — `mutableRoot` is entered by the effect write ⇒ manifest gate hits.
5. **Missing artifact proof** — Node's `proof()` (`graph-generation.ts:40-50`) requires: artifact row
   belongs to this run, not `discarded`, receipt revision/textHash equal the reference,
   `textHash(text) === textHash`, non-empty trimmed text, no `failureCode`,
   `attempt.status ∈ {settled, unknown}`, `result.finishReason === 'stop'`. Go can express this with
   `receipt` (`generation_ledger.go:519`) but has no helper.
6. **Missing plot basline check** — `targetBaselineHash` must equal the *current*
   `project_core.plot_tree_snapshot` hash before save (`graph-generation.ts:114-117`,
   `GENERATION_GRAPH_TARGET_CHANGED`). `PlotTree.Save` only CASes on `sourceRevision`, so the
   baseline comparison must happen in the provider **inside** the same transaction window.
7. **Sealing** — `assertMutable` (`graph-generation.ts:97-99`) refuses further mutation once any
   effect exists (`GENERATION_GRAPH_EFFECT_SEALED`).
8. **DTO** — `PlotTreeSnapshot` (`src/shared/plot-tree.ts:37-43`) /
   `NarrativeThreadPlanRecord` / `NarrativeThreadEvent` field names match Go's stored rows, but the
   GraphGenerationEffect envelope (`success`, `index`, `kind`) has no Go type.

### 4.5 Provider shape

Same as §1.5 (`GraphConfirm`). **However**, `confirm` is the first member of the family that needs the
shared effect primitive (§12, B1) — the same primitive that `finalization-generation:commit` and the
S3 agent tool ops need.

---

## 5. `graph-generation:cancel`

### 5.1 Request args

`src/shared/graph-generation.ts:45` — `{ handle }` (arity 2). Call site
`src/services/graph-generation.ts:40`.

### 5.2 Response DTO

`MainGenerationRunView` (`src/services/generation/generation-runtime.ts:100-115`) — **not** the
recovery wrapper. The client only checks `sameRun(view.handle, previous)` (`graph-generation.ts:41`).

### 5.3 Go implementation status

Fully built. `OrdinaryService.Cancel` (`internal/generationowner/ordinary.go:181`) → ledger `pause`
with `cancel=true` (`generation_ledger.go:745-798`); `syntheticGeneration.stopRun`
(`synthetic_generation_linux.go:1319-1355`) already implements the exact sequence *including* the
"service returns the post-stop budget, so re-read the run" correction at `:1344-1352`. `viewOf`
already produces a valid `MainGenerationRunView`.

### 5.4 Blockers

1. **G1** — unbound. This is the only structural blocker.
2. **G4** — `service.Cancel` → `pause` → `mutableRoot` (`generation_ledger.go:751`) ⇒
   `GENERATION_OPERATION_UNSUPPORTED` for a graph run's root. Note this makes cancel *strictly*
   blocked even though the S1 pipeline it reuses is complete.
3. **No new DTO fields needed.**

### 5.5 Provider shape

Same as §1.5 (`GraphCancel`). Closest of the ten to shipping: after G4 widening, it is a
`stopRun(..., cancelRoot=true)` call plus a name in a new binder.

---

## 6. `finalization-generation:read`

### 6.1 Request args

`src/shared/finalization-generation.ts:39` — `{ slot: FinalizationGenerationSlot }` (arity 2).
**Slot-keyed, not handle-keyed** — this is the one structural difference from every other read op.

```
FinalizationGenerationSlot = {
  source: FinalizedSourceIdentity { draftId:number, finalizationId:string, chapterNumber:number, contentHash:string }
  stepKey: 'chapter_notes' | 'character_cards'
}
```

Validated by `finalizationSlotKey` (`electron/services/finalization-generation-source.ts:12-18`); the
slot's own key is `textHash(JSON.stringify([draftId, finalizationId, chapterNumber, contentHash, stepKey]))`.
Call sites: `src/services/finalization-generation.ts:38`;
`src/services/workflows/commands/finalize-chapter.command.ts:374`.

### 6.2 Response DTO — `FinalizationGenerationRecovery | null`

`src/shared/finalization-generation.ts:28-37` — **nullable**, unlike graph read:

```
attemptCount:   number
latestAttempt?: { attemptId, inFlight, finishReason: string|null, failureCode: string|null }
view:           MainGenerationRunView
modelId:        string
context:        FinalizationGenerationContext
sourceStatus:   'current' | 'conflict'
effect?:        FinalizationGenerationEffect
```

`FinalizationGenerationContext` (`:11-20`): `slot`, `identity: FinalizedCharacterContext`,
`chapterTitle`, `chapterEntities: string[]`, `writingLanguage`, `template: PromptTemplate`,
`notesBaseline: { blueprint: {exists, notes}, continuityHash }`.

### 6.3 Go implementation status

Nothing at the op level. Read inputs exist piecemeal:
`internal/store/finalization_outbox.go:141` (`GetByDraftID`) supplies `chapter_title`;
`internal/store/blueprints.go:82` supplies `characters` (→ `chapterEntities`) and `notes` (→
`notesBaseline.blueprint`); `internal/store/summary.go:446` (`ReadFinalizedSource`) supplies the
finalized body + `source` identity. No `notesBaseline` builder, no `readFinalizedCharacterContext`.

### 6.4 Blockers

1. **G1** — unbound.
2. **G4** — reads do not enter `mutableRoot`, so the ledger read path is *not* blocked; the blocker is
   the missing lookup and projection.
3. **No slot-keyed run lookup** (the single largest missing store primitive in this family). Node:
   `SELECT run_id … WHERE json_extract(binding_json,'$.sourceManifest.finalizationGenerationSlotKey')=? ORDER BY rowid`
   (`electron/services/finalization-generation.ts:21`). Go has **no** equivalent; `ListRuns`
   (`internal/store/generation_ledger.go:313-365`) projects only `projectId/epoch/rootActionId/runId/status/operation`.
4. **`latestAttempt`** requires the *last attempt of this run only* plus a volatile-receipt overlay
   (`main-generation-owner.ts:1076-1085`). `budget` (`generation_ledger.go:381-419`) returns all
   attempts under the root, so the provider must filter by `runId`.
5. **`notesBaseline`** requires a port of `finalizationNotesBaseline`
   (`finalization-generation-source.ts:19-23`): the blueprint `notes` plus
   `textHash(JSON.stringify(rows))` over `summary_snapshots` ordered by `id`. Go has
   `plotJSON`/`plotOrdered` (`internal/store/plottree.go:30-49`) to reproduce JS key order but no
   builder.
6. **`identity`** requires `SummaryRepository.readFinalizedCharacterContext` — no Go counterpart;
   `summary.ReadFinalizedSource` returns `{status, snapshot}` only, not the character identity shape.
7. **`template: PromptTemplate`** — the builtin prompt template table
   (`src/services/builtin-prompt-templates.ts`) has no Go counterpart and no Go prompt renderer
   (`renderPrompt`, `src/shared/render-prompt.ts`).
8. **Nullable on unknown slot** — must return `nil`, not an error. `security.WriteJSON` will emit
   `{"result":null}`.

### 6.5 Provider shape

**Separate binder.** `transport.GenerationProvider` has no field that can carry a slot-keyed lookup,
and the response is nullable. New `FinalizationGenerationProvider` + `Server.BindFinalizationGeneration`.
Go method names: `FinalizationRead/Begin/Execute/Commit/Cancel`.

---

## 7. `finalization-generation:begin`

### 7.1 Request args

`src/shared/finalization-generation.ts:40` — `{ slot: FinalizationGenerationSlot; modelId: string; parentRootActionId?: string }`
(arity 2). Call site `src/services/finalization-generation.ts:42-44` — note the client **replaces**
`modelId` with `recovery?.modelId` when a prior recovery exists.

`begin` may return an existing effect (`:40`, `:48`), an existing retryable run, or a fresh run.

### 7.2 Response DTO — `FinalizationGenerationRecovery` (non-null)

Same shape as §6.2.

### 7.3 Go implementation status

Nothing. Node's `beginFinalizationGeneration`
(`electron/services/main-generation-owner.ts:1242-1270`) is ~28 dense lines of prior/sibling/parent
admission with zero Go counterpart.

### 7.4 Blockers

1. **G1** — unbound.
2. **G5** — `slot` is not an allowed key (`synthetic_generation_linux.go:396-402`).
3. **G4** — the run must open with `sourceManifest.finalizationGenerationSlotKey` +
   `finalizationGenerationContext` + `finalizationGenerationContextHash` +
   `finalizationGenerationOriginEpoch` + `finalizationGenerationTask` +
   `finalizationGenerationTaskHash` (`finalization-generation-source.ts:38-50`) ⇒
   `GENERATION_OPERATION_UNSUPPORTED` at `open` (`generation_ledger.go:596`).
4. **Slot lookup** — needs §6.4 item 3, twice (own slot, then the sibling slot).
5. **Retryability law** — `finalizations.retryable` (`finalization-generation.ts:32-38`): retryable
   iff no effect, no in-flight attempt, and (`root cancelled` OR (`stepKey==='chapter_notes'` AND the
   live `notesBaseline` differs from the frozen one)). No Go counterpart.
6. **Predecessor law** — the sibling step must already have an effect or the call fails
   `GENERATION_FINALIZATION_PREDECESSOR_REQUIRED` (`main-generation-owner.ts:1259`). This creates a
   hard ordering dependency: `chapter_notes` before `character_cards`.
7. **Parent admission** — when `parentRootActionId` is given it must be a `batch-chapters` root of the
   same project whose `chapter-draft` run committed this exact `draftId`+`contentHash`
   (`:1253-1258`). Go has no batch draft-commit effect store (`generation:commit-draft` is itself
   unmigrated, `web/GO-MIGRATION-MATRIX.md`).
8. **Root sharing** — the new run is opened with `parentRootActionId = predecessor.runId's root`
   (`:1248`, `:1268`), which is precisely what makes G4 fatal for the whole family (§0).
9. **Projection** — §6.4 items 4-8.

### 7.5 Provider shape

Separate binder (§6.5).

---

## 8. `finalization-generation:execute`

### 8.1 Request args

`src/shared/finalization-generation.ts:42` — `{ handle: MainGenerationRunHandle; retryOf?: FinalizedCharacterArtifact; recoverFrom?: string }`
(arity 2). Call site `src/services/finalization-generation.ts:68`.

Client rules: `recoverFrom` and `retryOf` are mutually exclusive; `recoverFrom` must match
`ATTEMPT_ID`; `retryOf` only applies when it is one of the run's own candidates
(`src/services/finalization-generation.ts:56-58`).

### 8.2 Response DTO — `MainGenerationExecuteReceipt`

Same shape as §3.2. The client reads `receipt.run.handle` (`:69`), `receipt.outcome.receipt.failureCode`
(`:73`), `receipt.outcome.status`/`finishReason` (`:75`), `receipt.run.candidates ?? artifacts` (`:76`)
and **`receipt.outcome.receipt.visibleArtifact`** (`:77-78`).

### 8.3 Go implementation status

Same skeleton as §3.3. Everything about the repair loop is missing.

### 8.4 Blockers

1. **G1** — unbound.
2. **G4** — `reserve` → `mutableRoot` ⇒ blocked.
3. **No task source.** Node replays `sourceManifest.finalizationGenerationTask`
   (`finalization-generation.ts:46-51`). The builder is
   `finalizationGenerationTask` (`finalization-generation-source.ts:47-59`), which needs
   `PromptTemplate`, `renderPrompt`, `composePromptSystemRole`, `finalizedCharacterPromptCards`, and
   two ~700-character bilingual output contracts. **Zero Go counterpart** — this is the largest
   single port in the family.
4. **Invocation-nonce protocol** (must be reproduced exactly or replay diverges):
   first attempt `finalization:0`, repairs `finalization:1` / `finalization:2` (max 3 total);
   explicit retry `finalization:retry:<attemptId>`; explicit recovery
   `finalization:recover:<failedAttemptId>` then `…:<ordinal>` (`main-generation-owner.ts:1319-1402`).
5. **Repair loop** — for `character_cards` only, up to 3 dispatches in the same run, each built from
   the previous invalid text plus a fixed guidance string
   (`main-generation-owner.ts:1279-1318`). Requires `parseFinalizedCharacterStateResponse`
   (`src/shared/finalized-continuity.ts:177`) and `FinalizedCharacterEvidenceError` (`:169-173`) —
   no Go counterpart.
6. **`visibleArtifact` missing in Go** (`syntheticOutcome:1086-1113` emits the raw ledger receipt).
   This is a *hard* blocker for `execute`: without `visibleArtifact` the client's candidate lookup at
   `src/services/finalization-generation.ts:77-78` finds nothing and throws
   `FINALIZATION_GENERATION_RESULT_UNUSABLE`.
7. **Root-idle law** — `assertRootIdle` (`main-generation-owner.ts:1327-1335`): no `blockedCode`,
   no unsaved tails for this root, root `active|paused`, no `reserved|dispatch-marked` attempt, no
   pending in-process dispatch. `GENERATION_DISPATCH_BLOCKED` / `GENERATION_FINALIZATION_ARTIFACT_INVALID`.
   `ReadUnsavedTails` exists (`internal/generation/service.go:777`) but is not exposed through
   `OrdinaryService`.
8. **Recovery-unavailable law** — `GENERATION_FINALIZATION_RECOVERY_UNAVAILABLE` when the frozen
   attempt already produced a usable `stop` result (`:1361-1363`); `GENERATION_FINALIZATION_ATTEMPT_CONFLICT`
   when the attempt order does not match (`:1381`, `:1395`). Neither has a Go form.
9. **Terminal-failure preservation** — `src/services/finalization-generation.ts:9`, `:62`, `:74`
   require the outcome receipt to carry `NETWORK_ERROR` / `GENERATION_PROVIDER_FAILED` /
   `ROOT_BUDGET_EXHAUSTED` / `GENERATION_STORAGE_FAILED` verbatim with **no automatic paid
   redispatch**. Go surfaces these as `settle` result codes (`generation_ledger.go:1048-1063`) but not
   on the receipt the client reads.

### 8.5 Provider shape

Separate binder (§6.5).

---

## 9. `finalization-generation:commit`

### 9.1 Request args

`src/shared/finalization-generation.ts:43` — `{ handle, artifact: FinalizedCharacterArtifact }`
(arity 2), where `FinalizedCharacterArtifact` is `{artifactId, revision, textHash}`
(`src/shared/finalized-character-generation.ts:6-10`). Call site
`src/services/finalization-generation.ts:84-86`; the artifact is always rebuilt from the candidate
row, never taken from the model.

### 9.2 Response DTO — `FinalizationGenerationEffect`

`src/shared/finalization-generation.ts:21-27`:

```
| { success:true; stepKey:'chapter_notes'; chapterNotes:string; factCount:number; blueprintUpdated:boolean }
| ( FinalizedCharacterGenerationReceipt & { success:true; stepKey:'character_cards' } )
```

where `FinalizedCharacterGenerationReceipt` (`src/shared/finalized-character-generation.ts:16-19`) =
`FinalizedCharacterStateCommitReceipt { applied:number; unchanged:number;
candidates: FinalizedCharacterStateCandidate[] }` +
`{ unresolved: FinalizedCharacterStateOccurrence[]; proposalBatchId?: string }`.

### 9.3 Go implementation status

**Nothing for the domain writes.** This is the family's real gap.

| Node write | Go counterpart |
|---|---|
| `SummaryRepository.saveFinalizedContinuity` (`finalization-generation.ts:102`) | **none** — `grep -rn "SaveFinalizedContinuity" backend-go/` ⇒ 0 hits; `internal/store/summary.go` has only `SaveSnapshot` (`:368`, a different `character_states`-shaped API) |
| `BlueprintRepository.updateNotes` (`finalization-generation.ts:103`) | **none** — `internal/store/blueprints.go` has `Upsert` (`:119`), `UpsertMany` (`:123`), `CommitRange` (`:277`); no notes-only `UPDATE blueprints SET notes=?”` path, and `notes_updated_at` is only ever *read* (`:44`, `plottree.go:57`) |
| `SummaryRepository.commitFinalizedCharacterStates` (`:108`) | **none** |
| `buildFinalizedContinuityFacts` (`:101`) | **none** — `src/shared/finalized-continuity-facts.ts` unported; Go's `summaryFacts` (`summary.go:233`) is the reverse-direction parser |
| `proveFinalizedCharacterGeneration` (`finalized-character-generation-proof.ts`) | **none** |
| `CharacterProposalService.stage` (`:110`) | **none** — `character_identity_proposals` is only touched by schema migration (`internal/admission/upgrade_v2.go:29`, `:463`) and the read-side `summaryFrozen` (`summary.go:518`) |

### 9.4 Blockers

1. **G1** — unbound.
2. **G3 (deliberate)** — the effect must persist as `usage_receipt_json.finalizationEffect`
   (Node `finalization-generation.ts:113-114`, read at `:61-80`); rejected by
   `generation_ledger.go:495-501`.
3. **G2** — no effect-write ledger method.
4. **G4** — `mutableRoot` for the effect write AND for each domain write.
5. **`GENERATION_FINALIZATION_NOTES_CHANGED`** — `chapter_notes` re-checks the live
   `notesBaseline` inside the transaction (`finalization-generation.ts:99`). Requires the §6.4 item 5
   builder.
6. **Sealing/supersession** — `assertMutable` (`finalization-generation.ts:81-85`):
   `GENERATION_FINALIZATION_EFFECT_SEALED` if an effect exists, `GENERATION_FINALIZATION_SUPERSEDED`
   if a newer run for the same slot exists.
7. **Atomicity** — Node wraps artifact proof + domain write + effect record in one
   `db.transaction(...).immediate()` (`:87`, `:116`). Go's `NewReviewRevisions(db).immediate`
   (`internal/store/reviewrevision.go`) is the equivalent, but the provider must not straddle two
   `immediate()` calls or a crash leaves the domain written and the effect unrecorded.
8. **Error codes the frontend maps** — `FINALIZED_CHARACTER_SOURCE_CONFLICT` and
   `FINALIZED_CHARACTER_FIELD_CONFLICT` are asserted in `src/i18n/__tests__/app-errors.test.ts:37-38`
   as reachable from `finalization-generation:commit`. The Go error must be emitted as a business DTO
   (`draftSaveFailure`, `internal/transport/generation.go:159-162`), not an HTTP error.

### 9.5 Provider shape

Separate binder (§6.5). Shares the §4.5 effect primitive.

---

## 10. `finalization-generation:cancel`

### 10.1 Request args

`src/shared/finalization-generation.ts:44` — `{ handle }` (arity 2). Call site
`src/services/finalization-generation.ts:27`, driven by a 25 ms poller (`:36`).

### 10.2 Response DTO

`MainGenerationRunView` (non-null), same as §5.2. The client discards the body and re-reads.

### 10.3 Go implementation status

Identical to §5.3 — `stopRun` (`synthetic_generation_linux.go:1319`) is a direct match.

### 10.4 Blockers

1. **G1** — unbound.
2. **G4** — `pause` → `mutableRoot` ⇒ blocked.
3. Nothing else. The cheapest op in the family after §5.

### 10.5 Provider shape

Separate binder (`FinalizationCancel`).

---

## 11. Cross-cutting answers

### 11.1 Does `store.FinalizationOutbox` already provide the durable state `finalization-generation:*` needs?

**No. It is a different subsystem that happens to share the word "finalization".**

- **What it is.** `internal/store/finalization_outbox.go:87-91` — an owner-proof-gated **publication
  outbox** over the `finalization_outbox` table (`:106-108`), keyed by `finalization_id`/`draft_id`,
  holding frozen manuscript prose (`content_snapshot`), `content_hash`, `content_revision`,
  `target_file_name`, `knowledge_document_id`, `publication_status`, `last_error`, `published_at`
  (`:46-59`). Its job is "author confirmed prose is frozen and ready to publish".
- **What the ops need.** A **generation-run** record: `generation_runs` + `generation_attempts` +
  `generation_artifacts` keyed by `binding_json.$.sourceManifest.finalizationGenerationSlotKey`, with
  an attempt-scoped `usage_receipt_json.finalizationEffect`. The outbox has no run, attempt, artifact,
  handle, or slot concept.
- **What `finalization-generation:commit` actually writes** (`electron/services/finalization-generation.ts:98-112`):
  - `chapter_notes` → `summary_snapshots` (+ `continuity_facts`) via `saveFinalizedContinuity`, then
    `blueprints.notes` via `updateNotes`, then the attempt's `finalizationEffect`.
  - `character_cards` → character state via `commitFinalizedCharacterStates`, then
    `character_identity_proposals` via `CharacterProposalService.stage`, then the attempt's
    `finalizationEffect`.
  **No `finalization_outbox` row is written or updated by this operation at all.**
- **What it *does* legitimately supply** (three read inputs, worth reusing):
  1. `chapter_title` for `FinalizationGenerationContext.chapterTitle`
     (`finalization-generation-source.ts:32` ← `internal/store/finalization_outbox.go:141`);
  2. the `finalizationId` / `contentHash` / `contentRevision` identity a slot freezes
     (`FinalizedSourceIdentity`, `src/shared/finalized-continuity.ts:12-17`);
  3. the graph `event`-kind context's joined `drafts⨝contents⨝finalization_outbox⨝
     continuity_projection_meta` row (`graph-generation-source.ts:53-65`).
- **API mismatch on both axes.** (a) Every write is gated by `FinalizationOwnerProofVerifier`
  (`:80-82`) and returns `ErrFinalizationOwnerRequired` (`:98`) with no Go verifier in existence —
  the type explicitly forbids a permissive default ("No permissive/default implementation or proof
  minting API exists here", `:79`). (b) Its error vocabulary is `FINALIZATION_COMMIT_*` / `FINALIZATION_*`
  (`:14-25`), a different code family from the `FINALIZATION_GENERATION_*` / `FINALIZED_CHARACTER_*`
  codes the frontend branches on (`src/services/finalization-generation.ts:34`, `:49`, `:65`, `:75`,
  `:82`; `src/i18n/__tests__/app-errors.test.ts:37-38`).
- **Verdict:** reuse `FinalizationOutbox.Get`/`GetByDraftID` as **read-only context inputs**; do not
  attempt to build `finalization-generation:commit` on top of it.

### 11.2 Is there any existing Go code for graph generation at all?

**No provider, handler, DTO or projector — only the rejection gate, its test, and the confirm sink.**
`grep -rn "graph" backend-go/` yields four substantive sites plus the store primitives:

| Site | What it is |
|---|---|
| `internal/store/generation_ledger.go:434` | the quarantine: `graphGenerationContext` in the manifest key list |
| `internal/generationowner/binding.go:47-49` | doc comment naming `graph` as an unsupported ref family |
| `internal/transport/plottree.go:22` | `db:plot-tree-save` **deliberately** returns `GENERATION_GRAPH_ADMISSION_REQUIRED` |
| `internal/projectsession/plottree_http_test.go:26`, `internal/transport/plottree_test.go:15` | tests asserting that refusal |
| `internal/store/plottree.go:26-70` | `PlotTree.Read` / `PlotTree.Save` — the graph-confirm **sink**, already correct |
| `internal/store/narrative.go:75`, `:131` | `CreatePlan` / `ConfirmEvent` — the plan/event confirm sinks |
| `internal/store/plottree_validation.go:296` | `plotValidate` — the snapshot validator `PlotTree.Save` calls |

What is absent: any `graph-generation` identifier, any context capture/read, any of the three result
parsers (`derivePlotTreeSnapshot`, `parseNarrativeThreadPlanCandidates`,
`parseNarrativeThreadEventCandidates`), any task builder, any effect store.

### 11.3 Deliberate security gate vs missing implementation

**Deliberate (documented as requiring reviewer sign-off):**

- **G4, the manifest quarantine** (`internal/store/generation_ledger.go:434-438`). Documented at
  `web/GO-GENERATION-CHAIN-SPEC.md:19-21` ("The ledger **deliberately rejects**
  agent/graph/import/finalization manifests") and `:151-152` ("Widening the ledger gates for
  agent/graph/import/finalization is a design decision requiring reviewer sign-off before S3/S4");
  restated at `web/GO-S3-AGENT-GENERATION-MAP.md:100-101`, `:585`, `:606`.
- **G3, the effect-authority quarantine** (`internal/store/generation_ledger.go:495-501`) — the same
  design decision applied to `usage_receipt_json` ("Unknown workflows are errors, never successful
  no-ops", `:269-270`).
- **`db:plot-tree-save` refusal** (`internal/transport/plottree.go:22`) — deliberate, intentional
  bypass prevention, recorded at `web/GO-MIGRATION-MATRIX.md:108` ("graph-save shortcut remains
  rejected"). **Not a blocker for any of the ten**: `graph-generation:confirm` (kind `plot`) is the
  sanctioned replacement path, so this gate is the *reason* the family exists rather than an obstacle.

**Mapping to the ten:**

| Op | Deliberate gate | Missing implementation |
|---|---|---|
| `graph-generation:begin` | G4 | binder, manifest keys, context builder + captured-source shape, slot→run nonce lookup, recovery projection |
| `graph-generation:read` | G4 (only via sibling ops; the read itself is not gated) | binder, recovery projection, result derivation |
| `graph-generation:execute` | G4 | binder, server-side task replay + verifier, `visibleArtifact` receipt projection, attempt-count law |
| `graph-generation:confirm` | **G3 + G4** | effect-write ledger method, artifact proof helper, baseline-hash check, sealing law |
| `graph-generation:cancel` | G4 | **binder only** |
| `finalization-generation:read` | — (reads are not gated) | binder, slot→run lookup, `notesBaseline`/`identity`/`template` builders |
| `finalization-generation:begin` | G4 | binder, slot lookup ×2, retryability/predecessor/parent laws, whole projection |
| `finalization-generation:execute` | G4 | binder, full task builder + validator port, nonce protocol, repair loop, `visibleArtifact`, root-idle + recovery laws |
| `finalization-generation:commit` | **G3 + G4** | effect-write method, `SaveFinalizedContinuity`, `Blueprint.UpdateNotes`, `commitFinalizedCharacterStates`, facts builder, proof, proposal staging |
| `finalization-generation:cancel` | G4 | **binder only** |

**Net:** all ten are gated by *one* deliberate decision (G4), and two of them by a second (G3). Beyond
those, six of the ten are substantial implementations and two (`graph-generation:cancel`,
`finalization-generation:cancel`) are pure binder work.

---

## 12. Recommended batches

### The two primitives everything waits on

**P1 — manifest widening (deliberate; reviewer sign-off).** Extend the key list at
`internal/store/generation_ledger.go:434-438` so `graphGenerationContext` and
`finalizationGenerationSlotKey` are admitted, ideally behind a narrow predicate that requires the
matching `operation` (`plot-tree-snapshot` / `narrative-thread-plan-candidate` /
`narrative-thread-event-candidate` / `finalized-chapter-notes` / `finalized-character-state`) and a
present, hash-matching companion key — not by simply deleting the names. `glPlanning` (`:421-423`)
needs no change: none of those five operation strings contains `planning`/`blueprint`/`plot-outline`.

**P2 — the effect primitive (highest leverage).** A ledger `write` method
`recordEffect(ctx, attemptID, key, value)` that, inside `mutableRoot`'s existing IMMEDIATE
transaction:

1. re-runs the mutable-root guard (so the manifest gate applies to the effect writer too),
2. patches one named top-level key of `usage_receipt_json` via `glPatch`
   (`glPatch` is `internal/store/generation_ledger.go:165`, already used by `settle` at `:1055`),
3. is idempotent — replaying the same `[key, hash(value)]` returns the stored value, a differing one
   is a conflict.

Add `"recordEffect"` to the `write` set at `:268` and a `case` at `:799`, and add `graphEffects` /
`finalizationEffect` to the key allowlist at `:497`. **This one primitive is shared by
`graph-generation:confirm`, `finalization-generation:commit`, and (per
`web/GO-S3-AGENT-GENERATION-MAP.md` §S3-3) `agent-generation:claim-tool` / `finish-tool` /
`register-workflow` / `commit-domain-tool`** — six operations across two families.

Two additional shared helpers, cheap but reused by all ten: `syntheticHandle` parsing is already
generic (`cmd/novel-server/synthetic_generation_linux.go:346-371`) ✅; a **`runSummaryFor(h)`** and a
**`manifestKeyRuns(key, jsonPath, value)`** lookup (§6.4 item 3) are not.

### B1 — `graph-generation:cancel` + `finalization-generation:cancel` (binder + P1)

Smallest possible unit that proves the new binder plumbing end to end. Two new
`transport` binders in a new file `internal/transport/graph_finalization_generation.go`
(`GraphGenerationProvider{Begin,Read,Execute,Confirm,Cancel}`,
`FinalizationGenerationProvider{Read,Begin,Execute,Commit,Cancel}`), nil fields leave the other eight
at 501. Reuse `stopRun` (`synthetic_generation_linux.go:1319`). Wire next to
`server.BindGeneration(binder)` at `cmd/novel-server/synthetic_durable_open_linux.go:527`.

Also required in **every** batch: edit `cmd/novel-server/synthetic_generation_http_linux_test.go:450-458`
— move the bound ops out of `unmigrated`, decrement the `32` (32 → 24 over four batches of two), and
add positive envelope assertions.

Test: cancel a graph run → `MainGenerationRunView` with `status:'cancelled'`, same handle, one
`generation_roots` status flip; cancel twice is idempotent; cancel with a foreign `epoch` →
`GENERATION_RUN_NOT_FOUND`; both remaining eight ops still 501.

### B2 — `graph-generation:begin` + `graph-generation:read`

Prerequisites: P1; the graph manifest keys (`syntheticManifest`,
`synthetic_generation_linux.go:704-731`); a port of
`captureGraphGenerationContext` + `readGraphGenerationContext`
(`electron/services/graph-generation-source.ts:26-102`) including the captured
`PlotTreeSourceBundle` shape (drop `snapshot`/`storedSnapshotInvalid`, keep `sourceRevision` out of
the hash inputs exactly as Node does); the `manifestKeyRuns` lookup for `uiActionNonce`
idempotency; and the `graphRecovery` projection (`main-generation-owner.ts:1092-1106`) with
`attemptCount`, `sourceStatus` (via an exported `Binding` revalidation), and the
`GENERATION_GRAPH_ATTEMPT_CONFLICT` rule.

Defer `artifact`/`result` to B3 (they are `undefined` until an attempt exists, and `read` must still
work then).

Test: begin(`kind:'plot'`) → `result`/`artifact` absent, `effects: []`, `attemptCount: 0`; repeat with
the same `uiActionNonce` returns the same `runId`; same nonce + different `input` →
`GENERATION_NONCE_CONFLICT`; begin(`kind:'plan'`) with an unknown `chapterNumber` →
`GENERATION_GRAPH_BLUEPRINT_REQUIRED`; begin(`kind:'event'`) with a non-finalized `draftId` →
`GENERATION_GRAPH_FINALIZED_SOURCE_INVALID`; `read` after begin is byte-identical.

### B3 — `graph-generation:execute` (+ `result`/`artifact` in `read`) and `graph-generation:confirm`

Prerequisites: P1, P2; the three task builders + three result parsers
(`src/shared/plot-tree-generation-pure.ts`, `narrative-thread-generation-pure.ts`); the frozen-task
replay + hash verifier (`graph-generation.ts:152-156`, `:99-100`); the artifact proof
(`graph-generation.ts:40-50`); the `targetBaselineHash` check for plot; the sealing law
(`graph-generation.ts:97-99`); and — shared with B4 — the **`visibleArtifact` receipt projection**
fix, since §8.4 item 6 proves the current `syntheticOutcome` cannot serve a receipt-reading client.

Do not split `execute` from `confirm`: `confirm` is unreachable without a settled artifact, and the
`plot` kind has exactly one candidate (`src/services/graph-generation.ts:80`), so a partial batch
would ship a provider whose only reachable path throws.

Test: execute(`plot`) with a fake dispatch → `outcome.status:'completed'`, one attempt, `read`
returns `artifact{...}` + `result.kind:'plot'`; confirm(0) → `{success:true,index:0,kind:'plot',
snapshot,derivation}` and `project_core.plot_tree_snapshot` changed exactly once; confirm(0) again
returns the stored effect; confirm after an author snapshot edit → `GENERATION_GRAPH_TARGET_CHANGED`;
confirm(1) on a plot → `GENERATION_GRAPH_SELECTION_INVALID`; a mutating S1 op on the same root after
a confirm → `GENERATION_GRAPH_EFFECT_SEALED`.

### B4 — `finalization-generation:read` + `finalization-generation:begin`

Prerequisites: `manifestKeyRuns` for `finalizationGenerationSlotKey` (the single most important new
store primitive in the family); `finalizationSlotKey` (`finalization-generation-source.ts:12-18`);
`finalizationNotesBaseline` (`:19-23`); a finalized-character-context reader (no Go counterpart to
`SummaryRepository.readFinalizedCharacterContext`); the `PromptTemplate` table + `renderPrompt` port
(needed for `context.template` *and* for B5's task); and the `retryable`/`predecessor`/`parent` laws
(`finalization-generation.ts:32-38`, `main-generation-owner.ts:1259`, `:1253-1258`).

`read` is nullable and slot-keyed — do not model it on `graph-generation:read`.

Test: `read` on an untouched slot → `null`; begin(`chapter_notes`) → `attemptCount:0`,
`context.chapterTitle` non-empty, `context.notesBaseline.blueprint.exists` correct; begin
`character_cards` before `chapter_notes` has an effect →
`GENERATION_FINALIZATION_PREDECESSOR_REQUIRED`; begin twice on the same slot → same `runId`; a slot
whose draft body changed → `GENERATION_FINALIZATION_SOURCE_CHANGED`; `parentRootActionId` of a
non-`batch-chapters` root → `GENERATION_FINALIZATION_PARENT_INVALID`.

### B5 — `finalization-generation:execute`

Prerequisites: everything in B4 plus the **largest single port in S4** — the task builder and its two
bilingual contracts (`finalization-generation-source.ts:47-59`); the callable parser
`parseFinalizedCharacterStateResponse` + `FinalizedCharacterEvidenceError`
(`src/shared/finalized-continuity.ts:169-177`); the three-nonce repair loop
(`main-generation-owner.ts:1279-1318`); the exact nonce vocabulary (`:1394-1402`); `assertRootIdle`
(`:1327-1335`, needs `ReadUnsavedTails` surfaced through `OrdinaryService`); the
recovery-unavailable / attempt-conflict laws; and the `visibleArtifact` + `failureCode` receipt
projection from B3.

Alone among the batches this one has no data-writing effect, which makes it independently
verifiable: run `chapter_notes`, assert `summary_snapshots` is **unchanged**.

Test: execute(`chapter_notes`) → `completed`/`stop`, one attempt at nonce `finalization:0`, no domain
row written; execute again → replay, still one attempt; execute(`character_cards`) with a malformed
JSON artifact → two repairs (`finalization:1`, `finalization:2`) and three attempts; a fourth-order
failure → `GENERATION_FINALIZATION_ATTEMPT_CONFLICT`; `recoverFrom` naming a usable attempt →
`GENERATION_FINALIZATION_RECOVERY_UNAVAILABLE`; a root with a pending dispatch →
`GENERATION_DISPATCH_BLOCKED`.

### B6 — `finalization-generation:commit`

Prerequisites: P2; `store.Summary.SaveFinalizedContinuity` (new); `store.Blueprints.UpdateNotes`
(new — `UPDATE blueprints SET notes=?, notes_updated_at=datetime('now'), updated_at=datetime('now')
WHERE chapter_number=?`, returning `changes > 0`); `store.Summary.CommitFinalizedCharacterStates`
(new); a port of `buildFinalizedContinuityFacts`; `proveFinalizedCharacterGeneration`; the
`CharacterProposalService.stage` equivalent over `character_identity_proposals`; the live
`notesBaseline` re-check; and the sealing/supersession laws (`finalization-generation.ts:81-85`).

All of it must land **inside one** `NewReviewRevisions(db).immediate(...)` transaction together with
the P2 effect write — mirroring `finalization-generation.ts:87`/`:116` — or a crash strands a written
domain effect with no recorded receipt.

Test: commit(`chapter_notes`) → `{success, stepKey:'chapter_notes', chapterNotes, factCount,
blueprintUpdated:true}`, `summary_snapshots` has exactly one new row, `blueprints.notes` equals the
trimmed artifact text; commit again → identical receipt, no new rows; author edits blueprint notes
between execute and commit → `GENERATION_FINALIZATION_NOTES_CHANGED`; commit(`character_cards`) →
`{applied, unchanged, candidates, unresolved}`, and with a non-empty `unresolved` a
`proposalBatchId` naming a real `character_identity_proposals` row; a forced failure mid-transaction
leaves neither the domain write nor `usage_receipt_json.finalizationEffect`.

### Why this order

- **P1 is a decision, not code.** It blocks all ten and cannot be phased. Do it first, in one review,
  with the narrow operation-string predicate so the widening is auditable.
- **P2 is the only primitive shared across families.** Building it in B3 (where the first effect op
  lands) means B6 and the whole S3-3 batch reuse it instead of inventing three variants.
- **B1 before B2** because it is the smallest change that proves the new binder, the arity/session
  envelope, the 501 accounting, and the P1 widening against a *real* run — with no new DTO. If P1's
  predicate is wrong, B1 finds out in one test instead of five batches.
- **B2 before B3** because `begin` is the only op that can create a run; `execute`/`confirm` have
  nothing to operate on otherwise, and `read` must be proven to work on a zero-attempt run (Node's
  `artifact`/`result` are `undefined` there).
- **B3 as one batch** because `confirm` is unreachable without `execute` and `execute`'s receipt is
  unverifiable without `confirm`'s stored effect.
- **B4 before B5** for the same reason, plus `begin` establishes the slot→run identity that
  `execute`'s nonce protocol depends on.
- **B5 before B6** because `commit` needs a real settled artifact to prove, and B5 is the only batch
  that can produce one. B5 is also independently shippable and independently verifiable (it writes
  nothing), which makes it the right place to absorb the largest port.
- **B6 last** because it is the only batch that writes author domain data and therefore carries the
  domain-regression risk that `web/FEATURE-MATRIX.md:36` (F14) already flags
  (`postprocess DOMAIN REGRESSION`). It should not be the first thing reviewers see.

**Total: 1 policy decision (P1), 1 new ledger primitive (P2), 8 new store methods/ports, 2 binders,
6 test batches. Moves both families from `5 missing` to `0 missing` on
`web/GO-GAP-MATRIX.md:99-100`; the first shippable unit is B1 (both `cancel` ops, `10 missing` →
`8 missing`).**
