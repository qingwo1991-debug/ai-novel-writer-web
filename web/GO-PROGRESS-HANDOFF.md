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

### 2026-11-04 — project-core writes verified (durable 91 -> 93)

Green: `49a0d789`, all three jobs. `db:project-core-update` and `db:project-core-synopsis-commit` are
bound, registered and proven by write-then-readback over the real handler.

The last blocker was a **wrong test expectation, not wrong code**: the assertion demanded
`"Error: PROJECT_CORE_SYNOPSIS_CHANGED"` while the original returns the bare
`"PROJECT_CORE_SYNOPSIS_CHANGED"`. The implementation already matched the original; the test had been
written against the buggy prefix. Fixed the expectation.

The annotation channel earned its keep: it printed

    _test.go:778: stale synopsis CAS was not reported as a refusal: map[error:PROJECT_CORE_SYNOPSIS_CHANGED success:false]

which named the exact assertion while every log API was broken. Keep using it.

### 2026-11-04 — why editor-inline cannot be bound yet (architecture, not effort)

Attempted the four `editor-inline:*` operations. They are **not** `db:*` names and do not have the
durable write family's shape: each is arity 1, a single request object, **no path argument and no
frozen 3-key context** (proving lines: `electron/controllers/generation-controller.ts:248-251`,
`src/shared/editor-inline-generation.ts:28-31`). `begin` and `execute` need an editor-inline
generation run, and the Go ledger cannot create one: `store.GenerationLedger.open`
(`internal/store/generation_ledger.go:639`) takes a `RunBinding` whose editor keys
`internal/generationowner/binding.go:37-44` cannot resolve - it reads project-core, blueprint, draft and
prompt only. The original also requires editor admission (`main-generation-owner.ts:418,564`) and
reconstructs a UTF-16 selection context that has no Go port.

So these four stay at 501 `GO_OPERATION_NOT_MIGRATED` **on purpose**, now pinned by assertions so they
cannot silently flip to success. Binding them today would mean inventing the owner pipeline inside a
transport file.

**Planning consequence:** editor-inline is downstream of the generation lane, not a parallel chunk. Any
plan that schedules it before the generation owner port is wrong. The same is likely true of the
`generation:*` family as a whole, which is where the remaining budget should go next.

### 2026-11-04 — the generation budget blocker was already fixed; CI is now billing-blocked

**Correction to the pause point of this handoff.** The `budgetDiagnostics` item is *not* open. The
attempt insert (`store/generation_ledger.go:957`) runs with the correct `rootActionId`, in a committed
transaction, and `budget` returns `[]map[string]any` correctly. The defect was one layer above, in the
lane projection: `syntheticSlice` (`synthetic_generation_linux.go:232`) only accepted `[]any`, so the
ledger's `[]map[string]any` narrowed to nil and the loop emitted an empty array. That was fixed in
`d253f49e`, and the file is byte-identical to the CI-green revision `6f1bc572`.

Consequence: the 11 S1 generation operations were un-wired (`059c57a2`) for a reason that no longer
exists. **Re-wiring them is the highest-value next step**, not more peripheral binders.

A store-level assertion was added (`generation_ledger_test.go`) so an empty `attempts` slice now fails
at the store boundary, not only in the HTTP lane.

**CI is blocked by account billing**, not by code:

    The job was not started because recent account payments have failed
    or your spending limit needs to be increased.

Every job fails to start, so nothing can be verified until that is resolved. All compile/test happens
on Actions by design, so this stops the migration completely. Source-only work can continue, but it
lands unverified.

### 2026-11-04 — CI unblocked: the repo is now public

Actions was blocked by the private-repo minute allowance (2,000/month on Free, billed per **minute**,
each job counted separately - a three-job run costs roughly 20 minutes). The owner chose to make the
repository public, where Actions minutes are free and unmetered. A pre-flip scan found no credentials or
author text in the tracked tree: `git grep` for private-key headers and the usual token prefixes
returned nothing, `.npmrc` holds only registry settings, and the largest blobs are open fonts.

Verified after the flip: run `41874837177` is green in all three jobs, including the store-level
assertion that `budget` carries its attempt rows.

**Operating rule from now on:** batch changes and push once per batch. Pushing after every small edit is
what exhausted the allowance, and it makes each diagnosis cost a full three-job cycle.

### 2026-11-04 — S1 generation is DONE and verified (11 operations), not pending

Verified at HEAD, not inferred:

- The eleven names are wired at `synthetic_durable_open_linux.go:685-717` inside the
  `config.GlobalRoot != ""` block: `generation:begin`, `bind-material-decision`, `execute`, `read`,
  `list`, `pause`, `cancel`, `resume`, `restart`, `discard-candidate`, `retry-draft-short-outline`.
  They are the conditional part of durable 93 (77 unconditional + 16 conditional).
- `synthetic_generation_http_linux_test.go` (552 lines) exercises them end to end and asserts, among
  other things, that `budgetDiagnostics` is non-empty (`:544-547`).
- That test does **not** skip in CI: `:62-67` reads `GO_SQLITE_CONTRACT` and, when
  `GITHUB_ACTIONS=true`, calls `t.Fatal` instead of `t.Skip`. The workflow exports that variable, so the
  green run `41874837177` really executed it.

So the round-59 blocker (`budgetDiagnostics` empty) is closed twice over: fixed in `d253f49e`, and now
pinned by a store assertion plus this lane test.

**Next targets, in the order the dependencies allow:** S2 batch/composition/preparation storage, then
S3 agent-generation, then S4 graph/finalization/import. Editor-inline stays blocked behind the
generation owner port (see the entry above); do not schedule it in parallel.

### 2026-11-04 — nine more operations wired (durable 93 -> 102)

Green: `d5958ecd`, all three jobs.

- Unconditional: `db:narrative-thread-event-confirm` (arity 3, path 1), `db:plot-tree-clear`
  (arity 2, path 0). Both refuse as transport errors - HTTP 500 with a symbolic code and **no**
  `Error: ` prefix - asserted verbatim rather than normalised.
- Conditional on `GlobalRoot` + model origins, mirroring `main.go:275-302`: `config:get`,
  `config:set`, `startup:get-state`, `startup:appearance-ack`, `llm:list-models`, `llm:save-model`,
  `llm:delete-model`.

**mcp (8) was refused, correctly.** `BindMCP` needs a pristine `*mcp.Manager` over a trusted config
path and runtime dir plus a server-owned admin predicate; this lane carries no MCP field and
`cmd/novel-server` has no mcp import, so any manager here would be fabricated. It stays at 501 with an
assertion pinning that.

Next cheapest pool, from the survey: cloud-backup (8, `internal/webdav` is complete with no binder) and
finalization (3, `store/finalization_outbox.go` complete but called only from its own tests).

### 2026-11-04 — finalization writes bound (durable 102 -> 105)

Green: `3050e6a6`.

Bound, unconditional: `finalization:commit`, `finalization:retry`,
`db:finalization-link-knowledge-document`. Each goes through a lane-owned owner proof
(`durableFinalizationOwner`) that only accepts a `transport.FinalizationProof` matching the frozen
scope, so the outbox cannot be driven by an unproven caller.

`MarkPublishedWithOwnerProof` (`store/finalization_outbox.go:290`) is deliberately **not** wired: no
manuscript publisher exists in this lane, so a published receipt could not be minted honestly. A commit
therefore returns the original's failing-publisher shape (`success:false, committed:true,
publicationStatus:"pending"`, `error:"Error: FINALIZATION_PUBLISHER_UNAVAILABLE"`), persisted to
`last_error`, which is exactly `finalization-service.ts:163-168, 201-211`.

**cloud-backup (8) stays 501, correctly.** Every one of them needs a credential or binding store this
lane does not have, a deployment-supplied WebDAV dispatch (`internal/webdav/client.go` `New` refuses
without one), portable export/restore, dialog grants, or an in-flight backup registry nothing can
populate. Pinned at 501 by assertions.

One compile fix this batch: `fail` in `finalization.go` is `func(code string) (any, error)`, so
`return fail(x), nil` is a two-value-in-single-value error. Fixed 29 call sites.

### 2026-11-04 — `db:character-roster-commit` needs a subsystem, not a store method

Investigated, then stopped before writing code. The evidence:

On this lane's baseline (v7, which carries `character_identity_meta`), the original never reaches the
generation-freshness branch: `CharacterRosterRepository.commit` refuses identity-schema databases with
`CHARACTER_ID_WRITE_REQUIRED` at `electron/repositories/character-roster-repository.ts:984`, and the
`assertGenerationSourcesCurrent` thunk (`db-controller.ts:706`) sits *after* that at `:1012`. So the only
reachable positive path is the **author branch** (`db-controller.ts:699-705` ->
`electron/services/character-roster-author.ts:63`), which rejects `generationRunHandle` outright and
depends on `commitCharacterIdentities` (`character-roster-repository.ts:1168-1273`): approvals, aliases,
`static_provenance`, proposals and a projection.

Go has no counterpart - **no production code writes `characters` at all**. Anything bound here would
either exercise the branch the original refuses or fake the identity envelope.

**So this is a subsystem gap, not a missing store method.** Sizing it honestly: one store method is not
enough; it needs the identity commit path (5+ tables, a projection, proposal handling) before any of the
character-write operations can be honest. The existing 501 assertion
(`synthetic_durable_http_linux_test.go:392-411`) stays correct and stays.

**Two subsystem-scale items now bound the remaining work, and both should be scheduled deliberately
rather than discovered again mid-batch:**
1. character identity write path - unblocks `db:character-roster-commit` and the
   `character-proposal` / `finalized-character` / `character-avatar` families (~15 operations);
2. LanceDB native store - unblocks the `kb` family (~18 operations); `lancenative` is still a stub.

### 2026-11-04 — character identity commit: first slice landed, green

Green: `7fce250f`. `internal/store/character_identity.go` (674 lines) plus
`character_identity_test.go` (332 lines, 22 refusal cases including check-order precedence).

**`commitCharacterIdentities` is staged** (`character-roster-repository.ts`): schema gate `:1173` ->
payload digest `:1174` -> authorize `:1176` -> re-serialise and compare `:1177` -> approval replay
`:1178-1182` -> revision check `:1183-1184` -> operationId/source/action `:1185` -> source-kind table
`:1186-1193` -> source-ref shape `:1194-1197` -> resolutions `:1201` -> field set `:1203-1206` ->
uniqueness `:1207-1208` -> retire/change overlap `:1209` -> creations `:1215-1222` -> changes
`:1223-1236` -> retire `:1237` -> receipt `:1238-1239` -> relationships `:1240-1253` -> resolutions
`:1254-1271` -> meta bump `:1272`.

**Implemented:** creations, changes, retirements, alias open/close, approval row, receipt, meta bump,
with the original's partial-write sequencing and 12 mirrored refusals. **Deliberately not yet:**
relationships `:1240-1253`, proposal resolutions `:1254-1271`, projection refresh and roster-meta bump
(`character-roster-author.ts:160-175`).

The binder stays **501**: without the projection refresh a commit would report success and leave the
roster inconsistent. That is the correct trade - the store slice is testable without it.

**Three documented deviations, all fail-closed:**
1. the payload digest uses this port's canonical encoder, so a cross-implementation replay yields
   `NONCE_CONFLICT` rather than a wrong success;
2. `CHARACTER_ID_META_MISSING` stands in for the original's uncaught TypeError at `:1183`;
3. a non-object `static_provenance` is refused rather than spread.

**Remaining before `db:character-roster-commit` can be bound:** the projection refresh, the
relationships stage, and the proposal resolutions. Roughly the same size as this slice plus the roster
readback in `character-roster-author.ts:160-175`.

### 2026-11-04 — `db:character-roster-commit` is served (durable 105 -> 106)

Green: `7142cf88`. This is the first character **write** operation Go can serve, and it is the fruit of
the identity slice landed last round.

The three missing stages were completed:
- relationships (`character-roster-repository.ts:1240-1253`) - both endpoints resolved and re-read as
  active rows *before* the edge is judged (empty/self/duplicate), then one `character_relationships` row;
- proposal resolutions (`:1254-1271`) - already-resolved and `source_key` shape refusals, resolved
  character written, plus an owner-shaped relationship row;
- projection refresh (`character-roster-author.ts:135-147`) - `project_core.characters_arch`,
  `character_roster_meta` (revision+1, `migration_state='ready'`, projection/fact hash), then a
  readback that refuses on any disagreement.

Bound as `BindCharacterRosterCommit` (`character_roster_commit.go:38`), arity 3 / path 1 unchanged. The
old placeholder refusal was **replaced, not deleted**: a provider without `CharacterRosterCommitter`
still borrows-then-refuses, so the read-only lanes keep their 501.

Assertions (`synthetic_durable_http_linux_test.go:394-517`): a two-entry `manual_edit` commit returns
`success:true` with a receipt whose `revision` is old+1; the snapshot reports `status:"ready"`; the rows
show the renamed character with identity and roster revisions incremented; `db:character-roster-read`
agrees; then `CHARACTER_ID_REVISION_CONFLICT` and `CHARACTER_ID_DUPLICATE` refusals, plus an
unchanged-rows check. The old 501 block is gone.

Two compile fixes on the way, both from the same class - name collisions in a 1300-line file:
`rosterRole` (const vs the older func in `character_roster.go`) and then `rosterAuthorRole` (const vs the
predicate). The constants now carry an `rosterAuthor*` prefix and the predicate is `rosterAuthorRoleValid`.

**Known runtime divergences, ranked:** stored non-string columns read back as a mismatch (the original's
`!==` refused too, so this fails closed); hashes use the canonical Go encoder; a missing approval row
yields `CHARACTER_AUTHOR_RECEIPT_INVALID` instead of a TypeError; sorts use UTF-16 ordering rather than
`localeCompare`.

### 2026-11-04 — LanceDB recon: the family is NOT the next win (correcting the plan)

The previous round proposed LanceDB as the highest-value next target. Reconnaissance says that is wrong,
and the reason is structural rather than a matter of effort.

**`lancenative` is not a stub.** `backend-go/internal/lancenative/` (`native.go` 227 lines, `vector.go`
116) is a real cgo binding to 789 lines of Rust (`native-lance/src/lib.rs`, lancedb `=0.22.3`). The stub
is the *wiring*: `projectaccess/native_knowledge_stub.go:8-12` returns `false`/`ErrConfiguration` for
every non-`lance_native` build, and `BindKnowledge` / `BindKnowledgeSearch` are called only from tests.

**CI can build it** (`.github/workflows/go-lance-native.yml`: rustup 1.90.0, protoc, clang, cargo build,
~3 GB target, 75-minute budget) - this machine cannot (rustc 1.63, no protoc/clang/go).

**But the ABI is read-only.** `native-lance/include/novel_lance.h:74-121` exposes exactly four symbols:
export, import, vector query, free. There is **no append, no delete, no drop, no FTS build**. So even a
CI-built library cannot serve the twelve writing operations (`import-text`, `import-finalized`,
`import-planning`, `import-reference`, `backfill-vectors`, `remove-document`, `clear-all`, the ingestion
grants, and reindex). A Lance round buys **at most 4-5 read operations**, not the 19-op family.

**And the SQLite shortcut is closed:** `electron/migrations/baseline-schema.ts` has no documents or chunks
table; knowledge lives only in Lance. A metadata-only Go implementation would match an *empty* knowledge
base and diverge on every real project - that is theatre, not migration.

**Corrected consequence:** the `kb` family (19 operations, not 18) is blocked on extending the Rust ABI,
which means a Rust change plus a cgo rebuild - a different kind of project than anything done so far, and
one this machine cannot help with at all. It should be scheduled deliberately, not picked up mid-batch.

The count in the earlier entry ("LanceDB native store - unblocks the kb family") was optimistic; treat this
entry as the correction.

### 2026-11-04 — six llm routing-config operations bound (durable 106 -> 112)

Green: `1bfa4f6d`.

Bound, conditional on `GlobalRoot` like the existing llm bindings: `llm:get-stage-model`,
`llm:set-stage-model`, `llm:get-default-model`, `llm:set-default-model`,
`llm:get-default-embedding-model`, `llm:set-default-embedding-model`. New binder
`internal/transport/model_routing.go`, new store `internal/globalconfig/model_routing_linux.go`.
Frozen context: `llm:*` is **not** project-scoped (`src/services/ipc-client.ts:43-83`), so these take
0/1 arguments and no trailing session - unlike every other family bound so far.

`config:set` cannot carry `stageModelIds` (`transport/globalconfig.go:31`), which is why the routing
writes need their own path rather than riding the existing config binder.

**The seven execution names stay 501, deliberately:** `generate`, `generate-stream`, `cancel`,
`discover-models`, `test-connection`, `begin-execution-lease`, `close-execution-lease`. Their only real
effect is a provider round trip or a lease/stream registry whose sole consumer is the unbound `generate`
family. Binding them would mint capability this lane cannot honour - the lease pair is the one genuinely
debatable call, and it was decided fail-closed.

**LLM family status:** 9 of 13 bound, 7 left (13 unbound total including the 4 `editor-inline:*`, which
remain blocked on the generation owner).

### 2026-11-04 — six draft author/authority operations bound (durable 112 -> 118)

Green: `49fcf5d8`. New `transport/draft_author.go`, `store/draft_author.go`, `store/draft_authority.go`.

Reads (`BindDraftAuthorityReads`, arity/path `2/0, 2/0, 3/1` from `ipc-channels.ts:1031-1036`):
`db:draft-authority-sequence`, `db:draft-export-snapshot`, `db:draft-export-authority-current`.

Writes (`BindDraftAuthorWrites`, `3/1, 5/3, 3/1` from `ipc-channels.ts:1023,1099,1101`):
`db:draft-create`, `db:draft-update-status`, `db:draft-delete`.

Notable assertion detail: `db:draft-delete` on a **finalized** draft returns an `errorCode` with **no**
`"error":` key at all and the row survives - that asymmetry is the original's, and it is now pinned.
Deleting the working draft removes both the draft and its content row.

**The `db` family is now 90 of 103 served.** 27 remain: 20 are the import pipeline, 4 are the
retire-generation-runs writes, and 3 need subsystems (`project-core-commit-generated`,
`blueprint-character-sync-complete`, `review-cycle-list-pending-rechecks` - the last is an unported M03
projection).

**Import pipeline is the single largest coherent block left in this family** (20 operations, all
`db:import-run-*` plus `import-global-facts-commit` and `draft-import-finalized-batch`). It is pure Go
work - no Lance, no generation owner - which makes it the best next target by the criteria that have
actually been delivering.

### 2026-11-04 — ten import-run operations bound (durable 118 -> 128)

Green: `11194f77`. New `store/importrun.go` (1451 lines) and `transport/import_run.go`.

Bound, all plain SQLite work on the admitted author database: `db:import-run-get`, `-list-resumable`,
`-list-chapters`, `-start-resume`, `-renew-execution`, `-request-cancel`, `-cancel-at-boundary`,
`-complete-batch`, `-advance-stage`, `-complete`.

Left at 501 on purpose: the four run **creation** stages (`prepare-inspection`, `author-preview`,
`finalize-parsing`, `restart`) need the tab-bound in-memory inspection owner or a generation-source
proof; the three **effect receipts** need authoritative readback from the global-facts, blueprint-range
and finalized-draft repositories; `fail` and `import-global-facts-commit` and
`draft-import-finalized-batch` still need their own proofs.

One test defect found by CI and fixed: the "still unbound" loop kept all ten names, so the first bound
name returned 400 and the loop failed. The list is now pruned to the genuinely unbound names - and that
list is exactly the kind of thing that rots, so it is worth checking whenever a batch binds into a family
that already has a pin loop.

**Process note:** this batch was produced by a subagent whose run was *cancelled* mid-flight. It had in
fact finished the code and the seeder, but I did not notice the cancellation until the next round, so its
work sat in the tree unverified for one round. When a delegated run is cancelled, inspect the working
tree for partial output immediately rather than assuming nothing landed.

### 2026-11-04 — four small-family operations bound, and three families closed as blocked (durable 128 -> 132)

Green: `ecf599e6`. Bound: `prompt:save-global`, `prompt:delete-global`, `skills:uninstall-user`,
`project:recent-remove`. Assertions read the effects back (template files on disk, skill directory gone,
recent list empty, `project.db` untouched, second removal refusing without leaking a path).

**A negative result worth recording, because it closes three families for now.** In the recon of
`fs`, `dialog`, `project`, `prompt`, `skills` and `legacy-roster`, the `(a) implementable` group was
**4 names out of 37**. Everything else needs a subsystem:

- `fs:*` (8) - `projectfs` exposes only storage-relative `OpenFileRead` and `ListVisibleTree`; there is
  no project-root text read/write, no mkdir, no exists, no atomic write, and no external-grant service.
- `dialog:*` (9) - the pickers are native; `select-novel-files` additionally needs
  `importsource.NewStore` plus a picker this lane cannot compose.
- `legacy-roster:*` (7) - needs `main-generation-owner` (`generation-controller.ts:233-239`).
- `project:*` 5 of 6 - create/delete/peek/import-legacy-copy/archive need directory grants, deletion
  authority, or dialogs.
- `skills:*` 4 of 5 - GitHub egress or a native picker.

So these families are not "unfinished"; they are **blocked on injection points that do not exist in a
server-side lane** (native pickers, desktop dialogs, external path grants). That is an architectural
boundary of the headless Go server, not a gap in the porting work, and it should be decided by the owner
rather than ground down further: a web deployment cannot hand a user a native file dialog.

Consequence for the count: the reachable ceiling without new injection points is well below 281. The
operations that genuinely need a desktop shell should be marked as such in the matrix rather than carried
as "pending".


### 2026-11-05 — `finalization-generation:read` bound, the other ten names stay 501 (durable 132 -> 133)

Recon of the eleven server-side AI names (`review-revision:*` 4, `finalization-generation:*` 4,
`graph-generation:*` 3). Eight of them are `(c)` needs-a-subsystem, three were already bound, and one was
`(a)`:

- **Already bound:** `graph-generation:read` (`internal/transport/graph_generation.go`),
  `graph-generation:cancel`, `finalization-generation:cancel` (`BindGeneration`).
- **`(c)` blocked:** `graph-generation:begin/execute/confirm` need graph run admission with a
  `graphGenerationContext` manifest and a nonce-deduped run open (`syntheticParseSelection` refuses the
  `input`/`modelId`/`uiActionNonce` request); `finalization-generation:begin` needs
  `captureFinalizationGenerationContext` plus prompt-template rendering, `execute` a live model call, and
  `commit` the artifact readback plus `rebuildBinding`/`compareGenerationSourceBindings`; the three
  `review-revision` writes need a settled run carrying `reviewRevisionContext`, which needs
  `freezeChapterGoals`, `findBlueprintContinuityRisks`, `parseHumanConfirmedReviewSnapshot` and
  `ReviewCycleRepository.planRecheck` - none of them ported.
- **`(a)` bound:** `finalization-generation:read`. It takes `{ slot }` (not `{ handle }`), validates the
  slot exactly like `finalizationSlotKey`, re-derives the key with `graphgen.EncodeJSON`+`TextHash`
  (`textHash(JSON.stringify([draftId, finalizationId, chapterNumber, contentHash, stepKey]))`) and looks
  it up in `generation_runs` via `json_extract(binding_json,'$.sourceManifest.finalizationGenerationSlotKey')`.
  No row means JSON **null**, which is the answer `src/services/finalization-generation.ts:38` needs to
  start a finalization at all: refusing it would break the first step of the normal flow rather than
  protect anything.
  A matching row is refused, not projected. Only `finalization-generation:begin` writes that manifest key
  and this lane does not bind it, so recovering the run would mean proving
  `textHash(JSON.stringify(context))` against the stored `finalizationGenerationContextHash` - insertion
  order, which every digest in this lane deliberately canonicalises. The row is answered with
  `GENERATION_FINALIZATION_CONTEXT_INVALID` (foreign `binding.projectId`:
  `GENERATION_FINALIZATION_SLOT_CONFLICT`), so the lane never hands the renderer a context it cannot prove.

Shape notes worth keeping: the binder row is **conditional** (`FinalizationRead != nil`), because
`bindGenerationOperations` installs every row it is handed and a bound-but-unsupported row would answer
500 instead of 501. The read is `mutating: false`, so its refusals are HTTP errors, not the
`{"success":false,...}` business DTO. `familyRequestHandle` was split into
`familyRequestField(value, field, code)` so the `{ slot }` envelope reuses the same single-key rule.

Tests: `internal/transport/finalization_read_test.go` pins the unwrap, the read envelope, and that a nil
provider leaves the name unbound; the pin test grew a `graph-generation-with-finalization-read` case; the
durable HTTP test seeds one owning run in this project and one in another (row insert shape from
`contract/m03-generated-contract.mjs:81`, before `project:open` so the write admission fingerprints that
database) and asserts null / conflict / context-invalid plus ten malformed slots. `finalization-generation:read`
was pruned from the "still unbound" loop (29 -> 28) - the same rot the previous entry warns about.

### 2026-11-05 — OWNER DECISION: the target is a Dockerised Go server, browser-only, data server-side

Stated by the owner:

> 我要的就是 docker 服务跑，然后我通过浏览器就可以实现全部功能，并且我的小说内容是在服务器上（本机），
> 电脑写一半的，手机可以接着写，不要 nodejs 架构，要轻量一些的 go

This settles the question the previous entry raised, and it settles it the *second* way: the desktop-only
operations are **not** out of scope. They are to be re-expressed with web semantics, because the product is
"open a browser, from any device, and everything works, with the manuscripts living on the server".

Consequences, in order of how much they change the plan:

1. **`dialog:*` (9) becomes upload/download, not a dropped family.** A native file picker has no server
   equivalent, but its *purpose* does: choosing files to import is a browser upload (multipart), and
   choosing a save destination is an HTTP download (attachment) or a server-side path choice confined to
   the project root. These need new routes and a new browser-side caller, which is why they were never
   bindable as-is.
2. **`fs:*` (8) becomes a project-root-confined file API.** The original's external-grant model exists to
   let a desktop app reach arbitrary user paths. Server-side, the correct analogue is stricter, not
   looser: read/write/mkdir/exists confined to the storage root, with the grant model simply absent.
   This is a security improvement over the original, and a *different* contract - it must be documented
   per operation, not silently substituted.
3. **`legacy-roster:*` (7) is unchanged** - it needs the generation owner, which is real work, not a
   desktop artefact.
4. **Cross-device resumption is already the lane's design**: single-writer lease, full-body CAS, draft
   authority sequence. Nothing new is needed for "write half on the desktop, continue on the phone"
   beyond the lease and CAS that are already bound and tested.

So the reachable ceiling is **still 281**, but roughly 17 of them will be served by *web-native* contracts
rather than by porting a desktop contract verbatim. That difference must be visible in the matrix, because
"the same operation" with a different security model is exactly the kind of thing that must not be
presented as a faithful port.

### 2026-11-05 — finalization-generation:read bound (durable 133), and a silent-failure hazard

Green: `60f9e845`.

One defect cost two CI cycles and is worth recording: the binder appends the row only when its optional
provider field is non-nil (`transport/graph_generation.go:40` `if provider.FinalizationRead != nil`). The
durable opener passed only `Read`, so the row was never appended, `Server.Bind` never saw the name, and the
route answered 501 - **with no startup error**. Optional provider fields in this codebase fail silently:
the only things that catch them are non-501 assertions and the `BoundOperations` pins. When adding an
operation to a binder with optional fields, wire the field in the same commit as the registration.

The read itself: `{ slot }`, arity 1, non-mutating, answering JSON `null` for a slot no run owns,
`GENERATION_FINALIZATION_SLOT_CONFLICT` for a foreign project, and `GENERATION_FINALIZATION_CONTEXT_INVALID`
for a run whose manifest this lane cannot reproduce (the insertion-order digest the desktop build froze).

**The eleven AI-generation names are now accounted for:** 3 bound (`graph-generation:read`,
`graph-generation:cancel`, `finalization-generation:read`), 8 left at 501 because they need a model
dispatch or a settled-artifact proof, and `review-revision` (4) needs four unported composition helpers
(`freezeChapterGoals`, `findBlueprintContinuityRisks`, `parseHumanConfirmedReviewSnapshot`, `planRecheck`).

### 2026-11-05 — web-native file access landed (durable 133 -> 137)

Green: `67c0571f`. This is the first slice built for the owner's actual product shape - Dockerised Go
server, browser only, manuscripts on the server.

Bound with **relative, root-confined** paths: `fs:read-file`, `fs:write-file`, `fs:mkdir`,
`fs:check-exists`. New routes: `POST /api/uploads?path=<relative>` (multipart, CSRF + Origin checked,
201 with `{success,path,name,size}`) and `GET /api/downloads/<relative>` (attachment, `no-store`,
`nosniff`). Both resolve the project from the caller's live durable lease, never from the request path.

**Confinement (`internal/projectfs/browser_files.go`), deliberately stricter than the desktop contract:**
refuses empty/oversize/invalid-UTF-8/NUL/backslash/absolute input, `..`, non-canonical spellings, any
component beginning with `.` (so `.ai-novel` is unreachable), and storage-reserved names (`project.json`,
`project.db*`); every hop is pinned with `Lstat` + `OpenRoot` + same-file re-check so symlinks cannot
escape; writes are temp + fsync + rename inside the same pinned parent, then read back. The tests assert
**refusal and on-disk absence** for traversal, absolute paths (including the project's own file), and a
real symlink to the parent root.

**Still 501 and honestly so:** `fs:write-json` (its only caller writes the storage marker), the three
`fs:grant-*` (a browser has no server-issued external grant), all nine `dialog:*`. Host-folder pickers
(`select-folder`, `select-legacy-project`, `select-project-restore-target`,
`select-project-archive-export`) have no web equivalent at all - upload/download replace the *file*
pickers, not the *folder* pickers. Those need their own design decision: a server-side directory browser
confined to the storage root is the natural answer, but it is a new UI surface, not a port.

Known risks recorded: the family is now mixed (`fs:list-dir` / `fs:read-json` still take absolute paths),
no per-file mutex beyond the lane's single writer lock, and the download route has no expiring token -
lease liveness is the only gate.

### 2026-11-05 — first real local run of the Go image: it works, but there is no browser UI

Built by CI (`9c574f31`), exported as an artifact, downloaded, `docker load`ed and run here. This is the
first time the Go server has been exercised outside a test harness, and it changed the picture.

**What works.** The container starts and serves HTTP. Authentication is a trusted-proxy identity header:
`security.Authenticate` (`internal/security/security.go:67`) reads `X-Novel-User` and fails closed with
401 `AUTHENTICATION_REQUIRED` - correct for a server behind a reverse proxy, and it means the browser
cannot talk to it directly without that proxy. With the header, `GET /api/bootstrap` returns a CSRF token
and the runtime descriptor, and `GET /api/library` answers `CLIENT_ID_REQUIRED` (the single-writer client
identity doing its job).

**What does not work, and it is the whole product.** `GET /` returns **404 `NOT_FOUND`**. The binary
serves no frontend: `-static-dir` (`cmd/novel-server/main.go:38`) defaults to empty and the image ships no
built assets. **So the current image is an API-only server; there is nothing to open in a browser.**

That single fact outranks every remaining 501. The owner's requirement is "open a browser and write"; an
API that passes 137 operation tests cannot satisfy it.

**Consequences, which reorder the remaining work:**
1. Build the React app in CI (npm runs there, not here), copy the output into the Go image, pass
   `-static-dir`. Until then nothing is browser-usable.
2. Establish how the browser authenticates through the proxy in the local deployment (the compose file
   currently fronts the *Node* container; the proxy must inject `X-Novel-User` for the Go one).
3. The React side must speak the HTTP transport to this server. `web/browser/browser-transport.mjs`
   exists, so the piece may already be written - verify rather than assume, and verify it against this
   binary rather than against a mock.
4. Only then does the operation count mean anything, because only then can a real author click through.

**Artifact route (recorded because it unblocks local verification):** GHCR pulls need the
`read:packages` scope, which the available token lacks. `.github/workflows/go-backend.yml` now exports the
smoke-tested container as `novel-go-image` (about 43 MB gzipped); `gh run download -n novel-go-image` plus
`docker load` gives the same image with repository-scoped credentials only. The earlier attempt to add this
to `native-web-image.yml` was reverted: that workflow's publishing job builds the **legacy Node** image and
only runs on manual dispatch.

### 2026-11-05 — the Go image now serves the React app (browser path unblocked)

Green: `9947b504`, all three jobs, including the container smoke.

**What changed.** `Dockerfile.go` gained a `node:22-bookworm-slim` build stage that reuses the repo's own
frontend recipe (`pnpm install --ignore-scripts --frozen-lockfile` then
`vite build --mode web --config vite.web.config.ts`, exactly as `Dockerfile.web:12-17,36`), and copies
`/build/dist-web` into the final `ubuntu:24.04` stage, which runs the server with
`-static-dir /app/dist-web`. Node never reaches the runtime stage, so the existing "no node/npm in the
container" assertion still holds.

**Static serving had three real gaps, now fixed** (`internal/transport/static.go`): client-side routes
returned 404 (no SPA fallback - now extensionless unresolved paths and trailing-slash routes fall back to
`index.html`, while 400/403 and dotted paths keep their status); every asset got a flat `max-age=3600`
(now `assets/<name>-<8-hash>.*` is one-year immutable, `index.html` is `no-store`, other public files 1h);
and the MIME table lacked fonts/images/maps, so the CJK reading faces shipped as
`application/octet-stream`. All 193 built asset names match the hash pattern.

**CI now proves it on the built image** (`go-backend.yml:597-616`): `/` is 401 bare and 200 `text/html`
with the header; a deep client route is byte-identical to the index; a hashed asset returns 200
`text/javascript`; and the container still has no `node`/`npm`.

**Deployment consequence, now explicit:** static files require `X-Novel-User` just like `/api/*`, and the
server refuses non-loopback listeners (`main.go:57-59`). So the deployment must run the container with
`--network host` (as the CI smoke does) and put an authenticating proxy in front that **overwrites** the
header - `docker/nginx-ai-novel-writer-web.conf` already does Basic Auth into
`proxy_set_header X-Novel-User $remote_user`; it must be retargeted from the Node port to
`127.0.0.1:16082`. `-p 16082:16082` cannot work, because the process binds loopback inside its network
namespace.

**Cost:** the image and the exported artifact grow by roughly 96 MB, most of it the CJK font files under
`public/fonts`. The container job's timeout went 15 -> 25 minutes for the cold `pnpm install` + Vite build.

### 2026-11-05 — ACCEPTANCE FINDING: the shipped image serves 279 of 281 operations as 501

Measured by replaying the frontend's real call sequence against the actual image (`novel-go:migration-ci`,
no special flags). This outranks every progress number recorded above.

**Startup.** `GET /` 200, `/api/bootstrap` 200, `/api/events` 200 (SSE). Then `startup:get-state` ->
**501**, which blocks `RendererStartup`, so `App` never mounts. The user sees the "Startup is incomplete"
screen and nothing downstream is reachable.

**Create / write / save.** `dialog:select-folder` 501 -> no parent grant, so create stays disabled in the
UI; `project:create` 501 (and it has **no Go handler at all**, not merely unbound); `project:open` 501;
`db:draft-list`, `db:draft-get-full`, `db:draft-create`, `db:draft-update-content`,
`db:blueprint-get-all` all 501. `POST /api/release` is 501 `GO_ROUTE_NOT_MIGRATED`.

**Inventory sweep:** 279/281 operations answer 501 as shipped. Adding `-synthetic-library-dir`,
`-synthetic-global-dir`, `-synthetic-model-origins` brings 9 more online (`startup:*`, `config:get/set`,
the three `llm` model names). **The "137 durable" figure is a synthetic test lane, not production
behaviour** - it requires flags and synthetic databases, and without them the same binary serves almost
nothing. The earlier claim that 144 remain was wrong; the real number is 279.

**Two protocol-level defects, independent of unimplemented operations:**
1. the frontend bundle sends only `X-Client-Id` / `X-CSRF-Token`; the server requires proxy-injected
   `X-Novel-User` on **every** route, so served by its own entrypoint the app 401s everywhere. The image
   alone cannot serve the frontend - the authenticating proxy is mandatory, not optional.
2. browser dialogs go through `/api/dialogs/<id>`, a route that does not exist server-side (501
   `GO_ROUTE_NOT_MIGRATED`), so folder/library choosing has no backend at all.

**What this means for the plan.** The remaining work is not "bind another family". There is a missing
layer: a **production (non-synthetic) repository and admission path** that turns the same store code on
for a real data directory without the test-only flags, plus the routes the browser actually calls. Until
that exists, operation counts are meaningless and the deployment cannot be handed to anyone.

This supersedes the "137 of 281" framing everywhere it appears above.

### 2026-11-05 — a production lane exists at last: `NOVEL_DATA_ROOT`

Green: `46fb2972`.

**The gap that was measured last round is now partly closed.** `main.go` previously refused every data
mount (`:49-51`) and served only auth/bootstrap/health. Now `NOVEL_DATA_ROOT` selects a real lane
(`production_open_linux.go`, 418 lines + 348 test), and `NODE_ENV=production` **without** a data root is
refused rather than silently degraded.

**What a data root must contain:** `config/ai-novel-writer/.migration/receipt.json`,
`generations/<uuid>/config.json`, a distinct `config/ai-novel-writer-web-runtime`, optional
`legacy-global`, and `workspaces/<name>/.ai-novel/{project.json,project.db}`. It **creates nothing** - no
`MkdirAll`, no `WriteFile`. Each project database is byte-copied into a private 0700 scratch directory and
verified against the pinned v7 contract; **the author's database is never opened in place**. An
inadmissible library or a missing receipt is a hard startup failure, which is the right default for a
server that must not damage manuscripts.

**Now 200 in production:** `startup:get-state`, `startup:appearance-ack`, `config:get`,
`project:recent-list`, `GET /api/library`, static/bootstrap/health, and the two fixed external-link
intents. These are real reads of the receipt, `config.json`, and DB-verified projects - not fixtures.

**Still 501, and this is the honest boundary:** `project:create`, `project:open`, `db:draft-*` and
`fs:list-*`, because there is no project catalog, no lease/peek/lifecycle provider, and no author-runtime
publication path outside the synthetic lanes. Also `config:set`, which would write into the real
generation directory.

**So the browser still cannot write a novel**, but it can now get past the startup screen and list a real
library - which is the first time any production behaviour has worked. The next layer is the same shape:
catalog + lease + author-runtime publication for the real data root, not more synthetic binders.

### 2026-11-05 — production lane opens projects and writes drafts (17 operations)

Green: `90dad2cd`.

**What was synthetic-only, now real.** The synthetic lane reached author bytes through flag roots, a
seeded `baseline-v7.db`, and a scratch copy reopened per read. Production now derives every path from
`NOVEL_DATA_ROOT` (`productionLayout`) and opens the author's own `project.db`. Catalog entries are built
at startup from `projectfs.Library` (`production_durable_linux.go:106-123`) and keyed to the trusted path,
so a browser-supplied path selects a key but never *becomes* one.

**Lease.** One exclusive `writerlock.AcquireLegacy` is taken before any database is touched
(`production_open_linux.go:141`) and transferred to the runtime, which owns the only `Close`: `Shutdown`
closes every admitted database and then releases (`:359`); a runtime that cannot prove that keeps the
lease (`errProductionLeaseRetained`). A second server gets `writerlock_busy`; a stale lease is never
stolen because the entry is only ever acquired, never repaired. Per-project, `ActiveCoordinator` refuses a
second owner for the same path before any SQL (`production_durable_linux.go:183-187`).

**Save is full-body CAS:** `CommitDraftContent` -> `CommitDraftCASDurable` ->
`store.Drafts.UpdateContentIfCurrent` (BEGIN IMMEDIATE, exact baseline compare, receipt fence, lease
rotation) at `:260-298`. Reads hold the same lease (`internal/authorruntime/draft_read.go:46`).

**17 operations now 200 in production:** `project:open`, `db:close`, `project:get-runtime-context`,
`project:save`, `project:update-config`, the eight `db:draft-*` reads, `db:draft-update-content`,
`db:draft-create`/`-update-status`/`-delete`.

**`project:create` stays 501 on purpose:** a newly created database could not honestly pass the pinned v7
fingerprint (that needs the schema oracle this lane does not own), so a created project would be
un-admissible on restart - a false success, worse than a refusal. `project:recent-remove` stays 501: the
shelf is derived from the library, there is no persisted navigation document.

**Safety tests** (temp root only): the root takes the lease and a second `AcquireLegacy` gets `ErrLocked`;
refused startup and clean shutdown both release; a save persists into `project.db` (verified on disk) and
reads back under the rotated lease; a rotated-away lease is refused with the body unchanged; a stale
baseline under a live lease is `DRAFT_CONTENT_CONFLICT` with the body unchanged; a second tab gets
`PROJECT_OPEN_IN_ANOTHER_TAB`.

**Highest residual risk:** this lane writes real author databases. Misordered shutdown would release the
lease before the handles (guarded only by `Shutdown`'s error path), and a crash leaves a WAL the next
start must recover - the pinned verification refuses rather than repairs.

### 2026-11-05 — the production journey now passes inside the real container

Green: `2a34bb7e`, and the third job is renamed to say so: "Unpublished Go container with disposable
smoke and production journey".

This is the first time the writing journey has been proven in the **shipped image** rather than in a Go
test or a replayed curl script. The job builds a synthetic production data root with a new CI-only helper
(`backend-go/cmd/production-fixture/main.go`), whose only schema input is the TypeScript lane's own
`baseline-v7.db` from `export-sqlite-contract.mjs` - no DDL is re-declared, and the helper re-proves the
result against `admission.PinnedSchemaContract` before the server starts. The tree is chowned to uid 65532
with group/world bits stripped, because the receipt, writer lock and project files are checked against the
runtime's own euid.

Then, against the container on `--network host` with `NOVEL_DATA_ROOT`: bootstrap 200, `project:open` 200,
`db:draft-list` 200, `db:draft-update-content` 200, `db:draft-get-full` 200 - every step asserted exactly,
so a 501 or a `success:false` fails the job. The saved and read-back CJK text is printed in the step
summary, and the body is also found in the mounted `project.db`/WAL on the host, proving it is durable
outside the process. Bootstrap reporting `go-production-admitted` is what distinguishes this from a
synthetic lane.

**What this settles:** a Docker container, no Node inside it, browser-facing, opens a project, saves a
chapter with CAS, and reads the exact text back. That was the owner's stated requirement, and it now has
CI evidence rather than a claim.

**What it does not settle:** 264 of 281 operations still answer 501, the editor's other reads (characters,
overview, navigation, reviews, generation) are absent, and no real author data has been through this lane.
Switching the production deployment is still wrong.

