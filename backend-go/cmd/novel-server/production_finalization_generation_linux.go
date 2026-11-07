//go:build linux

package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is where the finalization generation family's three remaining names
// would be served, and it serves none of them. The two names the family already
// answers are bound elsewhere and are not touched here:
//
//	finalization-generation:read     production_graph_reads_linux.go:68
//	finalization-generation:cancel   production_generation_write_linux.go:153
//
// The three that remain stay at the fail-closed 501
// (GO_OPERATION_NOT_MIGRATED, internal/transport/server.go:116). Each of them
// needs state this lane cannot freeze or a port this lane does not own, and a
// handler installed over a missing piece is worse than an unbound name: the
// browser would get a 5xx - or, worse, a claimed run whose own slot read then
// refuses it - instead of the plain 501 that says the server cannot do this yet.
// productionGraphReads (production_graph_reads_linux.go:15-27) records the same
// judgement for graph-generation:read.
//
// # begin
//
// finalization-generation:begin answers with a FinalizationGenerationRecovery
// whose context IS the frozen finalization context
// (src/shared/finalization-generation.ts:11): the slot, the finalized identity,
// the chapter title and entities, the writing language, the resolved
// PromptTemplate and the notes baseline. What has to move is listed below, and
// one missing field is enough to refuse a freeze:
//
//   - The reduced admission has no finalization slot: syntheticSelection
//     (synthetic_generation_linux.go:386) carries no field for it and
//     syntheticParseSelection's key allowlist (:410) rejects one, so a
//     selection-shaped begin is refused with GENERATION_BEGIN_INVALID. That is a
//     statement about the selection, not about the run: a write half that
//     hand-writes its manifest the way production_editor_inline_write_linux.go
//     does for editorInline* does not have to carry it in the selection at all.
//   - syntheticManifest (synthetic_generation_linux.go:736-758) writes no
//     finalizationGenerationSlotKey, no finalizationGenerationContext and no
//     finalization task, so no run opened through openRun can name a slot - and
//     the slot key is the only key the served read matches
//     (synthetic_graph_linux.go:116).
//   - The ledger is NOT the seam, and this bullet used to say it was. A re-test
//     against the openRun pattern (production_finalization_generation_write_linux.go,
//     TestProductionFinalizationGenerationWriteLedgerProbe) shows the opposite:
//     glFamilyManifestAllowed (internal/store/generation_ledger.go:426) admits all
//     seven finalization keys for both finalization operations (:447, and the same
//     two for the effect at :477), and glPlanning (:492) is a name test that
//     "finalized-chapter-notes" and "finalized-character-state" do not match, so
//     neither gate applied at :518 or :694 touches this family. A hand-written
//     CallJSON("open", ...) request can name this run; the ledger row that used to
//     be cited here is dropped.
//   - The context itself is not assemblable here, and the gap is one field:
//     `template`. Six of the seven members have a Go producer - slot
//     (syntheticFinalizationSlotKey, which the served read already validates),
//     identity (store.FinalizedCharacters.ReadContext, served by
//     production_finalized_character_linux.go), chapterTitle
//     (finalization_outbox.chapter_title), chapterEntities (blueprints.characters),
//     writingLanguage (the project core) and notesBaseline (blueprints.notes plus
//     the draft's summary_snapshots rows). The template does not:
//     captureFinalizationGenerationContext resolves it from the renderer's asset
//     (electron/services/finalization-generation-source.ts:29, :35) and this
//     module resolves exactly one prompt template, edit_selected_text
//     (internal/prompttemplate/editselectedtext.go:165). The task the desktop
//     builds from it (electron/services/finalization-generation-source.ts:47-59)
//     is a prompt author, not a projection - which is why begin stays refused.
//
// A claimed slot would also be unreadable: FinalizationRead refuses every row
// that carries the slot key with GENERATION_FINALIZATION_CONTEXT_INVALID
// (synthetic_graph_linux.go:156) because it cannot prove the frozen context
// digest. The renderer asks that read first (src/services/finalization-generation.ts:38),
// so a begin written by this file would hand the author a run whose very next
// read throws.
//
// # execute
//
// The earlier reason for this name was that the contract carries no task, so a
// handler would have to author one. That is wrong, and the sharper seam is
// worse for it: main freezes the task at begin (generation-source-binding.ts:590-591)
// and execute only re-proves its hash (electron/services/finalization-generation.ts:46-49),
// so reading a frozen task back would not make this lane a prompt author. What
// is missing is everything around that task:
//
//   - No row can become this session's run. begin is the refusal documented
//     above, and the served read refuses every row that does carry the slot key
//     with GENERATION_FINALIZATION_CONTEXT_INVALID (synthetic_graph_linux.go:156)
//     because it cannot prove the frozen context.
//   - The freeze has no Go producer. finalizationGenerationTask
//     (electron/services/finalization-generation-source.ts:47) renders a
//     PromptTemplate through renderPrompt, and no Go file in this module names
//     either; the task and context keys exist here only as allowlist entries
//     (internal/store/generation_ledger.go:437).
//   - The one dispatch path this lane owns takes its task from the browser
//     request (synthetic_generation_linux.go:1337) and its nonce from the same
//     request (:1306). This contract carries neither
//     (src/shared/finalization-generation.ts:42), and the frozen task it does
//     have is reachable only after the context proof above.
//   - The reference execute is not one call. It runs up to three ordinals when a
//     character-cards answer does not parse, and every repair task is authored
//     from literal bilingual prose in main
//     (electron/services/main-generation-owner.ts:1285-1303). Porting that
//     would be authoring prompts in Go, which this lane forbids outright.
//
// # commit
//
// The reference commit is one IMMEDIATE transaction over the frozen context:
// readFinalizationGenerationContext first, the notes-baseline CAS, then the
// domain writes and the effect stamped into the attempt's usage receipt
// (electron/services/finalization-generation.ts:86-114). The stamp itself is
// not the gap - the ledger can patch usage_receipt_json with finalizationEffect
// (internal/store/generation_ledger.go:471, recordEffect at :1211). The gap is
// the domain write the stamp has to be atomic with. Its writers are inventoried
// denials in this module:
//
//	db:continuity-save-finalized                     internal/transport/summary.go:45
//	db:continuity-save-character-state-candidates    internal/transport/summary.go:45
//	db:blueprint-update-notes                        internal/transport/blueprints.go:26
//
// all three answering GENERATION_FINALIZATION_ADMISSION_REQUIRED
// (internal/transport/summary.go:125), and the store behind them can read a
// finalized source (internal/store/summary.go:446) but has no finalized
// continuity writer and no blueprint notes updater - production_blueprint_write_linux.go:40-49
// already records that judgement for the notes write: it is "deliberately NOT
// bound, here or anywhere else in this lane", because satisfying it needs
// exactly this admission plus the artifact/ACK transaction, and "there is no
// UpdateNotes store method at all". The receipt is not a projection either:
// chapter_notes needs buildFinalizedContinuityFacts and character_cards needs
// the finalized proof, neither of which exists in Go. A commit also needs a
// settled artifact from an attempt, which only execute can produce. With begin
// and execute refused there is no artifact and no provable context, so a commit
// handler would answer GENERATION_FINALIZATION_ARTIFACT_INVALID for its refused
// field and GENERATION_FINALIZATION_CONTEXT_INVALID for the run behind it -
// never a domain write.
//
// What is left is this: the exact seam, name by name, so the next change starts
// from the code that has to move instead of from a guess.

// The three names this file refuses, spelled once so a test and the seam table
// cannot drift from the binder.
const (
	productionFinalizationBegin   = "finalization-generation:begin"
	productionFinalizationExecute = "finalization-generation:execute"
	productionFinalizationCommit  = "finalization-generation:commit"
)

// productionFinalizationLine is one declaration or refusal that has to exist
// before a name can be served: the path of the Go file, the line inside it, and a
// marker that must be on that line. TestProductionLaneRefusesFinalizationGeneration
// re-reads every one of them, so a moved declaration fails this lane's test
// instead of leaving a note that points at nothing.
type productionFinalizationLine struct {
	Path   string
	Line   int
	Marker string
}

// productionFinalizationSeam is one refused name together with the missing piece
// that keeps it unbound.
type productionFinalizationSeam struct {
	Operation string
	Missing   []productionFinalizationLine
}

// productionFinalizationGenerationOperations is the family's three remaining
// names, in the order the desktop controller registers them
// (electron/controllers/generation-controller.ts:224-226). read and cancel are
// deliberately absent: they are served by other files.
func productionFinalizationGenerationOperations() []string {
	return []string{productionFinalizationBegin, productionFinalizationExecute, productionFinalizationCommit}
}

// productionFinalizationGenerationSeams is the whole answer of this file: for
// each refused name, the exact code that has to move first.
func productionFinalizationGenerationSeams() []productionFinalizationSeam {
	return []productionFinalizationSeam{
		{
			Operation: productionFinalizationBegin,
			Missing: []productionFinalizationLine{
				{Path: "synthetic_generation_linux.go", Line: 736, Marker: "func syntheticManifest("},
				{Path: "synthetic_graph_linux.go", Line: 156, Marker: "GENERATION_FINALIZATION_CONTEXT_INVALID"},
				// The refused field: the step's template exists only in the
				// renderer's asset, and this module resolves one unrelated one
				// (production_finalization_generation_write_linux.go).
				{Path: productionFinalizationGenerationWriteTemplateAsset, Line: 1063, Marker: "key: 'generate_chapter_notes',"},
				{Path: "../../internal/prompttemplate/editselectedtext.go", Line: 165, Marker: "func EditSelectedTextTemplate(language string) (Template, error) {"},
			},
		},
		{
			Operation: productionFinalizationExecute,
			Missing: []productionFinalizationLine{
				{Path: "synthetic_generation_linux.go", Line: 1337, Marker: "request[\"task\"]"},
				{Path: "synthetic_generation_linux.go", Line: 1306, Marker: "request[\"invocationNonce\"]"},
				{Path: "synthetic_graph_linux.go", Line: 156, Marker: "GENERATION_FINALIZATION_CONTEXT_INVALID"},
				// The task is rendered from the same template the begin above cannot
				// freeze, so the field-level seam is the same one step later.
				{Path: "../../../electron/services/finalization-generation-source.ts", Line: 55, Marker: "const prompt = renderPrompt(context.template, {"},
			},
		},
		{
			Operation: productionFinalizationCommit,
			Missing: []productionFinalizationLine{
				{Path: "../../internal/transport/summary.go", Line: 45, Marker: "db:continuity-save-finalized"},
				{Path: "../../internal/transport/summary.go", Line: 125, Marker: "GENERATION_FINALIZATION_ADMISSION_REQUIRED"},
				{Path: "../../internal/transport/blueprints.go", Line: 26, Marker: "db:blueprint-update-notes"},
				{Path: "../../internal/store/summary.go", Line: 446, Marker: "func (s *Summary) ReadFinalizedSource"},
				{Path: "production_blueprint_write_linux.go", Line: 40, Marker: "deliberately NOT bound"},
				{Path: "../../internal/store/generation_ledger.go", Line: 471, Marker: "finalizationEffect"},
			},
		},
	}
}

// productionFinalizationGenerationSeam returns the documented missing piece for
// one name, or false when the name is not one of the three.
func productionFinalizationGenerationSeam(operation string) (productionFinalizationSeam, bool) {
	for _, seam := range productionFinalizationGenerationSeams() {
		if seam.Operation == operation {
			return seam, true
		}
	}
	return productionFinalizationSeam{}, false
}

// bindProductionFinalizationGeneration installs the three remaining names of the
// finalization generation family. It installs nothing, and it refuses to start a
// lane whose refusal is undocumented: every one of the three must carry at least
// one named missing piece, so the day one of them gains a body the table has to
// change with it instead of silently becoming a lie.
//
// The nil-server refusal is the same one every other binder in this package
// answers, so wiring this into production_open_linux.go cannot mask a
// construction bug.
//
// The import family's two model-executing names are one answer with these three,
// so this binder also validates the import seam table
// (production_import_finalization_generation_linux.go). That keeps one wired
// entry point for the four names instead of a second binder nothing calls.
//
// The write half's own decision table is validated here too
// (production_finalization_generation_write_linux.go): the seam table above says
// which declarations have to move, that file says which field of the wire
// contract is refused, and one wired entry point keeps the two from drifting into
// two answers for the same three names.
func bindProductionFinalizationGeneration(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	for _, operation := range productionFinalizationGenerationOperations() {
		seam, found := productionFinalizationGenerationSeam(operation)
		if !found || len(seam.Missing) == 0 {
			return errors.New("PRODUCTION_FINALIZATION_SEAM_INCOMPLETE")
		}
	}
	if err := bindProductionFinalizationGenerationWrite(server); err != nil {
		return err
	}
	if err := bindProductionImportGeneration(server); err != nil {
		return err
	}
	// Nothing is bound: the five names keep failing closed at
	// GO_OPERATION_NOT_MIGRATED (internal/transport/server.go:116), exactly the
	// answer the desktop-mirroring tests already pin for the synthetic lane
	// (synthetic_generation_http_linux_test.go:475-479).
	return nil
}
