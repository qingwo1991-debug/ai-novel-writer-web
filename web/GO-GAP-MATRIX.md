# Go migration — evidence-based gap matrix

Baseline: `HEAD=9cc21933` == `origin/main`. Durable author write lane landed and CI-green
(`54b5b754` bridge + HTTP test, `3bace738` open-error redaction, `9cc21933` docs).

## Inventory

`web/operations.json:42-324` = **281 operations** (217 project / 42 global / 22 flexible scopes).

## Three tiers

### Tier A — bound at the transport level: 110 ops via 35 binders

### Tier B — wired into a running lane: 42 ops total

| Lane | Ops | Bindings |
|---|---|---|
| **durable** (`migrationHandlerWithDurable`, `synthetic_durable_open_linux.go`, from `main.go:116-117`) | **31** | `BindNavigation` + `BindProjects` (7: `project:open`, `project:save`, `project:update-config`, `project:get-runtime-context`, `db:close`, `official-homepage:open`, `model-provider-resource:open`) + 8 disjoint editor read binders landed in `af31c463` (20: 8 draft reads, `project:recent-list`, `fs:list-dir`, `project:overview-current`, `fs:read-json`, `db:project-core-get`, `db:blueprint-get-all`, `db:revision-get-pending`, `db:revision-list`, `db:review-list`, `db:review-get-full`, `db:revision-get-full`, `db:review-cycle-get`) + `BindCharacterLiveReads` (3: `db:character-get-all`, `db:character-roster-read`, `character-identity:read`; served by `syntheticDurableOpen.ReadCharactersJSON` → `authorruntime.ReadCharactersJSONDurable` on the admitted author DB) + `BindDraftLiveWrites` (1: `db:draft-update-content`; served by `syntheticDurableOpen.CommitDraftContent` → `authorruntime.CommitDraftCASDurable` → `store.Drafts.UpdateContentIfCurrent`, with the lease rotated in place on the same `*admittedAuthor`) |
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
| db | 103 | 71 | 1 | 32 |
| generation | 23 | 0 | 0 | 23 |
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
| graph-generation | 5 | 0 | 0 | 5 |
| finalization-generation | 5 | 0 | 0 | 5 |
| finalized-character | 5 | 0 | 0 | 5 |
| chapter / character-avatar / editor-inline / review-revision | 4 each | 0 | 0 | 16 |
| prompt / startup / character-identity / config / model-provider-resource / official-homepage | — | all | nav+startup | 0 |
| **TOTAL** | **281** | **110** | **31** | **171** |

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
