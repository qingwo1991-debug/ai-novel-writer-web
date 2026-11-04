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
