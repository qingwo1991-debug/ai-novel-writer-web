# Go migration — evidence-based gap matrix

Baseline: `HEAD=9cc21933` == `origin/main`. Durable author write lane landed and CI-green
(`54b5b754` bridge + HTTP test, `3bace738` open-error redaction, `9cc21933` docs).

## Inventory

`web/operations.json:42-324` = **281 operations** (217 project / 42 global / 22 flexible scopes).

## Three tiers

### Tier A — bound at the transport level: 110 ops via 35 binders

### Tier B — wired into a running lane: 38 ops total

| Lane | Ops | Bindings |
|---|---|---|
| **durable** (`migrationHandlerWithDurable`, `synthetic_durable_open_linux.go:310`, from `main.go:116-117`) | **7** | `BindNavigation` (`:322`) + `BindProjects` (`:347`): `project:open`, `project:save`, `project:update-config`, `project:get-runtime-context`, `db:close`, `official-homepage:open`, `model-provider-resource:open` |
| read-only (`main.go:315-362`) | 28 | 8 draft reads + `db:draft-update-content` CAS (`syntheticReadOpen.CommitDraftContent` → `authorruntime.CommitDraftCAS`), character reads, editor reads, `project:recent-list`, `fs:list-dir`, `fs:read-json`, `project:overview-current` |
| model (`main.go:275-278`) | 9 | config/startup/`llm:list-models`/`llm:save-model`/`llm:delete-model` |

### Tier C — bound at transport but wired into NO lane: 72 ops

`BindDrafts`, `BindCharacters`, `BindBlueprints`, `BindNarrative`, `BindPlotTree`, `BindPostHistory`,
`BindRecovery`, `BindSummary`, `BindProjectCore`, `BindReviewRevisions`, `BindKnowledge`,
`BindKnowledgeSearch`, `BindMCP`, `BindPromptSkills`, `BindImportSelection`, `BindArchiveExport`,
and `project:recent-remove` (no provider implements `RemoveRecentProject` anywhere — never live).

### Tier D — no Go handler at all: 171 ops

Return `501 GO_OPERATION_NOT_MIGRATED` (`transport/server.go:97-100`); unknown ops → `404 OPERATION_NOT_EXPOSED`.

## Cheapest next win: wire the read binders into the durable lane (21 ops)

The durable `authorruntime.ReadLifecycle` **already implements** the readers:

- `ReadDraftJSON` (`draft_read.go:17`)
- `ReadCharactersJSON` (`character_read.go:57`)
- `ReadEditorStageJSON` (`editor_stage_read.go:16`)
- `ReadReviewRevisionJSON` (`editor_review_read.go:17`)
- `ReadReviewRevisionFullJSON` (`editor_review_full_read.go:16`)
- `ReadAbsentReviewCycleJSON` (`editor_review_cycle_read.go:16`)
- `ReadCurrentOverviewJSON` (`project_overview_read.go:17`)
- `ReadArchCheckpoint` (`editor_checkpoint.go:22`)
- `BeginKnowledgeRead` (`knowledge_read_linux.go:30`)

Binders to add (disjoint from `BindProjects`; the read-only lane already proves them against the
same lifecycle): `BindDraftLiveReads` (8), `BindCharacterLiveReads` (3), `BindEditorStageReads` (2),
`BindEditorReviewReads` (3), `BindEditorReviewFullReads` (2), `BindEditorReviewCycleAbsentRead` (1),
`BindCurrentOverview` (1), `BindEditorCheckpoint` (1) = **21 ops**.

## Draft CAS write (medium effort)

`db:draft-update-content` needs `DraftCommitService` (`draft_commit_linux.go:59`) plus a
`CommitDraftContent` adapter in the durable builder. The read-only lane proves the pattern.

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
