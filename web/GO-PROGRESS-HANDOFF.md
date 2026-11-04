# Go migration — progress checkpoint and handoff

Written at a deliberate stopping point (the user asked to wrap up this phase, push
everything, and resume later). Everything below is committed and pushed; `main` is
CI-green. Nothing here claims the migration is complete.

## START HERE (next session, cold start)

State: durable lane serves **64 of 281** operations over real HTTP, `main` is CI-green at
`9592bad3`, working tree clean, production container untouched.

The single blocking prerequisite for the next batch is the **binding rebuild**: rebuild a run's
frozen source binding from current project state and compare it with the stored one. Everything
else in the graph and finalization families waits behind it.

1. Reconstruct `syntheticSelection` and the model receipt from the frozen manifest (`modelId`,
   `promptKeys`, `skillStages`, `authorInputs`, `selectedDraftIds`,
   `selectedFinalizedBlueprintChapters`, `chapterNumber`, `budget`, `output`).
2. Rebuild the nine reduced hashes with `syntheticFingerprint` (it already derives them from real
   project state; do not write a second derivation).
3. Compare with the frozen `binding.fingerprint`; any difference means the sources moved.
4. Wire it into `syntheticGraphSourceStatus` so `plot` and `plan` stop reporting `conflict`, and
   into the finalization family.
5. Only then bind `graph-generation:begin` / `execute` / `confirm` and the finalization chain.

**Correction (this paragraph was wrong when first written).** `targetBaselineHash` is not a hash of
the plot source bundle. It is `textHash` (sha256 of the UTF-8 text) of the **raw
`project_core.plot_tree_snapshot` column**, hashed verbatim - invalid JSON included
(`electron/services/graph-generation-source.ts:44`, consumed at
`electron/services/graph-generation.ts:114-115`, and pinned by
`electron/services/__tests__/graph-generation-source.test.ts:51-56`, whose fixture stores
`'旧无效快照'` and still expects `textHash` of it). `store.PlotTree.Read` returns the *parsed*
snapshot and `sourceRevision` (`internal/store/plottree.go:53-69`) - a different encoder over a
different input, so comparing either against the baseline is false with certainty.

Do **not** wire the plot baseline into `sourceStatus` as a freshness proof. The baseline covers only
`plot_tree_snapshot`; a run whose synopsis, blueprints, finalized-chapter summaries or narrative
threads changed after the freeze would then read `current`, which is more permissive than the value
Node reports. That is the binding rebuild again, not a shortcut. The baseline belongs to `confirm`'s
CAS, and is only meaningful once `confirm` is bound.

Two rules that were learned the hard way and must not be relaxed:

- **A bound operation needs a positive assertion through the durable admission path.** A "not 501"
  assertion cannot distinguish a working handler from one that reads the wrong argument shape; two
  such defects shipped green (`{ handle }` envelopes, and op-body errors escaping the DTO envelope).
- **Fail closed on anything that gates confirmation.** `syntheticGraphFreshness` returns `conflict`
  unless sameness is positively proven, and its seven-case test pins that direction.

## S1 generation lane: RESOLVED (2026-11-04)

The eleven migrated `generation:*` operations are now bound in the durable lane and the
fake-dispatch HTTP contract test (`TestSyntheticGenerationHTTPDurableLane`) passes in CI
(`6f1bc572`, run `41782708522`, all three jobs green). Three real defects were fixed on the way:

1. `store.GenerationLedger.ListRuns` set `committed = true` and returned **without issuing
   COMMIT**, so the pooled connection kept an open read transaction and the next operation on
   that database failed with `cannot start a transaction within a transaction`. This bit a
   second run in the same session and would have bitten production identically.
2. `syntheticSlice` only accepted `[]any`, while the ledger returns budget attempts as
   `[]map[string]any`, so every projection (diagnostics, candidates, artifacts) was silently
   empty even though execute settled correctly.
3. `stopRun`/`Resume` projected `result["run"]`, but `generation.Service.Pause/Cancel` returns
   the post-stop **budget** map, not the run; both now re-read the run from the ledger.

Remaining generation-family work: 12 `generation:*` (batch/composition/preparation),
8 `agent-generation:*`, 5 `graph-generation:*`, 5 `finalization-generation:*`.

## Where `main` stands

- `HEAD = 059c57a2` == `origin/main`, clean tree.
- GitHub Actions run `41758583171` (workflow *Go backend migration*): all three jobs
  **success** (parallel compile+static checks, format/vet/race/synthetic tests/binary,
  unpublished container smoke).
- Scope coverage unchanged by this checkpoint: **281 = 110 bound / 171 unbound**,
  of which the **durable lane serves exactly 31 verified operations** and Tier C/D
  operations keep returning `GO_OPERATION_NOT_MIGRATED` / `GO_ROUTE_NOT_MIGRATED`.
- Nothing about production changed: the live container, image digest, author volume
  and nginx front end are untouched; no traffic was switched.

## Verified and landed (safe to build on)

| Area | Evidence |
|---|---|
| Durable author write lane: 7 project-lifecycle + 20 editor-read + 3 character-read + 1 draft-CAS = **31 ops over real HTTP** against a synthetic v7 project | `af31c463`, `cf076681`; `cmd/novel-server/synthetic_durable_http_linux_test.go` |
| Durable Linux primitives: `AuthorPermit`/`AuthorDatabase` (`projectfs`), `AdmitAuthor`/`AdmittedAuthor.Verify|Refresh` (`projectaccess`), `admittedAuthor` + `SaveCore` + `CommitDraftCASDurable` + `ReadCharactersJSONDurable` (`authorruntime`) | `4066f782`, `83d616a5`, `3b259bd3`, `c098456a` |
| Lease rotation after a durable CAS happens in place on the same `*admittedAuthor`, so a following `project:save` still works | `cf076681` |
| Generation slice-A plumbing: `transport.BindGeneration` (registers exactly 11 S1 names), `BeginGenerationLedger`/`BeginGenerationLedgerDB`, the `CredentialStorage`+`PlaintextCredential` seam, `OrdinaryService` control accessors, the ledger list query | `f3d2de9d`, `3354b8f9` |
| S1 durable generation provider draft (`cmd/novel-server/synthetic_generation_linux.go`, 1855 lines) plus a durable refresh hook | `232fa52f`, `059c57a2` — **compiled but not wired, and not yet correct** |

## Quarantined on purpose (this is the main open item)

The eleven S1 generation operations were briefly wired into the durable lane and
then deliberately unbound again in `059c57a2`, because they are **not yet correct**.
Fixing them by guessing would have meant claiming a green slice that lies, so the
wiring and its HTTP test were removed and the lane went back to the 31 verified
operations.

Two concrete defects found on the way, in order:

1. **`AUTHOR_WRITE_STALE` on the second operation — fixed.** The generation ledger
   writes the author database through the raw `*sql.DB`, so the permit's recorded
   content identity stopped matching and every following operation was refused.
   `BeginGenerationLedgerDB` now also returns a refresh hook (`permit.Refresh`
   under the still-held legacy lock, then `handle.Verify`, mirroring `SaveCore`)
   and the provider composes it into the single per-operation release. This fix is
   landed and is correct on its own.
2. **Empty `budgetDiagnostics` after `generation:execute` — open.** With the
   refresh hook in place, `begin` + `read` succeed and `execute` returns the
   expected `outcome` (`status:"incomplete"`, `finishReason:"length"`, artifact
   text = the streamed text), but the run view returned alongside it reports
   **zero attempts**: `viewOf` builds `budgetDiagnostics` from
   `GenerationLedger.budget(rootActionId).attempts`, which in turn is
   `SELECT attempt_json FROM generation_attempts WHERE root_action_id=?`
   (`internal/store/generation_ledger.go:375-407`), and that query comes back
   empty even though the execute path clearly produced a settled artifact.
   Something between the reservation insert
   (`internal/store/generation_ledger.go:899`, which keys the row by
   `run["rootActionId"]`) and that read does not line up — either the insert is
   rolled back, lands under a different root id, or the run document handed to
   `viewOf` is not the ledger run document. The failing assertion was
   `synthetic_generation_http_linux_test.go:236`, and it panicked rather than
   failing cleanly, so the regression test itself also needs the unchecked
   index fixed.

## Exact resume plan

1. Re-add the wiring: in `migrationHandlerWithDurable`
   (`cmd/novel-server/synthetic_durable_open_linux.go`) bind
   `transport.GenerationProvider` with all eleven fields populated from
   `newSyntheticGeneration(durable, server, config)`, gated on the reserved
   `syntheticDurableConfig.GlobalRoot`. The refresh hook return value of
   `BeginGenerationLedgerDB` is `(ledger, db, refresh, release, err)`.
2. Recover the HTTP contract test from git: `git show 232fa52f:backend-go/cmd/novel-server/synthetic_generation_http_linux_test.go`.
   Three defects already diagnosed and fixed in that version, keep them: the
   `project:open` envelope must be read through `{"result": …}`, and
   `assertStringField` must walk dotted paths.
3. Diagnose the missing attempts first — add a temporary assertion or a direct
   read of `generation_attempts` right after `execute` in the test, and check
   whether `receipt["run"]` is the ledger run document (it needs
   `rootActionId`, not a nested `handle.rootActionId`).
4. Then continue the plan already written down: **S2** batch/composition/
   preparation stores, **S3** the eight `agent-generation:*` ops ‖ **S4** the
   twelve graph/finalization/import generation ops (`web/GO-GENERATION-CHAIN-SPEC.md`).
5. After the generation family: import, archive/cloud backup/restore,
   WebDAV/prompts/Skills/MCP, then the full 281-op/browser/data/credential
   acceptance and only then the digest-pinned image swap on the user's host.

## Rules that must not be broken (they cost real debugging time)

- **Never** run Go build/test/vet/gofmt, cargo, npm install, docker build or a
  frontend build on this machine; CI is the only build/test oracle. Locally:
  source edits, `git diff --check`, `node --check`, git/`gh`.
- Push with `git -c http.proxy=http://192.168.1.15:7890 -c http.version=HTTP/1.1 push origin main`
  and always pass `--repo qingwo1991-debug/ai-novel-writer-web` to `gh`.
- CI reformats rather than gating on gofmt, but `go vet` and the test job are real
  gates; one failing test costs ~9 minutes of round trip, so keep cuts small.
- The durable lane must not rebind `project:open`/`db:close`
  (`OPERATION_ALREADY_BOUND` is fatal and then nothing registers), and only one
  durable runtime may exist per process (the legacy lock is process-exclusive).
- Do not touch `preflight.go`'s `func preflight`, and do not modify the M03 tests.
- Production stays untouched until full acceptance: image
  `ghcr.io/qingwo1991-debug/ai-novel-writer-web@sha256:84801a8a9016f115899c5be419cf2a29a1d112f82be3312fdc12609b0e95cf37`,
  author volume `/root/AI/ai-novel-writer-data`, compose
  `--env-file .env -f docker/compose.web.yaml` (never read `.env`), pre-migration
  backup at `/root/AI/ai-novel-writer-backups/pre-native-20261101T235846Z/`.

## Next concrete increment (ready to wire, no new runtime needed)

`internal/transport/recovery.go` already contains `Server.BindRecovery` with the exact four
inventoried candidate operations (`db:recovery-candidate-record` arity 3/path 1, `-list` 2/0,
`-update` 4/2, `-resolve` 4/2) plus `internal/transport/recovery_test.go` covering the envelopes
and the no-other-authority rule. `internal/store/recovery.go` already implements
`Record` / `ListPending` / `UpdatePending` / `Resolve`. The only missing piece is a
`RecoveryProvider` implementation in the durable lane:

```go
func (s *syntheticDurableOpen) BeginRecovery(ctx, caller, raw any) (*store.Recovery, func(), error)
```

It must follow the exact shape of `syntheticDurableOpen.ReadCharactersJSON` (lines ~255-274):
parse the frozen context, take `s.mu`, delegate to a new
`authorruntime` durable helper that admits the author project and hands back `store.NewRecovery(db)`
with a release that keeps the admitted project stable for the whole repository call — mirroring
`ReadCharactersJSONDurable` / `CommitDraftCASDurable`. Then add the `server.BindRecovery(...)`
line next to `BindGeneration` in `synthetic_durable_open_linux.go` and count 46 durable operations.

## Gate decisions that block the remaining generation families (2026-11-04)

Two maps were produced by parallel analysis and are committed:
`web/GO-S3-AGENT-GENERATION-MAP.md` (8 operations) and
`web/GO-S4-GRAPH-FINALIZATION-MAP.md` (10 operations). Both families are blocked at the
ledger, not at the transport, and the blocking keys are **deliberate** quarantines that the
original chain spec marked as needing reviewer sign-off (`web/GO-GENERATION-CHAIN-SPEC.md:151`).

| Gate | Location | Blocks | Minimal safe widening |
|---|---|---|---|
| `sourceManifest.agentSession` | `store/generation_ledger.go:434-438` | all 8 `agent-generation:*` | accept only when `operation == "agent-round"` and the companion hash matches |
| `sourceManifest.graphGenerationContext` / `finalizationGenerationSlotKey` | same | all 10 `graph-generation:*` + `finalization-generation:*` (checked in `mutableRoot` too, so even `cancel` fails) | accept only for the five documented operation strings with a hash-matched companion key |
| `usage_receipt_json` key allowlist | `:495-501` | `graph-generation:confirm`, `finalization-generation:commit` (+ the four agent tool ops) | add `graphEffects` / `finalizationEffect` behind the same operation predicate |
| `AgentToolNames != nil` | `generation/service.go:259-263`, `:289-291` | `agent-generation:round`, `:resume` | accept a non-nil slice only for the agent purpose |

Beyond the gates, both families need real new work rather than wiring:
`GenerationLedger.RecordEffect` (one write method shared by six operations across two families),
a slot-keyed run lookup for finalization, agent-protocol parsing, and the graph/finalization
task builders and result parsers, none of which exist in Go.

**No traffic is switched and the production container is untouched, so an unwidened gate is
not a regression: it is the current, tested 501 behaviour.**

### 2026-11-04 — the user authorised the narrow widening

The gate owner chose "授权：按窄谓词放开" (widen narrowly, per operation). Implemented and green
in `d3e3ed5e`:

- `store.generation_ledger.go`: `glFamilyManifestAllowed(key, operation)` replaces the blanket
  refusal. A family key is readable **only** for the operation that owns it —
  `agentSession`/`agent-round`; `graphGenerationContext`/the four graph operation strings;
  `finalizationGenerationSlotKey`/`finalized-character-state`. `importSlot` and
  `legacyRosterContext` stay closed for every operation, and `$runEffects` /
  `$blueprintRetirement` still return `GENERATION_EFFECT_UNSUPPORTED`.
- `store/generation_ledger_family_test.go` pins both directions: six admitted pairs and seven
  refused pairs, so a future edit cannot quietly turn the widening into an open manifest.

Still gated, deliberately: the `usage_receipt_json` key allowlist (`:519-527`) has no
`graphEffects` / `finalizationEffect` yet, and `AgentToolNames != nil` is still rejected at
`generation/service.go:259-263`, `:289-291`. Those are the next two widenings, and they must
follow the same shape: an operation-scoped predicate plus a test that pins the refusals.

### Ordered plan for the two families (from the S4 map)

1. `RecordEffect` on the ledger — one write method re-running `mutableRoot`, patching a single
   named `usage_receipt_json` key through `glPatch`, idempotent on `[key, hash(value)]`, with the
   allowlist above extended behind the same operation predicate. Shared by six operations across
   `graph-generation` and `finalization-generation` (and four `agent-generation` tool operations).
2. B1 `{graph,finalization}:cancel` — binder only, proves the widened gate end to end.
3. B2 `graph:begin` + `graph:read`; B3 `graph:execute` + `graph:confirm`.
4. B4 `finalization:read` + `finalization:begin` (needs a slot-keyed run lookup).
5. B5 `finalization:execute` (writes nothing, so it is independently verifiable).
6. B6 `finalization:commit` — the only batch that writes author domain data.

Every batch must also decrement the `want 32` assertion at
`cmd/novel-server/synthetic_generation_http_linux_test.go:456`.

### 2026-11-04 — P2 and B1 landed

- `88137187` **`RecordEffect`** — the ledger's thirteenth write method. It patches one named
  `usage_receipt_json` key through `glPatch`, refuses any key the run's own operation does not
  own, and is idempotent on `[key, value]`. The receipt allowlist in `mutableRoot` now admits
  `graphEffects` / `finalizationEffect`, but only when some run under the same root legitimately
  owns that key, so an ordinary attempt still cannot carry one. Pinned by
  `generation_ledger_family_test.go` (five admitted and six refused key/operation pairs).
- `7cfc143d` **B1** — `graph-generation:cancel` and `finalization-generation:cancel` now route
  to the ordinary frozen-handle stop. Verified in the HTTP test by asserting both operations no
  longer answer `501 GO_OPERATION_NOT_MIGRATED`; the unmigrated generation assertion dropped to
  `want 30`.

Durable lane: **63 / 281**. Next is B2 (`graph-generation:begin` + `:read`), which is the first
batch that needs the request-shape widening (G5) and the full recovery projection (G6/G7), not
just wiring.

### 2026-11-04 — the graph family's missing substrate is now in Go

`65814b77`/`8670f2ee` add `internal/graphgen/` (nine files, ~3 400 lines), a mechanical port of
the modules `graph-generation:read` depends on:

| Go | Source |
|---|---|
| `task.go` | `plot-tree-generation-pure.ts`, `narrative-thread-generation-pure.ts` builders |
| `derive.go` | `derivePlotTreeSnapshot` plus the whole `src/shared/plot-tree.ts` validator set |
| `parse.go` | `parseNarrativeThreadPlanCandidates`, `parseNarrativeThreadEventCandidates` |
| `hash.go` | `textHash` and a `JSON.stringify`-faithful encoder |
| `language.go` | `resolveWritingLanguage` |

The twelve prompt literals are pinned **byte for byte** in the tests. That matters more than the
usual golden test: a reflowed template changes what the model is asked without failing anything
behavioural, so the only way to keep the port honest is to compare the exact bytes.

The port records seven known divergences, of which two deserve attention before the family is
declared equivalent:

1. **Surrogate-pair truncation** — JS keeps a lone surrogate when a character budget splits an
   astral pair; Go decodes the half as `U+FFFD`. Reachable only at the exact cut point.
2. **`Date.parse`** is approximated by a fixed layout list. Everything the app writes matches;
   exotic legacy strings would be rejected where JS accepts them.

Also landed: `BoundOperations()`, an exported view of what a server actually serves, and
`bound_operations_test.go`, which pins `BindGeneration` to its thirteen names, `BindReviewRevisions`
to fourteen and `BindReviewRevisionRemainder` to nine. A family binder could previously be
written, never wired, or wired into a collision, and every test still passed — that is exactly how
`BindReviewRevisions` sat as dead code until it was parameterised.

**Next:** bind `graph-generation:read` on top of `graphgen`. It needs a run whose manifest carries
`graphGenerationContext` plus its three hashes, so the acceptance test has to seed that manifest
through the ledger directly. `begin` follows, and it is the batch that also needs the request-shape
widening (`syntheticParseSelection` still rejects `input`/`slot`/`artifact`/`index`).

### 2026-11-04 — independent review of the graphgen port

The port was verified rather than trusted:

- **Prompt literals** — a separate extractor (not the porting agent's) normalised both sides
  (removing `${…}`/`%d` interpolation and all whitespace) and compared every literal: 11 long
  literals, 11 exact matches, including all six system prompts in both languages. This is the
  check that matters, because a reflowed template changes model behaviour without failing any
  behavioural test.
- **Panic surface** — the derivation sorts and indexes `Sources[0]`, `sortedEvents[0]` and
  `Tracks[0].Events[0].Sources[0]`. Each site was traced: every candidate is constructed with
  exactly one source, `sortedEvents` is guarded non-empty before use, and the fence match is
  nil-checked before `fenced[1]`. The four `MustEncodeJSON` calls all encode
  `PlotTreeSourceReference` structs, never a Go map, so the panic branch is unreachable from
  model output.
- **Untrusted input** — the parsers and derivation receive raw model output. They bound text
  through the ported UTF-16 budgets and never index without a length guard, so malformed or
  hostile completions fail with a sentinel error rather than a panic.

The two divergences that remain reachable are recorded above (surrogate splitting at an exact
cut point, and `Date.parse` approximated by a fixed layout list).

### 2026-11-04 — why `graph-generation:read` is not a wiring job (concrete blocker)

Attempting the port surfaced the real obstruction, and it is in the store, not in the projection:

`readGraphGenerationContext` verifies `textHash(JSON.stringify(context)) === contextHash`. Node hashes
the object **in insertion order**, and that order is preserved in `binding_json` on disk. But
`GenerationLedger.get` returns `binding` as a `map[string]any` — Go maps have no order, so by the
time the projection sees the context, the byte sequence Node hashed no longer exists. And
`graphgen.EncodeJSON` deliberately refuses Go maps rather than guessing an order, so there is
nothing to fall back to.

Two ways out, and only these:

1. **Expose the raw binding text.** Add a ledger read that returns `binding_json` (and, if needed,
   `usage_receipt_json`) as the stored string, then decode it with an order-preserving pass into
   `graphgen.Object` before hashing. This is the smaller change and keeps the hash semantics exact.
2. **Re-order after the fact.** Decode into a map, then re-emit with a fixed key order and compare
   against a hash computed the same way. This silently changes what is being verified — the hash
   would then prove "this context re-encodes to itself", not "this is the context Node froze", so
   a Node-written run and a Go-written run would never agree.

Option 1 preserves cross-version parity; option 2 does not.

**Option 3 is the one to take**, and it follows from a fact already recorded in the S4 map: the Go
lane's request hash can never match Node's, so a Go deployment must not point at a database the
Node build already executed generation against. Graph runs are short-lived recoverable state, not
author data, so in-flight graph runs written before cutover are not something we can resume anyway.
That makes Go the **only writer** of a graph context, and then a single canonical encoding is
sufficient: write with sorted keys, verify with sorted keys. No order-preserving decoder is needed,
and `graphgen.EncodeJSON` — which refuses maps precisely to avoid inventing an order — should keep
refusing them; the graph lane should pass `graphgen.Object` built from an explicit field list in
both directions, which also makes the on-disk shape obvious at a glance.

Note the knock-on effect: this same raw-text requirement will apply to `graph-generation:execute`
(the task hash) and to `finalization-generation:*` (slot hash), so solving it once unblocks the
whole family rather than one operation.

**Implemented:** `graphgen.CanonicalJSON` / `graphgen.CanonicalHash` now provide that single
canonical form — object keys sorted at every level, array order preserved, leaf encoding unchanged
(so the JavaScript number, string and escape rules still hold). `EncodeJSON` keeps refusing Go
maps; the graph lane canonicalises instead of guessing an order. Six pinning tests cover
per-level sorting, stability across map iteration, array-order preservation, construction-shape
independence, Object-versus-map equivalence, and unencodable leaves. CI green at `42576fff`.

The first CI run of it caught a real semantic bug of my own: the `Object` branch preserved the
caller's order while the map branch sorted, so the same value hashed two ways. A function called
*canonical* cannot preserve order — `EncodeJSON` is the order-preserving one, and that separation
is now explicit in both doc comments. Nothing consumes it yet: `graph-generation:read` is the first
caller, and it must verify with exactly this function.

### 2026-11-04 — two envelope defects found by checking the wire shape against the renderer

Both were introduced by me and both were green in CI before being found, which is the point.

1. **The family cancels read the wrong object.** `generation:read` and `generation:cancel` take the
   bare handle; `graph-generation:cancel` and `finalization-generation:cancel` take
   `{ handle }` (`src/services/graph-generation.ts:40`,
   `src/services/finalization-generation.ts:27`, and the Node owner rejects any other key). The Go
   binder passed the envelope straight to `syntheticParseHandle`, so every *real* call would have
   failed to find its run. The HTTP test could not see it: it only asserted "not 501", and a
   malformed-run error is also not 501. `familyCancelHandle` now unwraps and rejects unknown keys
   with the family's own code, pinned by `TestFamilyCancelsUnwrapTheHandleEnvelope`.
2. **Op-body errors escaped the lane envelope.** The adapter wrapped only argument validation in
   `fail(...)` and returned `call(...)` verbatim, so a mutating op that reported
   `GENERATION_OPERATION_UNSUPPORTED` (or a malformed request) answered with an HTTP error instead
   of `{"success": false, "error": "Error: CODE"}`. The adapter now routes body errors through the
   same `fail`, which fixes the read and mutating shapes at once.

The lesson for the remaining batches is specific: **compare the Go handler's argument shape against
the renderer's call site, not against the contract's type signature.** The contract says
`args: [{ handle }]` for one family and `args: [handle]` for the other, and a test that only checks
"not 501" cannot tell the two apart.

### 2026-11-04 — graph context validation, and an honest reversal

`syntheticGraphContext` now verifies a frozen graph run and rebuilds its task: kind↔operation,
context digest, project, origin epoch, key, creation time, the doubly-frozen input, the stored task
digest, and the rebuilt task digest. Five tests pin the accepted case and four refusals (tampered
context, tampered task, another project, disagreeing input copies). CI green at `f96bd25f`.

**Reversal:** `graphgen.CanonicalJSON`/`CanonicalHash` from the previous round were removed. They
were correct and tested, but `cmd/novel-server` already had `syntheticCanonical` + `syntheticHash`
(the lane's own sorted-key formatter), and the single-writer decision — the same decision that
made the encoder possible — makes that existing helper sufficient. Keeping both would have left two
canonicalisation conventions in one process, which is exactly how two "identical" hashes stop
matching. The round's real product was the analysis; the code turned out redundant.

`syntheticGraphDigest` normalises a value through `encoding/json` before hashing, so a struct and
the map it decodes to digest identically — that is what makes "rebuilt task equals stored task"
checkable at all.

Next: `begin` writes these manifests and `read` projects them; both call `syntheticGraphContext`.

### 2026-11-04 — what `graph-generation:read` actually depends on (do not bind it yet)

`graphRecovery` is five pieces, and one of them is a security boundary:

1. `readGraphGenerationContext` — done (`syntheticGraphContext`).
2. `attemptCount`, and **`GENERATION_GRAPH_ATTEMPT_CONFLICT` when a run has more than one attempt**.
   A second attempt means the run was retried, and the recovery would have to choose which raw
   output the author is looking at.
3. `effects` — projected from stored graph effects.
4. `artifact`/`result` — from attempt 0's receipt, **omitted when `graphs.result` refuses**, because
   unqualified raw output stays in the run view for copying rather than being presented as a result.
5. `sourceStatus` — `assertFinalizationSources`, which is
   `budget(root).root.status !== 'cancelled'` **and** a full binding rebuild compared against the
   frozen one (`compareGenerationSourceBindings(run.binding, rebuildBinding(...))`, with the current
   model receipt folded in).

`sourceStatus` is not cosmetic: `'current'` is the signal the UI uses to enable confirmation, and
confirmation is what writes finalized text. **Reporting `'current'` without the rebuild would be a
data-safety defect** — the author's finalized content could be overwritten from sources that had
already changed. So `read` stays unbound until the rebuild exists, and when it does land it must
fail closed (`'conflict'`) on any doubt.

The Go lane already has the parts of the rebuild that matter: `syntheticFingerprint` derives the
same nine reduced hashes from real project state, and `generationowner` `binding.go` already
verifies epoch currency plus material revision/text. What is missing is reconstructing `selection`
and `receipt` from the frozen manifest so the rebuild can run at read time — that is the same work
`finalization-generation:*` needs, which is why it should be done once, for both families, rather
than patched into `read`.

### 2026-11-04 — `graph-generation:read` bound (durable 63 → 64)

Bound on the shared generation adapter (extracted from `BindGeneration`, so the graph family cannot
drift from the lane's envelopes). It validates the `{ handle }` envelope, revalidates the frozen
context and rebuilds the task, refuses a run with more than one attempt
(`GENERATION_GRAPH_ATTEMPT_CONFLICT`), and returns `{view, modelId, context, attemptCount,
sourceStatus, effects}`. CI green at `19241d65`.

**Three deliberate omissions, all recorded in the code rather than only here:**

- `effects` — recorded by confirmation, which is unbound, so no stored effect can exist.
- `artifact`/`result` — the original qualifies the stored result against a graph-specific reference
  and omits it when qualification fails, so raw output stays in the view for copying. Projecting it
  without that check would present unverified text as a result.
- `sourceStatus` now reports only that the root is not cancelled. It can therefore say `current`
  for a run whose sources moved. **No writing operation may be bound on the strength of this value**
  until the binding rebuild lands — that rebuild is the gate in front of the whole finalization
  family, not a detail of `read`.

### 2026-11-04 — security review of the durable lane additions (two real defects)

Reviewed the code added over the last rounds rather than only writing more of it. Both findings were
mine, both were green in CI, and both sit in `recordEffect`, the ledger write that stores
`graphEffects` and `finalizationEffect`:

1. **Cross-run effect write.** The attempt lookup was
   `SELECT usage_receipt_json FROM generation_attempts WHERE attempt_id=?` — no `run_id` scope. A
   caller able to mutate one run could pass an attempt id belonging to a *different* run and have an
   effect written into it, which is precisely the cross-run contamination the operation-scoped
   allowlist is supposed to prevent. Now scoped with `AND run_id=?`.
2. **Receipt returned for the wrong identity.** `g.receipt(ctx, conn, text(0), nil)` passed the
   *run* id where the function expects an *attempt* id, so the call would fail with
   `GENERATION_ATTEMPT_MISSING` after a successful write - an operation that mutates and then
   reports failure. Both call sites now pass `text(1)`.

Also checked, no issue:
- `draftSaveFailure` sanitises a code to `REQUEST_FAILED` unless it matches `[A-Z][A-Z0-9_]*`, so
  routing operation-body errors through the lane envelope does not leak SQLite text or paths.
- `syntheticGraphAttemptIDs` and the receipt lookup are parameterised; no string-built SQL.
- `familyRequestHandle` rejects any envelope key other than `handle`.

**Gap:** neither defect has a regression test, because exercising `recordEffect` needs a run seeded
through `open` with a full `GLBinding`. `internal/generation/contract_test.go` already builds exactly
that fixture against real SQLite, so the test belongs there; until it exists these fixes are verified
by inspection only.

### 2026-11-04 — source freshness now fails closed

`syntheticGraphSourceStatus` used to answer `current` for every run whose root was not cancelled,
which meant it could say "fine" about sources that had moved. It now answers `current` only when it
can re-read the frozen fact, and `conflict` otherwise:

- cancelled root → `conflict`;
- **event** → `current` only if `finalization_outbox` still holds the frozen
  `(draftId, finalizationId) → contentHash`; a missing row is a changed source, not an error;
- **plot** and **plan** → `conflict` until the plot baseline and blueprint re-derivations exist.

Choosing "wrong but visible" over "permissive but invisible" is the point: confirmation writes over
work the author already has, so an unverifiable state must read as changed. CI green at `db815fd6`.

### 2026-11-04 — post-process and LLM ledger reads bound (durable 64 → 69)

`BindPostHistoryReads` (`internal/transport/posthistory_read.go`) binds the five **read** members of
that family - `db:post-process-get-latest-run`, `-get-steps`, `-is-all-passed`, `db:get-llm-stats`,
`db:get-llm-history` - through `BeginPostHistory` on the admitted author database. The four mutating
names (`create-run`, `mark-step-ok`, `mark-step-failed`, `log-llm-call`) stay unbound and keep
failing closed; the HTTP test asserts both directions. CI green at `6c008577`, first attempt.

Note for the next batch of this family: `store.PostHistory` exists only as a `*sql.DB` repository
(`internal/store/posthistory.go:26-33`) with no transaction-read variant, so it cannot be served
through `syntheticReadSurface.readTx`. It is served the way `NewRecovery`/`NewSummary` are instead.

### 2026-11-04 — narrative and consistency reads bound (durable 69 → 72)

`BindNarrativeReads` (`internal/transport/narrative_read.go`) binds `db:narrative-thread-list`,
`db:narrative-thread-list-relevant` and `db:consistency-exemption-list`. The six writes in that
family stay unbound and keep failing closed. CI green at `7b71f0cc`.

The arity table is the part worth copying for the next batch: the two arity-2 names carry the path at
index 0, while `-list-relevant` is arity 3 with a `map[string]any` input at 0 and the path at 1
(`internal/transport/narrative.go:22`). Getting that wrong is invisible to any test that only
asserts "not 501" - which is why each batch lands with a positive assertion through the durable lane.

### 2026-11-04 — global prompt and user skill reads bound (durable 72 → 74)

`BindPromptSkillsReads` binds `prompt:load-global` and `skills:list-user`; the three mutations in that
family stay unbound. CI green at `8d3801b3`.

**The trap in this family: both reads are arity 0.** There is no project path and no frozen 3-key
session context (`internal/transport/promptskills.go:28,36` reject any argument), so copying the
sibling binder's `draftExpectedContext(args, arity-1)` would make every real caller 4xx. The caller
gate is `startupCaller(caller)` instead - empty means 409 `GLOBAL_DATA_NOT_READY`, the same predicate
the transport binder already used. The renderer really does send `{"args":[]}`
(`src/services/prompt-catalog.ts:360`).

Because both need `-synthetic-durable-global-dir`, they are **conditional**: the unconditional count
stays 58, the flag-gated count is now 16.

Path safety was already enforced by the owner and was reused rather than re-implemented:
`internal/promptskills/files_linux.go` pins the root with `O_NOFOLLOW`, re-checks identity before and
after opening, and `ListUser` skips any child that fails - the browser only ever sees
`managed://skills/<name>`.

### 2026-11-04 — blueprint reads bound (durable 74 → 77)

`BindBlueprintReads` binds `db:blueprint-get`, `db:blueprint-character-sync-get` and
`db:blueprint-character-sync-list-pending`; the five mutations stay unbound and the test proves the
`delete`/`commit-range` calls left no trace. `db:blueprint-get-all` was already bound elsewhere and
was deliberately not re-bound. CI green at `c9523711`.

Arities here are not uniform: `-get` and `-character-sync-get` are arity 3 with the path at index 1,
`-character-sync-list-pending` is arity 2 with the path at index 0 - and all three **do** carry the
frozen 3-key context last, unlike the global prompt/skill family. Check both facts per name before
copying a template.

**These two "reads" are not read-only.** `PendingSync` and `GetSync` execute the store's existing
`blueprintBackfillSQL` (`internal/store/blueprints.go:137`), an `INSERT OR IGNORE` that backfills the
sync tables. So a GET mutates the author database. That is the original behaviour and it is preserved,
but it matters twice: such a read cannot run on a read-only transaction, and any future
"reads are side-effect free" assumption in the admission lane would be wrong here.

### 2026-11-04 — plot tree read bound (durable 77 → 78)

`BindPlotTreeReads` binds `db:plot-tree-read` (arity 2, path at index 0, frozen context last);
`db:plot-tree-save` and `db:plot-tree-clear` stay unbound. `PlotTree.Read` runs on a read-only
transaction and `plotRead` is SELECT-only, so unlike the blueprint sync reads this one really does not
write. CI green at `c10f6a4b`.

**Do not pair `BindPlotTreeReads` with `BindPlotTree`** - the transport binder already covers this
name, and the duplicate would be a fatal `OPERATION_ALREADY_BOUND` at startup, not a test failure.

### 2026-11-04 — audit: there are no stub operations to collect

Before starting the write families I checked whether any of the 281 channels are stubs whose migration
would be free. All 281 have a literal registration; **277 do real work**, and exactly **4 are
unconditional refusals**:

| operation | original | note |
|---|---|---|
| `db:blueprint-update-notes` | `db-controller.ts:636/640` | `GENERATION_FINALIZATION_ADMISSION_REQUIRED` |
| `db:continuity-save-finalized` | `db-controller.ts:797/800` | same code |
| `db:continuity-save-character-state-candidates` | `db-controller.ts:806/809` | same code |
| `db:plot-tree-save` | `db-controller.ts:886/894` | `GENERATION_GRAPH_ADMISSION_REQUIRED` |

All four are admission gates: the real write happens inside the main generation transaction, so these
channels must keep refusing. Verified that Go already mirrors them - `internal/transport/summary.go:125`
refuses the two continuity writes, and the blueprint/plot-tree transport binders refuse theirs.

Consequence for planning: **there is no shortcut batch.** The remaining ~200 operations are real work,
not stubs waiting to be declared done. A Go handler that "succeeds" where the original refuses is a
divergence in the dangerous direction, so this four-row table is also the checklist to guard against it.

### 2026-11-04 — first write family lands: post-process and llm-call (durable 78 → 82)

`BindPostHistoryWrites` binds `db:post-process-create-run`, `-mark-step-ok`, `-mark-step-failed` and
`db:log-llm-call`. The test creates a run, marks one step ok and another failed, logs an LLM call, then
reads all of it back through the already-bound reads - persistence, not just a 200. CI green at
`fc1963de`.

Arities are not uniform (`posthistory.go:24-25`): create-run 3/path 1, mark-step-ok 4/path 2,
mark-step-failed 5/path 3, log-llm-call 3/path 1; all four carry the frozen 3-key context last.

**`db:log-llm-call` has its own failure shape** and it is preserved deliberately: a failure returns
HTTP 200 with `{"success":false}` and **no** error code (`posthistory.go:23`), unlike the other three
which return `{"success":false,"error":"Error: CODE"}`. Do not "fix" this into consistency - the
renderer distinguishes them.

These four did not need a lease rotation: the borrow refreshes the permit and releases, which is the
same shape as the draft write but without the CAS. The draft write's rotated-lease requirement
(`draft_live_write.go:100`) still has no counterpart for these, so do not copy it here.

### 2026-11-04 — narrative and exemption writes bound (durable 82 → 87)

`BindNarrativeWrites` binds `db:narrative-thread-plan-create/-update/-delete` and
`db:consistency-exemption-save/-revoke`; `db:narrative-thread-event-confirm` stays unbound because it
belongs to the graph admission gate. Each write is verified by reading the state back through the
already-bound list reads. CI green at `5ad1f9c4`.

**One open divergence, recorded rather than buried:** failures now come back as HTTP 200 with
`{"success":false,"error":"Error: CODE"}` (this lane's mutation convention), while the original binder
returned HTTP 500 for the three `plan-*` names. The tests assert the new shape, so CI cannot catch this
class of change. Before acceptance: check whether the renderer branches on the status code for these
names. Also note `db:consistency-exemption-revoke` reports success even when nothing was revoked - that
mirrors the original (`Narrative.RevokeExemption` discards its result at `internal/store/narrative.go:305`),
so it was preserved rather than "fixed".

### 2026-11-04 — blueprint writes bound (durable 87 → 91)

`BindBlueprintWrites` binds `db:blueprint-upsert`, `-upsert-many`, `-delete` and `-commit-range`.
`commit-range` was bound too: it is a single store call (`internal/store/blueprints.go:277`) that owns
the range write, both operation rows and the read-back proof inside its own `immediate` transaction.
`db:blueprint-update-notes` is still deliberately refused - the original refuses it unconditionally.
CI green at `ad1bab63`.

`Upsert`, `UpsertMany` and `Delete` return only an error, so **deleting an absent chapter reports
success**, exactly as the original does. That is preserved, not fixed; if a caller ever needs
"deleted nothing" to be visible, the store has to grow a rows-affected return first.

The wire shape here matches the existing Go `BindBlueprints` binder (HTTP 200 with a DTO on failure),
so this batch adds no new divergence on top of the one already listed as OPEN in the gap matrix.

### 2026-11-04 — project-core writes reverted (durable stays 91); CI diagnosis open

`db:project-core-update` and `db:project-core-synopsis-commit` were bound and pushed as `c79ef616`;
CI failed. The failure was **not** in the Go code: it landed on step 34, *Generate remaining-domain
original synthetic contracts*, which is silent by design - `run_contract` writes `FAIL <name>` into
`$GITHUB_STEP_SUMMARY` and the step only ends with `test "$failures" -eq 0`, so the job shows
`##[error]Process completed with exit code 1` with no failing command in the log.

Established facts, in order (all verified by runs, not by reading):

1. The Go tests in that same run **passed** - step 38 green, zero `--- FAIL`, zero `_test.go:` lines in
   the parsed `-json` output. So the failure is not a compile error and not a test failure.
2. Because step 34 reported non-success, step 39 *Race detector* was skipped, and step 40 *Enforce
   collected contract and test failures* then failed on `test "${{ steps.race-tests.outcome }}" = success`.
   The red job is downstream of step 34; do not chase the race step.
3. A failed-jobs-only rerun and then a full rerun both failed identically, so it is deterministic, not a
   flake.
4. `git revert` of `c79ef616` and its follow-up `8a5a8eda` returns CI to green (`f64e6242`).

Next step is a bisect, not a guess: reapply `internal/transport/writes_misc.go` **without** registering
it in `synthetic_durable_open_linux.go` and run CI. If that is green, the trigger is the registration or
the new test; if it is red, the trigger is the binder file itself. Then read
`$GITHUB_STEP_SUMMARY` of that step (it names the failing contract) rather than the job log.

Also worth keeping from the reverted work: `db:character-roster-commit` has **no store method** and the
original binder only denies it, so it is a fifth refusal-only candidate that the earlier audit did not
classify as one - the audit's "277 real implementations" may need amending.

### 2026-11-04 — bisect result: project-core writes bound, assertions NOT yet landed

Bisect, three CI runs, each verified by the run itself:

| commit | content | CI |
|---|---|---|
| `06fbb501` | `writes_misc.go` present, **not** registered | green |
| `348d6d8c` | registered via `BeginProjectCore` | green |
| `fb38d79b` | + 129 lines of HTTP assertions and the CAS fix | **red** |

The red is not a Go failure: step 38 logs zero `--- FAIL` and zero `_test.go:` lines, and step 34
*Generate remaining-domain original synthetic contracts* reports `conclusion: success` while its
`outcome` is `failure` (`continue-on-error` masks it), which skips the race step and fails the enforce
step. So the failing piece is the **step summary** of step 34 - `run_contract` writes `FAIL <name>`
there and nowhere else, and the step ends with a bare `test "$failures" -eq 0`. Read
`$GITHUB_STEP_SUMMARY`, not the job log; the job log cannot show it.

Current state at `8ba2c31b` (green): `db:project-core-update` and `db:project-core-synopsis-commit`
are **bound and registered but have no positive end-to-end assertion** - the identical anti-pattern
this handoff warns about elsewhere. Treat them as *unverified*, not as delivered, until their
assertions land. The CAS fix they depend on (`stale synopsis CAS must report the bare
`PROJECT_CORE_SYNOPSIS_CHANGED``) was reverted with them, so `writes_misc.go` currently still returns
the wrong `Error: PROJECT_CORE_OPERATION_FAILED`.

Do not count these two in the durable total until that is closed.

### 2026-11-04 — the masking chain is unit-tests, not step 34 (corrects the earlier bisect note)

Two workflow probes settled where the red comes from, and they **correct** the previous entry:

- A step was added that `cat`s `$GITHUB_STEP_SUMMARY` right after *Generate remaining-domain original
  synthetic contracts*. It printed **empty**: that step records no contract failure, so it is not the
  culprit and its earlier `outcome: failure` reading was my misdiagnosis.
- The race step's `if` was then reduced to `!cancelled() && steps.unit-tests.outcome == 'success'`. The
  race step **still skipped**, which means `steps.unit-tests.outcome` is not `success`.

So the chain is: `Synthetic-data unit and HTTP contract tests` has `outcome: failure` (masked by
`continue-on-error`, so its `conclusion` reads success) -> the race gate sees a non-success outcome and
skips -> the enforce step fails on `test race-tests.outcome = success`. The visible red is three steps
away from the cause.

The remaining contradiction to chase: when the project-core assertions are present the unit step's
outcome is failure, **yet the parsed `-json` stream contains zero `Action: fail` records**. That points
at a non-test failure inside that step - most likely the test package failing to build, which `go test
-json` reports as `[build failed]` text rather than as a per-test fail action. First thing to check next
time: whether the assertions restored from `fb38d79b` reference a helper that no longer exists.

Everything is reverted to `e69d2ae0` (green); the two project-core writes remain bound, registered and
**unverified**.

### 2026-11-04 — root cause found: the project-core test never compiled

The masked step was `Synthetic-data unit and HTTP contract tests`, and the reason it failed while
emitting no `Action: fail` record is that the **test package did not build**. `go test -json` reports a
build failure as `[build failed]` text, not as a per-test fail action, so every log-based diagnosis
looked clean.

The build errors, from CI:

    synthetic_durable_http_linux_test.go:730:15: cannot use func literal ...
    synthetic_durable_http_linux_test.go:742:10: undefined: call

Cause: the batch declared `coreWrite` with **two** parameters (read shape) while every call site passed
**four** (input, path, frozen context), and `coreRow` was never declared at all. The subagent reported
"tests green" because it was forbidden to build, and CI could not say so plainly.

After fixing both helpers the package compiled and the run failed later, but its log came back empty
from the API twice (`zip: not a valid zip file`), so the remaining assertion failure is undiagnosed.

State: reverted to `2438c60a` (green). durable is **91 verified**; the two project-core writes are
bound, registered and **unverified** - do not count them.

**Do this first next time**, before writing more binders: keep `continue-on-error` off the contract
step and add a `go build ./...` (or `go vet ./...` output check) as its own early step, so a
non-compiling test package fails loudly instead of being masked three steps downstream. Every round
spent on this was spent fighting the masking, not the code.

### 2026-11-04 — project-core assertions compile; two CI channels added

Root cause of the multi-round red, finally: the batch's own helper was wrong. `coreWrite` was called as
`coreWrite("db:project-core-update", []any{map[string]any{...}, projectPath, expected})` - the frozen
context was written **inside** the argument list, so the call had two arguments while the declared
closure took four. The package never compiled, and `go test -json` reports that as `[build failed]`
text, never as an `Action: fail` record, which is why every log-based diagnosis looked clean.

Fixed: `coreWrite` takes `(operation string, args []any)` and passes `args` straight to `rpc`, and the
duplicate `coreRow` I had added was removed. **Job 1 (compile gate) is green at `b9608c2e`.**

Two CI channels were added because `gh run view --log` now returns `zip: not a valid zip file` and
`gh api .../logs` returns an unreadable zip for this repo:

1. the compile gate now emits a `::error title=compile failure::` annotation with the first four
   `file:line:col` messages;
2. the unit-test step emits `::error title=go test failures::` with the last four fail records.

Read them with:

    ids=$(gh api repos/.../commits/$(git rev-parse HEAD)/check-runs --jq '.check_runs[]|select(.conclusion=="failure")|.id')
    for i in $ids; do gh api repos/.../check-runs/$i/annotations --jq '.[].message'; done

**Open:** job 2 fails at or before its test step with `Failed to restore: Cache service responded with
400` - a GitHub cache infra error, reproducible on every rerun right now. Job 1 proves the code
compiles; the runtime assertion for the two project-core writes is still **unproven**. Retry job 2 when
the cache service recovers, then read the annotation.

### 2026-11-04 — CI now names failures without the log API

The job log API is unusable for this repo (`zip: not a valid zip file`, and `gh run view --job --log`
returns nothing), so failure reporting was moved into **check-run annotations**, which do work:

- compile gate: `::error title=compile failure::` with the first four `file:line:col` messages;
- unit tests: `::error title=go test failures::` naming the failing package and test;
- enforce step: `::error title=step outcomes::contracts=... unit=... race=...`, which finally
  distinguishes "tests failed" from "a gate skipped".

That last one answered the whole multi-round mystery in a single line: **`contracts=success unit=failure
race=skipped`** - the contract generators were innocent all along, and the unit-test step was the real
failure.

Reading them:

    SHA=$(git rev-parse HEAD)
    for i in $(gh api repos/.../commits/$SHA/check-runs --jq '.check_runs[]|select(.conclusion=="failure")|.id'); do
      gh api repos/.../check-runs/$i/annotations --jq '.[].message'; done

**Where the project-core batch stands:** the binder, the registration and the CAS fix are merged and the
code compiles (job 1 green). The assertions themselves fail at runtime in
`TestSyntheticDurableHTTPOpenSaveCloseReopenPersists`, and the assertion message still cannot be read
through any working channel, so they were withheld again at `c13ac12b` to keep main green. The two
operations are **bound but unverified** - not counted in durable 91.

Next step, concretely: emit the failing assertion's own text into the annotation (the `t.Fatalf`
messages already carry context, so piping the last `_test.go:NNN:` line for the failing package into a
second `::error::` will surface it), then fix the shape and re-apply.

