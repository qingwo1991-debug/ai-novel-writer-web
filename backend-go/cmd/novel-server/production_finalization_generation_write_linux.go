//go:build linux

package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the write half of the finalization generation family. It serves
// none of its three names - finalization-generation:begin, :execute and :commit
// still answer the fail-closed 501 GO_OPERATION_NOT_MIGRATED
// (internal/transport/server.go:116) - and it exists because the ledger reason
// recorded in production_finalization_generation_linux.go does not survive a
// re-test, and a wrong seam is worse than an unbound name: the next change
// spends its time re-proving a claim instead of writing the missing code.
//
// # The re-tested claim: the ledger can open this run
//
// The family file said the ledger admits the slot key for one operation only and
// therefore could not freeze the chapter_notes step. That is not true of this
// tree:
//
//   - glFamilyManifestAllowed (internal/store/generation_ledger.go:426) is the
//     one manifest allowlist, and the seven keys this family freezes
//     (finalizationGenerationSlotKey, ...Slot, ...Context, ...ContextHash,
//     ...OriginEpoch, ...Task, ...TaskHash) are admitted for exactly the two
//     operations that own them, `finalized-character-state` and
//     `finalized-chapter-notes` (:447). The slot key is no longer a
//     one-operation key, and neither the context nor the task is.
//   - glPlanning (:492) is a name test - "planning", "blueprint" or
//     "plot-outline" as a substring - and neither finalization operation
//     contains one, so the gate applied at :518 (binding validation) and :694
//     (open) does not reach this family at all.
//   - The other arm of glOrdinaryBinding refuses a finalization key for every
//     other operation, which is the opposite statement: for these two names the
//     key is legal.
//
// So the pattern production_editor_inline_write_linux.go:660 uses for
// editor-inline:begin - a hand-written request through the ledger's own
// CallJSON("open", ...), with the manifest the reference freezes spliced in as
// the exact bytes it hashed - can name a finalization run. That is not a
// deduction here: TestProductionFinalizationGenerationWriteLedgerProbe opens
// both operations, with all seven keys, on a copy of an admitted file, and it
// fails the day the allowlist narrows again. This file therefore no longer
// records the ledger as the seam, and the family file's row that pointed at it
// is dropped in the same change.
//
// # The field that is genuinely refused
//
// begin cannot be served, and the blocker is one field of the frozen context:
// `template`.
//
// captureFinalizationGenerationContext
// (electron/services/finalization-generation-source.ts:26-37) freezes seven
// members - slot, identity, chapterTitle, chapterEntities, writingLanguage,
// template, notesBaseline. Six of them have a Go producer:
//
//	slot            syntheticFinalizationSlotKey, which the served read already
//	                validates (synthetic_graph_linux.go)
//	identity        store.FinalizedCharacters.ReadContext
//	                (internal/store/finalized_character_context.go, the value
//	                production_finalized_character_linux.go already serves)
//	chapterTitle    finalization_outbox.chapter_title
//	chapterEntities blueprints.characters
//	writingLanguage the project core the reduced lane already reads
//	notesBaseline   blueprints.notes plus the draft's summary_snapshots rows
//
// The template does not. It is the resolved builtin asset for the step
// (`importPrompts[stepKey === 'chapter_notes' ? 'generate_chapter_notes' :
// 'update_character_cards']`, :29), and it exists only in TypeScript:
// src/services/builtin-prompt-templates.ts:1063 and :1119, with the en-US
// overlay in src/services/prompt-language.ts. This module ships exactly one
// prompt asset (internal/prompttemplate/edit-selected-text-v1.json) and resolves
// exactly one template (EditSelectedTextTemplate,
// internal/prompttemplate/editselectedtext.go:165); nothing under
// internal/prompttemplate names either key. So the freeze is blocked by one
// field, not by the ledger and not by the shape of the request: the wire request
// is {slot, modelId, parentRootActionId?} (src/shared/finalization-generation.ts:40)
// and the reduced selection allowlist only constrains the lane's own internal
// selection, which a hand-written manifest does not have to carry.
//
// The task goes with it. finalizationGenerationTask
// (finalization-generation-source.ts:47) is renderPrompt over that template
// (:55) plus a bilingual output contract spliced into the user message (:50-51),
// and the reference freezes it by hash and reads it back by hash
// (electron/services/generation-source-binding.ts:587-591;
// electron/services/finalization-generation.ts:46-49 raises
// GENERATION_FINALIZATION_TASK_INVALID for a run whose task is absent or does not
// hash). A begin that froze a Go-authored task would ask the model a question
// the desktop never asks; a begin that omitted it would hand back a run the
// family's own task read refuses. Porting those two templates and that contract
// is the one change that lifts begin - writing the prose here instead would make
// this lane the prompt author it forbids itself from being.
//
// # The second refusal, in a file this change does not own
//
// Even with the template, a begun run would be unobservable. The served
// finalization-generation:read refuses every row that carries
// finalizationGenerationSlotKey with GENERATION_FINALIZATION_CONTEXT_INVALID
// (synthetic_graph_linux.go:156) because it cannot prove the stored context, and
// the renderer asks that read before every begin and before any resume
// (src/services/finalization-generation.ts:38). A begin served here would hand
// the author a slot whose own next read throws. That read lives in the graph
// family's synthetic_graph_linux.go; lifting it means teaching that read the
// context proof, not widening the ledger.
//
// # execute and commit
//
// Both are downstream of begin and neither can be honest without it. execute
// dispatches the task begin froze, and the family's own task read refuses a run
// without it; commit stamps a settled artifact from an attempt execute produced,
// so with no artifact every request is GENERATION_FINALIZATION_ARTIFACT_INVALID
// (electron/services/finalization-generation.ts:53-59). The domain write commit
// would have to be atomic with does not exist in this module either - the three
// inventoried denials recorded in production_finalization_generation_linux.go
// (db:continuity-save-finalized and db:continuity-save-character-state-candidates
// at internal/transport/summary.go:45, db:blueprint-update-notes at
// internal/transport/blueprints.go:26) - so even a commit over a forged artifact
// could not move an author row.
//
// What this file adds is the machine-readable half of that answer: one row per
// name, the field the reference freezes and this lane cannot produce, the code a
// handler would have to answer, and the declarations that must exist first.

const (
	// productionFinalizationGenerationWriteTemplateAsset is where the two
	// templates this family renders actually live. It is the one seam that is not
	// a Go file: the renderer's own asset, which this module has no copy of.
	productionFinalizationGenerationWriteTemplateAsset = "../../../src/services/builtin-prompt-templates.ts"
	// The three codes below are the reference's own refusals for the missing
	// field: an unprovable context (finalization-generation-source.ts:44), a run
	// whose frozen task cannot be re-proved (finalization-generation.ts:49) and an
	// artifact no attempt of this run produced (finalization-generation.ts:53-58).
	productionFinalizationGenerationWriteContextInvalid  = "GENERATION_FINALIZATION_CONTEXT_INVALID"
	productionFinalizationGenerationWriteTaskInvalid     = "GENERATION_FINALIZATION_TASK_INVALID"
	productionFinalizationGenerationWriteArtifactInvalid = "GENERATION_FINALIZATION_ARTIFACT_INVALID"
)

// productionFinalizationGenerationWriteDecision is one refused name's write half:
// the field the reference's begin freezes and this lane has no producer for, the
// code a handler would have to answer, and the declarations that must exist
// before that field can be frozen. It is deliberately keyed by field rather than
// by declaration, because the declarations already have a home in
// production_finalization_generation_linux.go's seam table; this table says what
// a handler would be missing, which is the question the wire contract asks.
type productionFinalizationGenerationWriteDecision struct {
	Operation string
	Field     string
	Code      string
	Missing   []productionFinalizationLine
}

// productionFinalizationGenerationWriteDecisions is the whole answer of this
// file, in the order the desktop controller registers the three names
// (electron/controllers/generation-controller.ts:224-226).
func productionFinalizationGenerationWriteDecisions() []productionFinalizationGenerationWriteDecision {
	return []productionFinalizationGenerationWriteDecision{
		{
			Operation: productionFinalizationBegin,
			Field:     "finalizationGenerationContext.template",
			Code:      productionFinalizationGenerationWriteContextInvalid,
			Missing: []productionFinalizationLine{
				// The template the step renders: two keys, both only in the
				// renderer's asset.
				{Path: productionFinalizationGenerationWriteTemplateAsset, Line: 1063, Marker: "key: 'generate_chapter_notes',"},
				{Path: productionFinalizationGenerationWriteTemplateAsset, Line: 1119, Marker: "key: 'update_character_cards',"},
				// The one template this module resolves, which is not this family's.
				{Path: "../../internal/prompttemplate/editselectedtext.go", Line: 165, Marker: "func EditSelectedTextTemplate(language string) (Template, error) {"},
				// The member the context freezes from it.
				{Path: "../../../electron/services/finalization-generation-source.ts", Line: 35, Marker: "template: structuredClone(template),"},
				// The served read that refuses any row carrying the slot key, so a
				// begun run would be unobservable even with the template.
				{Path: "synthetic_graph_linux.go", Line: 156, Marker: productionFinalizationGenerationWriteContextInvalid},
			},
		},
		{
			Operation: productionFinalizationExecute,
			Field:     "sourceManifest.finalizationGenerationTask",
			Code:      productionFinalizationGenerationWriteTaskInvalid,
			Missing: []productionFinalizationLine{
				// The task is rendered from the context's template, so the missing
				// asset is the same gap one step later.
				{Path: "../../../electron/services/finalization-generation-source.ts", Line: 55, Marker: "const prompt = renderPrompt(context.template, {"},
				// A run without a re-provable task is refused by the family's own read.
				{Path: "../../electron/services/finalization-generation.ts", Line: 49, Marker: productionFinalizationGenerationWriteTaskInvalid},
				// And there is no row for it to be read from.
				{Path: "synthetic_graph_linux.go", Line: 156, Marker: productionFinalizationGenerationWriteContextInvalid},
			},
		},
		{
			Operation: productionFinalizationCommit,
			Field:     "artifact",
			Code:      productionFinalizationGenerationWriteArtifactInvalid,
			Missing: []productionFinalizationLine{
				// No attempt of a run this lane can open ever settles an artifact.
				{Path: "../../electron/services/finalization-generation.ts", Line: 55, Marker: productionFinalizationGenerationWriteArtifactInvalid},
				// The domain writes the effect would have to be atomic with.
				{Path: "../../internal/transport/summary.go", Line: 45, Marker: "db:continuity-save-finalized"},
				{Path: "../../internal/transport/blueprints.go", Line: 26, Marker: "db:blueprint-update-notes"},
			},
		},
	}
}

// productionFinalizationGenerationWriteDecisionFor returns the documented
// decision for one name, or false when the name is not one of the three.
func productionFinalizationGenerationWriteDecisionFor(operation string) (productionFinalizationGenerationWriteDecision, bool) {
	for _, decision := range productionFinalizationGenerationWriteDecisions() {
		if decision.Operation == operation {
			return decision, true
		}
	}
	return productionFinalizationGenerationWriteDecision{}, false
}

// bindProductionFinalizationGenerationWrite installs the write half of the
// finalization family. It installs nothing, and it refuses to start a lane whose
// decision is undocumented: every one of the three names must name a refused
// field, the symbolic code a handler would answer, and at least one declaration
// that has to exist first. The day one of them gains a producer the table has to
// change with it instead of silently becoming a lie.
//
// The nil-server refusal is the same one every other binder in this package
// answers, so wiring it into production_open_linux.go cannot mask a construction
// bug. It is reached from bindProductionFinalizationGeneration, the binder the
// lane already calls, because the family's three names are one answer and a
// second unwired binder would be a note nobody runs.
func bindProductionFinalizationGenerationWrite(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	operations := productionFinalizationGenerationOperations()
	if len(operations) != len(productionFinalizationGenerationWriteDecisions()) {
		return errors.New("PRODUCTION_FINALIZATION_WRITE_DECISION_INCOMPLETE")
	}
	for _, operation := range operations {
		decision, found := productionFinalizationGenerationWriteDecisionFor(operation)
		if !found {
			return errors.New("PRODUCTION_FINALIZATION_WRITE_DECISION_INCOMPLETE")
		}
		if decision.Field == "" || decision.Code == "" || len(decision.Missing) == 0 {
			return errors.New("PRODUCTION_FINALIZATION_WRITE_DECISION_INCOMPLETE")
		}
	}
	// Nothing is bound: the three names keep failing closed at
	// GO_OPERATION_NOT_MIGRATED (internal/transport/server.go:116), exactly the
	// answer production_finalization_generation_linux_test.go already pins.
	return nil
}
