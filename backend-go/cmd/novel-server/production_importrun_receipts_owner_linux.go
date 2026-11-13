//go:build linux

package main

import (
	"os"
	"strings"
)

// This file is the WIRING RECORD for the import-run receipt pair, and it
// publishes no handler of its own: the handlers live in
// production_importrun_receipts_serve2_linux.go, and the seam that used to keep
// the pair at GO_OPERATION_NOT_MIGRATED is gone for both names
// (production_importrun_receipts_linux.go now holds prepare-inspection only).
//
// # What this file is for
//
// The pair needs the generation owner, and the owner is a local variable of one
// block:
//
//	production_open_linux.go:591  if owner, ownerErr := newProductionGeneration(open, server, config.Dispatch); ownerErr == nil {
//
// This pass may not edit that file, so the binder is installed by exactly one
// line that a pass allowed to edit it must add. Everything else already exists:
// four sibling families are installed from the same block the same way
// (newProductionFinalizationServe(owner), newProductionLegacyRosterServe(owner),
// newProductionAgentGenerationServe(owner), newProductionImportGenerationServe(owner)),
// and productionImportRunReceiptsServe2 is the fifth. The line, and where it
// goes, are one value here - productionImportRunReceiptsOwnerWiringLineOf - so the
// test, this comment and the report cannot drift:
//
//	inside the owner block, directly after
//	  production_open_linux.go:720  if bindErr := newProductionImportGenerationServe(owner).Bind(server); bindErr != nil {
//	(the `}` that closes that statement is line 723) and before the block's closing
//	  production_open_linux.go:724  } else {
//	the line is
//	  if bindErr := newProductionImportRunReceiptsServe2(owner).Bind(server); bindErr != nil {
//	      _ = open.Close()
//	      return nil, nil, bindErr
//	  }
//
// With it the served count moves 274 -> 276. Without it the two names stay at
// GO_OPERATION_NOT_MIGRATED and the served count stays 274, because
// productionImportRunReceiptsServe2.Bind installs nothing for a lane with no
// owner: no handler that can only refuse is ever published.
//
// # Why the owner and not just the durable open
//
// The proof's fourth step is generationowner.ReproduceBinding
// (internal/generationowner/binding.go:63), and the lease record it resolves is
// process memory minted by the begin (internal/generationowner/lease.go:164). A
// registry cannot be rebuilt from `open.durable`, so a binder constructed with the
// durable alone could only answer with a refusal. That is the whole difference
// this file records.
//
// # Why the callback cannot do the read itself, one more time
//
// The assert runs inside the receipt's BEGIN IMMEDIATE
// (internal/store/importrun.go:948 importRunImmediate reserves one connection;
// internal/projectfs/author_linux.go:121 opens the author pool with
// db.SetMaxOpenConns(1)). A prover that opened its own handle to read the
// generation run waited for that connection and ended in
// context.DeadlineExceeded - measured, not argued, by the previous pass. The
// store now performs that read on the connection it already holds and hands the
// run over as data (internal/store/importrun_receipts.go:130,
// internal/store/importrun_receipts_generation.go), so the prover reads no
// database at all: a prompt-only import selection means even the reproduction is
// pure (production_importrun_receipts_serve2_linux.go
// productionImportRunReceiptsServe2Sources).
//
// # What remains outside this family
//
// Two pins outside this pass's scope have to move in the same change as the
// owner-block line, or CI reports them instead of the lane: the served-count pin
// (production_served_count_linux_test.go:24, 274 -> 276) and the db family's
// residual cover assertion, which refuses a name that is both bound and listed
// (production_db_residual_linux_test.go:62-65). Neither file belongs to this
// family, and this pass does not edit either.

const (
	// productionImportRunReceiptsOwnerBlockLine opens the block that owns the
	// generation registry, its model-profile loader and its source readers.
	productionImportRunReceiptsOwnerBlockLine = "if owner, ownerErr := newProductionGeneration(open, server, config.Dispatch); ownerErr == nil {"
	// productionImportRunReceiptsOwnerLateOwnerLine is an owner-bound binder that is
	// already installed from the same block. The new line goes immediately after it,
	// so the ordering the test checks has one stable anchor on both sides.
	productionImportRunReceiptsOwnerLateOwnerLine = "newProductionImportGenerationServe(owner).Bind(server)"
	// productionImportRunReceiptsOwnerServe2Binder is the binder this pass adds and
	// the argument it needs. It is the literal call the owner-block line makes.
	productionImportRunReceiptsOwnerServe2Binder = "newProductionImportRunReceiptsServe2(owner).Bind(server)"
	// productionImportRunReceiptsOwnerResidualBinder is the neighbour the new line
	// sits beside; it stays one-argument on purpose, because this adapter holds no
	// owner and must not be handed a fake one.
	productionImportRunReceiptsOwnerResidualBinder = "newProductionImportRunResiduals(open.durable).Bind(server)"
	// productionImportRunReceiptsOwnerBinderInstall is how the new line must be
	// guarded: the same bindErr idiom every sibling uses, so a failed bind closes
	// the lane instead of serving a half-installed family.
	productionImportRunReceiptsOwnerBinderInstall = "if bindErr := newProductionImportRunReceiptsServe2(owner).Bind(server); bindErr != nil {"
	// productionImportRunReceiptsOwnerAssertSignature is the widened callback: it
	// now carries the run the store read on its own connection, which is what made
	// the deadlock go away.
	productionImportRunReceiptsOwnerAssertSignature = "type ImportRunGenerationSourceAssert func(handle any, slot ImportRunGenerationSlot, run ImportRunGenerationEvidence) error"
	// productionImportRunReceiptsOwnerEvidenceRead is the store's own read, on the
	// transaction's connection.
	productionImportRunReceiptsOwnerEvidenceRead = "func importRunGenerationEvidenceRead(ctx context.Context, q importRunQueryer, runID string) (ImportRunGenerationEvidence, error)"
	// productionImportRunReceiptsOwnerBindingColumn is the column the evidence is
	// built from, and the one requireRun's row read returns.
	productionImportRunReceiptsOwnerBindingColumn = "SELECT run_id, root_action_id, binding_json FROM generation_runs WHERE run_id = ?"
	// productionImportRunReceiptsOwnerPoolLine is the one connection the receipt
	// transaction holds while the assert runs.
	productionImportRunReceiptsOwnerPoolLine = "db.SetMaxOpenConns(1)"
	// productionImportRunReceiptsOwnerDeadlockLine is how the store already
	// describes a prover that takes a second connection.
	productionImportRunReceiptsOwnerDeadlockLine = "which is a deadlock rather than a proof"
	// productionImportRunReceiptsOwnerImmediateLine is the reservation the assert
	// runs inside.
	productionImportRunReceiptsOwnerImmediateLine = "func (r *ImportRuns) importRunImmediate(ctx context.Context, fn func(*sql.Conn) error) (retErr error)"
	// productionImportRunReceiptsOwnerReproduceLine is the reproduction the proof
	// performs.
	productionImportRunReceiptsOwnerReproduceLine = "func (r *Registry) ReproduceBinding("
	// productionImportRunReceiptsOwnerAssertLine is the reference's requireRun call,
	// the first step the old callback could not take.
	productionImportRunReceiptsOwnerAssertLine = "const run = requireRun(handle)"
	// productionImportRunReceiptsOwnerCommitWiring is the commit half of the
	// reference's own wiring, whose disjunction with prepare is why the served
	// handlers install one proof on both legs.
	productionImportRunReceiptsOwnerCommitWiring = "(handle, slot) => assertImportGenerationSourcesCurrent(handle, slot, true)"
)

// productionImportRunReceiptsOwnerWiring is the one edit this family still needs,
// as text: the file, the anchor it goes after, and the literal line. It is a value
// rather than a comment so the report and the test read the same string.
type productionImportRunReceiptsOwnerWiring struct {
	Path   string
	Anchor string
	Line   string
}

// productionImportRunReceiptsOwnerWiringLineOf is the wiring, spelled once.
func productionImportRunReceiptsOwnerWiringLineOf() productionImportRunReceiptsOwnerWiring {
	return productionImportRunReceiptsOwnerWiring{
		Path:   "production_open_linux.go",
		Anchor: productionImportRunReceiptsOwnerLateOwnerLine,
		Line:   productionImportRunReceiptsOwnerServe2Binder,
	}
}

// productionImportRunReceiptsOwnerInstalled reports whether the shipped lane
// already installs the binder. It reads the source tree and is only called from
// the test: the production binary's working directory is not the repository, and a
// startup that read the tree would fail for a reason unrelated to the author's
// database.
func productionImportRunReceiptsOwnerInstalled() bool {
	raw, err := os.ReadFile(productionImportRunReceiptsOwnerWiringLineOf().Path)
	if err != nil {
		return false
	}
	// The wiring line is a call expression, which gofmt leaves byte-identical, so a
	// plain containment is enough here; productionSeamContains lives in the test.
	return strings.Contains(string(raw), productionImportRunReceiptsOwnerServe2Binder)
}

// productionImportRunReceiptsOwnerFact is one declaration the audit above quotes,
// with the file and line this pass last saw it at. The test matches the marker
// with productionSeamContains, because CI formats the source before it runs, so
// the recorded line is a diagnosis rather than an assertion.
type productionImportRunReceiptsOwnerFact struct {
	Path   string
	Line   int
	Marker string
}

// productionImportRunReceiptsOwnerFacts is the audit, as a list: the owner block,
// the anchor the new line goes after, the widened callback, the store's own read,
// the one connection the receipt transaction holds, the store's own verdict on a
// second connection, and the reference's assert.
func productionImportRunReceiptsOwnerFacts() []productionImportRunReceiptsOwnerFact {
	return []productionImportRunReceiptsOwnerFact{
		{"production_open_linux.go", 591, productionImportRunReceiptsOwnerBlockLine},
		{"production_open_linux.go", 720, productionImportRunReceiptsOwnerLateOwnerLine},
		{"production_open_linux.go", 627, productionImportRunReceiptsOwnerResidualBinder},
		{"../../internal/store/importrun_receipts.go", 130, productionImportRunReceiptsOwnerAssertSignature},
		{"../../internal/store/importrun_receipts_generation.go", 71, "type ImportRunGenerationEvidence struct {"},
		{"../../internal/store/importrun_receipts_generation.go", 148, productionImportRunReceiptsOwnerEvidenceRead},
		{"../../internal/store/importrun_receipts_generation.go", 171, productionImportRunReceiptsOwnerBindingColumn},
		{"../../internal/store/importrun_receipts_generation.go", 205, "SELECT json_extract(action_json,'$.status') FROM generation_roots WHERE root_action_id = ?"},
		{"../../internal/store/importrun_receipts_generation.go", 232, "SELECT attempt_id, usage_receipt_json FROM generation_attempts WHERE run_id = ? ORDER BY rowid"},
		{"../../internal/store/importrun_receipts_generation.go", 259, "SELECT artifact_id, status, artifact_json FROM generation_artifacts WHERE attempt_id = ?"},
		{"../../internal/store/importrun.go", 948, productionImportRunReceiptsOwnerImmediateLine},
		{"../../internal/projectfs/author_linux.go", 121, productionImportRunReceiptsOwnerPoolLine},
		{"../../internal/store/character_proposal_pending.go", 37, productionImportRunReceiptsOwnerDeadlockLine},
		{"../../internal/generationowner/binding.go", 63, productionImportRunReceiptsOwnerReproduceLine},
		{"../../../electron/services/main-generation-owner.ts", 1454, productionImportRunReceiptsOwnerAssertLine},
		{"../../../electron/services/main-generation-owner.ts", 1462, "imports.context(run).slot"},
		{"../../../electron/services/main-generation-owner.ts", 1471, "GENERATION_IMPORT_EFFECT_SOURCE_CHANGED"},
		{"../../../electron/controllers/db-controller.ts", 437, productionImportRunReceiptsOwnerCommitWiring},
		{"production_importrun_receipts_serve2_linux.go", 0, "func newProductionImportRunReceiptsServe2(owner *productionGeneration) *productionImportRunReceiptsServe2 {"},
	}
}

// productionImportRunReceiptsOwnerReport is the one-line wiring report, in the
// form the pass's own summary uses: file, anchor, literal line.
func productionImportRunReceiptsOwnerReport() string {
	wiring := productionImportRunReceiptsOwnerWiringLineOf()
	return wiring.Path + ": after " + wiring.Anchor + " -> " + wiring.Line
}
