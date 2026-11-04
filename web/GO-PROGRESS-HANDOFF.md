# Go migration — progress checkpoint and handoff

Written at a deliberate stopping point (the user asked to wrap up this phase, push
everything, and resume later). Everything below is committed and pushed; `main` is
CI-green. Nothing here claims the migration is complete.

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
