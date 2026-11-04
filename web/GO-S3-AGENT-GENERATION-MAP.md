# S3 binding map — the eight `agent-generation:*` operations

Read-only analysis. Sources: `web/operations.json`, `src/shared/agent-generation.ts`,
`src/services/agent/*`, `electron/controllers/generation-controller.ts`,
`electron/services/agent-generation*.ts`, `backend-go/internal/**`,
`backend-go/cmd/novel-server/**`.

Repository state at time of writing: `git log --oneline -1` → `ad9868b9 docs(go): record the
resolved S1 generation lane in the gap matrix and handoff`.

---

## 0. Shared facts (apply to all eight)

**Inventory.** `web/operations.json` is the 281-op inventory (`"version": 1`, 281 entries,
`operations` map `name → scope`). All eight names exist exactly once with scope `project`:

```
agent-generation:begin               => project
agent-generation:read                => project
agent-generation:resume              => project
agent-generation:round               => project
agent-generation:claim-tool          => project
agent-generation:finish-tool         => project
agent-generation:register-workflow   => project
agent-generation:commit-domain-tool  => project
```

None is in `excluded`. Loader: `backend-go/internal/policy/registry.go:15` (`policy.Load`);
membership ≠ implementation (`registry.go:1-3`).

**Wire envelope.** Every one of the eight is invoked via
`ipc.invokeWithProjectSession(...)` (`src/services/ipc-client.ts:105-116`), so the browser sends
`args = [<request>…, <ProjectSessionContext>]`. The Electron route helper pops the trailing
session and asserts `raw.length === arity` (`electron/controllers/generation-controller.ts:179-184`).
Therefore **every one of the eight has wire arity 2** (one request object + one 3-key session
context `{projectId, leaseId, projectPath}`), matching the existing Go convention in
`backend-go/internal/transport/generation.go:120-142`.

**Current Go behaviour.** Zero of the eight is bound. `grep -rn "agent-generation" backend-go/`
returns hits in exactly **one** file — the S1 acceptance test
(`backend-go/cmd/novel-server/synthetic_generation_http_linux_test.go:419-436`), which lists all
eight in its `unmigrated` slice and asserts `501 GO_OPERATION_NOT_MIGRATED` for each. No Go handler,
no Go provider field, no Go identifier. An unbound but inventoried op returns
`security.NewHTTPError(501,"GO_OPERATION_NOT_MIGRATED")`
(`backend-go/internal/transport/server.go:100`). Confirmed by
`web/GO-MIGRATION-MATRIX.md:126-133` (`NOT STARTED`) and `web/GO-GAP-MATRIX.md:92`
(`agent-generation | 8 | 0 | 0 | 8`).

**Consequence every batch must handle:** because that slice is a hard `want 32` + per-op 501 loop
(`:428-436`), binding *any* of the eight breaks the existing S1 test. Each batch below must move its
ops out of `unmigrated`, decrement the count at `:428-430`, and assert the real envelope instead. This is
the cheapest available fail-closed test and it already exists — do not duplicate it.

**Authoritative behaviour lives in three Node modules** (the port target):

- `electron/services/agent-generation.ts` — the whole state machine (405 lines).
- `electron/services/agent-generation-context.ts` — input validation + context construction.
- `electron/services/agent-domain-tool-effect.ts` — the config/blueprint effect writer.

**All eight are one subsystem.** There is no `internal/agentruntime/` package, no agent table, no
second budget, no second writer. `electron/services/agent-generation.ts:25` states the design
constraint: *"Agent navigation/control lives on existing runs and attempt receipts. No second
budget or writer."* Agents are a projection over `generation_runs` + `generation_attempts` keyed by
`binding_json.$.sourceManifest.agentSession.key`, with action state stored in
`generation_attempts.usage_receipt_json`. So there is **no op among the eight that is a stateless
read/write on the admitted author database**, with one partial exception (`commit-domain-tool`,
§8, which additionally performs an ordinary domain write). Answer to question 5 for all eight:
**full durable generation lane operation** — `projectsession.ActiveCoordinator.Begin` (via an
`authorruntime` ledger admission) plus the generation ledger. No op can be served as a plain
`store` read.

**Substrate that already exists in Go.**

| Building block | Location |
|---|---|
| Admission + ledger + author `*sql.DB` in one release | `internal/authorruntime/generation_db_linux.go:25` (`BeginGenerationLedgerDB`) |
| Ledger-only variant | `internal/authorruntime/generation_linux.go:19` (`BeginGenerationLedger`) |
| Coordinator admission | `internal/projectsession/active.go:239` (`*ActiveCoordinator.Begin`) |
| Generation ledger RPC surface | `internal/store/generation_ledger.go:242` (`CallJSON`), `:253` (`Call`) |
| Ledger method dispatch | `internal/store/generation_ledger.go:799` (`call`) |
| Model lease / binding registry | `internal/generationowner/lease.go:143-162`, `:50` (`BindSources`) |
| Ordinary dispatch service | `internal/generationowner/ordinary.go:16` (`OrdinaryService`), `:114` (`Execute`), `:173` (`Pause`), `:181` (`Cancel`), `:190` (`Resume`), `:199` (`DiscardCandidate`) |
| Transport binder pattern to copy | `internal/transport/generation.go:18` (`GenerationProvider`), `:36` (`BindGeneration`), `:150` (`generationFailure`), `:167` (`generationSymbolicCode`) |
| S1 adapter (all eleven providers) | `cmd/novel-server/synthetic_generation_linux.go` — `admit:318`, `Begin:815`, `Execute:1015`, `Read:1262`, `Resume:1356`, `viewOf:1594` |
| Wiring point | `cmd/novel-server/synthetic_durable_open_linux.go:471` (`server.BindGeneration(binder)`) |
| sha256-hex `textHash` equivalent | `cmd/novel-server/synthetic_generation_linux.go:206` (`syntheticHash`) |
| Canonical JSON | `cmd/novel-server/synthetic_generation_linux.go:211` (`syntheticCanonical`) |
| `project_core` writer | `internal/store/projectcore.go:156` (`ProjectCore.Update`), `:57` (`Get`) |
| `blueprints` writer | `internal/store/blueprints.go:119` (`Blueprints.Upsert`), `:69` (`All`), `:82` (`Get`) |

### 0.1 The six hard blockers (each one is reachable by 3+ of the eight ops)

**B1 — the ledger refuses `agentSession`.** `internal/store/generation_ledger.go:424-443`
(`glOrdinaryBinding`) rejects any `sourceManifest` containing `agentSession`,
`graphGenerationContext`, `importSlot`, `legacyRosterContext`,
`finalizationGenerationSlotKey` with `GENERATION_OPERATION_UNSUPPORTED` (`:434-438`). It is called
from `open` at `:596`, and `open` is the only way a run row is created. Consequence: **no
`agent-round` run can exist in Go**, so all eight ops are unreachable. This is documented as an
explicit design decision requiring sign-off — `web/GO-GENERATION-CHAIN-SPEC.md:151` ("Widening the
ledger gates for agent/graph/import/finalization is a design decision requiring reviewer sign-off
before S3/S4").

**B2 — the dispatch service rejects the agent tool manifest.**
`internal/generation/service.go:259-263` (`validateRequest`) and `:289-291` (`Execute`) both return
`GENERATION_OPERATION_UNSUPPORTED` when `r.AgentToolNames != nil` — including an empty slice
(comment at `:259`). Blocks `round`, `resume`, and therefore `read` for any executed round.

**B3 — no agent response protocol parser, and no agent response producer.** The browser contract
requires `receipt.result.usage.agentResponse = {version:1, visibleText, toolCalls[]}`
(`electron/repositories/generation-run-repository.ts:50`), produced only at
`electron/services/generation-run-service.ts:261-270` from
`parseAgentResponseProtocol` (`src/shared/agent-response-protocol.ts:136`, 161 lines: XML / DSML /
raw-JSON tool-call extraction + visible-text cleaning + interrupted-text projection). In Go,
`grep -rni "tool_call\|parseAgent\|agentProtocol" backend-go/` returns **exactly one hit, a test
name** (`internal/generation/service_test.go:114`). `internal/llm/` models a message as
`{role, content}` only (`internal/llm/openai.go:18`) and has no tool-call type.

**B4 — `usage_receipt_json` has no agent-state mutation primitive.** Node stores per-action state
in top-level keys of `generation_attempts.usage_receipt_json`: `agentActions`,
`agentAuthorSelections`, `agentSourceTransition`, `agentControlEpoch`
(`electron/services/agent-generation.ts:29-30`), written by raw SQL at `:225-228` (updateAction),
`:234-241` (author selections), `:280-284` (control epoch), `:394-397`
(`agentSourceTransition`), `:35-45` (startup `running → unknown|completed` sweep). The Go ledger
only ever writes `usage_receipt_json` in one place — `settle`, which patches only `$.result`
(`internal/store/generation_ledger.go:1017-1065`, patch at `:1049-1055`). There is **no** ledger
method that writes an arbitrary top-level usage key; the method allowlist is
`internal/store/generation_ledger.go:267-268`.

**B5 — no agent context builder, and no `assistant_writing_identity` prompt.** `begin` needs
`buildAgentGenerationContext` (`electron/services/agent-generation-context.ts:34-57`), which reads
the `assistant_writing_identity` prompt template (`:37-39`) and composes a system message from
`composePromptSystemRole` plus the frozen tool manifest. `grep -rn
"assistant_writing_identity" backend-go/` → **0 hits**; `internal/promptskills/` is a
file/install/inspection service (`package.go`, `install_linux.go`, `inspection.go`,
`service.go`), not a template registry.

**B6 — the manifest and selection DTOs are closed and have no agent fields.**
`syntheticSelection` (`cmd/novel-server/synthetic_generation_linux.go:373-390`) rejects unknown
keys at `:403-406` (`GENERATION_BEGIN_INVALID`), and its allowlist at `:397-401` has no
`agentInput`/`agentSession`/`agentContext`/`agentContextHash`. `syntheticManifest` (`:704-726`)
emits fourteen fixed keys, none of them agent keys. Node's `AgentBeginSelection`
(`electron/services/agent-generation.ts:20`) is `BeginGenerationRequest & {agentInput,
agentSession:{key,roundIndex}}`, with `operation: 'agent-round'`, `uiActionNonce:
'agent:<key>:<roundIndex>'`, `promptKeys: ['assistant_writing_identity']`, `skillStages:
['planning','drafting','review','refinement']`, `output: 'visible-text'`
(`electron/services/agent-generation.ts:175-178`).

### 0.2 Shared response DTO `AgentGenerationRecovery` (returned by 4 of the 8)

`src/shared/agent-generation.ts:84-92`:

```
handle: MainGenerationRunHandle            // {projectId, epoch, rootActionId, runId}
modelId: string
context: AgentGenerationContext            // {version:1, input, writingLanguage, initialMessages[]}
rounds: AgentGenerationRound[]             // one per run in the session, index-dense
sourceStatus: 'current' | 'conflict'
nextRound: number | null                   // 0..7, null when done/blocked
run: MainGenerationRunView                 // full ordinary view, incl. ledger/budget/artifacts
```

`AgentGenerationRound` (`:74-82`): `index`, `handle`, `attemptId?`, `status:
'not-started'|'completed'|'incomplete'|'unknown'`, `visibleText`, `protocolText`, `actions[]`.
`AgentToolAction` (`:44-50`): `ref{handle, attemptId, toolCallId}`, `name`, `arguments`, `status:
'pending'|'running'|'completed'|'declined'|'failed'|'unknown'`, `observation?`, `workflow?`.
`MainGenerationRunHandle` (`src/services/generation/generation-runtime.ts:83-88`) is exactly the Go
`syntheticHandle` struct (`cmd/novel-server/synthetic_generation_linux.go:346-351`), so the handle
type is already portable. `run` is exactly what `syntheticGeneration.viewOf` already returns
(`cmd/novel-server/synthetic_generation_linux.go:1594`).

**Round cap is 8** (`roundIndex < 8`, `electron/services/agent-generation.ts:66`; the browser
mirrors it at `src/services/agent/agent-generation-client.ts:60` and
`electron/services/agent-generation.ts:183`).

**Error vocabulary the frontend matches on** (these must be preserved verbatim):
`GENERATION_AGENT_HANDLE_REQUIRED`, `GENERATION_AGENT_IDENTITY_MISMATCH`,
`GENERATION_AGENT_DETACHED`, `GENERATION_SOURCE_CHANGED`, `GENERATION_AGENT_ROUND_MISSING`,
`GENERATION_AGENT_TOOL_CHANGED`, `GENERATION_AGENT_CANCELLED`
(`src/services/agent/agent-generation-client.ts:24-79`).

---

## 1. `agent-generation:begin`

**1. Name and argument shape.** Inventory scope `project`. Wire arity 2. Request DTO
(`src/shared/agent-generation.ts:94`):

```
uiActionNonce: string                     // non-empty, trimmed, ≤256 chars
modelId: string                           // non-empty, trimmed
input: AgentGenerationInput
```

`AgentGenerationInput` (`src/shared/agent-generation.ts:24-32`), validated by
`validateAgentGenerationInput` (`electron/services/agent-generation-context.ts:5-32`): `mode:
'fast'|'planning'`, `uiLocale: Locale`, `historyMessages[] {role:'user'|'assistant', content}`
(≤32), `userMessage: string` (non-empty after trim), `editorContext?: string`, `tools:
AgentFrozenTool[]` (≤256, unique names matching `/^[a-zA-Z][a-zA-Z0-9_:-]{0,127}$/`, each with
`name/description/inputSchema/requiresConfirmation/isReadOnly/source:'builtin'|'mcp'|'skill'`),
total JSON ≤2 MiB, and the special rule `start_workflow ⇒ requiresConfirmation && !isReadOnly &&
source === 'builtin'` (`:30-31`). Request key allowlist is exact: only
`uiActionNonce|modelId|input` (`electron/services/agent-generation.ts:143`).

Call sites: `src/services/agent/agent-generation-client.ts:37-41`,
`src/stores/agent-store.ts:437-450`, Electron route
`electron/controllers/generation-controller.ts:244`.

**2. Response DTO.** `AgentGenerationRecovery` (§0.2). Frontend rejects it client-side if
`recovery.handle.projectId !== projectSession.projectId` or the `rootActionId` changed
(`src/services/agent/agent-generation-client.ts:30-35`).

**3. Which Go package implements it.** **None.** The nearest neighbour is the ordinary begin:
`cmd/novel-server/synthetic_generation_linux.go:815` (`(*syntheticGeneration).Begin`) →
`:1439` (`begin`) → `internal/store/generation_ledger.go:587` (`open`). The agent-specific
algorithm is `electron/services/agent-generation.ts:141-155`: derive
`key = textHash(JSON.stringify([projectId, uiActionNonce]))`, look for an existing session
(`sessionRuns`, `:65-72`), on hit compare `context.input` and `modelId` and return
`read(existing)` or throw `GENERATION_AGENT_NONCE_CONFLICT`; on miss call
`host.begin(selection(...))` with `roundIndex 0` and return `read(view.handle)`.

**4. Reachability.** **Not reachable.** Missing, in dependency order:
- **B1** (`generation_ledger.go:434`) — `open` refuses the `agentSession` manifest key.
- **B6** (`synthetic_generation_linux.go:403`) — `syntheticParseSelection` refuses
  `agentInput`/`agentSession`; `syntheticManifest` (`:704`) has no keys to carry
  `agentSession:{key,roundIndex}`, `agentContext`, `agentContextHash`.
- **B5** — `AgentGenerationContext` cannot be built: no `assistant_writing_identity` template, no
  `composePromptSystemRole`, no `buildAgentGenerationContext` port. The context hash is
  load-bearing: `read` re-verifies `textHash(JSON.stringify(context)) ===
  sourceManifest.agentContextHash` (`electron/services/agent-generation.ts:58-62`).
- Nonce idempotency requires a session-key index over `binding_json` (Node uses
  `json_extract(binding_json,'$.sourceManifest.agentSession.key')`,
  `electron/services/agent-generation.ts:66`). Go has no such query; the ledger exposes only
  `ListRuns` (`internal/store/generation_ledger.go:313`), which is per-project, not per-session.
- The reduced binding builder must emit the agent fingerprint; `syntheticFingerprint`
  (`:733-800`) computes nine hashes from `selection`/`set` and has no agent input.

**5. Binding style.** **Full durable generation lane.** Needs
`authorruntime.BeginGenerationLedgerDB` (admission + ledger + DB, one release), a new agent
manifest variant, and the reduced binding builder extended. A stateless store read cannot serve it:
`begin` creates a root (`generation_roots`) plus a run (`generation_runs`).

---

## 2. `agent-generation:read`

**1. Name and argument shape.** `project`, wire arity 2. Request (`src/shared/agent-generation.ts:95`):
`{ handle: MainGenerationRunHandle }` = `{projectId, epoch, rootActionId, runId}` (all four
non-empty strings; Go parser already exists at
`cmd/novel-server/synthetic_generation_linux.go:353` `syntheticParseHandle`, rejecting on any empty
field). Call site `src/services/agent/agent-generation-client.ts:43-45`; route
`electron/controllers/generation-controller.ts:245` passes `request.handle`, i.e. Electron unwraps
the object — the wire object is still `{handle}`.

**2. Response DTO.** `AgentGenerationRecovery` (§0.2). Normalised to the run's true handle:
Node returns `handle` of the **latest** run in the session while `rounds` covers all of them
(`electron/services/agent-generation.ts:135`).

**3. Which Go package implements it.** **None for agents.** The closest is
`cmd/novel-server/synthetic_generation_linux.go:1262` (`Read`) → `:899` (`syntheticRequireRun`,
which validates handle↔run↔project/epoch and needs no agent knowledge) → `:1594` (`viewOf`, the
`MainGenerationRunView` projection). The agent-specific algorithm is
`electron/services/agent-generation.ts:131-137` (`read`) over `:83-130` (`round`, 48 lines — the
single most intricate projection in the subsystem: attempt-count ambiguity check, completeness
triple (`agentResponse.version===1 && finishReason==='stop' && artifact.text===visibleText`), the
`start_workflow` planning-argument normalisation via
`normalizeAgentPlanningArguments` (`src/shared/agent-generation.ts:6-14`), and the synthetic
`propose_novel_config` → `propose_chapter_blueprint` follow-up action injection at `:120-129`).

**4. Reachability.** **Not reachable as an agent op, but it is the cheapest one to unblock.** It
needs: **B1** (no `agent-round` run can exist), **B3** (`receipt.result.usage.agentResponse` never
exists in Go), and the `round()` projection port. It does **not** need B2 (no dispatch) nor the
domain writers, and it reuses `syntheticRequireRun` + `viewOf` verbatim. `sourceStatus` needs
`sourcesCurrent` (`electron/services/agent-generation.ts:77-82`), which reads
`agentSourceTransition` — i.e. **B4** too, though a first cut can return `'current'`
unconditionally only if that deviation is labelled (it changes `resume`/`claim-tool` gating, so
labelling it as a shim is mandatory, not optional).

**5. Binding style.** **Full durable lane (read-only).** Still requires the coordinator admission
(`ActiveCoordinator.Begin`) because the projection reads `project_core` for the planning
normalisation at `electron/services/agent-generation.ts:102-103`. Not a bare `store` read: the
`sourceStatus` field is a live authority check, not a snapshot.

---

## 3. `agent-generation:resume`

**1. Name and argument shape.** `project`, wire arity 2. Request
(`src/shared/agent-generation.ts:96`): `{ handle: MainGenerationRunHandle }`. Call site
`src/services/agent/agent-generation-client.ts:47-51`; route
`electron/controllers/generation-controller.ts:252`.

**2. Response DTO.** `AgentGenerationRecovery`. Throws `GENERATION_SOURCE_CHANGED` first when
`recovery.sourceStatus !== 'current'` (`electron/services/agent-generation.ts:158`); the client
re-throws that before accepting (`src/services/agent/agent-generation-client.ts:33-34`).

**3. Which Go package implements it.** **None.** Nearest:
`cmd/novel-server/synthetic_generation_linux.go:1356` (`Resume`) →
`internal/generationowner/ordinary.go:190` (`OrdinaryService.Resume`) →
`internal/store/generation_ledger.go:1119-1192` (the ledger `resume` branch). Agent algorithm:
`electron/services/agent-generation.ts:156-173`, which **branches on round status**:

- last round `completed` → no new model attempt. Inside one `IMMEDIATE` transaction call
  `runs.activateRootForCommittedStage(rootActionId, scope)` — defined at
  `electron/repositories/generation-run-repository.ts:637-645`, which sets
  `root.epoch = project.epoch; root.status = 'active'` after refusing author-edited roots and any
  `reserved`/`dispatch-marked` attempt — then patch `usage.agentControlEpoch = scope.epoch` and
  return `read(handle)`.
- otherwise → `await host.resume(handle)` then `read(view.handle)`.

**4. Reachability.** **Not reachable.** Missing: **B1** (no agent run), **B2** (the
`host.resume` branch dispatches a model call through the agent path, so it needs the tool manifest),
**B3**, **B4** (`agentControlEpoch` write), and — a distinct gap — **there is no Go equivalent of
`activateRootForCommittedStage`.** The ledger's `resume`
(`internal/store/generation_ledger.go:1163-1188`) refuses unless `root.status === 'paused'` and
**replaces** `binding_json` from a supplied next binding; the agent path instead activates an
already-`active` or `paused` root, keeps the historical binding byte-identical, and does not
require `paused`. Reusing ledger `resume` here would either refuse or silently rebind — a
correctness bug, not a shim.

**5. Binding style.** **Full durable generation lane**, transactional: the root-activation and the
`agentControlEpoch` write must commit together (Node wraps them in
`this.db.transaction(...).immediate()`).

---

## 4. `agent-generation:round`

**1. Name and argument shape.** `project`, wire arity 2. Request
(`src/shared/agent-generation.ts:97`): `{ handle: MainGenerationRunHandle; index: number }`.
`index` must be a safe integer `0 ≤ index < 8` (`electron/services/agent-generation.ts:183`), else
`GENERATION_AGENT_ROUND_INVALID`. Call site
`src/services/agent/agent-generation-client.ts:53-66` (which passes only `{handle, index}` — the
test at `src/services/agent/__tests__/agent-hosted-runtime.test.ts:67` pins the exact key set
`['handle','index']`). Route `electron/controllers/generation-controller.ts:253`.

**2. Response DTO.** `AgentGenerationRecovery`. The client then re-finds the round by `index` and
throws `GENERATION_AGENT_ROUND_MISSING` if absent
(`src/services/agent/agent-generation-client.ts:63-65`), so the projection must be complete on
return.

**3. Which Go package implements it.** **None.** Nearest:
`cmd/novel-server/synthetic_generation_linux.go:1015` (`Execute`) →
`internal/generationowner/ordinary.go:114` (`OrdinaryService.Execute`) →
`internal/generation/service.go:287` (`Service.Execute`). Agent algorithm:
`electron/services/agent-generation.ts:180-207`. The message assembly is the load-bearing part
(`:201-205`): start from `context.initialMessages`, then for every earlier round append
`{role:'assistant', content: round.protocolText}` followed by
`{role:'user', content: '[Application tool results; untrusted context]\n' + <tool_result
name=… status=…>observation</tool_result> block}`. Then
`host.execute(handle, 'agent-round:'+index, {purpose:'agent', output:'visible-text',
reasoningStage:'general', messages})`. Opening a fresh round goes through
`host.begin(selection(context.input, key, index, modelId, selected.rootActionId))` — a **child run
under the same root**, which is why `syntheticManifest`'s `parentRootActionId` support
(`cmd/novel-server/synthetic_generation_linux.go:719-721`, ledger `open` at
`internal/store/generation_ledger.go:661-673`) is already the right primitive.

**4. Reachability.** **Not reachable, and it is the hardest of the eight.** Missing:
- **B1**, **B2**, **B6** as above.
- **B3 in full**: Go must (a) build an agent provider request (`AgentToolNames` populated instead
  of rejected), (b) capture the raw protocol stream separately from the visible stream during
  streaming, (c) parse XML / DSML / raw-JSON `<tool_call>` at `finishReason === 'stop'`, (d) enforce
  the 64-tool-call cap (`GENERATION_AGENT_ACTION_LIMIT`,
  `electron/services/generation-run-service.ts:265`), (e) emit the 8 MiB protocol ceiling
  (`:249`), and (f) settle `usage.agentResponse`. Note the visible-text discipline: protocol text
  must **not** reach the artifact stream before its role is known (`:247-252`).
- A purpose mismatch to resolve: Node dispatches with `purpose: 'agent'`
  (`electron/services/agent-generation.ts:206`), while Go's `syntheticPurpose` returns the
  operation string, i.e. `'agent-round'` (`cmd/novel-server/synthetic_generation_linux.go:550-555`).
  The two must agree with the browser's `budgetDiagnostics` mapping or `throwIfLastAttemptTerminal`
  diverges.

**5. Binding style.** **Full durable generation lane, model dispatch.** This is the only op of the
eight that creates a *new* attempt per call; it needs the whole S1 dispatch path plus the ledger
`reserve`/`markDispatched`/`settle` cycle.

---

## 5. `agent-generation:claim-tool`

**1. Name and argument shape.** `project`, wire arity 2. Request
(`src/shared/agent-generation.ts:98`):

```
ref: AgentToolActionRef                    // {handle, attemptId, toolCallId}
confirmed: boolean
authorBlueprintProposals?: AgentAuthorBlueprintProposal[]   // {name:'propose_chapter_blueprint', arguments}
```

`authorBlueprintProposals` is optional and omitted entirely when empty
(`src/services/agent/agent-generation-client.ts:85-88`). Constraints: `typeof confirmed ===
'boolean'` else `GENERATION_AGENT_CONFIRMATION_REQUIRED`
(`electron/services/agent-generation.ts:231`); selections ≤100 entries, each exactly
`{name:'propose_chapter_blueprint', arguments:<object, not array>}`, total ≤256 KiB, and only
legal when the frozen context has a `propose_chapter_blueprint` tool with
`source:'builtin' && requiresConfirmation && !isReadOnly` (`:233-238`), else
`GENERATION_AGENT_AUTHOR_SELECTION_INVALID`; a changed prior selection or a selection arriving on a
non-`pending` action is `GENERATION_AGENT_AUTHOR_SELECTION_CONFLICT` (`:242`).

**2. Response DTO.** `AgentToolClaim` (`src/shared/agent-generation.ts:53`):
`{ action: AgentToolAction, execute: boolean }`. `execute` is `true` **only** when the caller passed
`confirmed: true` and the action actually transitioned from `pending`
(`electron/services/agent-generation.ts:253`); the engine uses it to decide whether to run the tool
(`src/services/agent/agent-engine.ts:434-461`).

**3. Which Go package implements it.** **None.** Algorithm:
`electron/services/agent-generation.ts:230-258` on top of `updateAction` (`:215-229`) and
`action` (`:209-214`). Gates: root epoch must equal the session epoch, `sourcesCurrent(run)`, and
`root.status === 'active'`, else `GENERATION_AGENT_TOOL_STALE` (`:248`). An unknown tool name
resolves to `{status:'failed', observation: '未知工具：<name>' | 'Unknown tool: <name>'}` rather than
an error (`:249-251`) — this localisation branch is part of the contract.

**4. Reachability.** **Not reachable.** Missing: **B1**, **B4** (both `agentActions` and
`agentAuthorSelections` writes — no Go primitive, `generation_ledger.go:267-268`), B3 (the action
list it mutates must exist), plus the `sourcesCurrent` transition projection. Note this op does
**not** need dispatch (B2) nor the domain writers, so it is the cheapest *mutating* op — but it is
strictly downstream of `round`.

**5. Binding style.** **Full durable lane, write, transactional.** Needs coordinator admission to
read `generation_roots`/`generation_runs`/`generation_attempts` under a single permit and to read
`project_core` (for `sourcesCurrent`). Not a stateless write.

---

## 6. `agent-generation:finish-tool`

**1. Name and argument shape.** `project`, wire arity 2. Request
(`src/shared/agent-generation.ts:99`): `{ ref: AgentToolActionRef; status:
'completed'|'failed'|'unknown'; observation: string }`. `observation` is UTF-8 and must be
≤256 KiB, else `GENERATION_AGENT_TOOL_RESULT_INVALID`
(`electron/services/agent-generation.ts:260-261`). Call sites
`src/services/agent/agent-engine.ts:451, 461, 477` and
`src/services/agent/agent-generation-client.ts:90-92`.

**2. Response DTO.** `AgentToolAction` (`src/shared/agent-generation.ts:44-50`) — the decided
action including `workflow` when one was registered.

**3. Which Go package implements it.** **None.** Algorithm:
`electron/services/agent-generation.ts:259-275`. Gates: root epoch must match, else
`GENERATION_EPOCH_STALE` (`:263`); requesting `completed` for `propose_novel_config` or
`propose_chapter_blueprint` **without** a stored `domainEffect` is
`GENERATION_AGENT_DOMAIN_EFFECT_REQUIRED` (`:266-267`) — i.e. `finish-tool` is coupled to
§8's receipt; a non-`running` action is idempotent only for the exact same `(status, observation)`
pair, else `GENERATION_AGENT_TOOL_RESULT_CONFLICT` (`:269-272`).

**4. Reachability.** **Not reachable.** Missing: **B1**, **B4**, **B3**. Additionally the
`domainEffect` cross-check pulls in §8's writer, so `finish-tool` cannot be completed before
`commit-domain-tool` exists even for non-domain tools (the code path is shared).

**5. Binding style.** **Full durable lane, write, transactional** (`updateAction` uses
`this.db.transaction(...).immediate()`, `electron/services/agent-generation.ts:216`).

---

## 7. `agent-generation:register-workflow`

**1. Name and argument shape.** `project`, wire arity 2. Request
(`src/shared/agent-generation.ts:100`): `{ ref: AgentToolActionRef }`. Call site
`src/services/workflows/creative-workflow-launcher.ts:190`; route
`electron/controllers/generation-controller.ts:256`.

**2. Response DTO.** `AgentWorkflowRegistration` (`src/shared/agent-generation.ts:64-72`):
`registrationId`, `workflow: 'generate_draft'|'generate_blueprint'|'generate_architecture'`,
`planning?: {from, to, targetUnits}`, `chapterNumber?`, `parentHandle`, `modelId`,
`state: 'registered'|'started'`, `childHandles: MainGenerationRunHandle[]`.
`registrationId` format is `agent-workflow:<64-hex>` (`textHash(ref.toolCallId)`,
`electron/services/agent-generation.ts:287`); the lookup validates against
`/^agent-workflow:[a-f0-9]{64}$/u` (`:294-295`).

**3. Which Go package implements it.** **None.** Algorithm:
`electron/services/agent-generation.ts:276-292`. Gates, all inside `GENERATION_AGENT_WORKFLOW_NOT_AUTHORIZED`:
action must be `running`, name must be `start_workflow`, root epoch must match, root `active`,
`sourcesCurrent(run)` (`:279-281`). Argument allowlist is exact:
`workflow|chapter_number|start_chapter|chapter_count|target_units` (`:284-285`);
`generate_draft` requires a safe-integer `chapter_number ≥ 1`, planning workflows require
`start_chapter`/`chapter_count`/`target_units`; else `GENERATION_AGENT_WORKFLOW_INVALID`. Idempotent:
if `action.workflow` already exists it is returned unchanged (`:278`).

**4. Reachability.** **Not reachable.** Missing: **B1**, **B4** (the `workflow` sub-object is stored
in `agentActions[...]`), **B3**, and the planning-argument normaliser
`normalizeAgentPlanningArguments` (`src/shared/agent-generation.ts:6-14`, which reads
`project_core.total_chapters` and validates the target range — the Go projection reads
`store.ProjectCore.Get`, so the dependency exists, only the port does not).

**Cross-family coupling (flag, not a blocker for this op):** the two consumers of a registration are
`admitWorkflow` (`:305-345`, called from `generation:begin` at
`electron/services/main-generation-owner.ts:427`) and `recordWorkflowChild` (`:369-375`, called from
`electron/services/main-generation-owner.ts:528`). Both are `generation:begin`-family code paths, not
`agent-generation` ops. A Go `register-workflow` therefore produces a registration that **nothing in
Go consumes yet**: `generation:begin` in Go has no `agentWorkflowRegistrationId` handling
(`syntheticSelection` allowlist, `cmd/novel-server/synthetic_generation_linux.go:397-401`). This
must be recorded as a partial-migration state, not shipped as parity.

**5. Binding style.** **Full durable lane, write.** Needs the whole session's runs (`sessionRuns`
scan over `agentSession.key`) to resolve the parent handle — so coordinator admission plus a
session-keyed ledger query that does not exist (§1, point 4).

---

## 8. `agent-generation:commit-domain-tool`

**1. Name and argument shape.** `project`, wire arity 2. Request
(`src/shared/agent-generation.ts:101`): `{ ref: AgentToolActionRef }`. Call sites
`src/services/agent/tools/propose-novel-config.tool.ts:107` and
`src/services/agent/tools/propose-chapter-blueprint.tool.ts:108`; route
`electron/controllers/generation-controller.ts:257`.

**2. Response DTO.** `AgentDomainToolReceipt` (`src/shared/agent-generation.ts:54-62`):
`kind: 'config'|'blueprint'`, `toolCallId`, `changes: Record<string,unknown>`,
`chapterNumber?`, `beforeHash`, `afterHash`, `current?: boolean`.

**3. Which Go package implements it.** **None as an agent op; the domain primitives exist.**
Node algorithm: `electron/services/agent-generation.ts:377-401` (authorisation + transaction) and
`electron/services/agent-domain-tool-effect.ts:13-49` (`commitAgentDomainTool`) /
`:9-12` (`isAgentDomainToolCurrent`). The Go primitives already present:
`internal/store/projectcore.go:156` (`ProjectCore.Update`, allowlisted fields, rejects
`charactersArch`), `:57` (`Get`), `internal/store/blueprints.go:119` (`Upsert`), `:82` (`Get`),
`:69` (`All`), and `syntheticHash`/`syntheticCanonical`
(`cmd/novel-server/synthetic_generation_linux.go:206`, `:211`) for `textHash`.

Write rules to port verbatim (`agent-domain-tool-effect.ts:14-23`): config field allowlists are
`genre, subGenre, targetAudience, coreOutline, worldSetting, goldenFinger, protagonistProfile,
globalGuidance, writingStyle, referenceWorks` (strings), `totalChapters, wordsPerChapter`
(safe int ≥1), enums `plotStructure` (6 values), `narrativePOV` (4), `writingLanguage`
(`zh-CN|en-US` via `简体中文`/`English`); `narrativePov` is normalised to `narrativePOV` on input
and back on write. Blueprint fields: `title, role, purpose, keyEvents, suspenseHook, userGuidance,
notes` (strings), `characters` (array of strings), with `作者微操指导`/`用户指引` → `userGuidance`
(`:20`). Everything else is `GENERATION_AGENT_DOMAIN_TOOL_INVALID`. Every failure is that one code.

**4. Reachability.** **Not reachable.** Missing: **B1**, **B4** (writes `agentActions[...].domainEffect`
and/or `agentSourceTransition`), **B3**, and the authorisation gate `action.status === 'running' &&
root.epoch === epoch && root.status === 'active' && sourcesCurrent(run) && the frozen tool manifest
contains `action.name` as `builtin && requiresConfirmation && !isReadOnly`
(`electron/services/agent-generation.ts:386-389`). Also a **projection-equality risk** (not a
missing dependency, flagged as speculation): `isAgentDomainToolCurrent` and `beforeHash`/`afterHash`
compare `textHash(JSON.stringify(row))` of the Node repository's projection
(`electron/services/agent-domain-tool-effect.ts:9-12`, `:35-36`, `:47-48`). Go's
`ProjectCore.Get`/`Blueprints.Get` are separate projections (`internal/store/projectcore.go:57`,
`internal/store/blueprints.go:82`); unless they are byte-identical in key order and value
normalisation to the Node ones, `current` will report `false` forever against a Node-authored row
and `true`-vs-`false` drift will look like data corruption. **This must be verified by a
differential test before the op is claimed**, exactly as the S1 doc flags for
`MainGenerationRunView` (`web/GO-GENERATION-CHAIN-SPEC.md:149`).

**5. Binding style.** **Full durable generation lane, write, transactional, plus one ordinary
domain write.** Node wraps the whole thing in `this.db.transaction(...).immediate()`
(`electron/services/agent-generation.ts:378`) so the domain mutation and the attempt-usage patch
commit together; the Go binder must reuse `internal/authorruntime/generation_db_linux.go:25`
(which hands back the **same** `*sql.DB` the ledger writes) and must issue a single transaction, or
`finish-tool`'s `GENERATION_AGENT_DOMAIN_EFFECT_REQUIRED` gate becomes permanently unsatisfiable.

---

## Recommended implementation order

All eight share **one** binder, by direct analogy with `internal/transport/generation.go:18-103`:

```go
// new: backend-go/internal/transport/agent_generation.go
type AgentGenerationProvider struct {
    Begin, Read, Resume, Round, ClaimTool, FinishTool, RegisterWorkflow, CommitDomainTool func(...)
}
func (s *Server) BindAgentGeneration(provider *AgentGenerationProvider) error
```

Nil field ⇒ that op stays unbound ⇒ automatic `501 GO_OPERATION_NOT_MIGRATED`
(`internal/transport/server.go:100`). Same envelope rules as `BindGeneration`: arity = request + 1,
3-key `{projectId, leaseId, projectPath}` session map, read ops raise `security.HTTPError`, mutating
ops return the business DTO via `draftSaveFailure` (`internal/transport/generation.go:150-164`).
Wire it next to `server.BindGeneration(binder)` at
`cmd/novel-server/synthetic_durable_open_linux.go:471`.

Batching is driven by shared prerequisites, not by op family. **Every batch must also edit
`cmd/novel-server/synthetic_generation_http_linux_test.go`:411-436** — remove its ops from the
`unmigrated` slice, decrement the `want 32` count at `:428-430` (it drops to 32−2, −2, −2, −2 = 24), and
add a positive assertion for the newly bound envelope. Forgetting this is a guaranteed red CI run.

### Batch S3-1 — `begin`, `read` (foundation; everything else is downstream)

One binder field group (`Begin`, `Read`), one HTTP test
(`cmd/novel-server/synthetic_agent_generation_http_linux_test.go`).

Prerequisites, in order:
1. Widen `glOrdinaryBinding` for `agentSession`/`agentContext`/`agentContextHash`
   (`internal/store/generation_ledger.go:434-438`) — **reviewer sign-off required**
   (`web/GO-GENERATION-CHAIN-SPEC.md:151`).
2. Extend `syntheticSelection`/`syntheticManifest` with the agent keys
   (`cmd/novel-server/synthetic_generation_linux.go:397-406`, `:704-726`).
3. Port `validateAgentGenerationInput` + `buildAgentGenerationContext`
   (`electron/services/agent-generation-context.ts:5-57`) and source the
   `assistant_writing_identity` template (B5).
4. Port `round()` (`electron/services/agent-generation.ts:83-130`) — needed by `read`, and `begin`
   calls `read` at the end, so both land together.
5. Add a session-keyed ledger lookup (`agentSession.key` → ordered runs).

Test: begin → read → begin with the same `uiActionNonce` returns the same handle (nonce
idempotency), mismatched `input`/`modelId` → `GENERATION_AGENT_NONCE_CONFLICT`; source DB bytes
unchanged; `generation_runs` row carries `operation='agent-round'` and the agent manifest keys.

### Batch S3-2 — `round`, `resume` (dispatch and control)

One binder field group (`Round`, `Resume`), one test.

Prerequisites:
1. Un-gate `AgentToolNames` (`internal/generation/service.go:259-263`, `:289-291`) — **the second
   reviewer-sign-off item**.
2. Port the protocol parser (B3): `src/shared/agent-response-protocol.ts:136`
   (`parseAgentResponseProtocol`), `:146` (`cleanAgentProtocolVisibleText`), `:154`
   (`projectInterruptedAgentVisibleText`), plus the streaming discipline at
   `electron/services/generation-run-service.ts:240-270` and the 64-call / 8 MiB caps.
3. New ledger/root primitive equivalent to `activateRootForCommittedStage`
   (`electron/repositories/generation-run-repository.ts:637-645`) for the completed-round `resume`
   path — ledger `resume` (`internal/store/generation_ledger.go:1163-1188`) is **not** it.
4. Resolve the `purpose` mismatch: `'agent'` (`electron/services/agent-generation.ts:206`) vs
   `'agent-round'` (`cmd/novel-server/synthetic_generation_linux.go:550-555`).

Test: begin → round(0) with a fake dispatch emitting one `<tool_call>` → assert
`usage.agentResponse` round-trips, `protocolText` equals visible + `<tool_call>` lines, and
`nextRound === 1`; round with an out-of-order index → `GENERATION_AGENT_ROUND_ORDER`; resume on a
completed round with no new attempt row; resume with a stale source → `GENERATION_SOURCE_CHANGED`.

### Batch S3-3 — `claim-tool`, `finish-tool` (action state machine, read-write half)

One binder field group, one test. These two share the entire `updateAction` primitive
(`electron/services/agent-generation.ts:215-229`) and the exact same authorisation gates, so they
must never be split.

Prerequisite: a **usage-receipt patch primitive** (B4) — a ledger method that writes named
top-level keys of `usage_receipt_json` (`agentActions`, `agentAuthorSelections`,
`agentSourceTransition`, `agentControlEpoch`) under the ledger's transactional connection, matching
`electron/services/agent-generation.ts:225-228`, `:234-241`, `:280-284`, `:394-397`, `:35-45`. Add
it to the `write` allowlist at `internal/store/generation_ledger.go:268` and to `call` at `:799`.

Test: pending → claim(confirmed=false) → `{execute:false, status:'declined'}` → claim(confirmed=true)
→ `{execute:true, status:'running'}` → finish(completed) for a non-domain tool; finish(completed)
for `propose_novel_config` without an effect → `GENERATION_AGENT_DOMAIN_EFFECT_REQUIRED`; replay of
the identical finish is idempotent, a differing one is `GENERATION_AGENT_TOOL_RESULT_CONFLICT`;
stale epoch → `GENERATION_AGENT_TOOL_STALE` / `GENERATION_EPOCH_STALE`.

### Batch S3-4 — `register-workflow`, `commit-domain-tool` (effects half)

One binder field group, one test. Both require the same transaction discipline: a domain/registration
mutation plus a usage patch under `.immediate()`, plus the `sourcesCurrent` projection and the
`agentSourceTransition` before/after capture loop
(`electron/services/agent-generation.ts:340-349`, `:390-399`).

Prerequisites: everything in S3-3, plus (a) the domain-effect writer ported onto
`internal/store/projectcore.go:156` / `internal/store/blueprints.go:119` with a **byte-identical**
projection hash to `textHash(JSON.stringify(row))` (the risk flagged in §8 point 4), and (b) a
decision recorded that `generation:begin` in Go still ignores `agentWorkflowRegistrationId`, so a
registration written here has no Go consumer (`§7`, cross-family coupling).

Test: running `start_workflow` → register returns `agent-workflow:<64-hex>` and is idempotent on
repeat; wrong action status → `GENERATION_AGENT_WORKFLOW_NOT_AUTHORIZED`; commit a config change →
`{kind:'config', beforeHash, afterHash, current:true}` and a second call returns the stored receipt
with a recomputed `current`; commit a blueprint change → `{kind:'blueprint', chapterNumber,
…}`; non-allowlisted change key → `GENERATION_AGENT_DOMAIN_TOOL_INVALID`; assert the effect and the
attempt usage row commit in the same transaction (a forced rollback leaves neither).

### Why not fewer batches

S3-1 must ship before anything else (it is the only batch that can create an agent run). S3-2 is
gated by two independent reviewer decisions (dispatch un-gating and the root-activation primitive)
and by the largest single port in the family (the protocol parser) — merging it into S3-1 would make
the first review an all-or-nothing gate. S3-3 and S3-4 could technically be one batch (one binder,
one file), but S3-4 additionally needs the domain-projection byte-equality proof, which is a
separate acceptance risk; keeping it separate means S3-3 can land even if that proof fails.

**Total: 4 batches, 1 binder, 4 test files. Minimum viable first shipping unit: S3-1 (`begin` +
`read`), which alone moves `agent-generation` from `8 missing` to `6 missing` on
`web/GO-GAP-MATRIX.md:92`.**
