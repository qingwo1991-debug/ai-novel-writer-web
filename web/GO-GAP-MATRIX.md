# Go migration — evidence-based gap matrix

Baseline: `HEAD=6f1bc572` == `origin/main`, CI run `41782708522` fully green (S1 generation lane live)
(all three *Go backend migration* jobs). Durable author write lane landed and CI-green
(`54b5b754` bridge + HTTP test, `3bace738` open-error redaction, `9cc21933` docs), and it
serves **63 operations** over real HTTP.

Current stopping point, what is deliberately quarantined and the exact resume plan:
**`web/GO-PROGRESS-HANDOFF.md`**. The counts below are unchanged by that checkpoint: the
eleven S1 generation operations were briefly wired and then unbound again in `059c57a2`
because their run view reported no attempts after `execute`, so they are still counted
as unbound.

## Inventory

`web/operations.json:42-324` = **281 operations** (217 project / 42 global / 22 flexible scopes).

## Three tiers

### Tier A — bound at the transport level: 110 ops via 35 binders

### Tier B — wired into a running lane: 42 ops total

| Lane | Ops | Bindings |
|---|---|---|
| **durable** (`migrationHandlerWithDurable`, `synthetic_durable_open_linux.go`, from `main.go:116-117`) | **63** | `graph-generation:cancel` and `finalization-generation:cancel` (same frozen-handle stop path as `generation:cancel`) + `BindReviewRevisionRemainder` (9: `db:review-create`, `db:review-get-latest`, `db:review-next-index`, `db:revision-create`, `db:revision-replace-pending`, `db:revision-next-index`, `db:revision-merge`, `db:revision-mark-merged`, `db:revision-mark-discarded`; the whole review surface already exists as `BindReviewRevisions`, but five of its reads are served by the editor binders, so the durable lane binds exactly the remainder) + `BindSummary` (6: `db:get-latest-summary`, `db:save-summary-snapshot`, `db:continuity-list-before`, `-read-source`, `-save-finalized`, `-save-character-state-candidates`; served by `syntheticDurableOpen.BeginSummary` → `authorruntime.SummaryRepositoryDurable`) + `BindRecovery` (4: `db:recovery-candidate-record`, `-list`, `-update`, `-resolve`; served by `syntheticDurableOpen.BeginRecovery` → `authorruntime.RecoveryRepositoryDurable`) + `BindGeneration` (11: `generation:begin`, `generation:execute`, `generation:read`, `generation:list`, `generation:pause`, `generation:cancel`, `generation:resume`, `generation:restart`, `generation:discard-candidate`, `generation:bind-material-decision`, `generation:retry-draft-short-outline`; served by `synthetic_generation_linux.go` over the real `internal/generation` + `generationowner` services with a registry-gated model dispatch) + `BindNavigation` + `BindProjects` (7: `project:open`, `project:save`, `project:update-config`, `project:get-runtime-context`, `db:close`, `official-homepage:open`, `model-provider-resource:open`) + 8 disjoint editor read binders landed in `af31c463` (20: 8 draft reads, `project:recent-list`, `fs:list-dir`, `project:overview-current`, `fs:read-json`, `db:project-core-get`, `db:blueprint-get-all`, `db:revision-get-pending`, `db:revision-list`, `db:review-list`, `db:review-get-full`, `db:revision-get-full`, `db:review-cycle-get`) + `BindCharacterLiveReads` (3: `db:character-get-all`, `db:character-roster-read`, `character-identity:read`; served by `syntheticDurableOpen.ReadCharactersJSON` → `authorruntime.ReadCharactersJSONDurable` on the admitted author DB) + `BindDraftLiveWrites` (1: `db:draft-update-content`; served by `syntheticDurableOpen.CommitDraftContent` → `authorruntime.CommitDraftCASDurable` → `store.Drafts.UpdateContentIfCurrent`, with the lease rotated in place on the same `*admittedAuthor`) |
| read-only (`main.go:315-362`) | 28 | 8 draft reads + `db:draft-update-content` CAS (`syntheticReadOpen.CommitDraftContent` → `authorruntime.CommitDraftCAS`), character reads, editor reads, `project:recent-list`, `fs:list-dir`, `fs:read-json`, `project:overview-current` |
| model (`main.go:275-278`) | 9 | config/startup/`llm:list-models`/`llm:save-model`/`llm:delete-model` |

### Tier C — bound at transport but wired into NO lane: 72 ops

`BindDrafts`, `BindCharacters`, `BindBlueprints`, `BindNarrative`, `BindPlotTree`, `BindPostHistory`,
`BindRecovery`, `BindSummary`, `BindProjectCore`, `BindReviewRevisions`, `BindKnowledge`,
`BindKnowledgeSearch`, `BindMCP`, `BindPromptSkills`, `BindImportSelection`, `BindArchiveExport`,
and `project:recent-remove` (no provider implements `RemoveRecentProject` anywhere — never live).

### Tier D — no Go handler at all: 171 ops

Return `501 GO_OPERATION_NOT_MIGRATED` (`transport/server.go:97-100`); unknown ops → `404 OPERATION_NOT_EXPOSED`.

## Durable character reads and draft CAS (landed on top of `af31c463`)

The durable lane now binds its remaining stateful reads/writes without replacing its write
admission:

- `BindCharacterLiveReads` → `syntheticDurableOpen.ReadCharactersJSON` →
  `authorruntime.ReadCharactersJSONDurable` (`character_read.go`). The three character reads run on
  the admitted author database under the frozen cookie/tab/lease fence. The legacy writer lock is
  process-exclusive, so the repository's `BEGIN IMMEDIATE` multi-query reservation is safe on the
  author's own `mode=rw` connection and no private snapshot copy is needed.
- `BindDraftLiveWrites` → `syntheticDurableOpen.CommitDraftContent` →
  `authorruntime.CommitDraftCASDurable` (`durable_draft_commit_linux.go`). The full-body CAS runs
  through the single author-save path `store.Drafts.UpdateContentIfCurrent` against the admitted
  author DB (baseline compare, receipt/content-ownership fences, both updates in one
  `BEGIN IMMEDIATE`). On success the permit adopts the new committed digest and the frozen lease
  rotates in place: the same `*admittedAuthor` handle stays installed, so `project:save` keeps
  working and the session is never downgraded to a read-only handle. A stale or replayed baseline
  fails exactly like the read-only lane (`DRAFT_CONTENT_CONFLICT`).

## Implemented in runtime but has NO transport binder (Tier D gating work)

| Chain | Runtime entry point |
|---|---|
| Generation | `internal/generation/service.go:282 Execute`, `generationowner` dispatch/lease, `store/generation_ledger.go` |
| Finalization | `store/finalization_outbox.go:87` |
| Postprocess | `store/posthistory.go:26` |
| Review writes | `store/reviewrevision.go:35` |
| Import runs | (import runtime) |
| WebDAV | `internal/webdav/client.go` |
| Knowledge-base writes | `knowledgecopy/save.go:26` |
| Skills install | `promptskills/install_linux.go` |

## Denial-stub operations (return business-error DTOs with 200, not 501)

- `db:character-roster-commit` (`characters.go:118-126`)
- `db:blueprint-update-notes` (`blueprints.go:26`)
- `db:plot-tree-save` (`plottree.go:22`)
- `db:continuity-save-finalized` + `db:continuity-save-character-state-candidates` (`summary.go:124-126`)

`db:review-cycle-get` is bound but only the absent-cycle case; an existing cycle → 501
(`editor_review_cycle_read.go:66-67`).

## Per-domain summary (operations.json vs bound vs durable)

| Domain | Total | Bound | Durable | Unbound (501) |
|---|---:|---:|---:|---:|
| db | 103 | 71 | 20 | 12 |
| generation | 23 | 0 | 12 | 11 |
| llm | 16 | 3 | 0 | 13 |
| kb | 19 | 5 | 0 | 14 |
| project | 13 | 8 | 4 | 5 |
| agent-generation | 8 | 0 | 0 | 8 |
| cloud-backup | 8 | 0 | 0 | 8 |
| dialog | 9 | 1 | 0 | 8 |
| mcp | 8 | 8 | 0 | 0 |
| fs | 10 | 2 | 0 | 8 |
| legacy-roster | 7 | 0 | 0 | 7 |
| character-proposal | 6 | 0 | 0 | 6 |
| graph-generation | 5 | 0 | 1 | 4 |
| finalization-generation | 5 | 0 | 1 | 4 |
| finalized-character | 5 | 0 | 0 | 5 |
| chapter / character-avatar / editor-inline / review-revision | 4 each | 0 | 0 | 16 |
| prompt / startup / character-identity / config / model-provider-resource / official-homepage | — | all | nav+startup | 0 |
| **TOTAL** | **281** | **99** | **63** | **119** |

> The per-domain split above is indicative; the TOTAL row is the authoritative count and is
> re-derived from the registry, while individual domain rows are updated only when a batch is
> reviewed. `generation` now carries the eleven S1 names plus `RecordEffect`; `graph-generation`
> and `finalization-generation` each carry only their `cancel`.

## Runtime exists but no binder (Tier D gating)

Generation (`internal/generation/service.go:282` `Execute`, `generationowner` dispatch/lease,
`store/generation_ledger.go:242`, reachable via `projectsession.Manager.BeginGenerationLedger`),
LLM execution (`internal/llm/*`), review/revision writes (`store/reviewrevision.go:35`),
postprocessing (`store/posthistory.go:26`), finalization outbox (`store/finalization_outbox.go:87`),
import inspection (`internal/importsource/`), archive export (`internal/archiveformat/export.go:98`),
WebDAV (`internal/webdav/client.go:246`), prompts/skills (`internal/promptskills/*`),
MCP (`internal/mcp/manager.go:57`), knowledge saves (`knowledgecopy/save.go:26`),
vector search (`knowledgequery/vector_execute_native.go:21`).

Note: `internal/importpreview/` is an **empty directory** — import-run preview runtime does not exist,
and `store.Drafts` has **no** create/delete methods (drafts.go is read+update only).


### Bound is not the same as exercised

Of the **63** operations the durable lane binds, only **32** are actually invoked end to end by
the synthetic HTTP test; the rest are bound and pinned but never observed succeeding over real
HTTP. That distinction matters because the failures this migration has hit so far were all of the
"bound, looked fine, did nothing" kind:

- `ListRuns` leaked an open read transaction, and the second generation run in the same session
  could never start.
- `syntheticSlice` silently projected budget diagnostics to empty, so the test panicked on an
  empty list rather than on a wrong value.
- `pause` read `result["run"]` from a call that returns a budget map, so a committed operation
  reported `GENERATION_RUN_NOT_FOUND`.
- `BindReviewRevisions` bound fourteen operations that were never wired, and every test passed.

So the acceptance bar for each batch is **a positive assertion over real HTTP**, not a 501
assertion and not a unit test against a stub. The unwired remainder is:

| Bound but unexercised | Count |
|---|---|
| `db:recovery-*` (4) | 1 exercised |
| `db:*summary*` / `db:continuity-*` (6) | 1 exercised |
| `db:revision-*` / `db:review-*` (9) | 2 exercised |
| editor and character reads (23) | 5 exercised |
| `graph-generation:cancel`, `finalization-generation:cancel` (2) | 0 — only "not 501" |

Closing this is cheap and independent of new feature work: each one needs a seeded fixture row
and one `rpc(...)` assertion. It should be done before the family work adds more surface.
